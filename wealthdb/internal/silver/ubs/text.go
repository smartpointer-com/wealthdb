package ubs

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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
// without the separator is all booking type and is returned untouched;
// the `;Reversal` suffix has no space and stays on the type.
//
// The lead is the payer's message and becomes the row's memo — EXCEPT
// where it is the bank's own card reference, which a card-booked entry
// carries there. That is not the payer's words, and the memo is defined
// as exactly the payer's words: it is shown as such, and a config rule
// may key on it (docs/SPENDING.md §3). So a card reference yields no
// memo. Nothing is lost by dropping it — the raw column survives whole
// in the row's payload — and nothing else moves: the booking type is
// still what follows the separator.
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
// must not be reformatted here. It is completed where the bank cut a
// long name at its line width (rejoinWrappedName, pinned in
// wrapped_payee_test.go). It is replaced only where the promoted text
// is not a party at all, three cases pinned in text_columns_test.go:
// the statement's turnover-total line (the period's figures, dropped);
// a booking type the bank filed without a payee (refused, so the
// description decides); and one of the bank's own service charges,
// whose promoted text is an account or security reference or the
// product's name (the bank is named instead).
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
	payee = rejoinWrappedName(payee, p, pdfBackfill)
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

// partyLineWidth is the width, in characters, of one line of a payment
// party's name and address: the line of the SWIFT `35x` format that
// payment messages carry a party in. The export writes each line as one
// Description1 segment and the statement prints each as one
// continuation line. So the promoted payee is the first line, and a
// name longer than a line is cut at the width.
const partyLineWidth = 35

// rejoinWrappedName completes a payee the bank cut at its line width.
// A name longer than one line fills the line exactly, and its remainder
// (typically the surname) opens the next line. Left cut, the payee is
// the head of a name: the remainder is missing from the column, and
// gold's merchant signature keys every party whose name shares that
// head as one.
//
// The next line is the one lineAfterPayee returns. The join takes its
// leading name tokens, behind one space, and only when every condition
// holds:
//   - the payee is exactly partyLineWidth characters long (a cut that
//     lands on a space trims to one character less, and is not joined);
//   - the next line carries no digit, so an address line with a number,
//     an IBAN, a phone number or a numbered reference never joins;
//   - the payee does not end in a legal form (endsInLegalForm): a firm's
//     name is complete at its "AG" or "GMBH", and the next line is its
//     branch or its town;
//   - the next line is not one of the bank's booking types
//     (isFiledBooking);
//   - the next line does not open with a country code (opensWithCountryCode);
//   - it opens with a name token (isNameToken).
//
// The run of name tokens ends at the first token that is not one. A cut
// inside a word joins as two tokens: the space keeps every letter the
// bank wrote and invents none. The description keeps both lines as
// written.
func rejoinWrappedName(payee string, p webTxPayload, pdfBackfill bool) string {
	if utf8.RuneCountInString(strings.TrimSpace(payee)) != partyLineWidth {
		return payee
	}
	next := lineAfterPayee(payee, p, pdfBackfill)
	if strings.ContainsFunc(next, unicode.IsDigit) || endsInLegalForm(payee) ||
		isFiledBooking(next) || opensWithCountryCode(next) {
		return payee
	}
	var run []string
	for _, tok := range strings.Fields(next) {
		if !isNameToken(tok) {
			break
		}
		run = append(run, tok)
	}
	if len(run) == 0 {
		return payee
	}
	return strings.TrimSpace(payee) + " " + strings.Join(run, " ")
}

// endsInLegalForm reports whether a payee's last word is a legal-entity
// form (canonical.LegalForms), in any case, with or without a trailing
// dot ("AG", "Ltd.").
// A name that ends in one is complete, so it was not cut.
func endsInLegalForm(payee string) bool {
	words := strings.Fields(payee)
	if len(words) == 0 {
		return false
	}
	last := strings.ToUpper(strings.TrimSuffix(words[len(words)-1], "."))
	// A given name that is also a form (Asa) ends a person's name as
	// often as a firm's, and a surname-first payee is cut before it.
	return canonical.LegalForms[last] && !canonical.NameLikeLegalForms[last]
}

// opensWithCountryCode reports whether a narrative line opens with a
// two-letter upper-case word: the country prefix of an address line
// ("CH <town>", "AT <TOWN>"). A name's remainder that opens with a
// two-letter upper-case word is rare enough to refuse with it.
func opensWithCountryCode(line string) bool {
	words := strings.Fields(line)
	if len(words) == 0 || len(words[0]) != 2 {
		return false
	}
	for _, r := range words[0] {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// lineAfterPayee returns the narrative line that follows the promoted
// payee: the export's next Description1 segment, the statement's next
// continuation line. It returns "" when the narrative's first line is not
// the payee, so a payee from anywhere else is never joined to a line
// that does not follow it.
func lineAfterPayee(payee string, p webTxPayload, pdfBackfill bool) string {
	var lines []string
	if pdfBackfill {
		lines, _ = bookingLines(p.Continuation)
	} else {
		lines = strings.Split(p.Description1, ";")
	}
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != strings.TrimSpace(payee) {
		return ""
	}
	return strings.TrimSpace(lines[1])
}

// isFiledBooking reports whether a narrative line is one of the bank's
// booking types: one isBookingType knows, or one whose kind comes from
// the direction, which isBookingType cannot see. A card-booked entry
// writes its type as a Description1 segment behind the payee
// ("<payee>; ATM Withdrawal"), and a type reads as words.
func isFiledBooking(line string) bool {
	return isBookingType(line) || bookingOf(line).rail != noRail
}

// narrativeLabels are the words that open a narrative line the bank files
// under a label rather than a party: the payment's reason, the counter
// account, the transaction number, the cost note, a reference or invoice,
// the period's turnover total, and the rails that name themselves (TWINT,
// e-bill). Upper-cased. A line opening with one is never a name's
// remainder.
var narrativeLabels = map[string]bool{
	"REASON":      true,
	"PURPOSE":     true,
	"ACCOUNT":     true,
	"IBAN":        true,
	"BIC":         true,
	"SWIFT":       true,
	"TRANSACTION": true,
	"COST":        true,
	"REF":         true,
	"REFERENCE":   true,
	"INVOICE":     true,
	"TURNOVER":    true,
	"TWINT":       true,
	"EBILL":       true,
}

// isNameToken reports whether a word reads as part of a person's or a
// firm's name: letters only, accented ones included, with a hyphen or an
// apostrophe allowed between two letters ("MUSTERFRAU-BEISPIEL",
// "O'EXAMPLE"). A word with a digit, a stop, a colon or a slash is a
// label, an abbreviation or a reference, and so is any word whose
// hyphen-separated part is a narrative label ("TWINT-EXAMPLE").
func isNameToken(tok string) bool {
	runes := []rune(tok)
	if len(runes) == 0 || !unicode.IsLetter(runes[0]) || !unicode.IsLetter(runes[len(runes)-1]) {
		return false
	}
	for i, r := range runes {
		switch {
		case unicode.IsLetter(r):
		case r == '-' || r == '\'' || r == '’':
			if !unicode.IsLetter(runes[i+1]) {
				return false
			}
		default:
			return false
		}
	}
	for _, part := range strings.Split(tok, "-") {
		if narrativeLabels[strings.ToUpper(part)] {
			return false
		}
	}
	return true
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

// isOwnServiceCharge reports whether a booking type is one the bank
// bills for itself. A `;Reversal` suffix is stripped first, as webKind
// strips it: a reversed custody price is still the bank's booking.
func isOwnServiceCharge(bookingType string) bool {
	if base, ok := stripReversalSuffix(bookingType); ok {
		bookingType = base
	}
	return bookingOf(bookingType).ownCharge
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
// one rune would make the column read as composed of more than one
// thing, so the code would beat the Account-Statement narrative that
// has the payee — the opposite of what this test exists to decide.
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
// export records the same entry with the payee, the printed booking
// type and the cost note. A column
// that is a bare code — or absent — therefore gives way to one
// carrying more, and a column that already says something is kept: the
// row of record does not move, only the text it left as a code.
func richerText(have *string, alt string) *string {
	if !isCodeOnly(derefText(have)) || isCodeOnly(alt) {
		return have
	}
	return silver.StrPtrIfNonEmpty(alt)
}

// fill fills each text column of t that it left empty or as a bare code
// from w (richerText).
func (w webTxText) fill(t *canonical.TransactionChange) {
	t.Counterparty = richerText(t.Counterparty, w.counterparty)
	t.Description = richerText(t.Description, w.description)
	t.ProviderCategory = richerText(t.ProviderCategory, w.providerCategory)
}

// or is w with each column it left empty or as a bare code filled from
// alt, as fill fills a transaction's.
func (w webTxText) or(alt webTxText) webTxText {
	pick := func(have, alt string) string {
		return derefText(richerText(silver.StrPtrIfNonEmpty(have), alt))
	}
	return webTxText{
		counterparty:     pick(w.counterparty, alt.counterparty),
		description:      pick(w.description, alt.description),
		providerCategory: pick(w.providerCategory, alt.providerCategory),
	}
}

// extractInstrumentFromDescription1 pulls (ISIN, full caption)
// out of the silver row's payload.Description1 field, when
// present. UBS web statements format this field as
// "<security caption>; <ISIN>" — a typical caption looks like
// "UBS (Lux) Fund Solutions SICAV - UBS Core MSCI EMU UCITS ETF
// EUR dis-dist; LU0000000070". The trailing 12-char alphanumeric
// after "; " is a valid ISIN when the caption names a security; a
// length-and-charset check avoids grabbing other ";"-separated
// comments.
//
// Returns (nil, nil) when the field is missing, empty, or the
// trailing token doesn't look like an ISIN. The full caption
// (everything before the final separator, trimmed) is returned
// even when no ISIN matches — it's still useful as a name
// fallback.
func extractInstrumentFromDescription1(p webTxPayload) (instrumentID, description *string) {
	if p.Description1 == "" {
		return nil, nil
	}
	caption := strings.TrimSpace(p.Description1)
	// Try to split on the last "; ". Anything 12 chars long
	// with the ISIN shape (2 alpha + 10 alnum) is treated as
	// an ISIN; otherwise the caption stays whole.
	if i := strings.LastIndex(caption, "; "); i >= 0 {
		head := strings.TrimSpace(caption[:i])
		tail := strings.TrimSpace(caption[i+2:])
		if looksLikeISIN(tail) {
			id := tail
			desc := head
			return &id, silver.StrPtrIfNonEmpty(desc)
		}
	}
	return nil, silver.StrPtrIfNonEmpty(caption)
}

// webDescription returns the row's narrative — the gold description
// less the payer's message, which travels apart as the change's Memo.
// The Description1 caption (as extractInstrumentFromDescription1
// returns it, ISIN tail stripped) wins whenever the row has one —
// unchanged, because gold's name lookups key on it. Otherwise the
// narrative is composed from what the row does carry, in a fixed
// order: the booking type first, then the narrative lines — the PDF
// backfill's statement continuation lines less the statement's
// turnover-total line (bookingLines), or the CSV feed's Description3
// — joined with "; " (silver.JoinText). A row with a bare booking
// code and nothing else therefore reaches gold as that code; a row
// with no text at all stays NULL. Nothing is inferred or paraphrased.
// A payload that does not decode contributes only the booking type.
func webDescription(captionDesc *string, bookingType string, p webTxPayload) *string {
	if captionDesc != nil {
		// A deposit product is the one caption that is not a security.
		// Description1 names the PRODUCT ("UBS Call Deposit; Serial
		// no. …") and the booking type carries the only fact that
		// separates one of its rows from another — a principal
		// movement from the interest it pays. Without the type every
		// row of a deposit reads identically to any tier that works
		// from the narrative: not ambiguous, IDENTICAL. Prepending it
		// gives the row the same shape the statement era composes,
		// so one spelling reaches both eras.
		if isDepositProductBooking(bookingType) {
			return silver.StrPtrIfNonEmpty(silver.JoinText(bookingType, *captionDesc))
		}
		return captionDesc
	}
	own, _ := bookingLines(p.Continuation)
	parts := make([]string, 0, 2+len(own))
	parts = append(parts, bookingType)
	parts = append(parts, own...)
	parts = append(parts, p.Description3)
	return silver.StrPtrIfNonEmpty(silver.JoinText(parts...))
}

// isDepositProductBooking reports whether a booking type moves a
// deposit product's principal. Case-folded because the two feeds spell
// the same type differently — the statement era shouts it, the CSV
// export title-cases it — which is the same fold webKind applies for
// the same reason.
func isDepositProductBooking(bookingType string) bool {
	return bookingOf(bookingType).deposit
}

// isBookingType reports whether a string is nothing but the bank's own
// classification of the entry — "Third-Party Charges", "Dividend",
// "Custody Price".
//
// webKind already carries that vocabulary, and reading it there keeps
// one list rather than two that drift: with neither direction set, a
// booking type it names resolves to a kind, while anything else falls
// past the switch to the direction fallback and, with no direction, to
// TxKindOther. So "resolves to something" is exactly "is a booking
// type", and a type added to the classifier is recognised here for
// free.
//
// ONE BLIND SPOT, and it is structural rather than an oversight: a
// booking type whose kind comes from the DIRECTION — every payment
// order, "ORDER" among them — resolves to TxKindOther when asked with
// no direction, so this returns false for it. Such a type is a booking
// type by any other measure, and the callers are written to survive
// the answer: webKindHint returns the booking type unchanged when
// neither text is recognised, the payee refusal in text.go only
// declines to blank a payee it would have blanked, and isFiledBooking
// consults the booking's rail beside it. Widening the probe is not the
// fix: asked WITH a direction the fallback answers for every string,
// and then nothing is not a booking type.
func isBookingType(s string) bool {
	return strings.TrimSpace(s) != "" &&
		webKind(s, false, false) != canonical.TxKindOther
}

// turnoverTotalPrefix is the upper-cased head of the line an Account
// Statement prints before its closing balance: "Turnover total <debits>
// <credits>", the period's two totals.
const turnoverTotalPrefix = "TURNOVER TOTAL"

// isTurnoverTotalLine reports whether a statement continuation line is the
// period's turnover-total line: the two words, any case, followed by figures
// and nothing else — no letter after the prefix, so a line that merely
// begins with the words is not it. The line carries no date, so the
// collector's parser attaches it to whichever booking precedes it; it names
// the period's totals, never the booking's own text.
func isTurnoverTotalLine(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	if !strings.HasPrefix(upper, turnoverTotalPrefix) {
		return false
	}
	rest := upper[len(turnoverTotalPrefix):]
	if rest != "" && rest[0] != ' ' {
		return false
	}
	for _, r := range rest {
		if unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// bookingLines splits a PDF-backfill row's continuation lines into the
// booking's own text — every non-blank line that is not a turnover-total
// line, in order — and whether a turnover-total line was among them.
func bookingLines(continuation []string) (own []string, turnover bool) {
	for _, line := range continuation {
		switch {
		case strings.TrimSpace(line) == "":
		case isTurnoverTotalLine(line):
			turnover = true
		default:
			own = append(own, line)
		}
	}
	return own, turnover
}

// isStatementSummary reports whether a PDF-backfill row's narrative is
// nothing but the statement's turnover-total line — the shape of the
// period summary the parser emits as a booking at each period close. The
// caller pairs it with a zero amount: the same narrative under a real fee
// or interest amount is a booking, and stays.
func isStatementSummary(p webTxPayload) bool {
	own, turnover := bookingLines(p.Continuation)
	return turnover && len(own) == 0
}

// tickerFromDescription pulls the trailing `(TICKER)` segment
// out of a UBS web caption like "Reg.shs Example AG (XMPL)" or
// "Sponsored American Deposit Receipt Example Co Ltd
// (Repr. 5 shs)     (XMPL)". Returns nil
// when:
//
//   - The string has no trailing `(...)`.
//   - The bracketed content isn't 1-10 chars of upper-case
//     ASCII letters / digits / hyphens / dots (filters out things
//     like "(Lux)", and keeps a class suffix such as "(EXMP.B)").
//
// Strict enough to avoid false positives on long parenthetical
// phrases that happen to come last (e.g. "(Repr. 5 shs)").
func tickerFromDescription(desc string) *string {
	desc = strings.TrimRight(desc, " \t")
	if !strings.HasSuffix(desc, ")") {
		return nil
	}
	open := strings.LastIndex(desc, "(")
	if open < 0 {
		return nil
	}
	t := desc[open+1 : len(desc)-1]
	if len(t) < 1 || len(t) > 10 {
		return nil
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '.' {
			return nil
		}
	}
	return &t
}
