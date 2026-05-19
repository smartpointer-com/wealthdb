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
	out.LatestChangeNumber = maxInt64(out.LatestSnapshotAt, out.LatestTransactionAt)
	return out, nil
}

// ChangeWindow returns the union of new snapshots and new
// transactions strictly after `since`. Matches the PSN reader's
// convention so the merge layer's combine is straightforward.
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
	return w, nil
}

// Snapshots emits the per-snapshot rollup of banking
// relationships, portfolios, accounts, and positions for every
// dump_runs.snapshot_at in the window, FILTERED so that a row
// whose banking_relationship_id has a known PSN-start cutoff is
// dropped when snapshot_at >= cutoff. Web positions split into
// PositionChange (instrument_isin set) and CashBalanceChange
// (instrument_isin NULL).
//
// The orchestrator passes the *psnReader and config relationships
// in via Transactions; for Snapshots we re-resolve the cutoff
// map directly from the *Connection's stored relationships +
// psnReader (see merge.go). For now the signature is just
// (ctx, w) because the orchestrator's Snapshots forwards both
// streams independently — see merge.go.
func (r *webReader) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	return r.snapshotsWith(ctx, w, nil, nil)
}

// snapshotsWith is the merge-aware variant used by Connection
// when a psnReader is available. cutoffByWebRel maps web
// banking_relationship_id → exclusive cutoff Unix seconds;
// missing entries mean "no PSN counterpart, emit unconditionally".
// accountToWebRel + portfolioToWebRel let the position-level
// filter resolve a row's banking relationship when accounts.csv
// lacks the relationship column (e.g. securities-only rows).
func (r *webReader) snapshotsWith(ctx context.Context, w canonical.Window, cutoffByWebRel map[string]int64, accountToWebRel map[string]string) (silver.SnapshotStream, error) {
	portfolioToWebRel, err := r.buildPortfolioToRelMap(ctx)
	if err != nil {
		return nil, err
	}
	return r.snapshotsWithMaps(ctx, w, cutoffByWebRel, accountToWebRel, portfolioToWebRel)
}

func (r *webReader) snapshotsWithMaps(ctx context.Context, w canonical.Window, cutoffByWebRel map[string]int64, accountToWebRel map[string]string, portfolioToWebRel map[string]string) (silver.SnapshotStream, error) {
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

	// Pull each entity type, applying the per-relationship cutoff
	// as we route into byTime. Row-level filter (instead of
	// upfront pruning) keeps the SQL simple and is fine at
	// personal-portfolio scale.
	if err := r.appendWebPortfolios(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebAccounts(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebPositions(ctx, w, byTime, cutoffByWebRel, accountToWebRel, portfolioToWebRel); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		// Skip empty batches — happens when every entity at this
		// snapshot got filtered out by the cutoff.
		b := byTime[t]
		if len(b.Portfolios)+len(b.Accounts)+len(b.Positions)+len(b.CashBalances) == 0 {
			continue
		}
		out.batches = append(out.batches, *b)
	}
	return out, nil
}

// Transactions yields web transactions strictly before each
// banking relationship's PSN-start cutoff. Same row-level filter
// approach as Snapshots. When no psn is configured the cutoff
// map is empty and every row passes; the splice degenerates to
// "all-web".
//
// The signature accepts the *psnReader and config relationships
// so Connection's Transactions can stay shape-equivalent for
// callers that never see the merge.
func (r *webReader) Transactions(ctx context.Context, w canonical.Window, psn *psnReader, rels []silver.RelationshipPair) (silver.TransactionStream, error) {
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
		// Splice: drop rows whose value_date is at or past the
		// PSN cutover for the owning relationship.
		if rel, ok := accountToRel[accountID]; ok {
			if cut := cutoff[rel]; cut > 0 && valueDate >= cut {
				continue
			}
		}

		// Net amount = credit - debit. Web stores them as
		// separate columns; one is set per row.
		var net canonical.Decimal
		if credit.Valid {
			net = net.Add(canonical.NewDecimalFromFloat(credit.Float64))
		}
		if debit.Valid {
			net = net.Sub(canonical.NewDecimalFromFloat(debit.Float64))
		}
		netPtr := net

		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: txID,
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

// appendWebPositions splits the positions table into
// PositionChange (instrument_isin set) and CashBalanceChange
// (instrument_isin NULL). The cutoff applies via the row's
// banking relationship — looked up first from
// accountToWebRel (cash rows have account_external_id = IBAN),
// then via portfolioToWebRel (securities-only rows have
// account_external_id="" in the web silver and only the
// portfolio_external_id is meaningful). Rows whose relationship
// can't be resolved fall through unfiltered.
func (r *webReader) appendWebPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64, accountToWebRel, portfolioToWebRel map[string]string) error {
	const q = `
SELECT snapshot_at, portfolio_external_id, account_external_id,
       instrument_isin, currency_iso, units, market_value,
       description, payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebPositions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                      int64
			portfolioID, accountID, ccy, payload      string
			isin, description                         sql.NullString
			units, marketValue                        sql.NullFloat64
		)
		if err := rows.Scan(&snap, &portfolioID, &accountID, &isin, &ccy, &units, &marketValue, &description, &payload); err != nil {
			return err
		}
		rel := accountToWebRel[accountID]
		if rel == "" {
			rel = portfolioToWebRel[portfolioID]
		}
		if rel != "" {
			if cut := cutoff[rel]; cut > 0 && snap >= cut {
				continue
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		if !isin.Valid || isin.String == "" {
			// Cash position → cash_balances.
			if !marketValue.Valid {
				continue
			}
			amt := canonical.NewDecimalFromFloat(marketValue.Float64)
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: accountID,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            amt,
				Payload:           json.RawMessage(payload),
			})
			continue
		}
		// Securities position → positions.
		var qty, mv *canonical.Decimal
		if units.Valid {
			q := canonical.NewDecimalFromFloat(units.Float64)
			qty = &q
		}
		if marketValue.Valid {
			m := canonical.NewDecimalFromFloat(marketValue.Float64)
			mv = &m
		}
		instrID := isin.String
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    accountID,
			PositionKey:          isin.String,
			InstrumentExternalID: &instrID,
			AssetClass:           canonical.AssetClassOther, // web silver has no CFI; refined by PSN once it joins
			Currency:             ccy,
			Quantity:             qty,
			MarketValue:          mv,
			Payload:              json.RawMessage(payload),
		})
		// Emit the instrument row too — web has no dedicated
		// instruments table, but downstream gold.instruments
		// keeps name/symbol consistent across sources.
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin.String,
			AssetClass:           canonical.AssetClassOther,
			ISIN:                 &instrID,
			Name:                 nullStringPtr(description),
			Currency:             &ccy,
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
	}
	return rows.Err()
}

// buildPortfolioToRelMap returns a web portfolio_external_id →
// web banking_relationship_id lookup using the latest snapshot
// per portfolio. Used as a fallback for positions whose
// account_external_id is empty (securities-only rows).
func (r *webReader) buildPortfolioToRelMap(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT p.portfolio_external_id, p.banking_relationship_id
  FROM portfolios p
  JOIN (SELECT portfolio_external_id, MAX(snapshot_at) AS s
          FROM portfolios GROUP BY portfolio_external_id) m
    ON p.portfolio_external_id = m.portfolio_external_id
   AND p.snapshot_at = m.s
 WHERE p.banking_relationship_id IS NOT NULL`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("buildPortfolioToRelMap: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var port, rel string
		if err := rows.Scan(&port, &rel); err != nil {
			return nil, err
		}
		out[port] = rel
	}
	return out, rows.Err()
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
