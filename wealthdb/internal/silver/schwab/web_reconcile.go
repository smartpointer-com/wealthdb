package schwab

import (
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

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
