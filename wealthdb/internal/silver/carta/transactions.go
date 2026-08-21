package carta

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// cashFlowQuery selects the dated cash-flow ledger (collector migration 0003).
// flow_date is source TEXT in mixed shapes ('MM/DD/YYYY' for certs / statements,
// 'YYYY-MM-DD' for the cancellation), so it is parsed in Go, not via strftime.
const cashFlowQuery = `
SELECT cash_flow_external_id, entity_external_id, flow_date, kind,
       amount, shares, price_per_share, COALESCE(currency, 'USD'),
       COALESCE(description, '')
  FROM cash_flows
 WHERE flow_date IS NOT NULL
 ORDER BY flow_date, cash_flow_external_id`

// flowDateUnix parses a cash_flows.flow_date to a UTC-midnight unix timestamp,
// accepting both the ISO 'YYYY-MM-DD' and US 'MM/DD/YYYY' shapes the silver
// carries. Returns false if neither layout matches.
func flowDateUnix(s string) (int64, bool) {
	for _, layout := range []string{"2006-01-02", "01/02/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Unix(), true
		}
	}
	return 0, false
}

// Transactions projects the cash-flow ledger (collector DESIGN.md §5.2) as
// DOUBLE-ENTRY pairs on the custody account — the account carrying the value
// spine, so the returns engine sees the flows. Carta exposes no real cash
// balance — a capital call is wired from an external bank straight into the
// SPV/fund, an exercise is paid externally, and exit / distribution proceeds
// leave to an external account — so each event splits into an external-bank
// leg (deposit/withdrawal: cash crossing the Carta boundary, the external
// flows the ReturnsPolicy counts) and a holding leg
// (buy/sell/contribution/distribution: the internal half). The pair nets to
// zero, so no cash position is implied on the account (the same shape as a
// brokerage's same-day deposit + buy). Every leg links to the company's
// instrument; the buy/sell legs additionally carry the share lot + price.
//
//	exercise             deposit (+) + buy          (−)   shares acquired
//	convertible_purchase deposit (+) + buy          (−)   SAFE / note (no lot)
//	capital_call         deposit (+) + contribution (−)   capital into a fund
//	exit                 sell    (+) + withdrawal   (−)   shares realized
//	distribution         distribution (+) + withdrawal (−) fund cash returned
//
// A $0 exit (no recorded proceeds — Carta purges the payout) emits the $0 sell
// and omits the meaningless $0 withdrawal, still a net-0 event. Amounts are
// positive magnitudes in silver; ApplyCanonicalSign pins the canonical
// direction (gross == net; Carta surfaces no separate fee).
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	acct, err := c.accountKey(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, cashFlowQuery)
	if err != nil {
		return nil, fmt.Errorf("Transactions: %w", err)
	}
	defer rows.Close()

	var txs []canonical.TransactionChange
	for rows.Next() {
		var (
			entityID              int64
			cfID, flowDate, kind  string
			ccy, desc             string
			amount, shares, price sql.NullFloat64
		)
		if err := rows.Scan(&cfID, &entityID, &flowDate, &kind, &amount,
			&shares, &price, &ccy, &desc); err != nil {
			return nil, err
		}
		occurred, ok := flowDateUnix(flowDate)
		if !ok || occurred < w.Start || occurred > w.End {
			continue
		}
		isZero := !amount.Valid || amount.Float64 == 0
		inst := instrumentID(entityID)

		// emit appends one leg of the pair — both sit on the custody account
		// and link to the company's instrument; the silver cash-flow
		// description (e.g. a withdrawal's destination bank) rides on
		// each leg.
		emit := func(txKind canonical.TxKind, withLot bool) {
			signed := canonical.ApplyCanonicalSign(txKind, silver.DecimalPtrFromNullFloat(amount))
			i := inst
			tx := canonical.TransactionChange{
				TransactionExternalID: cfID + ":" + string(txKind),
				OccurredAt:            occurred,
				AccountExternalID:     acct,
				InstrumentExternalID:  &i,
				Kind:                  txKind,
				Currency:              ccy,
				GrossAmount:           signed,
				NetAmount:             signed,
			}
			if desc != "" {
				d := desc
				tx.Description = &d
			}
			if withLot {
				tx.Quantity = silver.DecimalPtrFromNullFloat(shares)
				tx.Price = silver.DecimalPtrFromNullFloat(price)
			}
			txs = append(txs, tx)
		}

		switch kind {
		// Auto-derived event kinds → a balanced double-entry pair.
		case "exercise":
			emit(canonical.TxKindDeposit, false) // cash in to fund the exercise
			emit(canonical.TxKindBuy, true)      // cash out to acquire the shares
		case "convertible_purchase":
			emit(canonical.TxKindDeposit, false) // cash in to fund the purchase
			emit(canonical.TxKindBuy, false)     // cash out to acquire the SAFE / note (no share lot yet)
		case "capital_call":
			emit(canonical.TxKindDeposit, false)      // cash in to fund the call
			emit(canonical.TxKindContribution, false) // capital contributed to the fund
		case "exit":
			emit(canonical.TxKindSell, true) // proceeds from realizing the shares
			if !isZero {
				emit(canonical.TxKindWithdrawal, false) // swept to the external bank
			}
		case "distribution":
			emit(canonical.TxKindDistribution, false) // fund cash returned
			if !isZero {
				emit(canonical.TxKindWithdrawal, false)
			}
		// Side-loaded explicit legs (collector `<account_id>-transactions.csv`)
		// → one canonical transaction each; the CSV provides both halves (e.g.
		// a sale plus the withdrawals it splits into), so they net to 0
		// without auto-pairing.
		case "sell":
			emit(canonical.TxKindSell, true)
		case "withdrawal":
			emit(canonical.TxKindWithdrawal, false)
		case "deposit":
			emit(canonical.TxKindDeposit, false)
		case "buy":
			emit(canonical.TxKindBuy, true)
		case "contribution":
			emit(canonical.TxKindContribution, false)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(canonical.TransactionBatch{Transactions: txs}), nil
}
