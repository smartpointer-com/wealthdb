package raiffeisenat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Transactions yields the whole deposit ledger. The silver `amount` is already
// signed the canonical way (positive = balance increase, debits negative), so
// the source sign is preserved rather than forced; summing NetAmount reproduces
// the account's net cash flow. The kontoumsaetze row id is the stable external
// id.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	ccy, err := c.accountCurrencies(ctx)
	if err != nil {
		return nil, err
	}
	const q = `
SELECT txn_id, posted_at, account_external_id, amount,
       COALESCE(category, ''), COALESCE(description, ''), payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?
 ORDER BY posted_at, txn_id`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("raiffeisen_at Transactions: %w", err)
	}
	defer rows.Close()

	var out canonical.TransactionBatch
	for rows.Next() {
		var (
			txnID, id, category, desc, payload string
			posted                             int64
			amtFloat                           float64
		)
		if err := rows.Scan(&txnID, &posted, &id, &amtFloat, &category, &desc, &payload); err != nil {
			return nil, err
		}
		// The collector rounds money to cents before storing, so the
		// float→decimal step is exact at display precision.
		amt := canonical.NewDecimalFromFloat(amtFloat)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: txnID,
			OccurredAt:            posted,
			AccountExternalID:     id,
			Kind:                  txKind(category, desc, amt),
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

// txKind maps a Raiffeisen deposit transaction to a canonical TxKind. Interest
// and fees are recognised from the source category (kategorieCode) or the
// description text — German and English tokens both, since the wire mixes them;
// everything else is a deposit or withdrawal by the (canonical) sign of the
// amount, the faithful classification for a cash conduit account. The full
// category is preserved in `payload` for any later refinement.
//
// The long, unambiguous tokens (INTEREST, ZINS, ENTGELT, …) are matched as
// substrings so a German compound (Kontoentgelt) or an inflection (Zinsen)
// still hits; the short, ambiguous ones (FEE) are matched only as whole slug /
// word tokens, so "coffee" is not mistaken for a fee.
func txKind(category, description string, amt canonical.Decimal) canonical.TxKind {
	hay := strings.ToUpper(category + " " + description)
	switch {
	case containsAny(hay, "INTEREST", "ZINS"):
		return canonical.TxKindInterest
	case containsAny(hay, "ENTGELT", "SPESEN", "GEBÜHR", "GEBUEHR", "SERVICE CHARGE", "SERVICE FEE"):
		return canonical.TxKindFee
	case hasWholeToken(hay, "FEE", "FEES", "CHARGE", "CHARGES"):
		return canonical.TxKindFee
	case amt.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// hasWholeToken reports whether any of `tokens` appears in `s` as a whole word
// or slug segment (splitting on every non-alphanumeric rune, so both
// underscore-slugged category codes and free-text descriptions are covered).
func hasWholeToken(s string, tokens ...string) bool {
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		for _, t := range tokens {
			if w == t {
				return true
			}
		}
	}
	return false
}
