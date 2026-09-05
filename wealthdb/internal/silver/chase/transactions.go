package chase

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Transactions yields the whole ledger of both products, deposit and card.
//
// SIGNS. A deposit row's silver `amount` is already signed the canonical way
// (positive = balance increase, debits negative), so the source sign is
// preserved rather than forced; summing NetAmount reproduces the account's net
// cash flow. A card row's amount is the provider's own convention — spend
// negative, anything reducing the balance owed positive — which agrees with
// the canonical convention for a liability carried as negative cash. It is
// still routed through ApplyCanonicalSign, because the card kinds have a fixed
// canonical direction: a purchase comes out negative and a refund / card
// payment positive whatever the row said.
//
// IDS are silver's own — content-derived and occurrence-indexed, not the
// provider's OFX FITID. Only the QFX export carries a FITID, each export
// format can fail on its own, and on a card the FITID is not even unique
// within one export (a credit reversing a charge repeats the charge's). The
// FITID rides along in `payload.fitid` for traceability.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	facts, err := c.accountFactsByID(ctx)
	if err != nil {
		return nil, err
	}
	const q = `
SELECT fitid, posted_at, account_external_id, amount,
       COALESCE(kind, ''), COALESCE(description, ''),
       COALESCE(merchant, ''), COALESCE(category, ''),
       txn_date, COALESCE(currency, ''), payload
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
			fitid, id, rawKind, desc   string
			merchant, category, rowCcy string
			payload                    string
			posted                     int64
			txnDate                    sql.NullInt64
			amtFloat                   float64
		)
		if err := rows.Scan(&fitid, &posted, &id, &amtFloat, &rawKind, &desc,
			&merchant, &category, &txnDate, &rowCcy, &payload); err != nil {
			return nil, err
		}
		// The collector rounds money to cents before storing, so the
		// float→decimal step is exact at display precision.
		amt := canonical.NewDecimalFromFloat(amtFloat)
		net := &amt
		var kind canonical.TxKind
		extra := map[string]any{}
		if isCard(facts, id) {
			var known bool
			kind, known = cardTxKind(rawKind, amt)
			if !known {
				extra["source_kind"] = rawKind
			}
			net = canonical.ApplyCanonicalSign(kind, &amt)
		} else {
			kind = depositTxKind(rawKind, amt)
		}
		// OccurredAt is the POST date for both products — the date the
		// account's balance moved, which is what the balance series is keyed
		// on. A card row also knows the date the card was used; it is carried
		// alongside rather than substituted, since the two eras disagree on
		// which one posted_at holds (the statement era has only the
		// transaction date and fills posted_at with it).
		if txnDate.Valid {
			extra["txn_date"] = time.Unix(txnDate.Int64, 0).UTC().Format("2006-01-02")
		}
		currency := rowCcy
		if currency == "" {
			// Silver's per-row currency is the landing spot for a
			// foreign-currency card row; NULL means the account's own.
			currency = currencyOf(facts, id)
		}
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: fitid,
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
			// The provider's own category, verbatim and un-normalised.
			// Empty on payments, which the provider leaves uncategorised.
			ProviderCategory: silver.StrPtrIfNonEmpty(category),
			Payload:          payloadWith(payload, extra),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// depositTxKind maps a Chase deposit transaction to a canonical TxKind.
// Interest and fees are recognised by their OFX TRNTYPE / CSV Type; everything
// else is a deposit or withdrawal by the (canonical) sign of the amount, which
// is the faithful classification for a cash account with no richer
// categorisation.
func depositTxKind(chaseKind string, amt canonical.Decimal) canonical.TxKind {
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

// cardTxKinds maps a card row's silver `kind` to the canonical taxonomy. Three
// vocabularies share that column: the CSV export's `Type`, the statement
// section a row was printed under, and — for a row that only ever landed in
// the QFX export, each format being fetched separately and able to fail on its
// own — the OFX TRNTYPE the loader falls back to.
var cardTxKinds = map[string]canonical.TxKind{
	"SALE":    canonical.TxKindPurchase,
	"RETURN":  canonical.TxKindRefund,
	"PAYMENT": canonical.TxKindCardPayment,
	// A QFX-only row carries the TRNTYPE, which says only which way the
	// money went. These entries make the type RECOGNISED; cardTxKind
	// then picks the direction off the sign, exactly as it does for an
	// Adjustment — a debit is spend, a credit reduces the balance owed,
	// which on a card is dominantly the monthly payment. Kinding a
	// credit `refund` instead would drop a large positive into the
	// spending base to cancel genuine purchases while leaving the
	// deposit-side bill unpaired, so the same money would count twice
	// with opposite signs; `other` would drop the row out of the
	// spending base altogether.
	//
	// THE COST IT ACCEPTS: a genuine refund that landed only in the QFX
	// is a credit, so it is kinded card_payment and leaves the spending
	// base — net spend for that period is overstated by it. And where
	// `other` was at least counted by `status -v`'s excluded-unmapped
	// line, card_payment is not, so a CSV-fetch outage shrinks net spend
	// with no drift signal. It is still the better of the two mappings:
	// the refund is a minority of card credits, and the converse
	// mapping mis-states every monthly bill instead.
	"DEBIT":  canonical.TxKindPurchase,
	"CREDIT": canonical.TxKindCardPayment,
	// An Adjustment carries the issuer's own sign and goes both ways: a
	// credit one offsets prior spend (a goodwill credit, a disputed charge
	// reversed, rewards cashed out against the balance), a debit one
	// re-bills it (a dispute decided the other way, a goodwill credit
	// withdrawn). cardTxKind keys the kind off the sign so `refund` nets
	// the credit against the spend it reverses and `purchase` keeps the
	// debit as spend; the entry here is the credit direction, which the
	// canonical-sign rule then pins. `other` in either direction would
	// drop the row out of the spending base and misstate what was spent.
	"ADJUSTMENT":    canonical.TxKindRefund,
	"FEE":           canonical.TxKindFee,
	"STMT_PURCHASE": canonical.TxKindPurchase,
	"STMT_PAYMENT":  canonical.TxKindCardPayment,
	"STMT_FEE":      canonical.TxKindFee,
	"STMT_INTEREST": canonical.TxKindInterest,
}

// cardTxKind maps a card row's kind and reports whether it was recognised. An
// unrecognised type lands as `other` with the raw string kept in the payload,
// per the global fallback rule (DESIGN.md §6.7).
//
// Adjustment and the two TRNTYPE values are the types whose direction is not
// fixed by their name, so they are read off the amount the same way
// depositTxKind reads a deposit row: a credit adjustment nets as a `refund`, a
// debit one as a `purchase`, and a DEBIT / CREDIT is spend or a card payment
// by its sign. All stay inside the spending base carrying the issuer's own
// sign, where a kind pinned to one direction would flip the other and
// understate net spend by twice the amount.
//
// TxKindReward has no producer here: chase issues no reward transaction — a
// redemption against the balance surfaces as an Adjustment like any other
// credit — so the kind is left unproduced rather than guessed at from
// descriptors.
func cardTxKind(raw string, amt canonical.Decimal) (canonical.TxKind, bool) {
	norm := strings.ToUpper(strings.TrimSpace(raw))
	k, ok := cardTxKinds[norm]
	if !ok {
		return canonical.TxKindOther, false
	}
	switch norm {
	case "ADJUSTMENT":
		if amt.IsNegative() {
			return canonical.TxKindPurchase, true
		}
	case "DEBIT", "CREDIT":
		if amt.IsNegative() {
			return canonical.TxKindPurchase, true
		}
		return canonical.TxKindCardPayment, true
	}
	return k, true
}

// payloadWith returns the silver payload with `extra`'s keys merged in — the
// projection's own annotations beside the collector's. A payload that does not
// decode as a JSON object (the loader never writes one, but the column is free
// text) is replaced by the annotations alone rather than losing them.
func payloadWith(payload string, extra map[string]any) json.RawMessage {
	if len(extra) == 0 {
		return json.RawMessage(payload)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m == nil {
		m = map[string]any{}
	}
	for k, v := range extra {
		m[k] = v
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(payload)
	}
	return blob
}
