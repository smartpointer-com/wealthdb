package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotTxnColumns are the columns of a transaction a policy's Classify
// reads beside its kind and quantity (lots.Txn), as lotTxnFields scans
// them.
const lotTxnColumns = `COALESCE(description, ''),
       COALESCE(json_extract_string(payload, '$.type'), ''),
       COALESCE(json_extract_string(payload, '$.comment'), ''),
       COALESCE(json_extract_string(payload, '$.Action'), json_extract_string(payload, '$.action'), ''),
       COALESCE(json_extract_string(payload, '$.buy_currency'), ''),
       COALESCE(json_extract_string(payload, '$.sell_currency'), ''),
       COALESCE(TRY_CAST(json_extract_string(payload, '$.fee_amount') AS DOUBLE), 0),
       COALESCE(json_extract_string(payload, '$.fee_currency'), '')`

// lotTxnFields are the fields of t that lotTxnColumns fill, in their
// order.
func lotTxnFields(t *lots.Txn) []any {
	return []any{&t.Description, &t.Type, &t.Comment, &t.Action, &t.BuyCurrency, &t.SellCurrency, &t.FeeAmount,
		&t.FeeCurrency}
}

func (f *lotFeed) readTransactions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, transaction_external_id, occurred_at, account_external_id,
       instrument_external_id, instrument_hint, kind, currency,
       CAST(quantity AS DOUBLE), CAST(net_amount AS DOUBLE), `+lotTxnColumns+`,
       CASE WHEN json_extract_string(payload, '$.equity_transfer_ledger') = 'true'
            THEN COALESCE(TRY_CAST(json_extract_string(payload, '$.cost_basis') AS DOUBLE), 0) ELSE 0 END
  FROM transactions
 WHERE `+inLotSources("silver_source_id")+`
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
			t                         lots.Txn
			ledgerCost                float64
		)
		dest := append([]any{&src, &txn, &at, &acct, &inst, &hint, &kind, &ccy, &qty, &amt}, lotTxnFields(&t)...)
		if err := rows.Scan(append(dest, &ledgerCost)...); err != nil {
			return fmt.Errorf("lots: read transactions: %w", err)
		}
		s := f.sources[src]
		name, isHint := inst.String, false
		if !inst.Valid {
			name, isHint = hint.String, true
		}
		t.ID, t.Kind, t.Quantity = txn, kind, qty.Float64
		classify := s.pol.Classify
		if classify == nil {
			classify = lots.DefaultClassify
		}
		c := classify(t)
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
		if c.Fee && t.FeeAmount != 0 {
			aux.fee, aux.feeCcy = math.Abs(t.FeeAmount), t.FeeCurrency
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
	rows, err := db.QueryContext(ctx, `SELECT src, txn FROM documented_txns() WHERE `+inLotSources("src"), f.ids)
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
