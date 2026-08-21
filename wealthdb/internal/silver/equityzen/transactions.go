package equityzen

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// cashFlowQuery selects the dated money ledger joined to its offering (the
// offering's kind decides how a flow maps). flow_date is source ISO TEXT →
// unix seconds.
const cashFlowQuery = `
SELECT c.cash_flow_external_id, c.deal_external_id,
       CAST(strftime('%s', c.flow_date) AS INTEGER) AS occurred,
       c.kind, c.amount, c.shares, c.price_per_share,
       COALESCE(c.currency, 'USD'), COALESCE(o.kind, '')
  FROM cash_flows c
  JOIN offerings o ON o.deal_external_id = c.deal_external_id
 WHERE c.flow_date IS NOT NULL
 ORDER BY occurred, c.cash_flow_external_id`

// Transactions yields the buyer's cash ledger as DOUBLE-ENTRY pairs on the
// custody account — the account carrying the value spine, so the returns
// engine sees the flows. EquityZen does not expose the real external funding
// account (unlike the angellist sibling, whose source carries a true funding
// ledger), so each investment leg is paired with an offsetting cash-conduit
// leg: deposit/withdrawal is the cash crossing the EquityZen boundary (the
// external flow the ReturnsPolicy counts), the investment leg the internal
// half. The two legs of every event net to zero, so no cash position is
// implied on the account (the same shape as a brokerage's same-day deposit +
// buy). Every leg is linked to the offering's instrument.
//
//	purchase, spv          deposit (+) + buy          (−)   shares bought
//	purchase, private_fund deposit (+) + contribution (−)   capital contributed
//	distribution, spv      sell (+) + withdrawal      (−)   underlying realized
//	distribution, fund     distribution (+) + withdrawal (−)
//
// A $0 distribution (an exit with no proceeds, e.g. a bankruptcy) emits the
// sell/distribution leg at $0 and omits the meaningless $0 withdrawal — still
// a net-0 event. Amounts are positive magnitudes in silver; ApplyCanonicalSign
// pins the canonical direction. execution_fee is informative, not netted (so
// gross == net). The buy / sell legs carry the share lot + price; the pure-cash
// legs do not.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	rows, err := c.db.QueryContext(ctx, cashFlowQuery)
	if err != nil {
		return nil, fmt.Errorf("Transactions: %w", err)
	}
	defer rows.Close()

	var txs []canonical.TransactionChange
	for rows.Next() {
		var (
			cfID, deal, cfKind, ccy, offKind string
			occurred                         int64
			amount, shares, price            sql.NullFloat64
		)
		if err := rows.Scan(&cfID, &deal, &occurred, &cfKind, &amount,
			&shares, &price, &ccy, &offKind); err != nil {
			return nil, err
		}
		if occurred < w.Start || occurred > w.End {
			continue
		}
		isZero := !amount.Valid || amount.Float64 == 0

		// emit appends one leg of the double-entry pair — both legs sit on the
		// custody account and link to the deal's instrument.
		emit := func(kind canonical.TxKind, withLot bool) {
			signed := canonical.ApplyCanonicalSign(kind, silver.DecimalPtrFromNullFloat(amount))
			inst := deal
			tx := canonical.TransactionChange{
				TransactionExternalID: cfID + ":" + string(kind),
				OccurredAt:            occurred,
				AccountExternalID:     accountKey,
				InstrumentExternalID:  &inst,
				Kind:                  kind,
				Currency:              ccy,
				GrossAmount:           signed,
				NetAmount:             signed,
			}
			if withLot {
				tx.Quantity = silver.DecimalPtrFromNullFloat(shares)
				tx.Price = silver.DecimalPtrFromNullFloat(price)
			}
			txs = append(txs, tx)
		}

		switch {
		case cfKind == "purchase" && offKind == "spv":
			emit(canonical.TxKindDeposit, false) // cash in to fund the buy
			emit(canonical.TxKindBuy, true)      // cash out to acquire shares
		case cfKind == "purchase":
			emit(canonical.TxKindDeposit, false)      // cash in to fund the call
			emit(canonical.TxKindContribution, false) // capital contributed to the fund
		case cfKind == "distribution" && offKind == "spv":
			emit(canonical.TxKindSell, true) // proceeds from realizing the underlying
			if !isZero {
				emit(canonical.TxKindWithdrawal, false) // swept to the external bank
			}
		default: // distribution on a fund (or unknown kind)
			emit(canonical.TxKindDistribution, false)
			if !isZero {
				emit(canonical.TxKindWithdrawal, false)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(canonical.TransactionBatch{Transactions: txs}), nil
}
