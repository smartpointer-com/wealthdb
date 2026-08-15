package firstcitizens

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Transactions yields the whole deposit ledger. The silver `amount` is already
// signed the canonical way (positive = balance increase, debits negative), so
// the source sign is preserved rather than forced; summing NetAmount reproduces
// the account's net cash flow. The history row's transactionId is the stable
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
       COALESCE(description, ''), payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?
 ORDER BY posted_at, fitid`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("firstcitizens Transactions: %w", err)
	}
	defer rows.Close()

	var out canonical.TransactionBatch
	for rows.Next() {
		var (
			fitid, id, desc, payload string
			posted                   int64
			amtFloat                 float64
		)
		if err := rows.Scan(&fitid, &posted, &id, &amtFloat, &desc, &payload); err != nil {
			return nil, err
		}
		// The collector rounds money to cents before storing, so the
		// float→decimal step is exact at display precision.
		amt := canonical.NewDecimalFromFloat(amtFloat)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: fitid,
			OccurredAt:            posted,
			AccountExternalID:     id,
			Kind:                  txKind(desc, amt),
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

// txKind maps a First Citizens deposit transaction to a canonical TxKind.
// Unlike chase there is no OFX TRNTYPE in silver (the coarse silver `kind` is
// only DEBIT/CREDIT from `isDebit`), so interest and fees are recognised from
// the description text; everything else is a deposit or withdrawal by the
// (canonical) sign of the amount, which is the faithful classification for a
// cash conduit account with no richer categorisation.
func txKind(description string, amt canonical.Decimal) canonical.TxKind {
	d := strings.ToUpper(description)
	switch {
	case strings.Contains(d, "INTEREST"):
		return canonical.TxKindInterest
	case strings.Contains(d, "SERVICE CHARGE") || strings.Contains(d, "SERVICE FEE") ||
		strings.Contains(d, " FEE") || strings.HasPrefix(d, "FEE"):
		return canonical.TxKindFee
	case amt.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}
