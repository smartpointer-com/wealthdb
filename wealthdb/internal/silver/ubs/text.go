package ubs

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Text-column helpers shared by the web and PSN emitters. The contract they
// implement — which silver field becomes gold's description, counterparty and
// provider_category in each era — is documented in docs/adapters/ubs.md §7.
// They only ever assign those three columns; kinds, signs, amounts, dates and
// ids are decided before they run and never read from the text they compose.

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

// splitBookingType splits a CSV-feed Description2 into the payer's
// message and the bank's booking type. The column holds the booking type
// alone ("e-banking payment order") unless a message was typed on the
// order, when it holds "<message>; <booking type>" — the message first,
// the type last — so the split is at the LAST "; ": the type is what
// follows it, the message what precedes it. A column without the
// separator is all booking type and is returned untouched, so a row
// without a message projects byte for byte as before; the `;Reversal`
// suffix has no space and stays on the type.
func splitBookingType(descKind string) (message, bookingType string) {
	i := strings.LastIndex(descKind, "; ")
	if i < 0 {
		return "", descKind
	}
	return strings.TrimSpace(descKind[:i]), strings.TrimSpace(descKind[i+2:])
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
// §7). The counterparty is silver's promoted column verbatim — the CSV
// feed's first Description1 segment, the PDF backfill's first
// continuation line — because it feeds gold's merchant signature and
// must never be reformatted here. The one line not kept is the
// statement's turnover-total line: promoted when it is the first line
// a period-close booking carries, it names the period's totals, never
// a payee.
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
	return webTxText{
		counterparty:     payee,
		description:      derefText(webDescription(captionDesc, bookingType, p)),
		providerCategory: bookingType,
	}, instrumentID, memo
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
// It decides only which of two records of the SAME entry carries the
// narrative (richerText). It never decides what a row is: no kind,
// sign, amount, date or id is read from it.
func isCodeOnly(s string) bool {
	s = strings.TrimSpace(s)
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
// wrote nothing but a code both columns are that code, and it carries
// no payee at all. The Account-Statement export records the same entry
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
