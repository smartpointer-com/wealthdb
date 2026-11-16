package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotActions are the lot engine's readings of the actions an adapter
// states: every action but the third-currency fee, which reads by the
// kind (lotClassify).
var lotActions = map[canonical.TxAction]lots.Classified{
	canonical.TxActionIgnore:          {Action: lots.Ignore},
	canonical.TxActionIn:              {Action: lots.In},
	canonical.TxActionOut:             {Action: lots.Out},
	canonical.TxActionIncome:          {Action: lots.Income},
	canonical.TxActionAcquired:        {Action: lots.Acquired},
	canonical.TxActionConverted:       {Action: lots.Exchange, Disposal: lots.DisposeSell},
	canonical.TxActionSpend:           {Action: lots.Exchange, Disposal: lots.DisposeSpend},
	canonical.TxActionFee:             {Action: lots.Exchange, Disposal: lots.DisposeFee},
	canonical.TxActionLost:            {Action: lots.Gone, Disposal: lots.DisposeLost},
	canonical.TxActionGift:            {Action: lots.Gone, Disposal: lots.DisposeGift},
	canonical.TxActionReorganisation:  {Action: lots.Corporate, Rule: lots.CorpReorg},
	canonical.TxActionReturnOfCapital: {Action: lots.Corporate, Rule: lots.CorpReturnOfCapital},
	canonical.TxActionCashInLieu:      {Action: lots.Corporate, Rule: lots.CorpCashInLieu},
	canonical.TxActionExpiry:          {Action: lots.Corporate, Rule: lots.CorpExpiry},
	canonical.TxActionCashOnly:        {Action: lots.Corporate, Rule: lots.CorpIgnore},
}

// lotClassify reads a transaction of kind and quantity qty by its
// action: an action the engine knows holds, a third-currency fee reads
// by the kind with the fee, and no action reads by the kind alone
// (lots.DefaultClassify).
func lotClassify(kind string, qty float64, action canonical.TxAction) lots.Classified {
	if c, ok := lotActions[action]; ok {
		return c
	}
	c := lots.DefaultClassify(lots.Txn{Kind: kind, Quantity: qty})
	c.Fee = action == canonical.TxActionThirdCurrencyFee
	return c
}

func (f *lotFeed) readTransactions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, transaction_external_id, occurred_at, account_external_id,
       instrument_external_id, instrument_hint, kind, currency,
       CAST(quantity AS DOUBLE), CAST(net_amount AS DOUBLE), COALESCE(action, ''),
       COALESCE(TRY_CAST(json_extract_string(payload, '$.fee_amount') AS DOUBLE), 0),
       COALESCE(json_extract_string(payload, '$.fee_currency'), ''),
       CASE WHEN json_extract_string(payload, '$.`+canonical.PayloadEquityTransferLedger+`') = 'true'
            THEN COALESCE(TRY_CAST(json_extract_string(payload, '$.`+canonical.PayloadCostBasis+`') AS DOUBLE), 0) ELSE 0 END
  FROM transactions
 WHERE `+inBoundList("silver_source_id")+`
   AND (instrument_external_id IS NOT NULL OR instrument_hint IS NOT NULL)
 ORDER BY occurred_at, silver_source_id, transaction_external_id`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read transactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			src, txn, acct, kind, ccy string
			inst, hint                sql.NullString
			at                        int64
			qty, amt                  sql.NullFloat64
			action                    canonical.TxAction
			fee, ledgerCost           float64
			feeCcy                    string
		)
		if err := rows.Scan(&src, &txn, &at, &acct, &inst, &hint, &kind, &ccy, &qty, &amt, &action, &fee, &feeCcy,
			&ledgerCost); err != nil {
			return fmt.Errorf("lots: read transactions: %w", err)
		}
		s := f.sources[src]
		name, isHint := inst.String, false
		if !inst.Valid {
			name, isHint = hint.String, true
		}
		c := lotClassify(kind, qty.Float64, action)
		if c.Action == lots.Ignore {
			continue
		}
		k := f.key(s, acct, name, isHint)
		if k < 0 {
			continue
		}
		ka := [2]string{src, f.info[k].acct}
		f.trades[ka] = append(f.trades[ka], lotTrade{at: at, key: k})
		if ka[1] != acct {
			w := lotWalletRef{k, acct}
			f.walletTrades[w] = append(f.walletTrades[w], lotWalletTrade{at, qty.Float64})
		}
		q := math.Abs(qty.Float64)
		ev := lots.Event{At: at, Key: k, Txn: txn, Account: acct, Qty: q, AcqDay: lots.DayOf(at)}
		aux := lotAux{ccy: ccy}
		if c.Fee && fee != 0 {
			aux.fee, aux.feeCcy = math.Abs(fee), feeCcy
		}
		switch c.Action {
		case lots.Buy:
			if ccy != name {
				f.setCurrency(k, ccy, false)
			}
			ev.Kind, ev.Origin, ev.CostOrigin = lots.Acquire, lots.OriginBuy, lots.CostTrade
			ev.Value, ev.HasValue = math.Abs(amt.Float64), amt.Valid
		case lots.Sell:
			if ccy != name {
				f.setCurrency(k, ccy, false)
			}
			ev.Kind, ev.Disposal = lots.Dispose, c.Disposal
			ev.Value, ev.HasValue = math.Abs(amt.Float64), amt.Valid
		case lots.Income, lots.Acquired:
			ev.Kind, ev.Origin, ev.CostOrigin, aux.fmv = lots.Acquire, lots.OriginIncome, lots.CostFMV, true
			if c.Action == lots.Acquired {
				ev.Origin = lots.OriginBuy
			}
		case lots.Exchange:
			ev.Kind, ev.Disposal = lots.Dispose, c.Disposal
			aux.fmv = true
		case lots.Gone:
			ev.Kind, ev.Disposal = lots.Dispose, c.Disposal
		case lots.In, lots.Out:
			f.legs = append(f.legs, lotLeg{src: s, key: k, inst: name, txn: txn, acct: acct, at: at, qty: q,
				in: c.Action == lots.In, cost: ledgerCost})
			continue
		case lots.Corporate:
			f.corp = append(f.corp, lotCorpLeg{src: s, key: k, acct: acct, txn: txn, at: at,
				qty: qty.Float64, amt: amt.Float64, ccy: ccy, has: amt.Valid && amt.Float64 != 0, rule: c.Rule})
			continue
		}
		if !(q > 0) {
			continue
		}
		f.add(ev, aux)
	}
	return rows.Err()
}

// readDocumented marks the transactions the sources document
// (documented_txns, the test the gains reports apply): the engine's
// realized lots for them are not primary, so no sale counts twice.
func (f *lotFeed) readDocumented(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT src, txn FROM documented_txns() WHERE `+inBoundList("src"), f.ids)
	if err != nil {
		return fmt.Errorf("lots: read documented sales: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			return err
		}
		f.documented[k] = true
	}
	return rows.Err()
}
