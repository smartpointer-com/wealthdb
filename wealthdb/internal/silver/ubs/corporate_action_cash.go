package ubs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A corporate action that pays cash — a dividend, a coupon — reaches
// PSN twice: as the MT566 confirmation on the custody account, and as
// the MT940 line crediting the cash account. Where the feed speaks for
// the cash account, that line is the row gold holds (`dividend`, by
// narrative or by the NDIV floor) and the confirmation stands beside it
// as a `corporate_action` marker with no amount. Where it does not —
// the mandates' minor-currency cash accounts — the confirmation is the
// only record there is, and its CASHMOVE block states what the
// statement line would have:
//
//	:22H::CRDB//CRED          credited
//	:97A::CASH//<account>     the cash account, in the bank's internal form
//	:19B::PSTA//JPY1234560,   the amount posted
//	:19B::GRSS//JPY1234560,   the gross entitlement
//	:19B::TAXR//JPY0,         the tax withheld
//	:98A::PAYD//20260818      the payment day
//
// So for such an account the adapter books the cash leg from the
// confirmation, beside the marker: one `dividend` (or `interest`) row
// on the cash account, on the payment day, with the confirmation's
// figures in its payload. Interest reads the posted amount. A dividend
// reads its gross, with the tax withheld as a `tax` row beside it, as
// an MT940 line paired with its confirmation does (dividends.go).
// Only where the feed does not speak for the account on that day
// (psnCashCoverage) — where it does, the statement line is the row,
// and a second one would double the income.
//
// Only the events that pay cash are read (CAOP CASH, a credit), and
// only the two whose kind is settled by the event indicator: DVCA is a
// dividend, INTR is interest. A capital call confirmed as OTHR, an
// optional dividend taken in shares, an exchange — anything else — is
// left to the rails that already carry it.

// corporateActionFields is the MT566 confirmation as the collector
// stores it: the tag sequence verbatim, and the fields it promoted.
type corporateActionFields struct {
	CAEV   string      `json:"caev"`
	ISIN   string      `json:"isin"`
	SEME   string      `json:"seme"`
	Fields [][2]string `json:"fields"`
}

// caCashMove is what one CASHMOVE block states.
type caCashMove struct {
	credit                      bool
	cashAccount                 string
	posted, gross, tax          canonical.Decimal
	postedCcy, grossCcy, taxCcy string
	paymentDay                  int64
}

// swiftMoney splits a `:QUAL//CCYamount` value into its currency and
// figure. The figure keeps SWIFT's spelling; parseSwiftDecimal reads it.
func swiftMoney(value string) (currency string, amount canonical.Decimal, ok bool) {
	_, rest, found := strings.Cut(value, "//")
	if !found || len(rest) < 4 {
		return "", canonical.Decimal{}, false
	}
	currency, figure := rest[:3], rest[3:]
	amount, err := parseSwiftDecimal(figure)
	if err != nil {
		return "", canonical.Decimal{}, false
	}
	return currency, amount, true
}

// swiftDay reads a `:QUAL//YYYYMMDD` date as the Unix time of that day.
func swiftDay(value string) (int64, bool) {
	_, rest, found := strings.Cut(value, "//")
	if !found || len(rest) < 8 {
		return 0, false
	}
	t, err := time.Parse("20060102", rest[:8])
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// qualifier is the `:QUAL//` a SWIFT field value opens with.
func qualifier(value string) string {
	q, _, _ := strings.Cut(strings.TrimPrefix(value, ":"), "//")
	return q
}

// parseCorporateActionCash reads the confirmation's cash option and its
// cash movement. ok is false for an event that pays no cash, or whose
// movement is not stated completely enough to book.
func parseCorporateActionCash(f corporateActionFields) (caCashMove, string, bool) {
	var (
		move       caCashMove
		inMove     bool
		payingCash bool
		name       string
		hasPosted  bool
	)
	for _, field := range f.Fields {
		tag, value := field[0], field[1]
		switch {
		case tag == "16R" && value == "CASHMOVE":
			inMove = true
		case tag == "16S" && value == "CASHMOVE":
			inMove = false
		case tag == "22H" && qualifier(value) == "CAOP":
			payingCash = strings.HasSuffix(value, "//CASH")
		case tag == "35B" && name == "":
			// "ISIN <isin>\n<name>\n…": the security's name is the
			// line after the identifier.
			lines := strings.Split(value, "\n")
			if len(lines) > 1 {
				name = strings.TrimSpace(lines[1])
			}
		case !inMove:
			continue
		case tag == "22H" && qualifier(value) == "CRDB":
			move.credit = strings.HasSuffix(value, "//CRED")
		case tag == "97A" && qualifier(value) == "CASH":
			_, move.cashAccount, _ = strings.Cut(value, "//")
		case tag == "19B":
			ccy, amount, ok := swiftMoney(value)
			if !ok {
				continue
			}
			switch qualifier(value) {
			case "PSTA":
				move.postedCcy, move.posted, hasPosted = ccy, amount, true
			case "GRSS":
				move.grossCcy, move.gross = ccy, amount
			case "TAXR":
				move.taxCcy, move.tax = ccy, amount
			}
		case tag == "98A" && (qualifier(value) == "PAYD" || qualifier(value) == "VALU"):
			// The payment day outranks the value day; either names
			// the day the cash arrived, and both are usually the same.
			if day, ok := swiftDay(value); ok && (move.paymentDay == 0 || qualifier(value) == "PAYD") {
				move.paymentDay = day
			}
		}
	}
	if !payingCash || !move.credit || move.cashAccount == "" || !hasPosted || move.posted.IsZero() {
		return caCashMove{}, "", false
	}
	return move, name, true
}

// corporateActionCashKind is the kind the event indicator settles, or
// false for an indicator the cash leg is not booked for.
func corporateActionCashKind(caev string) (canonical.TxKind, bool) {
	switch strings.ToUpper(strings.TrimSpace(caev)) {
	case "DVCA":
		return canonical.TxKindDividend, true
	case "INTR":
		return canonical.TxKindInterest, true
	}
	return "", false
}

// caCashPayload is what the booked leg's payload states: the
// confirmation it was read from and its figures. The posted amount is
// the cash the account received, which the row reads unless a
// dividend's confirmation states a gross it can book (bookGross).
type caCashPayload struct {
	CorporateAction string `json:"corporate_action"`
	CAEV            string `json:"caev"`
	PostedAmount    string `json:"posted_amount"`
	PostedCurrency  string `json:"posted_currency"`
	GrossAmount     string `json:"gross_amount,omitempty"`
	GrossCurrency   string `json:"gross_currency,omitempty"`
	TaxAmount       string `json:"tax_amount,omitempty"`
	TaxCurrency     string `json:"tax_currency,omitempty"`
}

// cashConfirmation is one MT566 confirmation that credited cash: the
// event, the security it names and what its cash movement states.
type cashConfirmation struct {
	eventID string
	caev    string // upper-cased
	isin    string
	name    string
	move    caCashMove
}

// eachCashConfirmation calls fn on each confirmation `where` (a clause
// over the events table, ANDed to the kind, or "") selects that credited
// cash (parseCorporateActionCash). A movement that states no payment
// day takes the event's own day.
func (r *psnReader) eachCashConfirmation(ctx context.Context, what, where string, args []any, fn func(cashConfirmation) error) error {
	if r == nil || r.db == nil {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT event_external_id, timestamp, payload
  FROM events
 WHERE kind = 'corporate_action_confirmation' `+where, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eventID, payload string
			at               int64
		)
		if err := rows.Scan(&eventID, &at, &payload); err != nil {
			return fmt.Errorf("%s scan: %w", what, err)
		}
		var f corporateActionFields
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			continue
		}
		move, name, ok := parseCorporateActionCash(f)
		if !ok {
			continue
		}
		if move.paymentDay == 0 {
			move.paymentDay = at
		}
		c := cashConfirmation{
			eventID: eventID,
			caev:    strings.ToUpper(strings.TrimSpace(f.CAEV)),
			isin:    strings.TrimSpace(f.ISIN),
			name:    name,
			move:    move,
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	return rows.Err()
}

// cashLegAccount is the account a confirmation books its own cash leg on
// (corporateActionCashLegs): the cash account it paid into, where the feed
// does not speak for that account on the payment day. false where the
// feed's own line carries the payment.
func (c cashConfirmation) cashLegAccount(coverage psnCashCoverage, ibans map[string]string) (string, bool) {
	account := cashIBAN(ibans, c.move.cashAccount)
	return account, !coverage.speaksFor(account, c.move.paymentDay)
}

// corporateActionCashLegs books, for the confirmations in the window, the
// cash each paid into an account the feed does not speak for (cashLeg).
func (r *psnReader) corporateActionCashLegs(ctx context.Context, w canonical.Window, coverage psnCashCoverage, ibans map[string]string) ([]canonical.TransactionChange, error) {
	var out []canonical.TransactionChange
	err := r.eachCashConfirmation(ctx, "ubs corporateActionCashLegs",
		"AND timestamp BETWEEN ? AND ?", []any{w.Start, w.End}, func(c cashConfirmation) error {
			tx, tax, withheld, ok, err := c.cashLeg(coverage, ibans)
			if err != nil || !ok {
				return err
			}
			out = append(out, tx)
			if withheld {
				out = append(out, tax)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// cashLeg is the row a confirmation books for the cash it paid, where the
// feed does not speak for the account on the payment day
// (cashLegAccount): the kind its event indicator settles, on the payment
// day, with the confirmation's figures in its payload. A dividend whose
// confirmation states its gross is booked at the gross, with the tax
// withheld beside it as its own row (bookGross). ok is false for an
// indicator the leg is not booked for, and where the feed's own line
// carries the payment.
func (c cashConfirmation) cashLeg(coverage psnCashCoverage, ibans map[string]string) (tx, tax canonical.TransactionChange, withheld, ok bool, err error) {
	kind, booked := corporateActionCashKind(c.caev)
	if !booked {
		return tx, tax, false, false, nil
	}
	account, booked := c.cashLegAccount(coverage, ibans)
	if !booked {
		return tx, tax, false, false, nil
	}
	move := c.move
	p := caCashPayload{
		CorporateAction: c.eventID,
		CAEV:            c.caev,
		PostedAmount:    move.posted.String(),
		PostedCurrency:  move.postedCcy,
	}
	if move.grossCcy != "" {
		p.GrossAmount, p.GrossCurrency = move.gross.String(), move.grossCcy
	}
	if move.taxCcy != "" {
		p.TaxAmount, p.TaxCurrency = move.tax.String(), move.taxCcy
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return tx, tax, false, false, fmt.Errorf("ubs cash leg payload: %w", err)
	}
	posted := move.posted
	tx = canonical.TransactionChange{
		TransactionExternalID: c.eventID + ":cash",
		OccurredAt:            move.paymentDay,
		AccountExternalID:     account,
		Kind:                  kind,
		Currency:              move.postedCcy,
		NetAmount:             canonical.ApplyCanonicalSign(kind, &posted),
		Description:           textPtr(c.name),
		ProviderCategory:      textPtr(c.caev),
		Payload:               json.RawMessage(encoded),
	}
	if c.isin != "" {
		isin := c.isin
		tx.InstrumentExternalID = &isin
	}
	if kind != canonical.TxKindDividend {
		// Interest keeps the posted amount, its gross beside it.
		if move.grossCcy == move.postedCcy && !move.gross.IsZero() {
			gross := move.gross
			tx.GrossAmount = canonical.ApplyCanonicalSign(kind, &gross)
		}
		return tx, tax, false, true, nil
	}
	tax, withheld, err = bookGross(&tx, c)
	return tx, tax, withheld, err == nil, err
}
