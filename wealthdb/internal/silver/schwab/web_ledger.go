package schwab

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

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
