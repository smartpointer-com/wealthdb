package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// webReader reads from the ubs-web silver SQLite. It owns
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
		return silver.NewSnapshotStream(nil), nil
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

	// Mortgages: unique to the web side — PSN doesn't carry them
	// at all. They flow through regardless of the PSN cutoff;
	// emitted as a triple per row (account + instrument +
	// position) so they show up as a single negative-value
	// liability holding in gold positions.
	if err := r.appendWebMortgages(ctx, w, byTime); err != nil {
		return nil, err
	}

	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		b := byTime[t]
		if len(b.Portfolios)+len(b.Accounts)+len(b.Instruments)+
			len(b.Positions) == 0 {
			continue
		}
		batches = append(batches, *b)
	}
	return silver.NewSnapshotStream(batches), nil
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
		// UBS web descriptions encode the listing ticker in
		// trailing parens — e.g. "Reg.shs Novartis Inc.
		// (NOVN)". Extract it and surface as the instrument's
		// Symbol so dividend / coupon transactions joined by
		// ISIN get a populated symbol column. Descriptions
		// without a trailing (TICKER) (ETFs identified only by
		// long-form name) keep Symbol nil.
		symbol := tickerFromDescription(description.String)
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           ac,
			ISIN:                 &isinCopy,
			Symbol:               symbol,
			Name:                 silver.StrPtrIfNonEmpty(description.String),
			Currency:             silver.StrPtrIfNonEmpty(ccy),
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
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	cutoff, err := buildPSNStartByWebRel(ctx, psn, rels)
	if err != nil {
		return nil, err
	}
	accountToRel, err := r.buildAccountToRelMap(ctx)
	if err != nil {
		return nil, err
	}
	ownIBANs, err := r.buildOwnIBANSet(ctx)
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
		kind := webKind(kindStr.String, debit.Valid, credit.Valid)

		// Pre-2024 Account-Statement PDF cash backfill: classify each
		// deposit/withdrawal as EXTERNAL (boundary-crossing owner
		// capital) or INTERNAL (conduit churn) at the relationship
		// boundary. UBS cash/current accounts are CONDUITS — external
		// capital enters as cash and is routed into securities /
		// mandates / FX, whose value spine carries the return — so
		// internal churn fed into a flow-based return double-counts.
		// The conservative rule (pdfCashIsExternal, default INTERNAL,
		// own-IBAN-based, PII-free) keeps only provably-external moves
		// in the flow stream; INTERNAL rows are demoted to a non-flow
		// kind (TxKindOther, absent from BankExternal) so they stay
		// queryable in gold but out of the return. The engine's UBS
		// policy (OnboardPerEntityOnce + ConduitKinds:[cash] +
		// ExternalOnly + Inception=first-real-snapshot) then onboards
		// the relationship's inception value ONCE and counts external
		// deposits on top, so capital is counted exactly once. MT940
		// rows (post-2024) carry no source marker and are unaffected.
		if kind == canonical.TxKindDeposit || kind == canonical.TxKindWithdrawal {
			if isPDFCashBackfill(payload) && !pdfCashIsExternal(payload, ownIBANs) {
				kind = canonical.TxKindOther
			}
		}

		// Reversal rows (description_kind tagged `<base>;Reversal`)
		// already carry the bank's correction sign in credit/
		// debit, so the canonical-sign helper would mask the
		// correction by forcing it back to the kind's normal
		// direction. Bypass the helper for those rows; the kind
		// itself still maps to the underlying canonical kind (so
		// reversals net against the originals when summed by
		// kind), only the sign-normalisation step is skipped.
		_, isReversal := stripReversalSuffix(kindStr.String)
		netAmount := &netPtr
		if !isReversal {
			netAmount = canonical.ApplyCanonicalSign(kind, &netPtr)
		}

		// Description1 in the silver carries the instrument
		// caption verbatim, with the ISIN appended after the
		// last "; " separator. Pull both out: the ISIN goes on
		// instrument_external_id so the gold-side instruments
		// join works for dividend / coupon / fee rows tied to a
		// security; the full caption is the row's Description
		// fallback for the CLI's name column.
		instrumentID, descriptionText := extractInstrumentFromDescription1(payload)

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
			InstrumentExternalID:  instrumentID,
			Kind:                  kind,
			Currency:              ccy,
			NetAmount:             netAmount,
			Description:           descriptionText,
			Payload:               json.RawMessage(payload),
		})
	}
	return silver.NewTransactionStream(out), rows.Err()
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
			snap               int64
			extID, payload     string
			relID, description sql.NullString
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
			DisplayName:         silver.StrPtrIfNonEmpty(description.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
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
			snap                                 int64
			extID, kind, payload                 string
			ccy, relID, portfolioID, description sql.NullString
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
			DisplayName:         silver.StrPtrIfNonEmpty(description.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(ccy.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
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

// buildOwnIBANSet returns the set of the relationship's OWN account IBANs, keyed
// by the normalized (spaces stripped, upper-cased) IBAN — exactly the shape a
// transaction payload's counter_account normalizes to. account_external_id IS the
// IBAN (schema: "IBAN no-spaces upper"), so this is a name-free, PII-free key:
// membership decides internal-vs-external without any holder name. Used by the
// pre-2024 PDF cash-flow classifier to recognise inter-own-account moves as a
// SUPPLEMENT to the parser's internal_transfer boolean: it can demote a KNOWN own
// counter to internal, but it cannot promote — the parser flag is checked first
// and is authoritative, so an own mandate/portfolio destination absent from
// `accounts` is still vetoed to internal by that flag, not by this set.
func (r *webReader) buildOwnIBANSet(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT account_external_id FROM accounts`)
	if err != nil {
		return nil, fmt.Errorf("buildOwnIBANSet: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var iban string
		if err := rows.Scan(&iban); err != nil {
			return nil, err
		}
		out[normalizeIBAN(iban)] = true
	}
	return out, rows.Err()
}

// buildPSNStartByWebRel resolves the PSN-start cutoff per web
// banking_relationship_id using the config relationships pairing
// and the *psnReader (if configured). Returns an empty map when
// psn is nil (degenerate splice — every web row passes).
//
// Resolution order per relationship:
//  1. RelationshipPair.PSNStartOverride if non-zero.
//  2. MIN(snapshot_at) in PSN for the paired PSNID, otherwise.
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
//
// UBS marks bank-side corrections with a `<base>;Reversal`
// suffix (the only one observed so far is
// `Dividend;Reversal`, where UBS clawed back a duplicate
// dividend booking). Reversals carry a negative amount in the
// credit column; we map them to the same canonical kind as the
// underlying event so they net out when summed by kind, and
// rely on ApplyCanonicalSign preserving the source's negative
// sign rather than forcing it positive.
func webKind(descKind string, hasDebit, hasCredit bool) canonical.TxKind {
	// Strip a `;Reversal` suffix if present and recurse on the
	// base. Lets us pick up any future reversal flavour the bank
	// invents without enumerating each.
	if base, ok := stripReversalSuffix(descKind); ok {
		return webKind(base, hasDebit, hasCredit)
	}
	// Match case-insensitively on the whole (trimmed) string. The
	// MT940 CSV feed and the PDF Account-Statement backfill spell the
	// same concept differently ("Dividend" vs "DIVIDEND",
	// "e-banking payment order" vs "E-BANKING PAYMENT ORDER"), so an
	// upper-cased EXACT-string match classifies both. Exact (not
	// prefix/substring) matching keeps the two vocabularies from
	// colliding: MT940's distinctive multi-token forms ("UCCDD…;
	// order") never equal a bare PDF booking type ("ORDER"), so each
	// feed's rows resolve independently.
	switch strings.ToUpper(strings.TrimSpace(descKind)) {
	// ---- Income / cost: NOT capital flows; excluded from returns.
	case "DIVIDEND", "REVERSAL DIVIDEND":
		return canonical.TxKindDividend
	case "COUPON":
		return canonical.TxKindCoupon
	case "INTEREST",
		"INTEREST CALCULATION BALANCE",
		"CALL DEPOSIT INTEREST PAYMENT",
		"FIXED TERM DEPOSIT INTEREST PAYMENT":
		return canonical.TxKindInterest
	case "FEE", "FEES",
		"CUSTODY PRICE",
		"ADR/GDR HANDLING FEES",
		"THIRD-PARTY CHARGES",
		"RENTAL FEE SAFE BOX",
		"BALANCE CLOSING OF SERVICE PRICES",
		"ADVICE", "UBS ADVICE":
		return canonical.TxKindFee
	// ---- Currency conversion between the holder's own accounts —
	// an internal reshuffle, not a capital flow. Spot, forward and
	// swap legs all reallocate cash across the holder's single-
	// currency accounts; none is external capital. The MT940 feed
	// names the instrument ("Purchase/Sale FX Spot/Forward", "…from
	// FX Swap"), the older PDF backfill only says "FOREX". All map
	// to non-flow fx kinds so they never enter net_flow — before
	// this, the multi-token MT940 forms fell through to the
	// direction switch below and were mis-booked as deposits /
	// withdrawals.
	case "FOREX PURCHASE", "FOREX SALE",
		"PURCHASE FX SPOT", "SALE FX SPOT":
		return canonical.TxKindFx
	case "PURCHASE FX FORWARD", "SALE FX FORWARD":
		return canonical.TxKindFxForward
	case "PURCHASE FROM FX SWAP", "SALE FROM FX SWAP":
		return canonical.TxKindFxSwap
	// ---- Securities settlements: reallocate between cash and
	// instruments; excluded from flows. Side by cash direction.
	case "BUY", "SECURITIES PURCHASE":
		return canonical.TxKindBuy
	case "SELL", "SECURITIES SALE":
		return canonical.TxKindSell
	case "SHARE", "MUTUAL FUNDS", "INVESTMENT FUNDS",
		"UBS INVESTMENT FUNDS", "STRUCTURED PRODUCTS",
		"ORDER", "PURCHASE", "SALE",
		"PRECIOUS METAL BUY", "PRECIOUS METAL SELL",
		"BUY PM SPOT W/O VAT", "SELL PM SPOT W/O VAT",
		"SUBSCRIPTION RIGHT",
		"UBS MANAGE", "REC UBS MANAGE", "CAN UBS MANAGE":
		return securitiesSide(hasDebit, hasCredit)
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

// securitiesSide maps a securities-settlement row to buy (cash out /
// debit) or sell (cash in / credit). Both kinds are excluded from
// the returns flow set, so the side is for analytics only.
func securitiesSide(hasDebit, hasCredit bool) canonical.TxKind {
	if hasCredit && !hasDebit {
		return canonical.TxKindSell
	}
	return canonical.TxKindBuy
}

// isPDFCashBackfill reports whether a transaction came from the
// pre-2024 Account-Statement PDF backfill (source=
// "account_statement_pdf"). Used to hold those cash movements out of
// the return flow stream; the MT940 feed carries no such marker and
// is unaffected.
func isPDFCashBackfill(payload string) bool {
	var p struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	return p.Source == "account_statement_pdf"
}

// normalizeIBAN strips spaces and upper-cases an IBAN-shaped string so a payload's
// formatted counter_account ("CH.. .... ....") compares byte-for-byte with an
// account_external_id (already no-spaces upper). Name-free.
func normalizeIBAN(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, " ", ""))
}

// pdfCashIsExternal decides whether a pre-2024 PDF-backfill cash movement is a
// genuine boundary-crossing (EXTERNAL) owner-capital flow or internal churn.
//
// UBS cash/current accounts are conduits: external cash lands and is routed into
// securities / mandates / FX inside the relationship, and the securities value
// spine carries the return. Feeding that internal churn into a flow-based return
// double-counts capital. The rule is therefore CONSERVATIVE toward internal —
// default INTERNAL, mark EXTERNAL only when the counterparty is PROVABLY a
// non-own party — because a missed external merely understates capital (safe)
// while a fabricated external double-counts (catastrophic; this is what sank the
// prior attempt via loose org-markers). It uses ONLY the normalized
// counter_account IBAN against the relationship's own-IBAN set — NO holder name,
// NO free-text counterparty, NO org markers, i.e. no PII.
//
// EXTERNAL iff the parser did NOT already flag the row internal_transfer AND
// counter_account is a populated, non-own Swiss/Liechtenstein IBAN that is not a
// mortgage (HYPOTHEK) payoff or a structured-product maturity / closing.
// Everything else — parser-confirmed internal_transfer, null counter, own IBAN,
// non-CH/LI IBAN, mortgage amortisation, maturity/closing — is INTERNAL.
func pdfCashIsExternal(payload string, own map[string]bool) bool {
	var p struct {
		CounterAccount   string `json:"counter_account"`
		BookingType      string `json:"booking_type"`
		InternalTransfer bool   `json:"internal_transfer"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	// The collector's own name-free markers (UEBERTRAG/UMBUCHUNG/MANDAT/MANAGE/
	// PORTFOLIO/REDUK on the continuation lines) already identified this row as an
	// intra-relationship mandate-funding / book-transfer move. That is authoritative
	// and VETOES external BEFORE the IBAN promotion below: a mandate/portfolio
	// destination absent from `accounts` is a known-internal row whose counter IBAN
	// would otherwise pass all four EXTERNAL conditions and fabricate owner capital
	// (the conduit direction this model exists to prevent). own-IBAN membership is a
	// supplement that can only DEMOTE a known-own counter to internal; it cannot
	// catch such a row, so the parser flag must gate first.
	if p.InternalTransfer {
		return false // parser-confirmed internal reshuffle ⇒ never external
	}
	ctr := normalizeIBAN(p.CounterAccount)
	if ctr == "" {
		return false // no counterparty IBAN ⇒ not provably external ⇒ INTERNAL
	}
	if own[ctr] {
		return false // inter-own-account move: supplement demoting a KNOWN own counter (parser flag already handled unknown-destination internals above)
	}
	if !strings.HasPrefix(ctr, "CH") && !strings.HasPrefix(ctr, "LI") {
		return false // only Swiss/Liechtenstein counterparties count; anything else stays INTERNAL
	}
	// Mortgage amortisation + structured-product maturity/closing net inside the
	// relationship (payoff of an own liability / roll of an own product), not owner
	// capital crossing the boundary.
	bt := strings.ToUpper(p.BookingType)
	if strings.Contains(bt, "HYPOTHEK") || strings.Contains(bt, "MATURITY") || strings.Contains(bt, "CLOSING") {
		return false
	}
	return true
}

// extractInstrumentFromDescription1 pulls (ISIN, full caption)
// out of the silver row's payload.Description1 field, when
// present. UBS web statements format this field as
// "<security caption>; <ISIN>" — a typical caption looks like
// "UBS (Lux) Fund Solutions SICAV - UBS Core MSCI EMU UCITS ETF
// EUR dis-dist; LU0000000070". The trailing 12-char alphanumeric
// after "; " is a valid ISIN ~always in observed data; we
// validate by length-and-charset to avoid grabbing other ";"-
// separated comments.
//
// Returns (nil, nil) when the field is missing, empty, or the
// trailing token doesn't look like an ISIN. The full caption
// (everything before the final separator, trimmed) is returned
// even when no ISIN matches — it's still useful as a name
// fallback.
func extractInstrumentFromDescription1(payload string) (instrumentID, description *string) {
	d1 := json.RawMessage(payload)
	var fields struct {
		Description1 string `json:"Description1"`
	}
	if err := json.Unmarshal(d1, &fields); err != nil || fields.Description1 == "" {
		return nil, nil
	}
	caption := strings.TrimSpace(fields.Description1)
	// Try to split on the last "; ". Anything 12 chars long
	// with the ISIN shape (2 alpha + 10 alnum) is treated as
	// an ISIN; otherwise the caption stays whole.
	if i := strings.LastIndex(caption, "; "); i >= 0 {
		head := strings.TrimSpace(caption[:i])
		tail := strings.TrimSpace(caption[i+2:])
		if looksLikeISIN(tail) {
			id := tail
			desc := head
			return &id, silver.StrPtrIfNonEmpty(desc)
		}
	}
	return nil, silver.StrPtrIfNonEmpty(caption)
}

// tickerFromDescription pulls the trailing `(TICKER)` segment
// out of a UBS web caption like "Reg.shs Novartis Inc.
// (NOVN)" or "Sponsored American Deposit Receipt Taiwan
// Semicon. Manuf.Co Ltd (Repr. 5 shs)     (TSM)". Returns nil
// when:
//
//   - The string has no trailing `(...)`.
//   - The bracketed content isn't 1-10 chars of upper-case
//     ASCII letters / digits / hyphens (filters out things
//     like "(IE)" / "(Lux)" — those appear mid-string in ETF
//     issuer suffixes, never at the very end).
//
// Strict enough to avoid false positives on long parenthetical
// phrases that happen to come last (e.g. "(Repr. 5 shs)").
func tickerFromDescription(desc string) *string {
	desc = strings.TrimRight(desc, " \t")
	if !strings.HasSuffix(desc, ")") {
		return nil
	}
	open := strings.LastIndex(desc, "(")
	if open < 0 {
		return nil
	}
	t := desc[open+1 : len(desc)-1]
	if len(t) < 1 || len(t) > 10 {
		return nil
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' {
			return nil
		}
	}
	return &t
}

// appendWebMortgages projects ubs-web silver `mortgages` rows
// into three canonical changes per row:
//
//   - AccountChange{Kind: mortgage}           — one liability account
//   - InstrumentChange{AssetClass: mortgage}  — synthetic instrument
//     keyed by the same external ID (UBS doesn't expose a separate
//     instrument-level identity for mortgages)
//   - PositionChange{AssetClass: mortgage,
//     MarketValue: outstanding_balance}       — already negative in
//     the silver row; flows through to net-worth roll-ups as a
//     negative contribution
//
// No PSN cutoff: PSN doesn't surface mortgages at all, so every web
// snapshot's mortgage rows pass through. Migration 0004 of the
// ubs-web silver created the table; if it's absent on an older
// silver, the COUNT-zero path skips cleanly.
func (r *webReader) appendWebMortgages(ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch) error {
	ok, err := r.hasMortgagesTable(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	const q = `
SELECT snapshot_at, account_external_id, banking_relationship_id,
       portfolio_external_id, currency_iso, outstanding_balance,
       start_date, end_date, rate_type, collateral_description,
       description, payload
  FROM mortgages
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebMortgages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                            int64
			extID, currency, payload                        string
			relID, portfolioID, rateType, collateral, descr sql.NullString
			outstanding                                     sql.NullFloat64
			startDate, endDate                              sql.NullInt64
		)
		if err := rows.Scan(&snap, &extID, &relID, &portfolioID,
			&currency, &outstanding, &startDate, &endDate, &rateType,
			&collateral, &descr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		_ = startDate // promoted into payload via the JSON below
		_ = endDate
		_ = rateType
		_ = collateral
		_ = outstanding // mortgage Positions are injected by the
		// psn-web fold stream, NOT emitted here. Emitting a Position
		// at the web dump's snapshot_at would create a "mortgage-only"
		// gold snapshot at the web dump time — and gold's "latest
		// snapshot per silver source" query (MAX over
		// positions.snapshot_at) would then land on that
		// mortgage-only time and hide every other UBS position from
		// the "today" view. The fold stream injects mortgages only
		// into PSN batches that already carry Positions, keeping
		// snapshot times aligned.

		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         canonical.AccountKindMortgage,
			DisplayName:         silver.StrPtrIfNonEmpty(descr.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(currency),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           canonical.AssetClassMortgage,
			Name:                 silver.StrPtrIfNonEmpty(descr.String),
			Currency:             silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
	}
	return rows.Err()
}

// latestMortgagePositions returns one PositionChange per mortgage
// account in web silver, populated from the row with the largest
// snapshot_at ≤ asOf for that account. Used by the fold stream to
// carry web-only mortgage data forward into each PSN snapshot:
// web dumps fire when the user logs in, PSN snapshots fire
// nightly, so without carry-forward the gold "latest snapshot per
// source" query falls onto a PSN-only time where the mortgage
// isn't refreshed and disappears from the position table.
//
// The returned templates have SnapshotAt = 0; the caller stamps
// the right time per batch.
func (r *webReader) latestMortgagePositions(ctx context.Context, asOf int64) ([]canonical.PositionChange, error) {
	ok, err := r.hasMortgagesTable(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	const q = `
SELECT m.account_external_id, m.currency_iso, m.outstanding_balance, m.payload
  FROM mortgages m
  JOIN (
        SELECT account_external_id, MAX(snapshot_at) AS s
          FROM mortgages
         WHERE snapshot_at <= ?
         GROUP BY account_external_id
       ) latest
    ON latest.account_external_id = m.account_external_id
   AND latest.s                    = m.snapshot_at`
	rows, err := r.db.QueryContext(ctx, q, asOf)
	if err != nil {
		return nil, fmt.Errorf("latestMortgagePositions: %w", err)
	}
	defer rows.Close()
	var out []canonical.PositionChange
	for rows.Next() {
		var (
			extID, currency, payload string
			outstanding              sql.NullFloat64
		)
		if err := rows.Scan(&extID, &currency, &outstanding, &payload); err != nil {
			return nil, err
		}
		idCopy := extID
		var mv *canonical.Decimal
		if outstanding.Valid {
			d := canonical.NewDecimalFromFloat(outstanding.Float64)
			mv = &d
		}
		out = append(out, canonical.PositionChange{
			AccountExternalID:    extID,
			PositionKey:          extID,
			InstrumentExternalID: &idCopy,
			AssetClass:           canonical.AssetClassMortgage,
			Currency:             currency,
			MarketValue:          mv,
			Payload:              json.RawMessage(payload),
		})
	}
	return out, rows.Err()
}

// hasMortgagesTable returns true if the connected silver carries
// the `mortgages` table (added by migration 0004). Older silvers
// loaded before that migration just return false and the caller
// skips the projection.
func (r *webReader) hasMortgagesTable(ctx context.Context) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
        SELECT COUNT(*)
          FROM sqlite_master
         WHERE type = 'table' AND name = 'mortgages'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasMortgagesTable: %w", err)
	}
	return n > 0, nil
}

// looksLikeISIN: 12 chars, first two ASCII letters (country
// code), remaining 10 ASCII alphanumerics. Strict enough to
// reject "(SCMN)"-style ticker fragments while accepting every
// real ISIN.
func looksLikeISIN(s string) bool {
	if len(s) != 12 {
		return false
	}
	if !isASCIIAlpha(s[0]) || !isASCIIAlpha(s[1]) {
		return false
	}
	for i := 2; i < 12; i++ {
		c := s[i]
		if !isASCIIAlpha(c) && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// stripReversalSuffix peels a `;Reversal` (case-insensitive)
// suffix off a description_kind. Returns (base, true) when a
// suffix was present, (descKind, false) otherwise.
func stripReversalSuffix(descKind string) (string, bool) {
	const suffix = ";Reversal"
	if len(descKind) > len(suffix) &&
		strings.EqualFold(descKind[len(descKind)-len(suffix):], suffix) {
		return descKind[:len(descKind)-len(suffix)], true
	}
	return descKind, false
}

// maxInt64 because Go 1.20 doesn't have generics-flavoured max in
// this codebase's helper set.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
