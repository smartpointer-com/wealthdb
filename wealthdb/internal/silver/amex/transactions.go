package amex

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions yields the whole card ledger.
//
// SIGNS. Silver already holds the fleet's card convention — spend negative,
// anything reducing the balance owed positive — the collector having negated
// Amex's own spend-positive figures at load. That agrees with the canonical
// convention for a liability carried as negative cash. It is still routed
// through ApplyCanonicalSign, because the card kinds have a fixed canonical
// direction: a purchase comes out negative and a refund or bill payment
// positive whatever the row said.
//
// IDS on the modern era are the provider's own stable reference, which is also
// the QFX FITID and (unquoted) the CSV Reference. Unlike chase, no
// content-derived id is needed: every channel agrees on it, and the loader
// reads only one. Statement-sourced rows carry no such reference and the
// loader keys them positionally within their period instead (the collector's
// migration 0001 records the shape), which is stable as long as a period's
// parse is.
//
// PENDING rows are emitted like any other. Their ids are provisional — they
// change when the charge posts — but silver replaces the pending set wholesale
// on each load, and gold re-emits the full window on every load, so the
// provisional row is deleted by the same deleteWindow that re-applies the
// posted one. What a pending row buys is that today's spending report sees
// today's spending.
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
       COALESCE(kind, ''), COALESCE(description, ''),
       COALESCE(merchant, ''), COALESCE(category, ''),
       txn_date, COALESCE(currency, ''), is_pending, payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?
 ORDER BY posted_at, txn_id`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("amex Transactions: %w", err)
	}
	defer rows.Close()

	var out canonical.TransactionBatch
	for rows.Next() {
		var (
			txID, id, rawKind, desc    string
			merchant, category, rowCcy string
			payload                    string
			posted                     int64
			txnDate                    sql.NullInt64
			isPending                  int
			amtFloat                   float64
		)
		if err := rows.Scan(&txID, &posted, &id, &amtFloat, &rawKind, &desc,
			&merchant, &category, &txnDate, &rowCcy, &isPending,
			&payload); err != nil {
			return nil, err
		}
		// The collector rounds money to cents before storing, so the
		// float→decimal step is exact at display precision.
		amt := canonical.NewDecimalFromFloat(amtFloat)
		kind, known := cardTxKind(rawKind, category, amt)
		extra := map[string]any{}
		if !known {
			extra["source_kind"] = rawKind
		}
		// OccurredAt is the POST date — the date the balance moved, which is
		// what the balance series is keyed on. The charge date is carried
		// alongside rather than substituted: a statement-era row knows only
		// the charge date and fills posted_at with it, so the two eras
		// disagree about what posted_at holds.
		if txnDate.Valid {
			extra["txn_date"] = time.Unix(txnDate.Int64, 0).UTC().
				Format("2006-01-02")
		}
		if isPending != 0 {
			extra["pending"] = true
		}
		currency := rowCcy
		if currency == "" {
			currency = currencyOf(ccy, id)
		}
		net := canonical.ApplyCanonicalSign(kind, &amt)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: txID,
			OccurredAt:            posted,
			AccountExternalID:     id,
			Kind:                  kind,
			Currency:              currency,
			GrossAmount:           net,
			NetAmount:             net,
			Description:           silver.StrPtrIfNonEmpty(desc),
			// The merchant verbatim, as silver holds it. It is the input to
			// gold's merchant signature, so any reformatting here would
			// re-key every merchant it touched.
			Counterparty: silver.StrPtrIfNonEmpty(merchant),
			// The provider's own spend category, verbatim and un-normalised —
			// what the spending provider tier translates without the model.
			ProviderCategory: silver.StrPtrIfNonEmpty(category),
			Payload:          silver.PayloadWith(payload, extra),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// feesCategory is the provider's own category for the charges that are not
// purchases — an annual fee, a late fee, interest, and the adjustments that
// reverse them. Matched as a prefix so a spelling variant of the same bucket
// still lands.
const feesCategory = "fees"

// statementSectionKinds maps the DEEP era's kinds — the section a row was
// printed under on its statement — to the canonical taxonomy.
//
// The deep era carries no spend category, so the section is the only thing
// that separates a bill payment from a statement credit from a purchase; the
// collector stamps it for exactly that reason. The mapping is finer than the
// modern era's in one respect: a statement states interest as its own section,
// where the activity JSON bills it into the fees category and cannot be
// separated.
var statementSectionKinds = map[string]canonical.TxKind{
	"STMT_PAYMENT":  canonical.TxKindCardPayment,
	"STMT_CREDIT":   canonical.TxKindRefund,
	"STMT_PURCHASE": canonical.TxKindPurchase,
	"STMT_FEE":      canonical.TxKindFee,
	"STMT_INTEREST": canonical.TxKindInterest,
}

// cardTxKind maps a row's direction and spend category — or, before the
// structured horizon, its statement section (`STMT_*`) — to the canonical
// kind, and reports whether the direction was recognised. The category is
// what separates the two credits Amex issues: an uncategorised CREDIT is the
// monthly bill (`card_payment`), a categorised one a refund; a DEBIT under
// fees is a `fee`, any other a `purchase`. An unrecognised direction keeps
// the raw string in the payload and falls back to the sign silver already
// normalised, rather than to `other`, which reaches neither the spending base
// nor the matcher. No reward kind is produced: Amex marks the purchases that
// earn cash back, and a redemption is an ordinary categorised credit.
// docs/adapters/amex.md §5 argues each rule and the cost it accepts.
func cardTxKind(rawKind, category string, amt canonical.Decimal) (canonical.TxKind, bool) {
	norm := strings.ToUpper(strings.TrimSpace(rawKind))
	if k, ok := statementSectionKinds[norm]; ok {
		return k, true
	}
	switch norm {
	case "CREDIT":
		if strings.TrimSpace(category) == "" {
			return canonical.TxKindCardPayment, true
		}
		return canonical.TxKindRefund, true
	case "DEBIT":
		// The fees test lives inside the DEBIT case, not above the
		// switch: a fee REVERSAL carries the same category, and `fee`
		// forces a negative canonical sign, which would state the
		// charge twice.
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(category)),
			feesCategory) {
			return canonical.TxKindFee, true
		}
		return canonical.TxKindPurchase, true
	}
	// No usable direction: fall back to the sign silver already normalised,
	// so the row still lands on the right side of the ledger.
	if amt.IsNegative() {
		return canonical.TxKindPurchase, false
	}
	return canonical.TxKindCardPayment, false
}
