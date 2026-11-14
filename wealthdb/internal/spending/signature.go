// Package spending turns gold transactions into categorised spend and
// categorised income.
//
// Two families, one engine. What runs is one enrichment pass over each
// family's population (pass.go), driven by a family descriptor
// (family.go) that carries the names — which macro, which store, which
// taxonomy column — where the two differ, with a hook only where the
// BEHAVIOUR differs.
//
// Five tiers assign a category (docs/SPENDING.md §3, docs/INCOME.md
// §3). Four are deterministic and free, and this package builds all
// four; they run after every load, and in precedence order they are:
//
//   - the PIN ledger (pins.go), the holder's own word about one
//     transaction;
//   - the MATCHER (matcher.go), which pairs an own-account move's two
//     legs and rules both out of spending and out of income
//     altogether. It runs ONCE, on the spending side, and income reads
//     its verdicts — one own-account move, one pairing;
//   - the RULE tier (rules.go) — the built-ins, then the config's
//     `spending.rules` or `income.rules` — which places a row from its
//     own narrative;
//   - the PROVIDER tier (providermap.go), which places a row from the
//     provider's own filing of it.
//
// The fifth is the model tier. It lives in cmd/wealthdb (`wealthdb
// categorize`) and prices a verdict per counterparty SIGNATURE — never
// per transaction — so the same merchant is paid for once however many
// cards met it, and the same payer once however many accounts it paid.
//
// Below all five sits a floor income has and spending does not: a
// transaction KIND that names its own type (a dividend is
// INCOME_DIVIDENDS) needs no tier at all. It is applied in SQL at query
// time rather than written into the overlay, so a taxonomy revision
// re-reads rather than re-migrates (docs/INCOME.md §3).
//
// Everything here is a pure function of gold's contents, so the pass
// can be re-asserted from scratch after every load without asking
// whether it already ran.
package spending

import (
	"strings"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// SignatureVersion stamps every signature the current Normalize
// produces. It exists so a change to the normalisation rules can be
// TOLD APART from a change in the data: the merchant store keys
// verdicts by signature, and a re-normalisation that silently re-keyed
// them would orphan work that was paid for. Bump it whenever Normalize —
// or an adapter feeding it — starts producing a different string for the
// same input; the enrichment pass then carries the older-version
// verdicts forward onto the new keys (see RunDeterministicPass). What
// each version changed is listed in docs/SPENDING.md §4.
const SignatureVersion = 13

// maxSignatureLen bounds a signature, at a whole-token boundary.
// Narratives run long — a full address, a terminal id, a
// foreign-exchange note — and the tail is almost never what
// distinguishes one merchant from another, while an unbounded key
// makes the merchant store's index pay for noise.
const maxSignatureLen = 64

// leadingProcessorTokens are payment-processor prefixes stripped when
// they lead a narrative: they name the rail the money took, not the
// merchant it reached, and leaving them in splits one merchant across
// every processor it ever used.
//
// The list is deliberately short and confined to rails that are never
// person-to-person. A P2P rail's narrative carries a PERSON where a
// merchant would be, so stripping its prefix would hand a bare name to
// the model tier; those rails are fenced by TransferShaped instead.
var leadingProcessorTokens = map[string]bool{
	"SQ":  true, // Square
	"TST": true, // Toast
}

// Normalize reduces a transaction's narrative to a merchant signature: the
// key that groups every visit to one merchant into a single thing worth
// categorising once. docs/SPENDING.md §4 argues each rule below.
//
// Both fields are reduced (reduce, then dropPhoneRuns), the description only
// up to its memo separator (canonical.SplitDescriptionMemo), and the
// signature is chosen on the reduced forms, in this order:
//
//  1. the counterparty is an e-bill rail marker (isEbillMarker): the
//     creditor from the description's segments (ebillCreditor);
//  2. the description reduces to nothing: the counterparty, whatever it
//     holds;
//  3. the counterparty holds no word (Uninformative): the narrative's head
//     (narrativeHead) when a field tag structures the narrative and the head
//     carries a word, nothing when the tag is all there is, and otherwise
//     the description;
//  4. the head begins with the counterparty's tokens and carries more, so
//     the counterparty is a truncation of it: the head — unless a
//     phone-shaped run follows the counterparty's tokens (phoneRunEnd),
//     which makes the counterparty the whole name;
//  5. otherwise the counterparty, the merchant field proper.
//
// An empty result means no narrative at all, or nothing but a rail marker, a
// field tag or a number; the caller records a NULL signature. SignatureVersion
// stamps whichever revision of these rules produced a given key.
func Normalize(counterparty, description string) string {
	description, _ = canonical.SplitDescriptionMemo(description)
	cp := dropPhoneRuns(reduce(counterparty))
	desc := reduce(description)
	headText, structured := narrativeHead(description)
	head := reduce(headText)
	// Whether the description is the counterparty followed by a phone
	// number (rule 4), read before the description's runs are dropped
	// and after the counterparty's are: a counterparty that carries
	// the number itself is still the name in front of it.
	contactLine := startsWith(desc, 0, cp...) && phoneRunEnd(desc, len(cp)) > len(cp)
	desc = dropPhoneRuns(desc)
	head = dropPhoneRuns(head)
	switch {
	case isEbillMarker(cp):
		return joinCapped(ebillCreditor(description))
	case len(desc) == 0:
		return joinCapped(cp)
	case !hasWord(cp):
		// The narrative is all there is, and only a STRUCTURED one is
		// read down to its first segment: the field tag is what says
		// the bank wrote the payee there and the address behind it.
		// An unstructured narrative leads with the booking type as
		// often as with a payee — `Direct debit; <merchant>; <what
		// for>` — so trimming it to the first segment would file every
		// direct debit under one key and lose the merchant that
		// follows. It keeps the whole line, where the name at least
		// survives.
		if structured && hasWord(head) {
			return joinCapped(head)
		}
		// A narrative that is NOTHING but the field tag has no payee
		// behind it to fall back to, so taking the whole line would
		// put the bank's own booking code back as the key — and a
		// booking code is not a merchant. No key is the honest
		// answer: the row is refused at candidacy and left to the
		// tiers that read something other than a payee.
		if structured && len(head) == 0 {
			return ""
		}
		return joinCapped(desc)
	case len(head) > len(cp) && startsWith(head, 0, cp...) && !contactLine:
		return joinCapped(head)
	default:
		return joinCapped(cp)
	}
}

// narrativeHead reduces a statement narrative to the segment that
// names the payee: the bank's structured-field tag dropped from the
// front, everything from the first segment separator dropped from the
// end. It also reports whether the narrative was STRUCTURED — whether
// a field tag led it — because that is what says the first segment is
// the payee rather than the booking type.
//
// Both halves are the bank's filing rather than the merchant's
// identity. A UBS MT940 :86: narrative is written `Z44?<payee>` and
// its segments are the payee, the street, the country and town, then
// what the payment was for; the export feed writes the same shape
// without the tag. The tag names the structured slot the payee went
// into and differs across bookings of one merchant, and the address
// behind it is spelled differently across bookings too — a town in
// full or abbreviated, a postcode before the name or after it. Left
// in, each spelling is a separate signature for one merchant, and an
// address trailing a name reads to a model as part of it.
//
// This trims the KEY only. The fence that decides whether a signature
// may be sent anywhere reads the whole narrative independently
// (RowTransferShaped), so no rail written in a later segment can slip
// through because the key no longer shows it.
func narrativeHead(s string) (head string, structured bool) {
	s, structured = stripFieldTag(s)
	if i := strings.IndexByte(s, narrativeSegmentSep); i >= 0 {
		s = s[:i]
	}
	return s, structured
}

// narrativeSegmentSep separates the segments of a statement narrative:
// the payee from the address, the address from the payment's reason.
const narrativeSegmentSep = ';'

// fieldTagLen is the length of an MT940 :86: structured-field tag —
// a letter, two digits and a '?' ("Z44?").
const fieldTagLen = 4

// stripFieldTag removes a leading MT940 :86: structured-field tag,
// reporting whether one was there. A narrative that is nothing but the
// tag reduces to the empty string, which carries no word and is
// refused at candidacy like any other wordless key.
func stripFieldTag(s string) (string, bool) {
	if len(s) < fieldTagLen || s[fieldTagLen-1] != '?' {
		return s, false
	}
	if s[0] < 'A' || s[0] > 'Z' {
		return s, false
	}
	if s[1] < '0' || s[1] > '9' || s[2] < '0' || s[2] > '9' {
		return s, false
	}
	return s[fieldTagLen:], true
}

// reduce applies every per-field step of Normalize's reduction short
// of the phone-run drop and the length cap and returns the tokens that
// survive; none for a blank field. The runs are left in because
// Normalize reads where one stood in the description before dropping
// it.
func reduce(s string) []string {
	tokens := stripDirectDebitBoilerplate(tokenize(s))
	if len(tokens) > 0 && leadingProcessorTokens[tokens[0]] {
		tokens = tokens[1:]
	}
	return dropReferenceNumbers(tokens)
}

// dropReferenceNumbers removes the reference-number-shaped tokens
// (isReferenceNumber) in place and returns what is left.
func dropReferenceNumbers(tokens []string) []string {
	kept := tokens[:0]
	for _, tok := range tokens {
		if isReferenceNumber(tok) {
			continue
		}
		kept = append(kept, tok)
	}
	return kept
}

// minPhoneDigits is the floor under a phone-shaped run: the digits its
// tokens carry between them, counted once the reference numbers are
// gone. A number a statement spaces in groups keeps more than this
// even after a four-digit group is lost to isReferenceNumber — `+41 00
// 000 00 00`, `000 000 0000` (synthetic) — while the shorter digit
// runs a narrative carries for other reasons stay under it: two house
// numbers, the hours of a shop open `24/7`, a day and month once the
// year is gone, a time, a price in francs and cents. It is one half
// of the test; minPhoneGroupDigits is the other.
const minPhoneDigits = 6

// minPhoneGroupDigits is the shortest digit group a phone-shaped run
// must carry at least one of. A number as a statement spaces it
// always has one — the `000` in `+41 00 000 00 00`, the leading `000`
// of `000 000 00 00`, the `000` left of `000 000 0000` once the
// four-digit group has gone to isReferenceNumber (all synthetic) —
// while the runs of pairs that clear minPhoneDigits for other reasons
// have none: a date with a two-digit year, a day and month with a
// time behind them once the year is gone, three house numbers in a
// row. A number written as five pairs (`00 00 00 00 00`) is below
// this bar and stays, deliberately: it is not a form a statement
// prints, and a run of pairs is the shape of everything that is not a
// number.
const minPhoneGroupDigits = 3

// dropPhoneRuns removes every phone-shaped run (phoneRunEnd) in place
// and returns what is left. A statement prints a phone number as
// spaced groups, `+41 00 000 00 00` (synthetic), with or without the
// country code, and each group is too short for isReferenceNumber, so
// the groups would otherwise ride into the signature one by one and
// key a person-to-person payment on the payee's number. The test is
// the run, never the group: a single digit group between words — a
// house number, a numbered chain, a month — is part of a name and
// stays, and so does a run short of minPhoneDigits, or one with no
// group of minPhoneGroupDigits in it. The runs are read after the
// reference numbers are gone, so a house number in front of a postcode
// (`EXAMPLE STREET 1; 9999 EXAMPLETOWN`) is a lone group by then and
// stays too, while a number whose last group runs to four digits loses
// that group to the reference test and the rest here.
func dropPhoneRuns(tokens []string) []string {
	kept := tokens[:0]
	for i := 0; i < len(tokens); {
		if end := phoneRunEnd(tokens, i); end > i {
			i = end
			continue
		}
		kept = append(kept, tokens[i])
		i++
	}
	return kept
}

// phoneRunEnd reports where the phone-shaped run beginning at
// tokens[at] ends — the index past its last token — or at, when none
// begins there. A run is spaced digit groups: two or more consecutive
// all-digit tokens carrying minPhoneDigits digits or more between
// them, at least one of them minPhoneGroupDigits digits or more. It
// is maximal: it takes in every all-digit token that follows.
func phoneRunEnd(tokens []string, at int) int {
	end, digits, longest := at, 0, 0
	for end < len(tokens) && allDigits(tokens[end]) {
		digits += len(tokens[end])
		longest = max(longest, len(tokens[end]))
		end++
	}
	if end-at < 2 || digits < minPhoneDigits || longest < minPhoneGroupDigits {
		return at
	}
	return end
}

// joinCapped joins tokens up to maxSignatureLen, dropping whole
// trailing tokens rather than cutting one in half — half a word is not
// a merchant name, and two merchants whose names diverge only after
// the cut would otherwise share a key. A single token longer than the
// cap is truncated, since dropping it would leave nothing.
func joinCapped(tokens []string) string {
	var b strings.Builder
	for _, tok := range tokens {
		next := len(tok)
		if b.Len() > 0 {
			next += 1 + b.Len()
		}
		if next > maxSignatureLen {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(tok)
	}
	if b.Len() == 0 && len(tokens) > 0 {
		return tokens[0][:maxSignatureLen]
	}
	return b.String()
}

// tokenize upper-cases, folds the Latin-1 accents the European sources
// emit down to ASCII, and splits on every non-alphanumeric run. The
// fold matters because the same merchant is spelled with and without
// its accents depending on which export the row came through.
func tokenize(s string) []string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(unicode.ToUpper(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if folded, ok := latinFolds[unicode.ToLower(r)]; ok {
				b.WriteString(folded)
			} else {
				b.WriteByte(' ')
			}
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// latinFolds maps the accented lower-case letters the German / French /
// Italian narratives carry onto their ASCII spelling. Anything outside
// the table becomes a separator, which keeps a non-Latin script from
// smearing into one unreadable token.
var latinFolds = map[rune]string{
	'ä': "AE", 'ö': "OE", 'ü': "UE", 'ß': "SS",
	'à': "A", 'á': "A", 'â': "A", 'ã': "A", 'å': "A", 'æ': "AE",
	'ç': "C", 'è': "E", 'é': "E", 'ê': "E", 'ë': "E",
	'ì': "I", 'í': "I", 'î': "I", 'ï': "I", 'ñ': "N",
	'ò': "O", 'ó': "O", 'ô': "O", 'õ': "O", 'ø': "O",
	'ù': "U", 'ú': "U", 'û': "U", 'ý': "Y",
}

// isReferenceNumber reports whether a token is a store / terminal /
// authorisation number rather than part of the merchant's name: four
// or more digits and nothing else. The threshold keeps the digits that
// ARE names — a numbered convenience-store chain, a street number that
// distinguishes two branches — while dropping the long serials that
// would otherwise give every visit its own signature.
func isReferenceNumber(tok string) bool {
	return len(tok) >= 4 && allDigits(tok)
}

// allDigits reports whether a tokenize'd token is digits and nothing
// else. tokenize emits ASCII only, so the test is the byte range.
func allDigits(tok string) bool {
	if tok == "" {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

// stripDirectDebitBoilerplate removes the Swiss direct-debit (LSV)
// mandate notice a UBS narrative puts BEFORE the creditor, in this
// shape (synthetic values, exact structure):
//
//	DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; EXAMPLE CREDITOR AG; ...
//
// Keyed on its leading tokens, every such row folds to the signature
// of the notice — one fake merchant swallowing a card issuer and a
// telecom alike. The notice is the bank's format, not the holder's
// data, so it comes off here, before the signature is built, and the
// signature starts at the creditor.
//
// The match is deliberately narrow: three recognisable phrases, in
// this order, at the head of the narrative, and nothing else.
//
//   - the DIRECT DEBIT marker, optional. The UBS web adapter carries
//     it as a structured booking type, so the free text may begin at
//     the notice;
//   - <CODE> OBJECTION TO <BANK>, required: the notice's own
//     fingerprint. CODE and BANK are one token each — the LSV
//     identification and the bank whose format this is;
//   - WITHIN <N> DAYS, optional: the objection window.
//
// A narrative that carries the words but not this shape is left
// alone. When the notice is all there was, the mandate CODE is kept
// as the signature rather than nothing: it is the creditor's LSV
// identity, and it is a code rather than a word, so Uninformative
// refuses it at candidacy instead of an empty signature dropping the
// row out of every store.
func stripDirectDebitBoilerplate(tokens []string) []string {
	i := 0
	if startsWith(tokens, i, "DIRECT", "DEBIT") {
		i += 2
	}
	// tokens[i] is CODE, tokens[i+3] is BANK; both must exist.
	if !startsWith(tokens, i+1, "OBJECTION", "TO") || i+3 >= len(tokens) {
		return tokens
	}
	code := tokens[i]
	i += 4
	if i+2 < len(tokens) && tokens[i] == "WITHIN" && allDigits(tokens[i+1]) && tokens[i+2] == "DAYS" {
		i += 3
	}
	if i == len(tokens) {
		return []string{code}
	}
	return tokens[i:]
}

// isEbillMarker reports whether a reduced field is, whole, the marker
// a Swiss statement prints on an e-bill to name the rail: EBILL or
// E-BILL, alone or followed by RECHNUNG or INVOICE. The three
// spellings a statement prints are EBILL-RECHNUNG, EBILL INVOICE and
// E-BILL; hyphen and space are the same separator to
// tokenize, so each is matched with either. The match is the whole
// field on purpose — a creditor whose name carries the word is a
// creditor, and only a field that holds the marker and nothing else
// is the rail.
func isEbillMarker(tokens []string) bool {
	i := 0
	switch {
	case startsWith(tokens, 0, "EBILL"):
		i = 1
	case startsWith(tokens, 0, "E", "BILL"):
		i = 2
	default:
		return false
	}
	switch len(tokens) - i {
	case 0:
		return true
	case 1:
		return tokens[i] == "RECHNUNG" || tokens[i] == "INVOICE"
	}
	return false
}

// ebillCreditor builds the signature tokens for a row whose counterparty is
// an e-bill rail marker, from the description alone (docs/SPENDING.md §4). On
// the UBS statement era the description is composed as (synthetic values,
// exact structure)
//
//	PAYNET ORDER; EBILL-RECHNUNG; EXAMPLE TELECOM AG; CH EXAMPLETOWN 9999; QRR; 0000…; 1 times E-Banking domestic
//
// The segments up to and including the marker are dropped, so the creditor
// leads. After that, segment by segment: the payment-order boilerplate goes
// (isPaymentOrderBoilerplate), so does the wordless second line of a page
// footer wherever the break falls, and so does any other segment with no
// word in it — except the creditor's own slot, the first segment kept, since
// a name can be an abbreviation the word test refuses. The postal address
// stays. A description without the marker is taken whole, a fallback the
// adapter contract never reaches; a marker with nothing behind it yields
// nothing, since as a key it would become the one fake merchant this strip
// exists to remove.
func ebillCreditor(description string) []string {
	segments := strings.Split(description, ";")
	for i, seg := range segments {
		if isEbillMarker(tokenize(seg)) {
			segments = segments[i+1:]
			break
		}
	}
	var out []string
	afterFooter := false
	for _, seg := range segments {
		tokens := tokenize(seg)
		salad := afterFooter && !hasWord(tokens)
		afterFooter = isPageFooter(tokens)
		if salad || isPaymentOrderBoilerplate(tokens) {
			continue
		}
		tokens = dropPhoneRuns(dropReferenceNumbers(tokens))
		if len(tokens) == 0 || (len(out) > 0 && !hasWord(tokens)) {
			continue
		}
		out = append(out, tokens...)
	}
	return out
}

// isPaymentOrderBoilerplate reports whether a narrative segment is one
// of the lines a statement prints around a payment order's creditor
// that name nothing about the creditor: the QR-reference marker, the
// `<N> times E-Banking domestic` execution line, and the first line
// of a page footer (isPageFooter). Each is matched as the whole
// segment — the first two exactly, the footer by its opening phrase,
// since the page numbers vary — so a creditor whose name happens to
// carry one of the words is untouched.
func isPaymentOrderBoilerplate(tokens []string) bool {
	switch {
	case len(tokens) == 1 && tokens[0] == "QRR":
		return true
	case len(tokens) == 5 && allDigits(tokens[0]) && startsWith(tokens, 1, "TIMES", "E", "BANKING", "DOMESTIC"):
		return true
	case isPageFooter(tokens):
		return true
	}
	return false
}

// isPageFooter reports whether a segment is the first line of the
// footer a statement PDF prints at a page break, `Form without
// signature Page <n> / <m>`, matched by its opening phrase since the
// page numbers vary. The second line, the form id and date salad,
// has no fixed phrase; ebillCreditor drops it by its position.
func isPageFooter(tokens []string) bool {
	return startsWith(tokens, 0, "FORM", "WITHOUT", "SIGNATURE", "PAGE")
}

// startsWith reports whether tokens[at:] begins with want.
func startsWith(tokens []string, at int, want ...string) bool {
	if at+len(want) > len(tokens) {
		return false
	}
	for k, w := range want {
		if tokens[at+k] != w {
			return false
		}
	}
	return true
}

// wordMinLetters is the shortest all-letter token that counts as a
// word. Two letters is a code — an MT940 :86: narrative can be nothing
// but a two-letter booking tag — and three is where the abbreviations
// that ARE names begin.
const wordMinLetters = 3

// hasWord reports whether any of the tokenize'd tokens is a word.
func hasWord(tokens []string) bool {
	for _, tok := range tokens {
		if isWord(tok) {
			return true
		}
	}
	return false
}

// isWord reports whether a tokenize'd token is all letters and at
// least wordMinLetters long.
func isWord(tok string) bool {
	return len(tok) >= wordMinLetters && allUpperASCII(tok)
}

// allUpperASCII is the letter test both of the above share. tokenize
// emits upper-case ASCII only, so the range IS the alphabet. An empty
// token passes vacuously; each caller states what it wants of length.
func allUpperASCII(tok string) bool {
	for i := 0; i < len(tok); i++ {
		if tok[i] < 'A' || tok[i] > 'Z' {
			return false
		}
	}
	return true
}
