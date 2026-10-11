package angellist

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// kindForFundingType maps an AngelList funding-ledger transaction type to a
// canonical TxKind. The funding account is the holder's cash account:
//
//	deposit                      external bank -> account   (TxKindDeposit, +)
//	withdrawal / transfer        account -> external bank   (TxKindWithdrawal, -)
//	investment                   account -> an SPV/fund     (TxKindContribution, -)
//	refund                       an over-subscribed commitment returned to the
//	                             account (TxKindContribution reversal, +)
//	disbursement                 a deal pays out -> account: an SPV's is the
//	                             sale of its one holding (TxKindSell, +), a
//	                             fund's a distribution (TxKindDistribution, +)
//
// refund maps to contribution (a positive reversal) rather than distribution
// so DPI / return metrics stay clean — it is returned un-deployed capital,
// not a return on investment. An SPV holds one company, so what it pays
// out is the proceeds of that stake, which the lot engine realizes
// against the paid-in basis; a fund's distribution returns capital and
// the fund goes on. The raw AngelList type rides in the payload.
func kindForFundingType(t, offering string) canonical.TxKind {
	switch t {
	case "deposit":
		return canonical.TxKindDeposit
	case "withdrawal", "transfer":
		return canonical.TxKindWithdrawal
	case "investment", "refund":
		return canonical.TxKindContribution
	case "disbursement":
		if offering == "spv" {
			return canonical.TxKindSell
		}
		return canonical.TxKindDistribution
	default:
		return canonical.TxKindOther
	}
}

// Transactions yields the funding-account cash ledger
// (silver.funding_transactions) as canonical transactions — the real DATED
// cash flows. AngelList already signs the amounts authoritatively (+ in / −
// out) and they reconcile to the funding balance, so they are used directly
// (the source sign wins — e.g. a refund is a positive `contribution`
// reversal — so ApplyCanonicalSign is intentionally bypassed). The K-1
// annual Line 19(a) distribution is NOT emitted here: it was redundant with
// these dated disbursements.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	account, err := c.accountSlug(ctx)
	if err != nil {
		return nil, err
	}
	var txs []canonical.TransactionChange
	if account != "" {
		const q = `
SELECT ft.transaction_external_id, ft.occurred_at, COALESCE(ft.type, ''),
       ft.amount_minor, COALESCE(ft.currency, 'USD'),
       COALESCE(ft.description, ''), COALESCE(ft.syndicate_name, ''),
       COALESCE(ft.position_external_id, ''), COALESCE(o.kind, '')
  FROM funding_transactions ft
  LEFT JOIN offerings o ON o.position_external_id = ft.position_external_id
 WHERE ft.occurred_at BETWEEN ? AND ?
 ORDER BY ft.occurred_at, ft.transaction_external_id`
		rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
		if err != nil {
			return nil, fmt.Errorf("Transactions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id, ftype, ccy, desc, synd, posID, offering string
				occurred                                    int64
				amtMinor                                    sql.NullInt64
			)
			if err := rows.Scan(&id, &occurred, &ftype, &amtMinor, &ccy, &desc, &synd, &posID, &offering); err != nil {
				return nil, err
			}
			var net *canonical.Decimal
			if amtMinor.Valid {
				d := canonical.Decimal(decimal.New(amtMinor.Int64, -2)) // source-signed
				net = &d
			}
			tx := canonical.TransactionChange{
				TransactionExternalID: "funding:" + id,
				OccurredAt:            occurred,
				AccountExternalID:     account,
				Kind:                  kindForFundingType(ftype, offering),
				Currency:              ccy,
				GrossAmount:           net,
				NetAmount:             net,
				Payload:               fundingPayload(ftype, synd),
			}
			if desc != "" {
				tx.Description = &desc
			}
			if posID != "" { // resolved SPV/fund this cash flow concerns
				inst := posID
				tx.InstrumentExternalID = &inst
			}
			txs = append(txs, tx)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return silver.NewTransactionStream(canonical.TransactionBatch{Transactions: txs}), nil
}

// fundingPayload records the raw AngelList type (and the syndicate, for
// investment/refund) so the source classification survives the canonical
// mapping. json.Marshal handles any quoting in the syndicate name.
func fundingPayload(ftype, synd string) json.RawMessage {
	m := map[string]string{"angellist_type": ftype}
	if synd != "" {
		m["syndicate"] = synd
	}
	b, _ := json.Marshal(m)
	return b
}
