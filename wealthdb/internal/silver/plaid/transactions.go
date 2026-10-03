package plaid

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions yields both ledgers in the window, in one batch ordered by
// (occurred_at, id). A buy or a sell occurs at its trade date
// (investmentDate), and every other row at its posting date. The ledgers
// are the bank and card ledger of every cash and card account, and the
// investment ledger of every investment account. A loan's own ledger is
// left out; its payments are booked on the account they were paid from.
//
// SIGNS. Silver holds both ledgers in the fleet's sign already (money into
// the account is positive), the collector having negated Plaid's own. The
// bank and card ledger still goes through ApplyCanonicalSign, so a kind with
// a fixed direction comes out signed that way. The investment ledger keeps
// each row's own sign: a row that disagrees with its kind is a correction,
// and forcing the sign would book it a second time.
//
// PENDING rows are emitted like any other, with Plaid's `pending` in the
// payload. A pending charge that posts gets a new id. Every read of the
// ledger replaces the pending rows of the accounts it covers, so silver
// holds the provisional row only until a run lists the posted one, and gold
// re-emits the whole window on every load.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	accounts, err := c.latestAccounts(ctx)
	if err != nil {
		return nil, err
	}
	secs, err := c.readSecurities(ctx)
	if err != nil {
		return nil, err
	}
	var out canonical.TransactionBatch
	if err := c.appendBankLedger(ctx, w, accounts, &out); err != nil {
		return nil, err
	}
	if err := c.appendInvestmentLedger(ctx, w, accounts, secs, &out); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out.Transactions, func(a, b canonical.TransactionChange) int {
		return cmp.Or(cmp.Compare(a.OccurredAt, b.OccurredAt),
			cmp.Compare(a.TransactionExternalID, b.TransactionExternalID))
	})
	return silver.NewTransactionStream(out), nil
}

// appendBankLedger adds the bank and card ledger of every cash and card
// account.
//
// The description is the institution's own text where Plaid kept it, else
// Plaid's cleaned name. The merchant goes to Counterparty verbatim: it is
// the input to gold's merchant signature, so any reformatting would re-key
// every merchant. Plaid's detailed category goes to ProviderCategory
// verbatim, for the spending and income provider tiers.
func (c *Connection) appendBankLedger(ctx context.Context, w canonical.Window,
	accounts map[string]account, out *canonical.TransactionBatch) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT transaction_id, account_id, posted_at, amount, COALESCE(currency, ''),
       COALESCE(name, ''), COALESCE(original_description, ''),
       COALESCE(merchant_name, ''), COALESCE(category_primary, ''),
       COALESCE(category_detailed, ''), COALESCE(check_number, ''),
       COALESCE(json_extract(payload, '$.category_id'), ''), payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?`, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("plaid Transactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, accountID, rawAmount, currency, name, original  string
			merchant, primary, detailed, check, legacy, payload string
			posted                                              int64
		)
		if err := rows.Scan(&id, &accountID, &posted, &rawAmount, &currency, &name,
			&original, &merchant, &primary, &detailed, &check, &legacy, &payload); err != nil {
			return err
		}
		a, ok := accounts[accountID]
		if !ok || !bankLedgerKind(a.kind) {
			continue
		}
		amount, err := canonical.NewDecimalFromString(rawAmount)
		if err != nil {
			return fmt.Errorf("plaid Transactions: %s: amount %q: %w", id, rawAmount, err)
		}
		kind := bankTxKind(a.kind, amount, primary, detailed, legacy)
		net := canonical.ApplyCanonicalSign(kind, &amount)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: id,
			OccurredAt:            posted,
			AccountExternalID:     accountID,
			Kind:                  kind,
			Currency:              cmp.Or(currency, a.currency),
			GrossAmount:           net,
			NetAmount:             net,
			Description:           silver.StrPtrIfNonEmpty(cmp.Or(original, name)),
			Counterparty:          silver.StrPtrIfNonEmpty(merchant),
			ProviderCategory:      silver.StrPtrIfNonEmpty(detailed),
			CheckNumber:           silver.CheckNumberOnOutflow(check, net),
			Payload:               json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// investmentRow is one row of silver's `investment_transactions` table,
// with the date it books at (investmentDate).
type investmentRow struct {
	id, accountID, securityID, typ, subtype, name, currency, cancels string
	rawAmount                                                        string
	quantity, price, fees                                            sql.NullString
	posted, occurred                                                 int64
	payload                                                          string
}

// appendInvestmentLedger adds the investment ledger of every investment
// account.
//
// A row on a security that is cash itself (a contribution, interest) names
// no instrument: it moves the account's cash. A row of Plaid's type `cash`
// or `fee` states no quantity or price of its own; Plaid fills in
// placeholders, which are dropped. A movement of a security in or out is
// valued at the securities' worth (book). A trade's gross amount is its net
// amount before Plaid's fees. A `cancel` row reverses the row it cancels:
// that row's kind, and its amount negated, so the two net inside one kind.
// A cancel whose row silver does not hold keeps its own type, which no map
// knows: `other`.
func (c *Connection) appendInvestmentLedger(ctx context.Context, w canonical.Window,
	accounts map[string]account, secs map[string]security, out *canonical.TransactionBatch) error {
	rows, err := c.readInvestmentRows(ctx, `WHERE `+investmentDate+` BETWEEN ? AND ?`, w.Start, w.End)
	if err != nil {
		return err
	}
	cancelled, err := c.cancelledRows(ctx)
	if err != nil {
		return err
	}
	nb := neighboursOf(rows)
	for _, r := range rows {
		a, ok := accounts[r.accountID]
		if !ok || !investmentLedgerKind(a.kind) {
			continue
		}
		m, err := measure(r, a, secs)
		if err != nil {
			return err
		}
		bk := book(r, m, nb)
		if orig, ok := cancelled[r.cancels]; ok && norm(r.typ) == "cancel" {
			oa, ok := accounts[orig.accountID]
			if !ok {
				oa = a
			}
			om, err := measure(orig, oa, secs)
			if err != nil {
				return err
			}
			bk = book(orig, om, nb)
			bk.net = bk.net.Neg()
		}
		extra := map[string]any{}
		if !bk.known || bk.kind == canonical.TxKindOther {
			extra["source_kind"] = r.typ + "/" + r.subtype
		}
		if bk.unvalued {
			extra["unvalued"] = true
		}

		net, gross := bk.net, bk.net
		if fees := silver.DecimalPtrOrNil(r.fees); fees != nil && isTrade(r.typ) {
			gross = net.Add(*fees)
		}
		tx := canonical.TransactionChange{
			TransactionExternalID: r.id,
			OccurredAt:            r.occurred,
			AccountExternalID:     r.accountID,
			Kind:                  bk.kind,
			Currency:              m.currency,
			GrossAmount:           &gross,
			NetAmount:             &net,
			Description:           silver.StrPtrIfNonEmpty(r.name),
			Payload:               silver.PayloadWith(r.payload, extra),
		}
		switch {
		case m.cash:
		case m.secKnown:
			key := instrumentKey(m.sec)
			tx.InstrumentExternalID = &key
		case r.securityID != "":
			// A security silver does not describe: the id is the one token
			// an override can name it by.
			tx.InstrumentHint = r.securityID
		}
		if !m.cash && !placeholderType(r.typ) {
			tx.Quantity = m.quantity
			tx.Price = m.price
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return nil
}

// isTrade reports whether a row is one of Plaid's trades: its type is `buy`
// or `sell`.
func isTrade(typ string) bool {
	t := norm(typ)
	return t == "buy" || t == "sell"
}

// placeholderType reports whether Plaid's type moves cash alone, so that
// the quantity and price Plaid states for the row are placeholders.
func placeholderType(typ string) bool {
	t := norm(typ)
	return t == "cash" || t == "fee"
}

// booking is how an investment row books: its kind, whether Plaid's type
// and subtype were recognised, its net amount, and whether it moves a
// security in or out at no worth Plaid states.
type booking struct {
	kind     canonical.TxKind
	known    bool
	net      canonical.Decimal
	unvalued bool
}

// sameDay is one security in one account on one posting date.
type sameDay struct {
	account, security string
	day               int64
}

// neighbours are what a row's neighbours on its posting date say about
// it: the days on which a security paid a dividend into an account, and
// those on which it had a corporate action there.
type neighbours struct {
	dividends, actions map[sameDay]bool
}

func neighboursOf(rows []investmentRow) neighbours {
	nb := neighbours{map[sameDay]bool{}, map[sameDay]bool{}}
	for _, r := range rows {
		if r.securityID == "" {
			continue
		}
		k := sameDay{r.accountID, r.securityID, r.posted}
		switch kind, _ := investmentTxKind(r.typ, r.subtype, false, false); kind {
		case canonical.TxKindDividend:
			nb.dividends[k] = true
		case canonical.TxKindCorporateAction:
			nb.actions[k] = true
		}
	}
	return nb
}

// book classifies a row and values it. A movement of a security in or out
// is worth the row's amount, else its quantity times its price, signed by
// its direction. Every other row keeps its own amount and sign: one that
// disagrees with its kind is a correction.
//
// A cash deposit or withdrawal whose text states what it is books as that
// (describedKind). Otherwise a cash or fee row that names a security that
// is not cash is read by what it names (securityCash). A movement of a
// security at no worth is a corporate action where the security is a
// derivative (an option that expired) or has a corporate action on the
// same day (the other leg of a merger). Any other such movement is marked
// unvalued: gold cannot price it.
func book(r investmentRow, m measured, nb neighbours) booking {
	if k, ok := describedKind(r.typ, r.subtype, r.name); ok {
		return booking{k, true, m.amount, false}
	}
	if k, ok := securityCash(r, m, nb); ok {
		return booking{k, true, m.amount, false}
	}
	kind, known := investmentTxKind(r.typ, r.subtype, m.inKind, m.inward())
	if kind != canonical.TxKindTransferIn && kind != canonical.TxKindTransferOut {
		return booking{kind, known, m.amount, false}
	}
	net := m.amount.Abs()
	if net.IsZero() && m.quantity != nil && m.price != nil {
		net = m.quantity.Mul(*m.price).Abs()
	}
	if net.IsZero() {
		if norm(m.sec.typ) == "derivative" || nb.actions[sameDay{r.accountID, r.securityID, r.posted}] {
			return booking{canonical.TxKindCorporateAction, known, net, false}
		}
		return booking{kind, known, net, true}
	}
	if !m.inward() {
		net = net.Neg()
	}
	return booking{kind, known, net, false}
}

// securityCash is the kind of a cash row that names a security that is not
// cash, under a subtype that would otherwise read it as money in or out:
//
//   - an `adjustment` of type `fee`, or a cash `withdrawal`, is tax withheld
//     where the security paid a dividend into the account on the same
//     posting date, else a fee;
//   - a cash `deposit` is the cash paid in lieu of a fractional share: a
//     sale of the fraction, which Plaid does not state.
func securityCash(r investmentRow, m measured, nb neighbours) (canonical.TxKind, bool) {
	if !m.secKnown || m.cash {
		return "", false
	}
	typ, sub := norm(r.typ), norm(r.subtype)
	switch {
	case typ == "fee" && sub == "adjustment", typ == "cash" && sub == "withdrawal":
		if nb.dividends[sameDay{r.accountID, r.securityID, r.posted}] {
			return canonical.TxKindTax, true
		}
		return canonical.TxKindFee, true
	case typ == "cash" && sub == "deposit":
		return canonical.TxKindSell, true
	}
	return "", false
}

// measured is what an investment row moved.
type measured struct {
	amount          canonical.Decimal
	quantity, price *canonical.Decimal
	currency        string
	sec             security
	secKnown, cash  bool
	// inKind is true when the row moves a security, not cash: it names one
	// that is not cash, in a quantity, and Plaid's type is not `cash`. A
	// cash row moves the account's cash whatever security it names, and
	// its quantity keeps Plaid's own sign.
	inKind bool
}

func measure(r investmentRow, a account, secs map[string]security) (measured, error) {
	amount, err := canonical.NewDecimalFromString(r.rawAmount)
	if err != nil {
		return measured{}, fmt.Errorf("plaid Transactions: %s: amount %q: %w", r.id, r.rawAmount, err)
	}
	m := measured{
		amount:   amount,
		quantity: silver.DecimalPtrOrNil(r.quantity),
		price:    silver.DecimalPtrOrNil(r.price),
		currency: cmp.Or(r.currency, a.currency),
	}
	m.sec, m.secKnown = secs[r.securityID]
	m.cash = m.secKnown && isCash(m.sec, m.currency)
	m.inKind = r.securityID != "" && !m.cash && norm(r.typ) != "cash" &&
		m.quantity != nil && !m.quantity.IsZero()
	return m, nil
}

// inward is the direction of what moved: a security's quantity, else the
// cash amount.
func (m measured) inward() bool {
	if m.inKind {
		return m.quantity.IsPositive()
	}
	return m.amount.IsPositive()
}

// investmentDate is, in SQL, the date an investment row books at. Plaid
// dates a trade at its settlement and states its trade date apart, as a
// time. A buy or a sell books at the UTC day of its trade date, where Plaid
// states one no later than the posting date. Every other row books at its
// posting date.
//
// A trade never books before the oldest date the investment ledger was
// read from. A reload clears gold from the oldest date silver holds
// (spanExtrema). A trade Plaid stops listing leaves no date of its own in
// silver, but that window start stays in the span.
const investmentDate = `(CASE
    WHEN LOWER(TRIM(type)) IN ('buy', 'sell') AND transaction_at / 86400 * 86400 <= posted_at
    THEN MAX(transaction_at / 86400 * 86400,
             COALESCE((SELECT MIN(window_start) FROM run_products
                        WHERE product = 'investment_transactions'), 0))
    ELSE posted_at END)`

const investmentColumns = `investment_transaction_id, account_id, COALESCE(security_id, ''),
       COALESCE(type, ''), COALESCE(subtype, ''), COALESCE(name, ''),
       COALESCE(currency, ''), COALESCE(cancel_transaction_id, ''), amount,
       quantity, price, fees, posted_at, ` + investmentDate + `, payload`

func (c *Connection) readInvestmentRows(ctx context.Context, where string, args ...any) ([]investmentRow, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+investmentColumns+`
  FROM investment_transactions `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("plaid Transactions: %w", err)
	}
	defer rows.Close()
	var out []investmentRow
	for rows.Next() {
		var r investmentRow
		if err := rows.Scan(&r.id, &r.accountID, &r.securityID, &r.typ, &r.subtype, &r.name,
			&r.currency, &r.cancels, &r.rawAmount, &r.quantity, &r.price, &r.fees, &r.posted,
			&r.occurred, &r.payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// cancelledRows are the rows a `cancel` row names, by id, wherever they are
// dated.
func (c *Connection) cancelledRows(ctx context.Context) (map[string]investmentRow, error) {
	rows, err := c.readInvestmentRows(ctx, `WHERE investment_transaction_id IN (
    SELECT cancel_transaction_id FROM investment_transactions
     WHERE cancel_transaction_id IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]investmentRow, len(rows))
	for _, r := range rows {
		out[r.id] = r
	}
	return out, nil
}
