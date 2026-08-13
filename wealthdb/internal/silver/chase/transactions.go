package chase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Transactions yields the whole deposit ledger. Chase's silver `amount` is
// already signed the canonical way (positive = balance increase, debits
// negative), so the source sign is preserved rather than forced; summing
// NetAmount reproduces the account's net cash flow. The QFX FITID is the stable
// external id.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	ccy, err := c.accountCurrencies(ctx)
	if err != nil {
		return nil, err
	}
	const q = `
SELECT fitid, posted_at, account_external_id, amount,
       COALESCE(kind, ''), COALESCE(description, ''), payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?
 ORDER BY posted_at, fitid`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("chase Transactions: %w", err)
	}
	defer rows.Close()

	var out canonical.TransactionBatch
	for rows.Next() {
		var (
			fitid, id, kind, desc, payload string
			posted                         int64
			amtFloat                       float64
		)
		if err := rows.Scan(&fitid, &posted, &id, &amtFloat, &kind, &desc, &payload); err != nil {
			return nil, err
		}
		// The collector rounds money to cents before storing, so the
		// float→decimal step is exact at display precision.
		amt := canonical.NewDecimalFromFloat(amtFloat)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: fitid,
			OccurredAt:            posted,
			AccountExternalID:     id,
			Kind:                  txKind(kind, amt),
			Currency:              currencyOf(ccy, id),
			GrossAmount:           &amt,
			NetAmount:             &amt,
			Description:           silver.StrPtrIfNonEmpty(desc),
			Payload:               json.RawMessage(payload),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// txKind maps a Chase deposit transaction to a canonical TxKind. Interest and
// fees are recognised by their OFX TRNTYPE / CSV Type; everything else is a
// deposit or withdrawal by the (canonical) sign of the amount, which is the
// faithful classification for a cash account with no richer categorisation.
func txKind(chaseKind string, amt canonical.Decimal) canonical.TxKind {
	k := strings.ToUpper(chaseKind)
	switch {
	case k == "INT" || strings.Contains(k, "INTEREST"):
		return canonical.TxKindInterest
	case k == "SRVCHG" || strings.Contains(k, "FEE"):
		return canonical.TxKindFee
	case amt.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}
