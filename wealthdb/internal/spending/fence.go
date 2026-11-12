package spending

import (
	"regexp"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The candidacy fences: whether a narrative, or the signature folded
// from it, may be shown to a model at all. They read the same folding
// as Normalize (tokenize) and answer a different question: not what a
// narrative's key is, but whether it may leave the machine.

// ibanLengths is the IBAN registry's total character count per
// country. A country the registry does not list issues no IBAN, so a
// two-letter token that opens no entry here opens no account number.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16,
	"BG": 22, "BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22,
	"CY": 28, "CZ": 24, "DE": 22, "DJ": 27, "DK": 18, "DO": 28, "EE": 20,
	"EG": 29, "ES": 24, "FI": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22,
	"GI": 23, "GL": 18, "GR": 27, "GT": 28, "HR": 21, "HU": 28, "IE": 22,
	"IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "LY": 25,
	"MC": 27, "MD": 24, "ME": 22, "MK": 19, "MN": 20, "MR": 27, "MT": 31,
	"MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23, "PK": 24, "PL": 28,
	"PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "RU": 33, "SA": 24,
	"SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "SO": 23,
	"ST": 25, "SV": 28, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22,
	"VG": 24, "XK": 20, "YE": 30,
}

// ibanShaped matches an IBAN's printed alphabet — two letters, two
// check digits, then the account body. It is the cheap pre-test; the
// registry length and the check digits are what decide.
var ibanShaped = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]+$`)

// ibanShapedRun reports whether the tokens, spaces removed, carry an
// IBAN that begins where a token begins. An IBAN is printed either as
// one token or as spaced groups whose first group is the country code
// and check digits, so a real one always starts a token; anchoring
// there is the first of three narrowings.
//
// The other two are what tell an IBAN from the letters-and-digits
// every European remittance carries anyway. Anchoring alone does not:
// a run is read from a token start, but SO IS a Swiss legal form in
// front of a postal code (`EXAMPLE VERSICHERUNG AG; 9999 EXAMPLE;
// 000000...`), a canton in front of an ESR reference (`ZH; 12 34567
// ...`), a reference prefix (`RN123456789`, `TN:012345678` — and a
// bank writes one of those on nearly every domestic e-banking
// narrative) and the word `No.` in front of an invoice number. Each
// of those is two letters and two digits at a token start followed by
// more, and each would fence an ordinary tradesman's bill out of
// candidacy.
//
// So the run must also be exactly as long as that country's IBAN and
// must satisfy the ISO 7064 mod-97 check. A real IBAN passes both by
// construction; a legal form, a canton, a postal code and a reference
// number pass neither.
func ibanShapedRun(tokens []string) bool {
	joined := strings.Join(tokens, "")
	off := 0
	for _, tok := range tokens {
		if isIBAN(joined[off:]) {
			return true
		}
		off += len(tok)
	}
	return false
}

// isIBAN reports whether s OPENS with a well-formed IBAN — the run may
// carry more text behind it, which is how a spaced IBAN reads once the
// spaces are gone.
func isIBAN(s string) bool {
	if len(s) < 4 {
		return false
	}
	n, ok := ibanLengths[s[:2]]
	if !ok || len(s) < n {
		return false
	}
	candidate := s[:n]
	return ibanShaped.MatchString(candidate) && ibanMod97(candidate) == 1
}

// ibanMod97 computes an IBAN's ISO 7064 MOD-97-10 residue: the first
// four characters move to the end, each letter expands to its
// position in the alphabet plus ten, and the resulting decimal is
// taken modulo 97. A valid IBAN leaves 1. The digits are folded in
// one at a time because the expansion of a full IBAN overflows every
// integer width.
func ibanMod97(s string) int {
	rem := 0
	for i := range s {
		switch c := s[(i+4)%len(s)]; {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return -1
		}
	}
	return rem
}

// transferFenceTokens fence a narrative when they appear as a whole
// token. Whole-token matching is what keeps WIRE from firing on
// WIRELESS and ACH from firing on a merchant whose name contains it.
//
// The mobile person-to-person rails are named here rail by rail,
// because their vocabulary is national, and a rail nobody has listed
// fences nothing. A rail token fences every booking type it appears
// in, whatever verb the era spells in front of it and whatever case
// the export uses, because tokenize upper-folds. The cost is that
// merchant payments on the same rail are fenced too — the rail cannot
// tell a person from a shop — and those rows stay placeable by a
// config rule or a pin. A further national rail is fenced by adding
// its token to this list.
//
// TWINT is deliberately NOT here, and it is the exception that shows
// what the cost above is worth. In Switzerland it is a consumer-to-
// BUSINESS rail at least as much as a person-to-person one, and the
// CARD feed's line says which: a payment to a person reads `Sent to
// <initials> <masked mobile>`, a payment to a shop reads the trading
// name and a place, and neither shape ever appears in the other. A
// token on the rail's name fences both, and a shop fenced from the
// model is a shop that gets lumped under a generic category instead
// of being named — which on this rail is most of it. So the card feed
// is fenced by SHAPE instead: maskedContact and the SENT TO phrase
// keep the people out and let the shops through to be identified.
//
// Two things do keep a name-shaped fence. Where a rail's line does not
// distinguish the two at all — a US P2P rail names a person and a
// business the same way — the token stays. And TWINT's BANK feed is
// fenced by its booking type below, because there the personal data
// is not the counterparty but the payer.
var transferFenceTokens = map[string]bool{
	"TRANSFER": true, "TRANSFERS": true, "XFER": true, "UEBERTRAG": true,
	"WIRE": true, "WIRES": true, "ACH": true, "SEPA": true, "IBAN": true,
	"GIRO": true, "REMITTANCE": true, "P2P": true,
	"ZELLE": true, "VENMO": true, "CASHAPP": true, "QUICKPAY": true,
	"AUTOPAY": true, "EPAY": true, "AUTOPMT": true,
}

// transferFencePhrases fence a narrative when they appear as a
// contiguous run of tokens — the multi-word spellings of the same
// rails, which no single token catches.
var transferFencePhrases = []string{
	"CASH APP", "AUTO PAY", "AUTOMATIC PAYMENT", "ONLINE PAYMENT",
	"ELECTRONIC PAYMENT", "BILL PAY", "BILL PAYMENT", "DIRECT DEBIT",
	"STANDING ORDER", "PAYMENT THANK YOU", "FUNDS TRANSFER",
	"MOBILE TRANSFER", "ONLINE TRANSFER",
	// What a mobile rail writes where a merchant would be. `SENT TO`
	// leads the payee half of a person-to-person line and no shop's
	// name contains it.
	//
	// `UBS TWINT` is the BANK feed's own booking type for the same
	// rail, and that half is fenced WHOLE — shops included — for a
	// reason the card feed does not share: every one of its rows
	// prints `TWINT-ACC.:<mobile>`, the account that INITIATED the
	// payment, unmasked. A mobile number is personal data whoever it
	// belongs to, and it is on the row whether the counterparty is a
	// person or a shop, so the feed carries personal data on every row
	// and no shape test can make any of it safe to send.
	"SENT TO", "UBS TWINT",
}

// maskedContact matches a contact number with its middle digits
// starred out — how a mobile rail names the party at the other end
// without printing the number. It is personal data whatever rail
// carries it, so it fences on its own.
//
// It is read on the RAW text, before tokenize, because tokenize drops
// the mask along with every other separator: `079***1234` reaches the
// token set as `079` and `1234`, two ordinary numbers, and the shape
// that made it a phone number is gone.
var maskedContact = regexp.MustCompile(`[0-9]{2,4}\*{2,}[0-9]{2,4}`)

// TransferShaped reports whether a narrative looks like money moving
// between accounts or between people rather than money being spent at
// a merchant.
//
// It is a FENCE, not a classifier. The rule tier decides what such a
// row IS; this decides what may be shown to the model tier at all. A
// P2P narrative carries a counterparty's NAME where a merchant would
// be, an IBAN carries an account number, and neither belongs in a
// prompt sent to a third party — so the fence errs towards firing, and
// a false positive costs nothing worse than one uncategorised row.
//
// ATM narratives are deliberately NOT fenced. They name a bank and a
// place, never a person, and the rule tier categorises them
// (`cash_withdrawal`) without any model involvement — fencing them
// would achieve nothing and would hide a rule-tier regression.
//
// The argument may be a raw narrative, a provider's own filing of the
// row, or a Normalize'd signature; the same folding is applied
// internally either way. Which one a caller passes matters: a rail
// leads a narrative rather than naming the payee, and the reduction
// can drop it — the counterparty wins the key, or the leading segment
// falls away — so a signature on its own may no longer say which rail
// the money took. Deciding candidacy on the key alone therefore fences
// less than deciding it on the whole row, which is what
// RowTransferShaped reads and what candidacy uses.
func TransferShaped(s string) bool {
	if maskedContact.MatchString(s) {
		return true
	}
	tokens := tokenize(s)
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		if transferFenceTokens[tok] {
			return true
		}
	}
	joined := strings.Join(tokens, " ")
	for _, phrase := range transferFencePhrases {
		if strings.Contains(joined, phrase) {
			return true
		}
	}
	return ibanShapedRun(tokens)
}

// RowTransferShaped applies the fence to a whole gold row rather than
// to its key: the reduced signature, the provider's own filing of the
// row, and the narrative half of the description. It fires if the rail
// is written in any of the three.
//
// The key alone under-fences. A mobile person-to-person rail LEADS a
// narrative and names no payee, so the reduction — which prefers the
// counterparty, and caps what is left at a token boundary — can drop
// the rail and leave a key that is a private individual's name and
// nothing else. Such a key is not transfer-shaped, carries a word, and
// is not the provider's filing, so TransferShaped, Uninformative and
// FilingOnly all pass it. The rail survives in the provider filing and
// in the raw narrative whatever the reduction did to the key, so
// reading those closes the hole; anything the fence already refuses on
// the key is refused here unchanged.
//
// The MEMO half of the description is deliberately not read. It is the
// payer's own words about the row (canonical.DescriptionMemoSeparator)
// rather than the payee's identity, it names no rail, and a fence that
// read it would refuse a merchant because a note beside the row
// happened to mention one.
//
// The candidacy loops in `wealthdb categorize` decide with this
// predicate, read over the whole spending population: a signature is
// refused when ANY row under it fires it, because the reduction is
// many-to-one and a signature — never a row — is what leaves the
// machine.
func RowTransferShaped(signature, providerCategory, description string) bool {
	if TransferShaped(signature) || TransferShaped(providerCategory) {
		return true
	}
	narrative, _ := canonical.SplitDescriptionMemo(description)
	return TransferShaped(narrative)
}

// organisationMarkers fence a signature as a BUSINESS when they appear
// as a whole token. They are the reason PersonShaped can be as blunt as
// it is: a person's name is two or three words and nothing else, so the
// way to spot one is to look for what a business puts in its name that
// a person never does.
//
// Whole-token matching, and single tokens only, for the reason
// transferFenceTokens gives and one more: the short legal forms here
// are substrings of ordinary words — AG is inside MANAGEMENT, PAGE and
// VILLAGE, SA is inside SALARY — so a substring test would fence
// almost everything. They are consulted as a set, never joined into a
// phrase.
//
// Three groups, and each earns its place differently. LEGAL FORMS (INC,
// GMBH, SARL, …; canonical.LegalForms, which the other readers of firm
// names share, less canonical.NameLikeLegalForms, which double as given
// names) are the strongest signal there is: no natural person carries
// one. INSTITUTIONAL WORDS (BANK, PENSION, TREASURY, IRS, FINANZAMT, …)
// name what an organisation does, and the income side is full of them —
// an employer, a pension fund, a tax office and an insurer are exactly
// the payers a household most wants named. TRADE WORDS (STORE, MARKET,
// CAFE, …) are the card side's, where a two-word merchant name is the
// norm.
//
// The cost of being wrong runs one way only. A business whose name
// carries no marker — two plain words, like a partnership named after
// its founders — is fenced with the people on a bank account, and stays
// placeable by a config rule or a pin, which is the remedy every fenced
// row has. A person whose name happens to contain a marker is NOT
// fenced, which is why this table is a fence on top of the rail fence
// rather than instead of it. An institutional or trade word extends it
// by a token here; a legal form goes in canonical.LegalForms. A marker
// that is also a common surname (say HOLDING) is a judgement call, and
// the list errs towards fencing LESS. Both directions cost something: an
// omitted marker quietly withholds a genuine organisation from the
// model tier, and an ambiguous one lets a person who shares it through.
// A deployment that cannot accept the second keeps its model endpoint
// on this machine, where what leaves is nothing and the arm's setting
// stops mattering.
var organisationMarkers = func() map[string]bool {
	m := map[string]bool{
		// What an organisation does.
		"BANK": true, "PAYROLL": true, "SALARY": true, "LOHN": true,
		"GEHALT": true, "TREASURY": true, "PENSION": true,
		"PENSIONSKASSE": true, "KASSE": true, "AMT": true, "OFFICE": true,
		"FUND": true, "FUNDS": true, "TRUST": true, "INSURANCE": true,
		"VERSICHERUNG": true, "UNIVERSITY": true, "UNIVERSITAET": true,
		"SCHOOL": true, "CITY": true, "STADT": true, "KANTON": true,
		"CANTON": true, "COUNTY": true, "STATE": true, "FEDERAL": true,
		"IRS": true, "HMRC": true, "FINANZAMT": true, "STEUERAMT": true,
		"AHV": true, "SVA": true, "KRANKENKASSE": true, "MUTUAL": true,
		"PARTNERS": true, "GROUP": true, "HOLDING": true, "HOLDINGS": true,
		"SERVICES": true, "SYSTEMS": true, "TECHNOLOGIES": true,
		"LABS": true,
		// Trades, which are the card side's two-word names.
		"STUDIO": true, "STORE": true, "MARKET": true, "SHOP": true,
		"CAFE": true, "RESTAURANT": true, "HOTEL": true,
	}
	// Legal forms, but those that are also a given name: a bare name
	// that carries one is still a person's.
	for w := range canonical.LegalForms {
		if !canonical.NameLikeLegalForms[w] {
			m[w] = true
		}
	}
	return m
}()

// PersonShaped reports whether a signature looks like a natural
// person's name and nothing else.
//
// It is the one shape that is PII BY ITSELF. Every other fence reads a
// marker beside the name — a rail token, an IBAN, a starred mobile
// number — and a narrative that carries none of them but is a bare
// name slips through all of them: TransferShaped sees no rail,
// Uninformative sees a word, FilingOnly sees something other than the
// bank's tag. Inbound bank rows are where that shape arrives, because a
// credit transfer's narrative IS the sender.
//
// The test is deliberately crude, and every clause is a way of not
// firing:
//
//   - TWO OR MORE tokens, so a single word is never a person. One word
//     is how a brand is written and how a bank abbreviates.
//   - EVERY token all letters, so a reference number, a date, a branch
//     code or a house number clears it. A person's name written on its
//     own carries no digit.
//   - NO organisation marker (organisationMarkers).
//   - NO rail token, so `WIRE JANE` is transfer-shaped rather than
//     person-shaped and is refused by the fence that names it.
//
// It says nothing about whether a row IS a person, and it is not a
// classifier: it decides what may be shown to a model. A business named
// like a person fires it and stays placeable by a rule or a pin.
//
// It is NOT applied on its own. The caller decides where it bites —
// candidacy applies it to rows on non-card accounts only, because most
// card merchants are two or three plain words and a name-shaped arm
// over them would gut the model tier — and a deployment can turn it off
// (`spending.categorization.fence_person_names`) when the model runs on
// this machine.
//
// The argument may be a raw narrative or a Normalize'd signature; the
// same folding is applied internally either way.
func PersonShaped(s string) bool {
	tokens := tokenize(s)
	if len(tokens) < 2 {
		return false
	}
	for _, tok := range tokens {
		if !isAllLetters(tok) {
			return false
		}
		if organisationMarkers[tok] || transferFenceTokens[tok] {
			return false
		}
	}
	return true
}

// isAllLetters reports whether a tokenize'd token is letters only, of
// any length. It is isWord without the wordMinLetters floor: an initial
// is a letter of a person's name, and `J EXAMPLE` is exactly the shape
// PersonShaped exists to catch.
func isAllLetters(tok string) bool { return tok != "" && allUpperASCII(tok) }

// Uninformative reports whether a signature carries nothing a model
// could name: after normalisation it holds no all-letter token of at
// least wordMinLetters letters. A bare booking code, a two-digit
// number, a code followed by a reference number — the shapes an MT940
// :86: narrative reduces to when the bank wrote nothing but its own
// tag — all fail that test; a merchant name always has a word in it.
//
// It is a further refusal at candidacy beside TransferShaped, and a
// narrower claim: not "this is a transfer" but "there is no word here
// at all". It says nothing about WHICH words are merchants. An
// all-letter code cannot be told from a word — that one goes to the
// model, where the gauntlet's verbatim-echo check remains the
// backstop.
//
// A brand with a reference number glued to it counts as a word too,
// which is a LOOSER test than the one Normalize keys on — see
// isBrandWithReference. The two predicates want different things: a
// signature must not be keyed on a booking code, but a signature that
// merely CONTAINS one alongside a name is still worth asking about.
//
// The argument may be either a raw narrative or a Normalize'd
// signature; the same folding is applied internally either way.
func Uninformative(s string) bool {
	return !hasNameable(tokenize(s))
}

// FilingOnly reports whether a signature is nothing but the provider's
// own filing of the row: the booking type an adapter composes a
// description from when the bank wrote nothing else — `order`,
// `credit`, `FEES` — reduced exactly as the signature was. There is a
// word in such a key, but it names how the bank booked the row, not
// whom it paid: one key covers every row the bank filed that way, and
// a verdict bought at it would cover them all. It is a further refusal
// at candidacy beside TransferShaped and Uninformative, and the same
// claim as the latter — nothing to name — so it is counted with it. A
// row with no provider filing is never filing-only, and a narrative
// that carries more than the filing (`credit; Ref 7`) is not either.
func FilingOnly(signature, providerCategory string) bool {
	return signature != "" && signature == Normalize("", providerCategory)
}

// mechanismTokens name how money moved, never who sent or received it:
// a cheque paid in by phone or at a remote scanner, a wire received, a
// cheque written. A bank that prints `CHECK 1234` and nothing else has
// named the paper, not the payee.
var mechanismTokens = map[string]bool{
	"MOBILE": true, "REMOTE": true, "ONLINE": true, "DEPOSIT": true,
	"WIRED": true, "FUNDS": true, "RECEIVED": true, "CHECK": true,
}

// nachaVerificationEntry is the company entry description NACHA mandates
// on an account-verification micro-deposit. It marks the row whoever sends
// it.
const nachaVerificationEntry = "ACCTVERIFY"

// MechanismOnly reports whether a signature names how money moved and no
// party: every token a mechanism or a number (`MOBILE DEPOSIT`,
// `REMOTE ONLINE DEPOSIT 1`, `WIRED FUNDS RECEIVED`, `CHECK`), or the verification
// literal anywhere. Such a signature holds words, so Uninformative passes
// it, but a model asked about it can only return a catch-all. It is a
// further refusal at candidacy, counted with Uninformative; a rule or a pin
// still places the rows.
func MechanismOnly(s string) bool {
	tokens := tokenize(s)
	if slices.Contains(tokens, nachaVerificationEntry) {
		return true
	}
	mechanism := false
	for _, tok := range tokens {
		switch {
		case mechanismTokens[tok]:
			mechanism = true
		case !allDigits(tok):
			return false
		}
	}
	return mechanism
}

// hasNameable reports whether any token is something a model could
// name: a word, or a brand carrying a reference number.
func hasNameable(tokens []string) bool {
	for _, tok := range tokens {
		if isWord(tok) || isBrandWithReference(tok) {
			return true
		}
	}
	return false
}

// brandMinLetters is the shortest leading letter run that reads as a
// NAME rather than a code once digits follow it. It is higher than
// wordMinLetters on purpose: an all-letter token of three characters
// is plausibly an abbreviated name and isWord admits it, but three
// letters followed by digits is the shape of a booking code — the
// mandate notice a Swiss direct-debit narrative opens with is exactly
// that, three letters then a digit and a letter, and admitting one
// would key a signature on the notice instead of on the creditor
// behind it.
const brandMinLetters = 5

// isBrandWithReference reports whether a token opens with a long
// enough letter run and then carries a digit — a brand with its order
// or reference number glued on, which is how a card descriptor
// routinely reaches the key as ONE token: a meal-kit delivery, an
// online course and an electronics order all arrive as
// `<BRAND><digits>` beside a two-letter state code.
//
// isWord refuses those, because the token is not all letters, and the
// refusal it feeds is about WASTE rather than privacy: the cost of
// being wrong here is one model call on a code, while the cost of
// refusing is a merchant the model could have named in one look sitting
// in the catch-all instead.
func isBrandWithReference(tok string) bool {
	n := 0
	for n < len(tok) && tok[n] >= 'A' && tok[n] <= 'Z' {
		n++
	}
	if n < brandMinLetters || n == len(tok) {
		return false
	}
	for i := n; i < len(tok); i++ {
		if tok[i] >= '0' && tok[i] <= '9' {
			return true
		}
	}
	return false
}
