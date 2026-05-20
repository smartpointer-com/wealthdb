package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// webReader reads from the ubs-web-dump silver SQLite. It owns
// the splice cutoff: every record passed downstream has either no
// known PSN counterpart (no cutoff) or a value/snapshot timestamp
// strictly less than PSN-start for its banking relationship.
//
// PSN-start per relationship is derived once on first
// Transactions/Snapshots call from the configured RelationshipPair
// list and the *psnReader handle; results are cached on the
// webReader (single-use Connection lifecycle).
type webReader struct {
	db   *sql.DB
	path string
}

func (r *webReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// Status mirrors the canonical.Status contract using web silver
// columns. Snapshot times come from `dump_runs`; transaction
// times come from `transactions.value_date`. The LatestChange-
// Number is MAX of either (whichever advanced).
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	out := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	// dump_runs always has at least one row per snapshot, even
	// when transactions/documents tables are empty for that run.
	err := r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(snapshot_at), -1),
               COALESCE(MAX(snapshot_at), -1)
          FROM dump_runs`).Scan(&out.OldestSnapshotAt, &out.LatestSnapshotAt)
	if err != nil {
		return canonical.Status{}, fmt.Errorf("ubs-web Status snapshots: %w", err)
	}
	err = r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(value_date), -1),
               COALESCE(MAX(value_date), -1)
          FROM transactions`).Scan(&out.OldestTransactionAt, &out.LatestTransactionAt)
	if err != nil {
		return canonical.Status{}, fmt.Errorf("ubs-web Status transactions: %w", err)
	}
	// Historical (PDF) snapshots extend OldestSnapshotAt back —
	// PDFs cover dates that pre-date the first live web dump.
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil {
		return canonical.Status{}, err
	}
	if ok {
		histLo, histHi, err := r.historicalRange(ctx)
		if err != nil {
			return canonical.Status{}, err
		}
		if histLo >= 0 {
			if out.OldestSnapshotAt == -1 || histLo < out.OldestSnapshotAt {
				out.OldestSnapshotAt = histLo
			}
		}
		if histHi >= 0 && histHi > out.LatestSnapshotAt {
			out.LatestSnapshotAt = histHi
		}
	}
	out.LatestChangeNumber = maxInt64(out.LatestSnapshotAt, out.LatestTransactionAt)
	return out, nil
}

// ChangeWindow returns the union of new snapshots and new
// transactions strictly after `since`. Matches the PSN reader's
// convention so the merge layer's combine is straightforward.
//
// Historical (PDF-reconstructed) data extends Start backwards
// when there's any new live content. The historical_position_
// snapshots and historical_cash_balances tables key on
// as_of_date / period_end / period_start (well before the live
// dump_run watermark), so if the loader's delete-then-insert
// window stayed at live-Start, re-emitting historical rows on
// the next load would collide on the gold PK. Including the
// historical range in Start guarantees the window-DELETE covers
// any existing historical gold rows before they're re-inserted.
//
// NewChangeNumber stays a live-time concept (MAX over snapshot_at
// / value_date) — the watermark advances only when live data
// advances. This means re-running with no new dump_run is a
// no-op even when historical data is present.
func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	var (
		snapMin, snapMax sql.NullInt64
		txMin, txMax     sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(snapshot_at), MAX(snapshot_at)
          FROM dump_runs WHERE snapshot_at > ?`, since).Scan(&snapMin, &snapMax); err != nil {
		return canonical.Window{}, fmt.Errorf("ubs-web ChangeWindow snapshots: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(value_date), MAX(value_date)
          FROM transactions WHERE value_date > ?`, since).Scan(&txMin, &txMax); err != nil {
		return canonical.Window{}, fmt.Errorf("ubs-web ChangeWindow transactions: %w", err)
	}
	w := canonical.Window{NewChangeNumber: since}
	merge := func(n sql.NullInt64) {
		if !n.Valid {
			return
		}
		w.HasChanges = true
		if w.NewChangeNumber < n.Int64 {
			w.NewChangeNumber = n.Int64
		}
	}
	mins, maxs := []sql.NullInt64{snapMin, txMin}, []sql.NullInt64{snapMax, txMax}
	for _, m := range mins {
		if m.Valid && (!w.HasChanges || m.Int64 < w.Start) {
			w.Start = m.Int64
		}
	}
	for _, m := range maxs {
		if m.Valid && m.Int64 > w.End {
			w.End = m.Int64
		}
	}
	merge(snapMax)
	merge(txMax)

	if w.HasChanges {
		ok, err := r.hasHistoricalTables(ctx)
		if err != nil {
			return canonical.Window{}, err
		}
		if ok {
			histLo, histHi, err := r.historicalRange(ctx)
			if err != nil {
				return canonical.Window{}, err
			}
			if histLo >= 0 && histLo < w.Start {
				w.Start = histLo
			}
			if histHi > w.End {
				w.End = histHi
			}
		}
	}
	return w, nil
}

// snapshotsForOverlap emits dimensions (portfolios, accounts,
// instruments) from web's live snapshots inside the window, with
// each row filtered to snapshot_at < PSN-start for its banking
// relationship. Positions and cash flow through PSN's snapshot
// stream instead; the orchestrator wraps PSN with a fold stream
// that injects web's per-(date, key) payload into PSN's rows.
//
// This split keeps PSN's faithful safekeeping / portfolio
// structure (web silver flattens all securities under one
// PrtflId) while still preserving web-only
// fields like cost_price inside `payload.web`. Web's instrument
// descriptions are a strict upgrade over PSN's InstrNm.LngNm-
// English (which is colon-formatted and less user-readable), so
// they're emitted here and let the per-column upsert guard pick
// the latest snapshot's name.
//
// Historical PDF snapshots are handled by snapshotsHistorical
// (historical.go); transactions are handled by
// transactionsBeforePSNStart.
func (r *webReader) snapshotsForOverlap(
	ctx context.Context,
	w canonical.Window,
	cutoffByWebRel map[string]int64,
	psnAssetClass map[string]canonical.AssetClass,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}
	times, err := r.dumpRunTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	// Dimensions only — portfolios, accounts, instruments.
	// Positions / cash come from the PSN side (with web payload
	// folded in by the orchestrator). Web's instruments-via-
	// description are a strict upgrade over PSN's InstrNm.LngNm-
	// English (which is colon-formatted and less user-readable),
	// so emit them here and let the per-column upsert guard pick
	// the latest snapshot's name.
	if err := r.appendWebPortfolios(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebAccounts(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebInstruments(ctx, w, byTime, psnAssetClass); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		b := byTime[t]
		if len(b.Portfolios)+len(b.Accounts)+len(b.Instruments) == 0 {
			continue
		}
		out.batches = append(out.batches, *b)
	}
	return out, nil
}

// appendWebInstruments emits one InstrumentChange per (snapshot,
// ISIN) in the window using the web positions.description as the
// instrument's user-facing Name. ISINs that appear multiple
// times in the same snapshot (cross-portfolio holdings) coalesce
// to one emission with the description from the first row seen —
// the descriptions don't vary by portfolio.
//
// psnAssetClass maps ISIN → PSN's CFI-derived asset_class.
// Web doesn't know an instrument's class (no CFI), and a naive
// `AssetClassOther` emission would overwrite PSN's specific
// class via the per-column upsert guard (web's last_seen_at is
// typically later than PSN's). Stamping PSN's class
// keeps the cross-source upsert idempotent on asset_class while
// letting web win on Name. Missing ISINs (not in PSN) fall back
// to AssetClassOther.
func (r *webReader) appendWebInstruments(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, psnAssetClass map[string]canonical.AssetClass) error {
	const q = `
SELECT snapshot_at, instrument_isin, currency_iso, description
  FROM positions
 WHERE instrument_isin IS NOT NULL
   AND snapshot_at BETWEEN ? AND ?
 GROUP BY snapshot_at, instrument_isin`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebInstruments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap        int64
			isin        string
			ccy         string
			description sql.NullString
		)
		if err := rows.Scan(&snap, &isin, &ccy, &description); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		ac := canonical.AssetClassOther
		if c, ok := psnAssetClass[isin]; ok && c != "" {
			ac = c
		}
		isinCopy := isin
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           ac,
			ISIN:                 &isinCopy,
			Name:                 nullStringPtr(description),
			Currency:             strPtrIfNonEmpty(ccy),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
	}
	return rows.Err()
}

// transactionsBeforePSNStart emits web transactions strictly
// before the per-relationship PSN-start cutover. A hard cut (not
// an overlap merge) because the web and PSN sources use entirely
// different transaction_external_id schemes — web uses UBS
// Transaction No. (e.g. `0104030TXNNNNNNN`), PSN events use
// MT-prefixed strings (e.g. `mt515:...`). Any cross-source
// identity match would be heuristic and risk double-counting.
func (r *webReader) transactionsBeforePSNStart(ctx context.Context, w canonical.Window, psn *psnReader, rels []silver.RelationshipPair) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return &txStream{consumed: true}, nil
	}
	cutoff, err := buildPSNStartByWebRel(ctx, psn, rels)
	if err != nil {
		return nil, err
	}
	accountToRel, err := r.buildAccountToRelMap(ctx)
	if err != nil {
		return nil, err
	}

	const q = `
SELECT transaction_external_id, value_date, account_external_id,
       currency_iso, amount_debit, amount_credit, description_kind, payload
  FROM transactions
 WHERE value_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs-web Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			txID, accountID, ccy, payload string
			valueDate                     int64
			debit, credit                 sql.NullFloat64
			kindStr                       sql.NullString
		)
		if err := rows.Scan(&txID, &valueDate, &accountID, &ccy, &debit, &credit, &kindStr, &payload); err != nil {
			return nil, fmt.Errorf("ubs-web Transactions scan: %w", err)
		}
		// Hard cut at PSN_start per relationship.
		if rel, ok := accountToRel[accountID]; ok {
			if cut := cutoff[rel]; cut > 0 && valueDate >= cut {
				continue
			}
		}

		var net canonical.Decimal
		if credit.Valid {
			net = net.Add(canonical.NewDecimalFromFloat(credit.Float64))
		}
		if debit.Valid {
			net = net.Sub(canonical.NewDecimalFromFloat(debit.Float64))
		}
		netPtr := net

		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			// Web silver's transactions PK is the compound
			// (transaction_external_id, account_external_id) so
			// that FX trades and other multi-leg events appear as
			// separate rows per leg. Gold's transactions PK is
			// (silver_source_id, transaction_external_id), so we
			// synthesize a per-leg ID here. The natural
			// "Transaction no." remains in the payload for
			// downstream queries that want to reassemble the trade.
			TransactionExternalID: txID + "@" + accountID,
			OccurredAt:            valueDate,
			AccountExternalID:     accountID,
			Kind:                  webKind(kindStr.String, debit.Valid, credit.Valid),
			Currency:              ccy,
			NetAmount:             &netPtr,
			Payload:               json.RawMessage(payload),
		})
	}
	return &txStream{batch: out}, rows.Err()
}

// dumpRunTimesInWindow returns the chronologically-sorted set of
// snapshot_at values in [w.Start, w.End] from dump_runs.
func (r *webReader) dumpRunTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `SELECT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ? ORDER BY snapshot_at`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs-web dumpRunTimesInWindow: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// appendWebPortfolios projects web silver portfolios into
// PortfolioChange. Cutoff applies via banking_relationship_id.
func (r *webReader) appendWebPortfolios(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64) error {
	const q = `
SELECT snapshot_at, portfolio_external_id, banking_relationship_id,
       description, payload
  FROM portfolios
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebPortfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                int64
			extID, payload                      string
			relID, description                  sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &relID, &description, &payload); err != nil {
			return err
		}
		if relID.Valid {
			if cut := cutoff[relID.String]; cut > 0 && snap >= cut {
				continue
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
			PortfolioExternalID: extID,
			DisplayName:         nullStringPtr(description),
			RelationshipID:      nullStringPtr(relID),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// appendWebAccounts projects web silver accounts (cash and
// safekeeping; the schema's CHECK constraint already excludes
// card accounts). Cutoff applies via banking_relationship_id.
func (r *webReader) appendWebAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64) error {
	const q = `
SELECT snapshot_at, account_external_id, kind, currency_iso,
       banking_relationship_id, portfolio_external_id,
       description, payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                                  int64
			extID, kind, payload                                  string
			ccy, relID, portfolioID, description                 sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &kind, &ccy, &relID, &portfolioID, &description, &payload); err != nil {
			return err
		}
		if relID.Valid {
			if cut := cutoff[relID.String]; cut > 0 && snap >= cut {
				continue
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var ak canonical.AccountKind
		switch kind {
		case "cash":
			ak = canonical.AccountKindCash
		case "safekeeping":
			ak = canonical.AccountKindSafekeeping
		default:
			ak = canonical.AccountKindOther
		}
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         ak,
			DisplayName:         nullStringPtr(description),
			BaseCurrency:        nullStringPtr(ccy),
			RelationshipID:      nullStringPtr(relID),
			PortfolioExternalID: nullStringPtr(portfolioID),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// buildAccountToRelMap returns a web account_external_id → web
// banking_relationship_id lookup, taking the latest snapshot per
// account. Used by Transactions to find each row's relationship
// for the cutoff filter.
func (r *webReader) buildAccountToRelMap(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT a.account_external_id, a.banking_relationship_id
  FROM accounts a
  JOIN (SELECT account_external_id, MAX(snapshot_at) AS s
          FROM accounts GROUP BY account_external_id) m
    ON a.account_external_id = m.account_external_id
   AND a.snapshot_at = m.s
 WHERE a.banking_relationship_id IS NOT NULL`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("buildAccountToRelMap: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var acct, rel string
		if err := rows.Scan(&acct, &rel); err != nil {
			return nil, err
		}
		out[acct] = rel
	}
	return out, rows.Err()
}

// buildPSNStartByWebRel resolves the PSN-start cutoff per web
// banking_relationship_id using the config relationships pairing
// and the *psnReader (if configured). Returns an empty map when
// psn is nil (degenerate splice — every web row passes).
//
// Resolution order per relationship:
//   1. RelationshipPair.PSNStartOverride if non-zero.
//   2. MIN(snapshot_at) in PSN for the paired PSNID, otherwise.
//
// A relationship with no PSN counterpart (PSNID empty, or empty
// MIN) gets cutoff=0 → no filter applied to its web rows.
func buildPSNStartByWebRel(ctx context.Context, psn *psnReader, rels []silver.RelationshipPair) (map[string]int64, error) {
	out := make(map[string]int64, len(rels))
	if psn == nil {
		return out, nil
	}
	for _, p := range rels {
		if p.WebID == "" {
			continue
		}
		if p.PSNStartOverride > 0 {
			out[p.WebID] = p.PSNStartOverride
			continue
		}
		if p.PSNID == "" {
			continue
		}
		var minSnap sql.NullInt64
		err := psn.db.QueryRowContext(ctx, `
            SELECT MIN(snapshot_at)
              FROM cash_accounts
             WHERE relationship_id = ?`, p.PSNID).Scan(&minSnap)
		if err != nil {
			return nil, fmt.Errorf("PSN-start lookup for %q: %w", p.PSNID, err)
		}
		if minSnap.Valid {
			out[p.WebID] = minSnap.Int64
		}
	}
	return out, nil
}

// webKind maps the web silver's `description_kind` string plus
// debit/credit indicators to a canonical.TxKind. Conservative —
// unknown / ambiguous shapes route to TxKindOther so we never
// invent semantics that PSN's own MT940 events would contradict.
func webKind(descKind string, hasDebit, hasCredit bool) canonical.TxKind {
	switch descKind {
	case "Dividend":
		return canonical.TxKindDividend
	case "Coupon":
		return canonical.TxKindCoupon
	case "Interest":
		return canonical.TxKindInterest
	case "Fee", "Fees":
		return canonical.TxKindFee
	case "Buy", "Securities purchase":
		return canonical.TxKindBuy
	case "Sell", "Securities sale":
		return canonical.TxKindSell
	}
	// No description_kind hint → use direction. Credit-only
	// without instrument context = deposit; debit-only =
	// withdrawal. Everything else stays "other".
	switch {
	case hasCredit && !hasDebit:
		return canonical.TxKindDeposit
	case hasDebit && !hasCredit:
		return canonical.TxKindWithdrawal
	}
	return canonical.TxKindOther
}

// maxInt64 because Go 1.20 doesn't have generics-flavoured max in
// this codebase's helper set.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
