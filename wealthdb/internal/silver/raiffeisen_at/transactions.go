package raiffeisenat

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions yields the whole deposit ledger. The silver `amount` is already
// signed the canonical way (positive = balance increase, debits negative), so
// the source sign is preserved rather than forced; summing NetAmount reproduces
// the account's net cash flow. The kontoumsaetze row id is the stable external
// id.
//
// TEXT COLUMNS. Silver promotes the purpose line (Verwendungszweck) to
// `description` and the participant line (Transaktionsteilnehmer) to
// `counterparty`; a card row carries neither, its merchant sitting in the
// preserved row's card-network object and its only narrative being the
// payment reference. The projection is therefore:
//
//   - counterparty: the participant line verbatim; when empty, the
//     card-network merchant name (`payload.ethocaHaendler.name`) verbatim.
//     Either feeds gold's merchant signature, so neither is reformatted.
//   - provider_category: the source category slug (`category`,
//     kategorieCode) verbatim.
//   - description: the purpose line when present (unchanged); otherwise the
//     short order purpose (`auftragskurzVerwendungszweck`) followed by the
//     payment reference (`zahlungsreferenz`), "; "-joined, whichever exist.
//     Nothing is fabricated: a row with no text stays NULL.
//
// The kind is still classified from the raw purpose column, never from the
// composed description, so the text projection cannot move a row's kind.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	ccy, err := silver.ReadAccountCurrencies(ctx, c.db, "EUR")
	if err != nil {
		return nil, err
	}
	const q = `
SELECT txn_id, posted_at, account_external_id, amount,
       COALESCE(category, ''), COALESCE(description, ''),
       COALESCE(counterparty, ''), payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?
 ORDER BY posted_at, txn_id`

	var out canonical.TransactionBatch
	if err := silver.ScanRows(ctx, c.db, "raiffeisen_at Transactions", q, func(rows *sql.Rows) error {
		var (
			txnID, id, category, desc, cp, payload string
			posted                                 int64
			amtFloat                               float64
		)
		if err := rows.Scan(&txnID, &posted, &id, &amtFloat, &category, &desc, &cp, &payload); err != nil {
			return err
		}
		// The collector rounds money to cents before storing, so the
		// float→decimal step is exact at display precision.
		amt := canonical.NewDecimalFromFloat(amtFloat)
		description, counterparty := textColumns(desc, cp, payload)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: txnID,
			OccurredAt:            posted,
			AccountExternalID:     id,
			Kind:                  txKind(category, desc, amt),
			Currency:              ccy.Of(id),
			GrossAmount:           &amt,
			NetAmount:             &amt,
			Description:           description,
			Counterparty:          counterparty,
			ProviderCategory:      silver.StrPtrIfNonEmpty(category),
			Payload:               json.RawMessage(payload),
		})
		return nil
	}, w.Start, w.End); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// textColumns applies the text contract described on Transactions to one
// row: the promoted `description` / `counterparty` columns win verbatim, and
// the preserved kontoumsaetze row (`payload`) supplies the fallbacks. A
// payload that does not decode contributes nothing.
func textColumns(desc, cp, payload string) (description, counterparty *string) {
	var p struct {
		ShortPurpose string `json:"auftragskurzVerwendungszweck"`
		PaymentRef   string `json:"zahlungsreferenz"`
		Merchant     struct {
			Name string `json:"name"`
		} `json:"ethocaHaendler"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	description = silver.StrPtrIfNonEmpty(desc)
	if description == nil {
		description = silver.StrPtrIfNonEmpty(silver.JoinText(p.ShortPurpose, p.PaymentRef))
	}
	counterparty = silver.StrPtrIfNonEmpty(cp)
	if counterparty == nil {
		counterparty = silver.StrPtrIfNonEmpty(p.Merchant.Name)
	}
	return description, counterparty
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
// word tokens, so "coffee" is not mistaken for a fee. Rows from a supplied
// transaction listing carry no category, so their account-closing entries
// are recognised from the booking text alone — "Kontoführung" (the account
// maintenance charge) names no ENTGELT and needs its own token.
func txKind(category, description string, amt canonical.Decimal) canonical.TxKind {
	hay := strings.ToUpper(category + " " + description)
	switch {
	case containsAny(hay, "INTEREST", "ZINS"):
		return canonical.TxKindInterest
	case containsAny(hay, "ENTGELT", "SPESEN", "GEBÜHR", "GEBUEHR", "KONTOFÜHRUNG", "KONTOFUEHRUNG",
		"SERVICE CHARGE", "SERVICE FEE"):
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
