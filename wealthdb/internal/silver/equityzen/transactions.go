package equityzen

import (
	"context"
	"database/sql"
	"encoding/json"
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
       c.kind, c.amount, c.execution_fee, c.shares, c.price_per_share,
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
// half. The legs of every event net to zero, so no cash position is implied
// on the account (the same shape as a brokerage's same-day deposit + buy).
// Every leg is linked to the offering's instrument.
//
//	purchase, spv          deposit (+) + buy          (−)   shares bought
//	purchase, private_fund deposit (+) + contribution (−)   capital contributed
//	distribution, spv      sell (+) + withdrawal      (−)   underlying realized
//	distribution, fund     distribution (+) + withdrawal (−)
//
// A purchase EquityZen charged an execution fee on is a THREE-leg event: the
// fee is charged on top, so the cash that crossed is `amount + execution_fee`
// and the deposit leg carries it, while the investment leg keeps `amount` —
// the basis EquityZen's own capital-account statement states, and the figure
// `shares × price_per_share` ties to. The fee is its own `fee` leg, so the
// three still net to zero.
//
//	purchase with a fee    deposit (+ amount+fee) + buy (− amount) + fee (− fee)
//
// Booking it that way rather than folding it into the buy is what keeps BOTH
// cost bases derivable — excluding fees from the investment leg alone,
// including them by adding the fee legs linked to it (see feePayload). Which
// one is correct is jurisdiction-dependent, so the ledger records the fee as
// a fact and leaves the choice to whatever reads it.
//
// A distribution's fee is NOT treated yet: for a purchase the direction is
// proven by the funding bank leg (debit == amount + fee, to the cent), and a
// distribution has no such witness whenever its cash lands in an account
// loaded positions-only. Until the deal's own statement
// settles whether such a fee is deducted from proceeds or charged on top, the
// distribution legs stay as they were.
//
// A $0 distribution (an exit with no proceeds, e.g. a bankruptcy) emits the
// sell/distribution leg at $0 and omits the meaningless $0 withdrawal — still
// a net-0 event. Amounts are positive magnitudes in silver; ApplyCanonicalSign
// pins the canonical direction. The buy / sell legs carry the share lot +
// price; the pure-cash legs do not.
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
			amount, fee, shares, price       sql.NullFloat64
		)
		if err := rows.Scan(&cfID, &deal, &occurred, &cfKind, &amount, &fee,
			&shares, &price, &ccy, &offKind); err != nil {
			return nil, err
		}
		if occurred < w.Start || occurred > w.End {
			continue
		}
		isZero := !amount.Valid || amount.Float64 == 0
		// A fee is only meaningful beside an amount; a fee on a NULL amount
		// would have nothing to be a fee ON.
		feeAmt := 0.0
		if fee.Valid && amount.Valid {
			feeAmt = fee.Float64
		}

		// emitLeg appends one leg — every leg sits on the custody account and
		// links to the deal's instrument. `value` is the leg's own magnitude;
		// ApplyCanonicalSign pins the direction from the kind.
		emitLeg := func(kind canonical.TxKind, value *canonical.Decimal,
			withLot bool, payload json.RawMessage) {
			signed := canonical.ApplyCanonicalSign(kind, value)
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
				Payload:               payload,
			}
			if withLot {
				tx.Quantity = silver.DecimalPtrFromNullFloat(shares)
				tx.Price = silver.DecimalPtrFromNullFloat(price)
			}
			txs = append(txs, tx)
		}

		// emit is the common case: a leg carrying the flow's own amount.
		emit := func(kind canonical.TxKind, withLot bool) {
			emitLeg(kind, silver.DecimalPtrFromNullFloat(amount), withLot, nil)
		}

		// emitPurchase writes the funding side of a purchase. Without a fee
		// that is the deposit + investment pair; with one the deposit carries
		// what actually left the bank and the fee stands as its own leg.
		emitPurchase := func(invest canonical.TxKind, withLot bool) {
			if feeAmt <= 0 {
				emit(canonical.TxKindDeposit, false)
				emit(invest, withLot)
				return
			}
			funded := canonical.NewDecimalFromFloat(amount.Float64 + feeAmt)
			emitLeg(canonical.TxKindDeposit, &funded, false, nil)
			emit(invest, withLot)
			charged := canonical.NewDecimalFromFloat(feeAmt)
			emitLeg(canonical.TxKindFee, &charged, false, feePayload(cfID, invest))
		}

		switch {
		case cfKind == "purchase" && offKind == "spv":
			emitPurchase(canonical.TxKindBuy, true) // cash in, shares acquired
		case cfKind == "purchase":
			emitPurchase(canonical.TxKindContribution, false) // cash in, capital contributed
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

// feePayload ties an execution fee to the leg it paid for.
//
// The fee points at the trade and never the reverse: one lot can accrue
// several costs over its life, but each cost belongs to exactly one lot.
// `fee_for` names the target by its `transaction_external_id`, which with
// `silver_source_id` is gold's primary key on `transactions`, so a
// cost-basis or tax feature joins on it directly:
//
//	SELECT b.net_amount + COALESCE(SUM(f.net_amount), 0)
//	  FROM transactions b
//	  LEFT JOIN transactions f
//	         ON f.silver_source_id = b.silver_source_id
//	        AND f.kind = 'fee'
//	        AND f.payload->>'fee_for' = b.transaction_external_id
//
// `fee_role` says what the cost IS, not what to do with it: an acquisition
// cost may join the basis, a disposal cost may reduce proceeds, and whether
// either does is a question of jurisdiction and year. Recording the role and
// leaving the decision out is what keeps both answers derivable.
func feePayload(cfID string, invest canonical.TxKind) json.RawMessage {
	b, _ := json.Marshal(map[string]string{
		"fee_type": "execution_fee",
		"fee_role": "acquisition",
		"fee_for":  cfID + ":" + string(invest),
	})
	return b
}
