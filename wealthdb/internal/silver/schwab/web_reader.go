package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// webReader reads from the schwab-web silver SQLite. The web
// feed complements the api feed by:
//
//   - Backfilling transactions older than each account's
//     api-coverage-start (Trader API only goes back ~2y; web
//     statements reach further).
//   - Providing per-statement-period position snapshots and cash
//     balances that api doesn't surface at all (api emits only
//     live snapshots per dump run).
//   - Supplying the human-readable `nickname` Schwab attaches to
//     each account.
//
// Identity: web's `account_external_id` is the 3-to-5-digit
// account suffix Schwab renders in the UI. The orchestrator
// (merge.go) builds a suffix → api-hashValue bridge at adapter
// open time and the methods on this reader rewrite the suffix to
// the bridged hash before emitting downstream.
type webReader struct {
	db *sql.DB
}

func (r *webReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// Status reports the silver-side observable range for the web
// feed. Snapshot-time extrema come from dump_runs plus the
// historical tables (which use as_of_date / period_end rather
// than dump times). Transaction-time extrema come from
// transactions.timestamp. LatestChangeNumber stays a live-time
// concept — MAX(dump_runs.snapshot_at) — so a reload with no new
// dump is a no-op even when historical content is present.
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	out := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(snapshot_at), -1),
               COALESCE(MAX(snapshot_at), -1)
          FROM dump_runs`).Scan(&out.OldestSnapshotAt, &out.LatestSnapshotAt); err != nil {
		return canonical.Status{}, fmt.Errorf("schwab-web Status snapshots: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(timestamp), -1),
               COALESCE(MAX(timestamp), -1)
          FROM transactions`).Scan(&out.OldestTransactionAt, &out.LatestTransactionAt); err != nil {
		return canonical.Status{}, fmt.Errorf("schwab-web Status transactions: %w", err)
	}
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil {
		return canonical.Status{}, err
	}
	if ok {
		histLo, histHi, err := r.historicalRange(ctx)
		if err != nil {
			return canonical.Status{}, err
		}
		if histLo >= 0 && (out.OldestSnapshotAt == -1 || histLo < out.OldestSnapshotAt) {
			out.OldestSnapshotAt = histLo
		}
		if histHi > out.LatestSnapshotAt {
			out.LatestSnapshotAt = histHi
		}
	}
	out.LatestChangeNumber = maxInt64(out.LatestSnapshotAt, out.LatestTransactionAt)
	return out, nil
}

// ChangeWindow returns the union of new snapshots and new
// transactions strictly after `since`, plus any historical
// content reachable by either dimension. Extends Start back to
// MIN(historical times) whenever there's any new live content so
// the loader's window-DELETE covers existing historical gold
// rows before they're re-inserted — same pattern as the UBS web
// reader.
func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	var (
		snapMin, snapMax sql.NullInt64
		txMin, txMax     sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(snapshot_at), MAX(snapshot_at)
          FROM dump_runs WHERE snapshot_at > ?`, since).Scan(&snapMin, &snapMax); err != nil {
		return canonical.Window{}, fmt.Errorf("schwab-web ChangeWindow snapshots: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(timestamp), MAX(timestamp)
          FROM transactions WHERE timestamp > ?`, since).Scan(&txMin, &txMax); err != nil {
		return canonical.Window{}, fmt.Errorf("schwab-web ChangeWindow transactions: %w", err)
	}
	w := canonical.Window{NewChangeNumber: since}
	mergeNew := func(n sql.NullInt64) {
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
	mergeNew(snapMax)
	mergeNew(txMax)

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

// snapshotsDimensions emits AccountChange rows from web's
// `accounts` table for every snapshot in the window, rewriting
// account_external_id from web's suffix to the bridged api
// hashValue. Web supplies the user-friendly `nickname`; api
// supplies almost everything else. The per-column upsert guard
// in gold merges them by (silver_source_id, account_external_id).
//
// Accounts whose web suffix doesn't bridge to an api hashValue
// (e.g. closed accounts no longer in api) are skipped — they
// have no api counterpart to merge with and emitting them under
// the raw suffix would create orphaned rows the rest of gold
// can't join.
func (r *webReader) snapshotsDimensions(
	ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
	bridge map[string]string,
) error {
	if !w.HasChanges {
		return nil
	}
	// Silver migration 0003 added `account_registration` (the
	// verbatim statement-PDF registration label —
	// "Contributory IRA" / "Schwab One® Custodial Account
	// (UTMA)" / "Education Savings" / etc.). Older silvers
	// don't have the column; degrade gracefully.
	hasRegistration, err := silver.HasColumn(ctx, r.db, "accounts", "account_registration")
	if err != nil {
		return err
	}
	regCol := "NULL"
	if hasRegistration {
		regCol = "account_registration"
	}
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, nickname, payload, COALESCE(%s, '')
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`, regCol)
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("schwab-web snapshotsDimensions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap         int64
			suffix       string
			nickname     sql.NullString
			payload      string
			registration string
		)
		if err := rows.Scan(&snap, &suffix, &nickname, &payload, &registration); err != nil {
			return err
		}
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		change := canonical.AccountChange{
			AccountExternalID: hash,
			AccountKind:       canonical.AccountKindBrokerage,
			Nickname:          silver.StrPtrIfNonEmpty(nickname.String),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		}
		if registration != "" {
			// Forward the raw Schwab label as AccountCategory
			// (verbatim, for forensics) and derive the canonical
			// TaxWrapper. Unknown labels leave TaxWrapper nil so
			// the render-time default (taxable_personal) kicks in.
			cat := registration
			change.AccountCategory = &cat
			if tw := taxWrapperForRegistration(registration); tw != "" {
				change.TaxWrapper = &tw
			}
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

// transactionsBeforeAPIStart emits web transactions that are
// strictly older than each account's api-coverage start. Web
// `activity_id`s are a synthetic SHA-256 prefix — they don't
// collide with api's real activityIds. Rewrites web suffix to
// api hashValue so the row lands under the same gold account as
// the api transactions for that account.
//
// `apiStartByHash` maps api hashValue → MIN(timestamp). A web tx
// is emitted only when:
//
//  1. its account bridges to an api hashValue; AND
//  2. its timestamp is strictly less than apiStartByHash[hash]
//     (or hash has no api transactions at all, in which case
//     everything from the web side passes).
//
// Hard cut, not overlap-merge — per INTEROP §2 the two silvers'
// transaction-id spaces are disjoint so any cross-source per-row
// match would be heuristic. Inside the api window, api wins
// (real activity_id, no parser approximation).
func (r *webReader) transactionsBeforeAPIStart(
	ctx context.Context,
	w canonical.Window,
	bridge map[string]string,
	apiStartByHash map[string]int64,
	symbolToCUSIP map[string]string,
) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	// ORDER BY makes the cross-feed dedup below deterministic (its greedy
	// first-fit consumes JSON legs in a stable order). `source` distinguishes
	// the two overlapping sub-feeds (statement_pdf vs tx_history_json).
	const q = `
SELECT activity_id, timestamp, account_external_id, kind, instrument_key, payload, source
  FROM transactions
 WHERE timestamp BETWEEN ? AND ?
 ORDER BY timestamp, activity_id`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("schwab-web Transactions: %w", err)
	}
	defer rows.Close()

	var built []builtWebTx
	for rows.Next() {
		var (
			activityID, suffix, kind, payload, source string
			ts                                        int64
			instrumentKey                             sql.NullString
		)
		if err := rows.Scan(&activityID, &ts, &suffix, &kind, &instrumentKey, &payload, &source); err != nil {
			return nil, err
		}
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}
		if cutoff, ok := apiStartByHash[hash]; ok && ts >= cutoff {
			continue
		}
		var instrPtr *string
		if instrumentKey.Valid && instrumentKey.String != "" {
			// Web stores the ticker as instrument_key. Translate
			// to the api-side CUSIP when known so the row lands
			// on the same gold instruments row the api side
			// registered (and thus the symbol/name/asset_class
			// columns populate via the LEFT JOIN). Web-only
			// tickers fall through to using the ticker as-is.
			s := instrumentKey.String
			if cusip, ok := symbolToCUSIP[s]; ok {
				s = cusip
			}
			instrPtr = &s
		}
		netAmount, quantity, price := extractWebTxAmounts(payload)
		description := extractWebTxDescription(payload)
		txKind := webKind(kind)
		built = append(built, builtWebTx{source: source, tx: canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            ts,
			AccountExternalID:     hash,
			InstrumentExternalID:  instrPtr,
			Kind:                  txKind,
			// Currency unknown from the web row — Schwab statements
			// don't structure it. Default to USD: Schwab
			// accounts are USD-denominated here, and
			// the canonical TransactionChange.Currency field is
			// NOT NULL.
			Currency:    "USD",
			NetAmount:   canonical.ApplyCanonicalSign(txKind, netAmount),
			Quantity:    quantity,
			Price:       price,
			Description: description,
			Payload:     json.RawMessage(payload),
		}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reconcile the overlapping web sub-feeds (INTEROP §8), in order:
	//   1. JSON is authoritative for non-external rows across its coverage span
	//      (statement PDFs only backfill older history).
	//   2. A third_party_distribution cash transfer is authoritative over a
	//      matching statement/tx-history cash debit (it names the counterparty);
	//      run before the PDF↔JSON dedup so the final external set is correct.
	//   3. form_1099b is authoritative for sales within its covered tax years;
	//      the superseded statement/tx-history sells are dropped.
	//   4. Remaining statement_pdf ↔ tx_history_json external flows are matched
	//      1:1 so net_flow isn't double-counted.
	// Securities distributions are new data (no other feed records them) and
	// pass straight through as TxKindTransferOut external flows.
	built = spliceNonExternalToJSON(built)
	built = supersedeStatementCashWithDistributions(built)
	built = supersedeSalesWith1099B(built)
	out := canonical.TransactionBatch{Transactions: dedupeCrossFeedExternalFlows(built)}
	return silver.NewTransactionStream(out), nil
}

// spliceNonExternalToJSON makes the tx-history-JSON export authoritative for
// non-external rows (trades, dividends, fees, …) across its per-account coverage
// span: a statement-PDF non-external row dated within [minJSONday, maxJSONday]
// for that account is dropped (the structured JSON copy, which carries the trade
// date, is kept), while PDF rows OUTSIDE that span are retained as the deep
// backfill the ~2-year JSON export can't reach. Mirrors the api-over-web
// transaction splice in merge.go, one level down.
//
// External flows are deliberately untouched here — they move net_flow, so they
// use the no-loss 1:1 cross-feed dedup in dedupeCrossFeedExternalFlows, which
// keeps feed-unique legs rather than dropping them. Caveat: an internal gap in
// the JSON export drops the PDF rows inside it too; acceptable because
// non-external rows don't affect net_flow or holdings and the transaction report
// stays single-sourced within the JSON span.
func spliceNonExternalToJSON(built []builtWebTx) []builtWebTx {
	type span struct{ lo, hi int64 }
	jrange := map[string]*span{}
	for i := range built {
		b := &built[i]
		if b.source != sourceTxHistoryJSON {
			continue
		}
		day := b.tx.OccurredAt / 86400
		if s := jrange[b.tx.AccountExternalID]; s != nil {
			if day < s.lo {
				s.lo = day
			}
			if day > s.hi {
				s.hi = day
			}
		} else {
			jrange[b.tx.AccountExternalID] = &span{lo: day, hi: day}
		}
	}

	out := make([]builtWebTx, 0, len(built))
	for i := range built {
		b := built[i]
		if b.source == sourceStatementPDF && !externalFlowKinds[b.tx.Kind] {
			if s := jrange[b.tx.AccountExternalID]; s != nil {
				day := b.tx.OccurredAt / 86400
				if day >= s.lo && day <= s.hi {
					continue // JSON is authoritative for non-external rows in its span
				}
			}
		}
		out = append(out, b)
	}
	return out
}

// builtWebTx is a parsed web transaction paired with the silver sub-feed it came
// from, so dedupeCrossFeedExternalFlows can tell statement-PDF rows from
// tx-history-JSON rows.
type builtWebTx struct {
	tx     canonical.TransactionChange
	source string
}

// schwab-web silver `source` discriminators for the transaction sub-feeds.
// statement_pdf and tx_history_json overlap on the same economic events and are
// reconciled against each other (splice + cross-feed dedup). form_1099b is the
// authoritative-for-sales tax-lot feed (supersedes the other two for sells in its
// covered tax years), and third_party_distribution carries transfer flows out
// (securities = new data; cash = deduped against statement debits). See INTEROP §8.
const (
	sourceStatementPDF           = "statement_pdf"
	sourceTxHistoryJSON          = "tx_history_json"
	sourceFORM1099B              = "form_1099b"
	sourceThirdPartyDistribution = "third_party_distribution"
)

// Cross-feed dedup tolerance. The statement-PDF and tx-history-JSON sub-feeds
// overlap (statements reach back to 2017, tx-history only ~2 years) and record
// the same external capital flow with a settlement-vs-trade-date offset
// (observed ~3 days) and sub-dollar rounding. A twin is matched within this
// window — the same tolerance the gold returns layer uses to net internal
// transfers (±3 days, 0.5% / $1).
const (
	crossFeedDayWindow = int64(3)
	crossFeedEpsFloor  = 1.0
	crossFeedEpsRel    = 0.005
)

// externalFlowKinds are the canonical kinds that move owner capital across the
// account boundary — the only ones that affect net_flow (and so returns). Trades
// are deliberately excluded: they have no net cash impact, are an order of
// magnitude noisier across the two feeds (vocabulary, lot granularity), and are
// reconciled by the feed-authority splice, not here.
var externalFlowKinds = map[canonical.TxKind]bool{
	canonical.TxKindDeposit:     true,
	canonical.TxKindWithdrawal:  true,
	canonical.TxKindTransferIn:  true,
	canonical.TxKindTransferOut: true,
	canonical.TxKindJournal:     true,
}

// dedupeCrossFeedExternalFlows drops the statement-PDF copy of an external
// capital flow that the tx-history-JSON feed also records. Both web sub-feeds
// cover the overlap years and book the same wires/journals (with the date offset
// and rounding noted above), so without this the gold transaction set — and the
// returns net_flow derived from it — double-counts every overlapping flow.
//
// Only external-flow kinds are deduped: they are what moves net_flow. Each
// statement-PDF external leg is matched 1:1 against an as-yet-unconsumed
// tx-history-JSON external leg for the same account within the tolerance window;
// on a match the PDF leg is dropped and the (structured) JSON leg kept. Every
// non-external row, and every external leg with no cross-feed twin, passes
// through untouched, so no transaction is lost.
func dedupeCrossFeedExternalFlows(built []builtWebTx) []canonical.TransactionChange {
	type leg struct {
		day      int64
		amount   float64
		consumed bool
	}
	// Index the JSON external legs per account (input-stable order).
	jsonByAcct := map[string][]*leg{}
	for i := range built {
		b := &built[i]
		if b.source == sourceTxHistoryJSON && externalFlowKinds[b.tx.Kind] && b.tx.NetAmount != nil {
			jsonByAcct[b.tx.AccountExternalID] = append(jsonByAcct[b.tx.AccountExternalID],
				&leg{day: b.tx.OccurredAt / 86400, amount: b.tx.NetAmount.InexactFloat64()})
		}
	}

	out := make([]canonical.TransactionChange, 0, len(built))
	for i := range built {
		b := &built[i]
		if b.source == sourceStatementPDF && externalFlowKinds[b.tx.Kind] && b.tx.NetAmount != nil {
			day := b.tx.OccurredAt / 86400
			amt := b.tx.NetAmount.InexactFloat64()
			matched := false
			for _, l := range jsonByAcct[b.tx.AccountExternalID] {
				if l.consumed {
					continue
				}
				if crossFeedMatch(day, amt, l.day, l.amount) {
					l.consumed = true
					matched = true
					break
				}
			}
			if matched {
				continue // duplicate of a JSON leg — drop the PDF copy
			}
		}
		out = append(out, b.tx)
	}
	return out
}

// crossFeedMatch reports whether two external-flow legs (given as epoch-day and
// signed amount) describe the same capital movement under the shared cross-feed
// tolerance: same calendar direction (sign), within ±crossFeedDayWindow days, and
// amounts within max(crossFeedEpsFloor, crossFeedEpsRel·max|amt|). Opposite signs
// never match (math.Abs(a-b) exceeds eps once the signs differ at these scales).
func crossFeedMatch(dayA int64, amtA float64, dayB int64, amtB float64) bool {
	eps := crossFeedEpsFloor
	if r := crossFeedEpsRel * math.Max(math.Abs(amtA), math.Abs(amtB)); r > eps {
		eps = r
	}
	return absInt64(dayA-dayB) <= crossFeedDayWindow && math.Abs(amtA-amtB) <= eps
}

// supersedeSalesWith1099B applies the 1099-B authoritative-for-sales precedence
// (INTEROP §8.1): within any (account, tax_year) that has at least one form_1099b
// lot, the 1099-B is the complete cost-basis-bearing record of sales, so the
// statement_pdf / tx_history_json copies of those sales (anything mapping to
// TxKindSell whose OccurredAt calendar year equals the covered tax_year) are
// dropped and the 1099-B lots kept instead. Outside covered (account, tax_year)
// pairs every feed passes through untouched. form_1099b rows carry no ticker —
// instrument_key is NULL and the only identifier is payload.security_name — so
// unresolved lots are surfaced name-keyed (via Description), never dropped.
func supersedeSalesWith1099B(built []builtWebTx) []builtWebTx {
	// Covered (account, tax_year). tax_year comes from form content
	// (payload.tax_year), not the row timestamp, because a lot sold in
	// December can land on the next year's form.
	type cover struct {
		acct string
		year int
	}
	covered := map[cover]bool{}
	for i := range built {
		b := &built[i]
		if b.source != sourceFORM1099B {
			continue
		}
		if y, ok := extractTaxYear(string(b.tx.Payload)); ok {
			covered[cover{acct: b.tx.AccountExternalID, year: y}] = true
		}
	}
	if len(covered) == 0 {
		return built
	}

	out := make([]builtWebTx, 0, len(built))
	for i := range built {
		b := built[i]
		if (b.source == sourceStatementPDF || b.source == sourceTxHistoryJSON) &&
			b.tx.Kind == canonical.TxKindSell {
			year := time.Unix(b.tx.OccurredAt, 0).UTC().Year()
			if covered[cover{acct: b.tx.AccountExternalID, year: year}] {
				continue // 1099-B is authoritative for sales in this tax year
			}
		}
		out = append(out, b)
	}
	return out
}

// supersedeStatementCashWithDistributions applies the third_party_distribution
// cash-transfer precedence (INTEROP §8.2): a cash distribution (method wire /
// schwab_third_party) may also surface as a statement_pdf / tx_history_json cash
// debit for the same movement. The distribution names the counterparty, so it is
// authoritative — every matching statement/tx-history external debit (within the
// shared cross-feed tolerance) is dropped so the movement isn't summed twice.
// Securities transfers are NOT deduped here (they are new data no other feed
// records); they pass straight through to net_flow. Distribution legs themselves
// are never dropped, and the downstream PDF↔JSON dedup ignores them (neither
// source), so no double-drop occurs.
//
// Matching is intentionally non-consuming: a statement/tx-history debit is
// dropped if ANY distribution leg matches it, so one distribution can supersede
// every same-amount debit inside the window. This biases toward over-drop, never
// under-drop — never summing a movement twice (the spec's goal) beats preserving
// a rare coincidental same-amount, same-window distinct debit. A consuming
// variant would have to consume exactly one copy per feed (PDF and JSON) per
// distribution; getting that per-feed bookkeeping wrong would under-drop and
// re-introduce the double-count this stage exists to prevent, so we keep the
// safe direction.
func supersedeStatementCashWithDistributions(built []builtWebTx) []builtWebTx {
	// Index the authoritative distribution cash legs per account.
	type leg struct {
		day    int64
		amount float64
	}
	distByAcct := map[string][]leg{}
	for i := range built {
		b := &built[i]
		if b.source == sourceThirdPartyDistribution && externalFlowKinds[b.tx.Kind] &&
			b.tx.NetAmount != nil && distributionIsCash(string(b.tx.Payload)) {
			distByAcct[b.tx.AccountExternalID] = append(distByAcct[b.tx.AccountExternalID],
				leg{day: b.tx.OccurredAt / 86400, amount: b.tx.NetAmount.InexactFloat64()})
		}
	}
	if len(distByAcct) == 0 {
		return built
	}

	out := make([]builtWebTx, 0, len(built))
	for i := range built {
		b := built[i]
		if (b.source == sourceStatementPDF || b.source == sourceTxHistoryJSON) &&
			externalFlowKinds[b.tx.Kind] && b.tx.NetAmount != nil {
			day := b.tx.OccurredAt / 86400
			amt := b.tx.NetAmount.InexactFloat64()
			matched := false
			for _, l := range distByAcct[b.tx.AccountExternalID] {
				if crossFeedMatch(day, amt, l.day, l.amount) {
					matched = true
					break
				}
			}
			if matched {
				continue // the distribution is authoritative for this cash movement
			}
		}
		out = append(out, b)
	}
	return out
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// webKind maps the web silver's `kind` discriminator (the
// space-separated text Schwab uses on statements and in
// transaction-history exports) to canonical TxKind. Conservative
// — unmapped strings route to TxKindOther so we never invent
// semantics. Schwab uses different vocab on statements vs the
// Trader API; the api-side mapping is in kindmap.go.
//
// Reinvested dividends land as two separate transactions on
// Schwab statements: the cash-income side ("Reinvest Dividend")
// and the share-purchase side ("Reinvest" / "Reinvest Shares").
// We map them to dividend and buy respectively so the canonical
// single-entry sums stay correct (the two cancel out, net zero
// cash impact). See sign.go for the per-kind sign rules.
//
// Options trades use Schwab's open/close legs: Buy to Open and
// Buy to Close both leave cash (acquiring a long position or
// closing a short); Sell to Open and Sell to Close both add
// cash (receiving premium or closing a long). All four collapse
// to plain buy/sell at the canonical layer.
//
// Corporate actions (Spin-off, Split, Reverse Split, Exchange,
// Reorganized Issue, Return Of Capital, Cash In Lieu, etc.)
// collapse to TxKindCorporateAction. The canonical sign helper
// passes source-supplied signs through for that kind because
// the cash impact varies: cash-in-lieu yields cash, a plain
// split is zero, a cash merger pays out.
func webKind(s string) canonical.TxKind {
	switch s {
	case "Buy", "Buy to Open", "Buy to Close", "Purchase",
		"Reinvest", "Reinvest Shares":
		return canonical.TxKindBuy
	case "Sell", "Sell to Open", "Sell to Close", "Sale", "Redemption":
		return canonical.TxKindSell
	case "Cash Dividend", "Dividend", "Non-Qualified Div",
		"Qualified Dividend", "Reinvest Dividend",
		"Pr Yr Cash Div", "Special Dividend", "Special Qual Div":
		return canonical.TxKindDividend
	case "Bond Interest", "Credit Interest", "Interest",
		"Bank Interest", "Bank Interest Adj", "Margin Interest":
		return canonical.TxKindInterest
	case "Fee", "Service Fee", "ADR Mgmt Fee":
		return canonical.TxKindFee
	case "Foreign Tax Paid", "NRA Tax Adj", "Pr Yr NRA Tax", "Tax":
		return canonical.TxKindTax
	case "Deposit", "MoneyLink Deposit", "Funds Received", "Wire Received":
		return canonical.TxKindDeposit
	case "Withdrawal", "Wire Sent":
		return canonical.TxKindWithdrawal
	case "Transfer Out":
		// Directional third_party_distribution rows (INTEROP §8.2);
		// distinct from the undirected "Transfer" below, which stays
		// a Journal because its capital direction is unknown.
		return canonical.TxKindTransferOut
	case "Transfer In":
		return canonical.TxKindTransferIn
	case "MoneyLink Transfer", "Transfer", "Security Transfer",
		"Journal", "Journaled Shares":
		return canonical.TxKindJournal
	case "Exchange", "Reorganized Issue", "Spin-off", "Split",
		"Reverse Split", "Return Of Capital", "Cash In Lieu",
		"Litigation", "Unissued Rights Redemption":
		return canonical.TxKindCorporateAction
	}
	return canonical.TxKindOther
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// schwabWebTxPayload captures the payload shapes the silver
// produces side-by-side. Statement-PDF rows use lower-case keys
// with numeric values (`amount`, `quantity`, `price`).
// Transaction-history JSON rows use Schwab's CSV-export-style
// capitalised string keys (`Amount`, `Quantity`, `Price`) with
// values like `"$384.01"`, `"(25,000.00)"`, or `""`.
// third_party_distribution rows carry the external-flow magnitude
// as `market_value` (securities) or `cash_amount` (cash) per the
// documented payload contract (schwab-web DESIGN.md §6 / INTEROP
// §8.2-8.3) — they don't structure a plain `amount`, so those are
// the NetAmount source for transfer rows. Both payloads land in
// the same silver row keyed by source, so the adapter probes
// every shape.
type schwabWebTxPayload struct {
	Amount   *float64 `json:"amount"`
	Quantity *float64 `json:"quantity"`
	Price    *float64 `json:"price"`

	// Distribution external-flow magnitude. Only consulted when
	// neither `amount` nor `Amount` is present, so a row that does
	// carry a plain amount is unaffected (and securities and cash
	// transfers never both set their magnitude key on one row).
	MarketValue *float64 `json:"market_value"`
	CashAmount  *float64 `json:"cash_amount"`

	AmountStr   string `json:"Amount"`
	QuantityStr string `json:"Quantity"`
	PriceStr    string `json:"Price"`
}

// extractWebTxAmounts parses NetAmount, Quantity, and Price out
// of a schwab-web transaction payload, handling both the
// statement_pdf and tx_history_json shapes. Errors are swallowed
// — a malformed field just stays nil so one bad row doesn't
// abort the whole batch.
func extractWebTxAmounts(payload string) (netAmount, quantity, price *canonical.Decimal) {
	var p schwabWebTxPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, nil, nil
	}
	netAmount = pickWebAmount(p.Amount, p.AmountStr)
	if netAmount == nil {
		// third_party_distribution transfer rows carry the
		// external-flow magnitude under `market_value` (securities)
		// or `cash_amount` (cash), not a plain `amount` (DESIGN.md
		// §6 / INTEROP §8.2-8.3). The canonical sign helper applied
		// in the build loop turns this magnitude into the directional
		// net_flow (e.g. TxKindTransferOut → negative outflow).
		netAmount = pickWebAmount(p.MarketValue, "")
		if netAmount == nil {
			netAmount = pickWebAmount(p.CashAmount, "")
		}
	}
	quantity = pickWebAmount(p.Quantity, p.QuantityStr)
	price = pickWebAmount(p.Price, p.PriceStr)
	return
}

// extractWebTxDescription pulls the security name out of the
// silver payload. statement_pdf uses lower-case `description`;
// tx_history_json uses capital `Description`. Returns nil when
// both are absent or empty so cash-only rows (interest, fees,
// transfers) don't get a spurious name.
func extractWebTxDescription(payload string) *string {
	var p struct {
		Lower string `json:"description"`
		Upper string `json:"Description"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil
	}
	for _, s := range []string{p.Lower, p.Upper} {
		s = strings.TrimSpace(s)
		if s != "" {
			return &s
		}
	}
	return nil
}

// extractTaxYear reads payload.tax_year off a form_1099b row. The tax year is
// form content (the year the lot is REPORTED under), not derivable from the row
// timestamp, because a December sale can land on the following year's form.
// Returns ok=false when the key is absent/null so an unparseable lot simply
// doesn't establish a covered (account, tax_year) — it never silently coerces.
func extractTaxYear(payload string) (int, bool) {
	var p struct {
		TaxYear *int `json:"tax_year"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.TaxYear == nil {
		return 0, false
	}
	return *p.TaxYear, true
}

// distributionIsCash reports whether a third_party_distribution row is a cash
// transfer (payload.transfer_kind == "cash"). Cash transfers may overlap a
// statement cash debit and are deduped against it; securities transfers
// (transfer_kind == "securities") are new data and are left untouched.
func distributionIsCash(payload string) bool {
	var p struct {
		TransferKind string `json:"transfer_kind"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	return p.TransferKind == "cash"
}

func pickWebAmount(num *float64, str string) *canonical.Decimal {
	if num != nil {
		d := canonical.NewDecimalFromFloat(*num)
		return &d
	}
	d, ok := parseSchwabAmountString(str)
	if !ok {
		return nil
	}
	return &d
}

// parseSchwabAmountString parses Schwab's CSV-export string
// format: optional surrounding parens (negative), optional "$"
// prefix, optional "-" sign, comma thousands separators.
// Returns ok=false for empty or unparseable input.
func parseSchwabAmountString(s string) (canonical.Decimal, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return canonical.Decimal{}, false
	}
	negative := false
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		negative = true
		s = s[1 : len(s)-1]
	}
	s = strings.NewReplacer("$", "", ",", "", " ", "").Replace(s)
	if strings.HasPrefix(s, "-") {
		negative = !negative
		s = s[1:]
	}
	if s == "" {
		return canonical.Decimal{}, false
	}
	d, err := canonical.NewDecimalFromString(s)
	if err != nil {
		return canonical.Decimal{}, false
	}
	if negative {
		d = d.Neg()
	}
	return d, true
}
