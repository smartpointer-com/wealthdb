package ubs

import (
	"encoding/json"
	"regexp"
	"strings"
)

// counterAccountInNarrative finds the counter account the EXPORT feed states
// in its own narrative. The statement era carries it in a field of its own
// (the PDF parser's `counter_account`); the CSV era does not, and writes it
// into free text instead, in a fixed three-part shape:
//
//	Reason for payment: <purpose>; Account no. IBAN: <iban>; Transaction no. <n>
//
// It is the bank's own statement of where the money went, and it is the
// strongest evidence of an own-account move there is — stronger than a
// narrative regex, and available on rows whose other leg the product does not
// collect at all. Left in the free text it would reach gold as prose that
// nothing reads.
//
// The IBAN alone is taken, never the purpose or the payee: those name a
// person, and this value is compared against the relationship's own account
// ids. A narrative that states no IBAN returns empty, which is the common
// case — most rows pay a third party whose account gold does not hold.
// Case-insensitive because normalizeIBAN raises the case anyway, and a
// matcher stricter than the normaliser it feeds would drop a row for a
// difference that makes no difference.
var counterAccountInNarrative = regexp.MustCompile(`(?i)IBAN:\s*([A-Z]{2}[0-9]{2}[0-9A-Z ]{10,32}?)\s*(?:;|$)`)

// counterAccountFromNarrative returns the normalised counter IBAN a row's
// narrative names, or "" where it names none.
func counterAccountFromNarrative(p webTxPayload) string {
	for _, s := range []string{p.Description3, p.Description1} {
		if m := counterAccountInNarrative.FindStringSubmatch(s); m != nil {
			return normalizeIBAN(m[1])
		}
	}
	return ""
}

// counterLegInNarrative finds the line on which a statement writes the OTHER
// LEG of a movement that converted currency: what the money became, or came
// from, and the rate between the two.
//
//	CCY 1 234.56 Rate 1.234567
//
// It is the counter account's sibling, and a strictly stronger claim. A
// counter account says WHERE the money went; this says what the other side of
// the booking IS — its currency and its figure — which is the one fact that
// can join two legs no amount test can compare, because a conversion's two
// legs never carry the same number.
//
// Anchored on the whole line, and on the rate that closes it. The amount uses
// a space as its thousands separator and a dot for decimals, and a minor unit
// the currency does not have is simply absent, so the rate is what marks the
// end of the figure and keeps a line of running text from being read as one.
// A row stating no conversion — the overwhelming majority — matches nothing
// and is left exactly as it was.
var counterLegInNarrative = regexp.MustCompile(
	`^([A-Z]{3}) ([0-9][0-9 ]*(?:\.[0-9]+)?) Rate [0-9]+(?:\.[0-9]+)?$`)

// counterLegFromNarrative returns the currency and amount a row's narrative
// states for the other leg of its movement, or two empty strings where it
// states none.
//
// The statement era writes each continuation line separately, which is where
// the line is looked for; the export era's two narrative columns are read
// after it, so a feed that ever spells the same fact there is covered by the
// same rule rather than by a second one.
func counterLegFromNarrative(p webTxPayload) (currency, amount string) {
	lines := make([]string, 0, len(p.Continuation)+2)
	lines = append(lines, p.Continuation...)
	lines = append(lines, p.Description3, p.Description1)
	for _, s := range lines {
		if m := counterLegInNarrative.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
			return m[1], strings.ReplaceAll(m[2], " ", "")
		}
	}
	return "", ""
}

// counterCurrencyKey and counterAmountKey carry the stated other leg into
// gold. Two keys rather than one object because the splice below writes a
// string value, and because either half is meaningless without the other:
// a consumer takes both or neither.
const (
	counterCurrencyKey = `"counter_currency":`
	counterAmountKey   = `"counter_amount":`
)

// withCounterLeg stamps the stated other leg onto a row's payload. Both keys
// or neither: a currency with no figure names no leg.
func withCounterLeg(payload json.RawMessage, currency, amount string) json.RawMessage {
	if currency == "" || amount == "" {
		return payload
	}
	out := spliceStringField(string(payload), counterAmountKey, amount)
	return spliceStringField(string(out), counterCurrencyKey, currency)
}

// mortgageStampInNarrative matches the reference UBS prints for the mortgage a
// payment services — `HYPOTHEK <base>.<tranche> <sequence>`.
//
// The export era states it in `Description2` and nowhere else. The statement
// era composed the same stamp into the description itself, which is why a
// mortgage payment from the statement era is recognised by the narrative rule
// and one from the export era is not: the bank stopped printing the word where
// the composed description could reach it, and nothing else on the row said so.
var mortgageStampInNarrative = regexp.MustCompile(`(?i)\bHYPOTHEK\s+([0-9]+\s*\.\s*[A-Z0-9]+\s+[0-9]{4})\b`)

// mortgageRefShape is the reference itself, once the spaces are gone:
// a digit run, a dot, a tranche code, and a four-digit sub-account sequence.
var mortgageRefShape = regexp.MustCompile(`^([0-9]+)\.([A-Z0-9]+?)([0-9]{4})$`)

// mortgageRefKey folds either spelling of a mortgage reference onto one
// lookup key, so a stamp the bank prints can be matched against an account id
// gold holds without either being rewritten into the other.
//
// The two spellings differ in the branch. An account id is
// `BBBB AAAAAAAA.MMM NNNN` — a four-digit branch, then the account base
// zero-padded to eight. The stamp carries the base alone, unpadded. So the
// fold drops the branch (withBranch) and then the padding, leaving the part
// both spellings agree on. Nothing is constructed and no branch is guessed:
// an unknown stamp simply finds no account.
func mortgageRefKey(ref string, withBranch bool) string {
	m := mortgageRefShape.FindStringSubmatch(
		strings.ToUpper(strings.ReplaceAll(ref, " ", "")))
	if m == nil {
		return ""
	}
	base := m[1]
	if withBranch && len(base) > 8 {
		base = base[len(base)-8:]
	}
	if base = strings.TrimLeft(base, "0"); base == "" {
		return ""
	}
	return base + "." + m[2] + m[3]
}

// mortgageRefFromNarrative returns the folded key a narrative's HYPOTHEK stamp
// names, or "" for a narrative carrying none.
func mortgageRefFromNarrative(s string) string {
	m := mortgageStampInNarrative.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return mortgageRefKey(m[1], false)
}

// counterAccountKey is the payload key the statement era already uses, so a
// consumer reads ONE field whichever feed produced the row.
const counterAccountKey = `"counter_account":`

// withCounterAccount stamps a narrative-derived counter account onto a row's
// payload. The statement era's own value is the parser's, and spliceStringField
// refuses to overwrite a key the payload already carries, which is what keeps a
// derived value from displacing a stated one.
func withCounterAccount(payload json.RawMessage, iban string) json.RawMessage {
	return spliceStringField(string(payload), counterAccountKey, iban)
}

// withResolvedMortgage rewrites a STATED counter account that names a
// mortgage in prose to the id silver holds that mortgage under.
//
// It replaces where withCounterAccount refuses to, and what is being
// replaced is the difference. That function guards the statement
// parser's answer against a value derived from free text. This one
// touches no derived value at all: it rewrites the parser's own answer
// from the mortgage's NAME to the mortgage's ID — the same fact, in the
// spelling every consumer joins on. A name nothing can join to is not
// an answer worth protecting, and leaving it would make the field look
// resolved while resolving to nothing.
//
// Surgical on purpose. The stamp occurs in the narrative lines too,
// where it belongs and is what the bank printed; only the value of the
// counter account key moves, and the rest of the payload stays the
// bytes the collector wrote.
func withResolvedMortgage(payload json.RawMessage, stamp, id string) json.RawMessage {
	if stamp == "" || id == "" || stamp == id {
		return payload
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return payload
	}
	re, err := regexp.Compile(`"counter_account"\s*:\s*"` + regexp.QuoteMeta(stamp) + `"`)
	if err != nil {
		return payload
	}
	return json.RawMessage(re.ReplaceAllLiteralString(
		string(payload), `"counter_account":`+string(encoded)))
}
