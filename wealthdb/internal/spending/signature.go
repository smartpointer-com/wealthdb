// Package spending turns gold transactions on spending accounts into
// categorised spend.
//
// Five tiers assign a category (docs/SPENDING.md §3). Four are
// deterministic and free, and this package builds all four; they run
// after every load, and in precedence order they are:
//
//   - the PIN ledger (pins.go), the holder's own word about one
//     transaction;
//   - the MATCHER (matcher.go), which pairs an own-account move's two
//     legs and rules both out of spending altogether;
//   - the RULE tier (rules.go) — the built-ins, then the config's
//     `spending.rules` — which places a row from its own narrative;
//   - the PROVIDER tier (providermap.go), which places a row from the
//     provider's own filing of it.
//
// The fifth is the model tier. It lives in cmd/wealthdb (`wealthdb
// categorize`) and prices a verdict per merchant SIGNATURE — never per
// transaction — so the same merchant is paid for once however many
// cards met it.
//
// Everything here is a pure function of gold's contents, so the pass
// can be re-asserted from scratch after every load without asking
// whether it already ran.
package spending

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// SignatureVersion stamps every signature the current Normalize
// produces. It exists so a change to the normalisation rules can be
// TOLD APART from a change in the data: the merchant store keys
// verdicts by signature, and a re-normalisation that silently re-keyed
// them would orphan work that was paid for. Bump it whenever Normalize
// starts producing a different string for the same input; the
// enrichment pass then carries the older-version verdicts forward onto
// the new keys (see RunDeterministicPass).
//
// History:
//   - 1: the original reduction.
//   - 2: leading Swiss direct-debit mandate boilerplate — DIRECT DEBIT,
//     <CODE> OBJECTION TO <BANK>, WITHIN <N> DAYS — is stripped, so a
//     signature that used to be the notice itself now starts at the
//     creditor (stripDirectDebitBoilerplate).
//   - 3: the counterparty no longer wins unconditionally. When it
//     reduces to no word at all, or is a truncation of the
//     description, the signature is built from the description
//     instead (see Normalize). On the UBS adapter the counterparty is
//     silver's promoted first narrative segment, so after the version
//     2 strip a direct debit's signature was the bare mandate code and
//     an ordinary transfer's was the bank's own name, with the
//     creditor sitting in the description; both are keyed by the
//     creditor now.
//   - 4: three reductions land together. An e-bill rail marker is
//     never a merchant: when the counterparty is one — EBILL-RECHNUNG,
//     EBILL INVOICE or E-BILL, the whole field — the signature is
//     built from the description's segments after the marker, less
//     the payment-order boilerplate the statement prints around the
//     creditor (ebillCreditor). On the UBS adapter's statement era the
//     marker is the first narrative segment and so the promoted
//     counterparty; keyed on it, every e-bill would share one signature
//     per spelling of the marker, and each is keyed by its creditor
//     instead. And a memo — the payer's own free text, which gold
//     stores at the description's end after
//     canonical.DescriptionMemoSeparator — is cut off before the
//     description is reduced, so a payment is keyed the same whatever
//     was written on it. The cut itself moves no version 3 key; the
//     adapter change that introduced the memo does move the keys of
//     rows whose description had carried the message, several old keys
//     onto one, and the bump is what lets the carry run
//     (docs/SPENDING.md §4). And a phone number is never part of a
//     merchant: a phone-shaped run — spaced digit groups, two or more
//     consecutive all-digit tokens carrying minPhoneDigits digits or
//     more between them with at least one group of minPhoneGroupDigits
//     or more, read once the reference numbers are gone; a number as a
//     statement spaces it — is dropped from either field
//     (dropPhoneRuns), and a description that is the counterparty
//     followed by one leaves the counterparty standing (Normalize,
//     rule 4). Version 3's digit test wanted four digits in one token
//     and let a spaced number's groups through one by one.
//   - 5: the memo fold covers the separator's edge shapes
//     (canonical.JoinDescriptionMemo). Version 4 folded each
//     separator a narrative carried, but three shapes outlived that
//     fold and were read back as a memo boundary; two of them move
//     keys. Two ADJACENT separators overlap, so the second survived a
//     single pass: the stored description was split there and
//     everything behind the survivor was dropped as the payer's
//     words, where the whole narrative is keyed now. And a narrative
//     OPENING on the bare separator read as all memo and no narrative
//     at all, so the description contributed nothing and the
//     counterparty decided the key alone; it is a narrative again.
//     The third — a narrative CLOSING on the bare separator — only
//     moves where the memo boundary falls, and the memo is dropped
//     from the key either way. Rows that shared one cut-down key move
//     onto keys of their own, which is the split case
//     (docs/SPENDING.md §4); every other narrative is keyed exactly
//     as version 4 keys it.
//   - 6: the memo cut reads the memo-only prefix before it looks for
//     the separator anywhere else (canonical.SplitDescriptionMemo).
//     A description that is all memo — no narrative at all — and
//     whose memo carried the separator in the payer's own words was
//     cut INSIDE the memo, so part of what the payer wrote became the
//     narrative and was keyed; the whole memo is memo now and such a
//     row falls back to the counterparty, or to no signature. The
//     lone-dash narrative is folded with it
//     (canonical.JoinDescriptionMemo): the separator's leading space
//     turned it into the memo-only prefix, so its description was read
//     as all memo and the counterparty decided the key alone; it is a
//     narrative again. Rows that shared one cut-down key move onto
//     keys of their own, which is the split case (docs/SPENDING.md
//     §4); every other narrative is keyed exactly as version 5 keys
//     it.
//   - 7: Normalize is unchanged; what moves is what the UBS adapter
//     gives it. Where the MT940 feed and the account-statement export
//     both recorded one booking, the MT940 row won and reached gold
//     with the bank's bare code as its whole narrative and no payee
//     at all — a key that is a code, refused at candidacy and
//     placeable by no tier. The adapter now folds the export's record
//     of the same entry onto that row (internal/silver/ubs/merge.go),
//     so those rows are keyed on the payee the export names. Each such
//     row leaves a code key that other rows may still share — a
//     code-only booking with no counterpart in the export keeps it —
//     so this is the split case (docs/SPENDING.md §4): a verdict at a
//     code key stays where it is and the rows that moved are back in
//     the backlog under a key that names someone. Every other
//     narrative is keyed exactly as version 6 keys it.
//   - 8: Normalize is again unchanged; again what moves is what the
//     UBS adapter gives it. Three eras record that cash ledger over
//     overlapping periods with disjoint id schemes, so a booking the
//     statement archive printed and the export or the MT940 feed also
//     carried reached gold as TWO rows keyed independently. The
//     adapter now folds them to one, keeping the machine-readable
//     record and carrying the statement's narrative onto whatever
//     column the survivor left empty or as a bare code
//     (internal/silver/ubs/web_overlay.go). A survivor that gains a
//     payee that way is keyed on it instead of on the code it had.
//     Rows that shared one code key may move onto keys of their own,
//     which is the split case (docs/SPENDING.md §4). Every other
//     narrative is keyed exactly as version 7 keys it.
//   - 9: the bank's filing stops entering the key, on two counts.
//     Normalize reads a narrative down to its head — the structured
//     field tag off the front, the address and the payment's reason
//     off the end — so one merchant is one key however its town is
//     spelled and whichever field slot the payee was written into,
//     and a model stops reading an address as part of a name. And the
//     UBS adapter no longer promotes a booking type into the
//     counterparty column (internal/silver/ubs/text.go): a row the
//     bank filed without a payee was keyed on how it was booked, which
//     filed every such row under one merchant and buried the payee the
//     MT940 feed carries for the same booking. Those rows move onto
//     the payee, and the key they leave is shared by whatever rows had
//     no payee anywhere — the split case (docs/SPENDING.md §4). A
//     narrative with no field tag, no segment separator and a payee in
//     its counterparty is keyed exactly as version 8 keys it.
//   - 10: a narrative that is NOTHING but the structured field tag
//     yields no signature at all. Version 9 read such a narrative down
//     to its head, found the head empty, and fell back to the whole
//     line — which is the tag, so the bank's booking code became the
//     key and then the merchant name. A code names no one: these rows
//     are refused at candidacy now and reach a verdict through the
//     tiers that read something other than a payee, the transaction's
//     own kind among them. Rows that shared a code key lose it; every
//     other narrative is keyed exactly as version 9 keys it.
//   - 11: Normalize is unchanged; what moves is again what the UBS
//     adapter gives it. A charge for one of the bank's own services —
//     custody, advice, a safe box, the service-price close, an
//     interest calculation — carried an account or security REFERENCE
//     in its payee column, and a bare booking code on the feed that
//     writes no payee at all. So one relationship's fees keyed as many
//     merchants as it had referenced accounts, none of them a party.
//     The adapter now names the bank on those, so they key as the
//     bank — or, where the narrative itself leads with the bank's name
//     and its product, on that head, which names the same merchant.
//     A depositary's pass-through and a third-party charge keep their
//     own keys, being collected on someone else's behalf. Every other
//     narrative is keyed exactly as version 10 keys it.
const SignatureVersion = 11

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

// Normalize reduces a transaction's narrative to a merchant signature:
// the key that groups every visit to one merchant into a single thing
// worth categorising once.
//
// Both fields are reduced, and which one becomes the signature is
// decided on the reduced forms, in this order:
//
//  1. The counterparty is an e-bill rail marker (isEbillMarker): the
//     description's segments after the marker, less the payment-order
//     boilerplate around the creditor (ebillCreditor). The marker
//     names the rail the bill came in on, never the creditor, and it
//     has words in it, so no later step would refuse it.
//  2. The description reduces to nothing: the counterparty, whatever
//     it holds. A bare code is kept over an empty signature, which
//     would drop the row out of every store.
//  3. The counterparty reduces to nothing, or to no word at all (the
//     Uninformative test): the description. An adapter's counterparty
//     is silver's promoted field, and a promotion that cut the
//     narrative at its first separator can hold nothing but the bank's
//     own notice — the Swiss direct-debit mandate, `<CODE> OBJECTION
//     TO <BANK>` — while the creditor sits in the description.
//  4. The description begins with the counterparty's tokens and
//     carries more: the description, because the counterparty is then
//     a truncation of it — `UBS SWITZERLAND AG` cut from `UBS
//     SWITZERLAND AG; C/O UBS CARD CENTER` — and the tail is what
//     tells one creditor from another. Not when what follows the
//     counterparty's tokens — the counterparty read with its own runs
//     gone — is a phone-shaped run (phoneRunEnd): a name with its
//     number behind it is the whole name, not a truncation of one, so
//     the counterparty stands whatever else the description carries —
//     `<counterparty>; <phone>; <more>` keys on the counterparty
//     alone. The shape that needs this is the person-to-person contact
//     line, the payee, the number and the rail's reference, which then
//     keys as it does where the statement era prints the same lines
//     behind a booking type, and as it does when the payee carries the
//     number in both fields; a merchant's line with the number in
//     front of its address keys on the name by the same rule, and with
//     the number behind its address on the name and the address, the
//     number gone. A number printed unbroken is a reference number to
//     isReferenceNumber, dropped before the precedence is read, so it
//     is no run and the description is read as a truncation like any
//     other.
//  5. Otherwise the counterparty: it is the merchant field proper, and
//     adapters are contractually bound not to reformat it.
//
// An empty result means the row carries no narrative at all, or
// nothing but a rail marker or a number; the caller records the row
// with a NULL signature rather than inventing one.
//
// The description is read only up to its memo separator
// (canonical.DescriptionMemoSeparator): what follows is the payer's
// own words about the row, not the payee's identity, and it must not
// key the row — on the shape where the description wins (rule 3, the
// UBS web caption) every message would otherwise become its own
// merchant.
//
// The reduction of a field, in order: fold to upper-case ASCII, turn
// every non-alphanumeric run into a single space, strip leading
// direct-debit mandate boilerplate, drop a leading processor token,
// drop reference-number-shaped tokens and then drop every phone-shaped
// run of what is left (dropPhoneRuns); the field chosen is then capped
// in length. Rule 4 reads where a run stood in the description before
// its runs are dropped, against a counterparty whose runs are gone
// already. The e-bill path reduces the description segment by segment
// instead (ebillCreditor): reference numbers and phone runs are
// dropped as above, and the segment filters replace the field-level
// strips. It is deterministic and allocation-cheap; SignatureVersion
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

// ebillCreditor builds the signature tokens for a row whose
// counterparty is an e-bill marker, from the description alone. On
// the UBS adapter's statement era the marker is the first narrative
// segment, and the description is composed as (synthetic values,
// exact structure)
//
//	PAYNET ORDER; EBILL-RECHNUNG; EXAMPLE TELECOM AG; CH EXAMPLETOWN 9999; QRR; 0000…; 1 times E-Banking domestic
//
// — the booking type, the marker, the creditor, its postal address,
// then the payment order's own boilerplate. The marker is the
// promoted counterparty, so under the ordinary precedence every
// e-bill on the adapter would share one signature per spelling of
// it, and a utility and an insurer would be one merchant with one
// verdict.
//
// The segments up to and including the marker are dropped, so the
// creditor leads. After that, segment by segment: the payment-order
// boilerplate lines (isPaymentOrderBoilerplate) go, and so does the
// line after a page footer when it has no word in it — the footer is
// two lines, `Form without signature Page …` and then the form id, a
// counter and the statement date, and the break can fall anywhere in
// the narrative, between the marker and the creditor included, where
// the salad would otherwise land in the creditor's slot; a bare
// reference number, or a phone number, goes through the ordinary
// digit stripping; and a segment with no word in it — a lone country
// code — goes too,
// EXCEPT in the creditor's own slot, the first segment kept: the bank
// prints the creditor there, and a name can be an abbreviation the
// word test refuses. Nothing else is stripped: a postal address
// stays, because a creditor named with nothing but an address is
// still that creditor, and the fence knows an address from an IBAN.
//
// A description without the marker is taken whole, booking type
// first. That is a fallback rather than a designed key — it would
// lead with the booking type, not the creditor — and it is
// unreachable under the adapter contract, where the counterparty is
// the description's first segment (after the booking type in the
// statement era): a marker counterparty means a marker segment.
//
// A marker with nothing behind it yields nothing. Unlike the
// direct-debit code, the marker carries no creditor identity worth
// keeping as a key, and it has words in it, so as a signature it
// would reach the model and become the one fake merchant this strip
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
// more, and each one used to fence an ordinary tradesman's bill out
// of candidacy.
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

// wordMinLetters is the shortest all-letter token that counts as a
// word. Two letters is a code — an MT940 :86: narrative can be nothing
// but a two-letter booking tag — and three is where the abbreviations
// that ARE names begin.
const wordMinLetters = 3

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

// hasWord reports whether any of the tokenize'd tokens is a word.
func hasWord(tokens []string) bool {
	for _, tok := range tokens {
		if isWord(tok) {
			return true
		}
	}
	return false
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
// being wrong here is one model call on a code, while the cost of the
// old reading was a merchant the model could have named in one look
// sitting in the catch-all instead.
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

// isWord reports whether a tokenize'd token is all letters and at
// least wordMinLetters long. tokenize emits upper-case ASCII only, so
// the letter test is the ASCII range.
func isWord(tok string) bool {
	if len(tok) < wordMinLetters {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < 'A' || tok[i] > 'Z' {
			return false
		}
	}
	return true
}
