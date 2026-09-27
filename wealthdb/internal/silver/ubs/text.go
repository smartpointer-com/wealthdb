package ubs

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Text-column helpers shared by the web and PSN emitters. The contract they
// implement — which silver field becomes gold's description, counterparty and
// provider_category in each era — is documented in docs/adapters/ubs.md §7.
// Kinds, signs, amounts and dates are decided before they run and are never
// read from the text they compose. The one identifier they do derive is the
// instrument id a security-bearing narrative names, which projectWebTxText
// returns alongside the columns.

// narrativeText flattens a multi-line narrative (an MT940 :86: block, whose
// lines the collector joined with "\n") into a single description line via
// silver.JoinText. Its "; " separator is the one UBS itself puts between the
// lines of a CSV Description1, so a composed description reads like one of the
// bank's own captions. The lines themselves are passed through verbatim apart
// from the surrounding whitespace; codes are never expanded or paraphrased.
func narrativeText(narrative string) *string {
	return silver.StrPtrIfNonEmpty(silver.JoinText(strings.Split(narrative, "\n")...))
}

// textPtr is the single-field form: a trimmed non-empty string, else nil.
func textPtr(s string) *string {
	return silver.StrPtrIfNonEmpty(strings.TrimSpace(s))
}

// cardReferenceRe matches the reference UBS prefixes to a card-booked
// entry's type — the card's number and its expiry, as in
// "<number>-<check> MM/YY; ATM Withdrawal". Anchored whole, so it
// describes the entire leading part or nothing.
//
// It is deliberately narrow. The leading slot is where a payer's typed
// message goes, and a message that merely opened with digits must not be
// mistaken for the bank's reference; requiring the check-digit suffix and
// the expiry together is what keeps them apart.
var cardReferenceRe = regexp.MustCompile(`^\d{4,}-\d\s+\d{1,2}/\d{2}$`)

// splitBookingType splits a CSV-feed Description2 into the payer's
// message and the bank's booking type. The column holds the booking type
// alone ("e-banking payment order") unless something precedes it, when it
// holds "<lead>; <booking type>" — the lead first, the type last — so the
// split is at the LAST "; ": the type is what follows it. A column
// without the separator is all booking type and is returned untouched, so
// a row without a lead projects byte for byte as before; the `;Reversal`
// suffix has no space and stays on the type.
//
// The lead is the payer's message and becomes the row's memo — EXCEPT
// where it is the bank's own card reference, which a card-booked entry
// carries there. That is not the payer's words, and the memo is defined
// as exactly the payer's words: it is shown as such, and a config rule
// may key on it (docs/SPENDING.md §3). So a card reference yields no
// memo. Nothing is lost by dropping it — the raw column survives whole
// in the row's payload — and nothing else moves: the booking type is
// still what follows the separator, so the row categorises as it did.
func splitBookingType(descKind string) (message, bookingType string) {
	i := strings.LastIndex(descKind, "; ")
	if i < 0 {
		return "", descKind
	}
	lead, bookingType := strings.TrimSpace(descKind[:i]), strings.TrimSpace(descKind[i+2:])
	if cardReferenceRe.MatchString(lead) {
		return "", bookingType
	}
	return lead, bookingType
}

// webTxText is a web transaction row's three narrative columns as they
// reach gold: the promoted payee, the composed description less the
// payer's message, and the bank's own booking type. Empty means the
// row carries none and the column lands NULL.
type webTxText struct {
	counterparty     string
	description      string
	providerCategory string
}

// projectWebTxText composes one web row's narrative columns from its
// promoted counterparty, its Description2 and its decoded payload. It
// is the single definition of that projection: the web transaction
// loop emits what it returns, and the era text fold (merge.go) reads
// it for the rows the hard cut suppresses, so one entry's text is the
// same string whichever path carries it. The instrument id and the
// payer's message fall out of the same reading and travel with it.
//
// Description1 carries the instrument caption verbatim with the ISIN
// appended after the last "; " separator, and both are pulled out of
// it (extractInstrumentFromDescription1): the ISIN becomes the row's
// instrument id, the caption its description.
//
// The CSV feed's Description2 is the booking type, led by the payer's
// message when one was typed on the order; the PDF backfill's is the
// printed booking type alone. The message travels as the row's memo,
// never as part of the narrative: the gold writer stores it at the
// description's end behind the memo separator (docs/adapters/ubs.md
// §7). The counterparty is silver's promoted column — the CSV feed's
// first Description1 segment, the PDF backfill's first continuation
// line — passed through because it feeds gold's merchant signature and
// must not be reformatted here. It is replaced only where the promoted
// text is not a party at all, three cases pinned in
// text_columns_test.go: the statement's turnover-total line (the
// period's figures, dropped); a booking type the bank filed without a
// payee (refused, so the description decides); and one of the bank's
// own service charges, whose promoted text is an account or security
// reference or the product's name (the bank is named instead).
func projectWebTxText(counterparty, descriptionKind string, p webTxPayload, pdfBackfill bool) (text webTxText, instrumentID *string, memo string) {
	instrumentID, captionDesc := extractInstrumentFromDescription1(p)
	bookingType := descriptionKind
	if !pdfBackfill {
		memo, bookingType = splitBookingType(descriptionKind)
	}
	payee := counterparty
	if pdfBackfill && isTurnoverTotalLine(payee) {
		payee = ""
	}
	// Nor is the bank's own booking type a payee. A row the bank filed
	// without one — a fee, a charge — leads its narrative with the
	// booking type, which the promotion above then reads as the payee
	// and gold's merchant signature prefers over the description. That
	// files every such row under one merchant named after the booking,
	// and buries a payee the description DOES carry: the MT940 feed
	// writes one for the same booking that the export feed left out.
	// Refused here, the description is what the signature reads.
	if isBookingType(payee) {
		payee = ""
	}
	// A charge for one of the bank's OWN services has no third party
	// in it, so the bank is the payee. The export feed writes the
	// product under the booking type and an account or security
	// reference in the payee column, and a reference is not a party:
	// left alone it becomes the merchant, one per referenced account,
	// splitting a single relationship's fees across as many merchants
	// as it has accounts. Some CSV-feed rows leave the booking-type
	// column empty, or fill it with a reference, and carry the product
	// as Description1's first segment instead — which the promotion
	// then reads as the payee — so the narrative's head is consulted
	// as well.
	description := derefText(webDescription(captionDesc, bookingType, p))
	if isOwnServiceCharge(bookingType) || isOwnServiceCharge(firstSegment(description)) {
		payee = bankName
	}
	return webTxText{
		counterparty:     payee,
		description:      description,
		providerCategory: bookingType,
	}, instrumentID, memo
}

// firstSegment is the head of a narrative — what precedes the separator
// that divides a payee from the address and reason behind it.
func firstSegment(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// bankName is the institution this adapter reads, as it should appear
// where the bank itself is the counterparty.
const bankName = "UBS"

// ownServiceCharges are the booking types under which the bank bills
// for its own services. Each names a product rather than a party,
// because the party is the bank.
//
// Two charge types are deliberately absent. An ADR/GDR handling fee is
// a DEPOSITARY's charge passed through, and a third-party charge says
// as much in its name: the bank collects both on someone else's
// behalf, so attributing them to the bank would misstate who was paid.
var ownServiceCharges = map[string]bool{
	"UBS ADVICE":                        true,
	"CUSTODY PRICE":                     true,
	"RENTAL FEE SAFE BOX":               true,
	"BALANCE CLOSING OF SERVICE PRICES": true,
	"INTEREST CALCULATION BALANCE":      true,
	// The mandate management charge, and the cancel / re-bill pair
	// that corrects one. Their payee column holds the relationship
	// the mandate runs under, which is a reference and not a party:
	// left alone it becomes the merchant, one per mandate.
	"UBS MANAGE":     true,
	"CAN UBS MANAGE": true,
	"REC UBS MANAGE": true,
}

// isOwnServiceCharge reports whether a booking type is one the bank
// bills for itself. A `;Reversal` suffix is stripped first, as webKind
// strips it: a reversed custody price is still the bank's booking.
func isOwnServiceCharge(bookingType string) bool {
	if base, ok := stripReversalSuffix(bookingType); ok {
		bookingType = base
	}
	return ownServiceCharges[strings.ToUpper(strings.TrimSpace(bookingType))]
}

// isServicePriceClose reports whether a booking is the service-price
// close the bank prints at each period end. At a zero amount it is a
// period marker rather than a charge, and the emitter drops it as it
// drops the statement era's summary line.
func isServicePriceClose(bookingType string) bool {
	if base, ok := stripReversalSuffix(bookingType); ok {
		bookingType = base
	}
	return strings.ToUpper(strings.TrimSpace(bookingType)) == "BALANCE CLOSING OF SERVICE PRICES"
}

// derefText reads an optional text column as a plain string, an absent
// column as "". The text helpers compose over strings and the
// canonical row carries pointers; this is the one conversion between
// them.
func derefText(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// codeOnlyMaxRunes bounds what reads as a bare code. The MT940 :61:
// transaction type is four characters (NTRF and its siblings), and a
// :86: narrative the bank wrote nothing else into is a booking code of
// the same order.
const codeOnlyMaxRunes = 5

// isCodeOnly reports whether a narrative column says nothing a reader
// could place: no text at all, or one short run of letters and digits
// — the bank's own code for the entry rather than a payee, a booking
// type or an instrument caption. Anything carrying a space or a
// separator is composed of more than one thing and is never a bare
// code, however short; anything longer than a code is not one either.
//
// A trailing `?` belongs to the code rather than to the text. An
// MT940 :86: block writes its subfields as `?20`, `?21`, … after the
// booking code, so an entry the bank filled in nothing for arrives as
// the code with the introducer and nothing behind it. Left in, that
// one rune made the column read as composed of more than one thing,
// so the code beat the Account-Statement narrative that had the payee
// — the opposite of what this test exists to decide.
//
// It decides only which of two records of the SAME entry carries the
// narrative (richerText). It never decides what a row is: no kind,
// sign, amount, date or id is read from it.
func isCodeOnly(s string) bool {
	s = strings.TrimRight(strings.TrimSpace(s), "?")
	if s == "" {
		return true
	}
	if utf8.RuneCountInString(s) > codeOnlyMaxRunes {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// richerText picks the narrative that says something, for one column
// of one entry two feeds both recorded.
//
// The MT940 feed composes its description from the :86: narrative and
// its provider category from the :61: type code, so where the bank
// wrote nothing but a code both columns are that code, and the only
// payee it can carry is the bank's own name, asserted from a charge
// code (isOwnChargeCode). That name is code-shaped by this test, so an
// export twin that names a real party wins over it, and one that also
// names the bank leaves the row of record's own. The Account-Statement
// export records the same entry
// with the payee, the printed booking type and the cost note. A column
// that is a bare code — or absent — therefore gives way to one
// carrying more, and a column that already says something is kept: the
// row of record does not move, only the text it left as a code.
func richerText(have *string, alt string) *string {
	if !isCodeOnly(derefText(have)) || isCodeOnly(alt) {
		return have
	}
	return silver.StrPtrIfNonEmpty(alt)
}
