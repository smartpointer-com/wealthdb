package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// webReader reads from the schwab-web silver SQLite. The web
// feed complements the api feed by:
//
//   - Backfilling transactions older than each account's
//     api-coverage-start (Trader API only goes back ~2y; web
//     statements reach further).
//   - Providing per-statement-period position snapshots, their
//     open lots and cash balances that api doesn't surface at all
//     (api emits only live snapshots per dump run).
//   - Providing the realized lots of the year-end tax documents
//     (realized.go).
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
	// parts is the file's optional schema (schema).
	parts silver.Lazy[webSchema]
	note  silver.SchemaNote
	file  string // the silver's base name, for the note
}

func (r *webReader) Close() error { return silver.CloseDB(&r.db) }

// webTxNot1099B is the web ledger the transaction stream reads: every
// row but the form_1099b ones (Transactions says why).
const webTxNot1099B = `transactions WHERE source <> '` + sourceFORM1099B + `'`

// clock is the web feed's. The snapshot range is the dumps', widened
// to the historical tables (historicalDates), which use as_of_date /
// period_end rather than dump times. The transaction range is the
// ledger without its form_1099b rows. The change number is the latest
// dump or transaction time, the value the window advances the watermark
// to; the historical period ends widen the snapshot range but not the
// change number, since a silver whose latest period end is past its
// latest dump and transaction would otherwise never read as up to date.
//
// A dump or a transaction past the watermark opens a window over the new
// rows, widened to the historical content so the loader's windowed
// delete covers the historical gold rows before they are re-inserted.
// The form_1099b rows reach no stream, so they widen no window.
func (r *webReader) clock() silver.Clock {
	return silver.Clock{
		Kind: "schwab-web",
		Runs: `SELECT MAX(t) FROM (SELECT MAX(snapshot_at) AS t FROM dump_runs
                    UNION ALL SELECT MAX(timestamp) FROM ` + webTxNot1099B + `)`,
		Snapshot: []string{`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs`},
		Extra:    r.historicalDates,
		Ledger:   []string{`SELECT MIN(timestamp), MAX(timestamp) FROM ` + webTxNot1099B},
		Observed: silver.Observed{Runs: silver.LatestDump},
		Mode: silver.Incremental{
			New: []string{
				`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs WHERE snapshot_at > ?`,
				`SELECT MIN(timestamp), MAX(timestamp) FROM ` + webTxNot1099B + ` AND timestamp > ?`,
			},
			Extra: r.historicalDates,
		},
	}
}

// historicalDates is historicalRange where the silver carries the
// historical tables, -1 each where it does not.
func (r *webReader) historicalDates(ctx context.Context) (lo, hi int64, err error) {
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil || !ok {
		return -1, -1, err
	}
	return r.historicalRange(ctx)
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	return r.clock().Status(ctx, r.db)
}

func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return r.clock().ChangeWindow(ctx, r.db, since)
}

// snapshotsDimensions emits AccountChange rows from web's
// `accounts` table for every snapshot in the window, rewriting
// account_external_id from web's suffix to the bridged account id.
// Web supplies the user-friendly `nickname`. With the api beside
// it, the api supplies almost everything else, and the per-column
// upsert guard in gold merges them by (silver_source_id,
// account_external_id).
//
// With the api beside it, an account whose web suffix doesn't bridge
// (e.g. a closed account no longer in api) is skipped: it has no api
// counterpart to merge with, and emitting it under the raw suffix
// would create orphaned rows the rest of gold can't join.
//
// alone says the web silver is configured without the api: each row
// then also carries the display name and currency the api's would.
func (r *webReader) snapshotsDimensions(
	ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
	bridge map[string]string,
	alone bool,
) error {
	if !w.HasChanges {
		return nil
	}
	// Silver migration 0003 added `account_registration` (the
	// verbatim statement-PDF registration label —
	// "Contributory IRA" / "Schwab One® Custodial Account
	// (UTMA)" / "Education Savings" / etc.). Older silvers
	// don't have the column; degrade gracefully.
	s, err := r.schema(ctx)
	if err != nil {
		return err
	}
	regCol := silver.ColumnOr(s.registration, "account_registration")
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, nickname, payload, COALESCE(%s, '')
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`, regCol)
	return silver.ScanRows(ctx, r.db, "schwab-web snapshotsDimensions", q, func(rows *sql.Rows) error {
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
			return nil
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		change := canonical.AccountChange{
			AccountExternalID: hash,
			AccountKind:       canonical.AccountKindBrokerage,
			Nickname:          silver.StrPtrIfNonEmpty(nickname.String),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		}
		if alone {
			// The columns the api's account rows carry otherwise: the
			// account number as its display name, in USD.
			change.DisplayName = silver.StrPtrIfNonEmpty(hash)
			change.BaseCurrency = silver.StrPtrIfNonEmpty("USD")
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
		return nil
	}, w.Start, w.End)
}

// transactionsBeforeAPIStart emits web transactions that are
// strictly older than each account's api-coverage start. Web
// `activity_id`s are a synthetic SHA-256 prefix — they don't
// collide with api's real activityIds. Rewrites web suffix to the
// bridged account id so the row lands under the same gold account as
// the api transactions for that account. With web alone the map of
// starts is empty and every row passes.
//
// `apiStartByHash` maps api hashValue → where that account's api
// history begins (apiCoverageStart). A web tx
// is emitted only when:
//
//  1. its account bridges; AND
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
	//
	// The form_1099b rows stay out. A 1099-B lot is a tax record, not a
	// cash movement: a short closed prints the premium of its opening sale
	// as proceeds on the closing date, and an order filled in several lots
	// prints as several sales. The statement and history feeds book every
	// sale as cash; the lots reach gold as realized lots (realized.go).
	s, err := r.schema(ctx)
	if err != nil {
		return nil, err
	}
	q := `
SELECT activity_id, timestamp, account_external_id, kind, instrument_key, payload, source,
       ` + silver.ColumnOr(s.txColumns, "amount", "quantity", "price", "description", "action") + `
  FROM transactions
 WHERE timestamp BETWEEN ? AND ?
   AND source <> ?
 ORDER BY timestamp, activity_id`

	var built []builtWebTx
	// The last day the tx-history JSON export reaches per account,
	// counted BEFORE the api cutoff drops its rows: the export runs
	// past the cutoff, and its span is what the splice below defers
	// to (spliceNonExternalToJSON).
	jsonThrough := map[string]int64{}
	if err := silver.ScanRows(ctx, r.db, "schwab-web Transactions", q, func(rows *sql.Rows) error {
		var (
			activityID, suffix, kind, payload, source string
			ts                                        int64
			instrumentKey                             sql.NullString
			amount, quantity, price, description      sql.NullString
			action                                    sql.NullString
		)
		if err := rows.Scan(&activityID, &ts, &suffix, &kind, &instrumentKey, &payload, &source,
			&amount, &quantity, &price, &description, &action); err != nil {
			return err
		}
		hash, ok := bridge[suffix]
		if !ok {
			return nil
		}
		if source == sourceTxHistoryJSON {
			jsonThrough[hash] = max(jsonThrough[hash], ts/86400)
		}
		if cutoff, ok := apiStartByHash[hash]; ok && ts >= cutoff {
			return nil
		}
		var instrPtr *string
		if instrumentKey.Valid && instrumentKey.String != "" {
			id := webInstrument(instrumentKey.String, symbolToCUSIP)
			instrPtr = &id
		}
		netAmount := silver.DecimalPtrOrNil(amount)
		security := silver.StrPtrIfNonEmpty(description.String)
		txKind := webKind(kind, netAmount, security, payload)
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
			NetAmount:   webSigned(kind, txKind, netAmount),
			Quantity:    silver.DecimalPtrOrNil(quantity),
			Price:       silver.DecimalPtrOrNil(price),
			Description: webNarrative(action.String, security),
			Payload:     json.RawMessage(payload),
		}})
		return nil
	}, w.Start, w.End, sourceFORM1099B); err != nil {
		return nil, err
	}
	// Reconcile the overlapping web sub-feeds (INTEROP §8), in order:
	//   1. JSON is authoritative for non-external rows across its coverage span
	//      (statement PDFs only backfill older history).
	//   2. A third_party_distribution cash transfer is authoritative over a
	//      matching statement/tx-history cash debit (it names the counterparty);
	//      run before the PDF↔JSON dedup so the final external set is correct.
	//   3. Remaining statement_pdf ↔ tx_history_json external flows are matched
	//      1:1 so net_flow isn't double-counted.
	// Securities distributions are new data (no other feed records them) and
	// pass straight through as TxKindTransferOut external flows.
	built = spliceNonExternalToJSON(built, jsonThrough)
	built = supersedeStatementCashWithDistributions(built)
	out := canonical.TransactionBatch{Transactions: dedupeCrossFeedExternalFlows(built)}
	setLotActions(out.Transactions)
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
// The span ends where the export does, which `through` states per account
// (an epoch day): the api cutoff drops the export's later rows from `built`,
// and a span measured on what is left ends at the last trade before the
// cutoff — one day short of that trade's statement copy, which is dated by
// settlement and would survive as a second booking of it.
//
// External flows are deliberately untouched here — they move net_flow, so they
// use the no-loss 1:1 cross-feed dedup in dedupeCrossFeedExternalFlows, which
// keeps feed-unique legs rather than dropping them. Caveat: an internal gap in
// the JSON export drops the PDF rows inside it too; acceptable because
// non-external rows don't affect net_flow or holdings and the transaction report
// stays single-sourced within the JSON span.
func spliceNonExternalToJSON(built []builtWebTx, through map[string]int64) []builtWebTx {
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

	for account, day := range through {
		if s := jrange[account]; s != nil {
			s.hi = max(s.hi, day)
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
// reconciled against each other (splice + cross-feed dedup). form_1099b holds
// the 1099-B lots, which reach gold as realized lots rather than transactions,
// and third_party_distribution carries transfer flows out (securities = new
// data; cash = deduped against statement debits). See INTEROP §8.
const (
	sourceStatementPDF           = "statement_pdf"
	sourceTxHistoryJSON          = "tx_history_json"
	sourceFORM1099B              = "form_1099b"
	sourceThirdPartyDistribution = "third_party_distribution"
)

// Cross-feed dedup tolerance. The statement-PDF and tx-history-JSON sub-feeds
// overlap (statements reach back years further than the ~2-year tx-history
// export) and record
// the same external capital flow with a settlement-vs-trade-date offset
// (a few days) and sub-dollar rounding. A twin is matched within this
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
// re-introduce the double-count this stage exists to prevent, so the stage
// keeps the safe direction.
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
// — unmapped strings route to TxKindOther so no semantics are
// invented. Schwab uses different vocab on statements vs the
// Trader API; the api-side mapping is in kindmap.go.
//
// Reinvested dividends land as two separate transactions on
// Schwab statements: the cash-income side ("Reinvest Dividend")
// and the share-purchase side ("Reinvest" / "Reinvest Shares").
// They map to dividend and buy respectively so the canonical
// single-entry sums stay correct (the two cancel out, net zero
// cash impact). See canonical/sign.go for the per-kind sign rules.
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
//
// "Transfer Out" is directional, and what it gets wrong is not the
// direction but the KIND. One word covers two movements: a securities
// delivery, which is an in-kind leg, and a CASH distribution, which is
// money leaving the household. Mapped alike they are read alike, and
// TxKindTransferOut is the wrong half of that pair to be read as — the
// cash flow statement excludes an unmatched one as "an in-kind ledger leg
// or a source's own tagging", and neither family's population admits the
// kind, so a cash row lands beyond every tier, the matcher and the pins
// ledger at once. It leaves the pool and nothing draws it. The payload
// already separates the two for the dedupe below (distributionIsCash), so
// the same fact decides the kind here, and a cash row books as the
// withdrawal the API era books the same movement as. The SIGN is not read:
// unlike the undirected words below this one states its direction, and
// ApplyCanonicalSign enforces it.
//
// THREE OF THE TRANSFER WORDS TAKE THEIR DIRECTION FROM THE AMOUNT rather
// than from the word, which states none. A journal is invisible to the
// internal-transfer matcher — which admits no such kind — and excluded from
// the cash flow statement outright, so an undirected word leaves the leg
// waiting at the other end unpairable and drawn as money arriving from, or
// vanishing into, nowhere.
//
// Two of them name an OUTSIDE bank: "MoneyLink Transfer" and the undirected
// "Transfer". The API era books those as withdrawals and deposits, so reading
// the sign is continuity across the feed cutover rather than invented
// semantics. The third is a funds journal inside the statement parser's
// "Unknown" catch-all, matched by narrative rather than by kind because that
// bucket is heterogeneous on purpose (see the case below).
//
// What stays a journal is narrower than "the holder's own accounts", and the
// distinction is the point: "Journal" and the two SHARE journals move
// securities, or move cash the boundary has no interest in, while a funds
// journal can reach an account outside the household. Whether a move
// between two accounts one person can log into crosses the household boundary
// is gold's question, not this adapter's — which is why the answer here is a
// DIRECTION and never a verdict.
//
// A row with no amount, or a zero one, stays a journal either way: the
// sign is the whole of the evidence, and a directionless row has no right
// answer.
func webKind(s string, netAmount *canonical.Decimal, description *string, payload string) canonical.TxKind {
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
		// distinct from the undirected "Transfer" below, whose
		// direction is read off the amount rather than the word.
		// A cash distribution under this word is money out of the
		// household rather than an in-kind leg, and books as a
		// withdrawal so that it reaches the spending population at
		// all (see above).
		if distributionIsCash(payload) {
			return canonical.TxKindWithdrawal
		}
		return canonical.TxKindTransferOut
	case "Transfer In":
		return canonical.TxKindTransferIn
	case "MoneyLink Transfer", "Transfer":
		return webTransferSide(netAmount)
	case "Security Transfer", "Journal", "Journaled Shares":
		return canonical.TxKindJournal
	case "Exchange", "Reorganized Issue", "Spin-off", "Split",
		"Reverse Split", "Return Of Capital", "Cash In Lieu",
		"Litigation", "Unissued Rights Redemption":
		return canonical.TxKindCorporateAction
	case "Unknown":
		// The statement parser's catch-all, and it is heterogeneous on
		// purpose: option legs, ADR fees, corporate actions, share
		// journals. The shapes in it that are plainly cash are read by
		// the name the statement gives them (catchAllShapes); remapping
		// the catch-all wholesale would re-kind everything else in it.
		//
		// One of them is a funds journal between two of the holder's
		// accounts, which the statement narrates as a direction and a
		// partner account. Such a journal is NOT automatically
		// internal: the partner can be an account outside the
		// household. Making the row directional is what lets the
		// household boundary decide — kept as an unsigned catch-all
		// it could decide nothing, and a move out of the household
		// would draw as no movement at all.
		if description == nil {
			break
		}
		if fundsJournalDescription.MatchString(*description) {
			return webTransferSide(netAmount)
		}
		for _, shape := range catchAllShapes {
			if shape.name.MatchString(*description) {
				return shape.kind
			}
		}
	}
	return canonical.TxKindOther
}

// fundsJournalDescription names the funds journal inside the statement
// parser's "Unknown" bucket. Anchored, because the word has to lead: a
// narrative merely mentioning a journal is not one.
var fundsJournalDescription = regexp.MustCompile(`^Journaled Funds\b`)

// catchAllShapes are the other cash shapes of the statement parser's
// "Unknown" bucket, by the name the statement prints first. A short sale
// is cash raised by selling what the account does not hold (a stock, or
// an option written) and a cover the cash spent closing it; the rest are
// a pass-through or borrow fee, a withholding or its reclaim, and a
// fund's capital-gain payout or a dividend's correction. Anchored like
// the journal above. Whatever else the bucket carries — a share
// journal, an in-kind transfer, a corporate action, an expiry — moves
// no cash of its own and stays TxKindOther.
var catchAllShapes = []struct {
	name *regexp.Regexp
	kind canonical.TxKind
}{
	{regexp.MustCompile(`^Short Sale\b`), canonical.TxKindSell},
	{regexp.MustCompile(`^Cover Short\b`), canonical.TxKindBuy},
	{regexp.MustCompile(`^(?:ADR Pass Thru Fee|Service Fee)\b`), canonical.TxKindFee},
	{regexp.MustCompile(`^(?:Foreign Tax Paid|Frgn Tax Reclaim|NRA Withholding)\b`), canonical.TxKindTax},
	{regexp.MustCompile(`^(?:Adjust Dividend|LT Cap Gain|Short Term Cap Gn)\b`), canonical.TxKindDividend},
}

// webTransferSide reads an external transfer's direction off its amount, and
// refuses to guess one for a row that carries none.
func webTransferSide(netAmount *canonical.Decimal) canonical.TxKind {
	switch {
	case netAmount == nil || netAmount.IsZero():
		return canonical.TxKindJournal
	case netAmount.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}

// webSigned orients a web row's figure by its kind. The web feeds print
// most figures as magnitudes and leave the direction to the section or
// the word — a reinvestment, a distribution's market value — so the
// kind's canonical sign is applied. A minus printed on an inflow is the
// exception and is kept: the statements print one only on a correction
// (a dividend clawed back), which the canonical sign would book as a
// second dividend.
//
// The statement parser's "Unknown" bucket keeps its sign whole. Its rows
// carry the figure as printed, a debit in parentheses, and the kind a
// shape in it is read as does not state a direction the row lacks: a
// withholding reclaimed is a tax row with a credit.
func webSigned(silverKind string, kind canonical.TxKind, amount *canonical.Decimal) *canonical.Decimal {
	if silverKind == "Unknown" {
		return amount
	}
	signed := canonical.ApplyCanonicalSign(kind, amount)
	if amount != nil && amount.IsNegative() && signed != nil && signed.IsPositive() {
		return amount
	}
	return signed
}

// webNarrative is the row's narrative for gold: the movement, then the
// security it concerns — the fidelity adapter's action-then-security
// reading. The security alone cannot tell a dividend from the withholding
// taken from it, or an ADR's pass-through fee from either: all three name
// the same security. The movement is the row's action column: the
// transaction history's `Action` ("NRA Tax Adj", "ADR Mgmt Fee"), or on a
// statement row its `action` or `category`, where it states one.
func webNarrative(movement string, security *string) *string {
	sec := ""
	if security != nil {
		sec = *security
	}
	if movement != "" && strings.HasPrefix(strings.ToUpper(sec), strings.ToUpper(movement)) {
		movement = "" // the narrative already leads with it
	}
	return silver.StrPtrIfNonEmpty(strings.TrimSpace(movement + " " + sec))
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
