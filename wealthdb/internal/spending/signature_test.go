package spending

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestNormalize pins the reduction rule by rule. Every narrative here
// is invented; the shapes are real, the values are not.
func TestNormalize(t *testing.T) {
	// A token past the cap, built rather than typed so the length is
	// the thing the case is about.
	overlong := strings.Repeat("A", maxSignatureLen+16)
	cases := []struct {
		name         string
		counterparty string
		description  string
		want         string
	}{
		{"counterparty wins over description",
			"Corner Market", "POS PURCHASE CORNER MKT 7788", "CORNER MARKET"},
		{"description is the fallback when no merchant column",
			"", "Blue Harbour Cafe", "BLUE HARBOUR CAFE"},
		{"both blank yields no signature", "  ", "", ""},
		{"case and punctuation fold away",
			"blue-harbour   cafe, inc.", "", "BLUE HARBOUR CAFE INC"},
		{"a leading processor token is the rail, not the merchant",
			"SQ *Blue Harbour Cafe", "", "BLUE HARBOUR CAFE"},
		{"a processor token elsewhere is part of the name",
			"Harbour SQ Cafe", "", "HARBOUR SQ CAFE"},
		{"reference numbers drop out so every visit shares a signature",
			"Corner Market #90210 REF 01234567", "", "CORNER MARKET REF"},
		{"short digit runs are part of the name",
			"Kiosk 24 Seven", "", "KIOSK 24 SEVEN"},
		{"accents fold to their ASCII spelling",
			"Café Grünhafen", "", "CAFE GRUENHAFEN"},
		{"an over-long narrative is capped at a whole token",
			"Blue Harbour Cafe And Roastery Of The Northern Districts Limited Partnership", "",
			"BLUE HARBOUR CAFE AND ROASTERY OF THE NORTHERN DISTRICTS LIMITED"},
		// The one place a token is cut mid-word: dropping it whole
		// would leave no signature at all, and an empty key drops the
		// row out of every store.
		{"a single token longer than the cap is cut mid-word",
			overlong, "", strings.Repeat("A", maxSignatureLen)},
		{"an over-long first token drops the tokens behind it",
			overlong + " Cafe", "", strings.Repeat("A", maxSignatureLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.counterparty, tc.description); got != tc.want {
				t.Errorf("Normalize(%q, %q) = %q, want %q",
					tc.counterparty, tc.description, got, tc.want)
			}
		})
	}
}

// TestNormalizeFallsBackToDescription pins the precedence between the
// two narrative fields, the change SignatureVersion 3 records. The
// counterparty is the merchant field proper and wins by default, but
// an adapter's counterparty can be a promotion of the narrative's
// first segment, and that segment can be nothing but the bank's
// notice, or a truncation of the description. In either case the
// creditor is in the description, and the signature is built from
// there; the card rule then fires on the signature ALONE, which is
// what was lost on live data. With the fallback disabled — the
// counterparty winning whenever it reduces to any token at all — every
// case in the first two groups fails except the reference-number one,
// which reduces to nothing, and the last group still passes; so the
// test is not vacuous. Every value is synthetic.
func TestNormalizeFallsBackToDescription(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		description  string
		want         string
		cardRule     bool
	}{
		// The counterparty holds no word: the description.
		{"a direct debit's promoted counterparty is the mandate notice",
			"CRD1W OBJECTION TO UBS",
			"CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; UBS CARD CENTER; CREDIT CARD STATEMENT 03/2026",
			"UBS CARD CENTER CREDIT CARD STATEMENT 03", true},
		{"the same notice in front of a telecom",
			"TEL1W OBJECTION TO UBS",
			"TEL1W OBJECTION TO UBS; WITHIN 30 DAYS; EXAMPLE TELECOM (SCHWEIZ) AG; 9999 MUSTERSTADT",
			"EXAMPLE TELECOM SCHWEIZ AG MUSTERSTADT", false},
		{"a bare booking code",
			"ZV01", "Blue Harbour Cafe 7788", "BLUE HARBOUR CAFE", false},
		{"a counterparty that is only a reference number",
			"01234567", "Corner Market", "CORNER MARKET", false},

		// The counterparty is a truncation of the description — of its
		// FIRST SEGMENT, since that is as far as the key reads (§4).
		// A care-of line and an address sit past the separator and are
		// the bank's filing of where to send the post, so the two rows
		// below key on the payee; what the description holds past it
		// still reaches the rule tier, which reads the raw fields.
		{"the bank's name, its card-centre qualifier a care-of line",
			"UBS SWITZERLAND AG", "UBS SWITZERLAND AG;C/O UBS CARD CENTER",
			"UBS SWITZERLAND AG", false},
		{"a creditor cut from its address",
			"Example Telecom AG", "Example Telecom AG; 9999 Musterstadt; Account: 0123456789",
			"EXAMPLE TELECOM AG", false},
		{"a description richer within the segment still wins",
			"Example Telecom", "Example Telecom AG; 9999 Musterstadt",
			"EXAMPLE TELECOM AG", false},

		// The counterparty stands.
		{"an informative counterparty that is not a prefix",
			"Blue Harbour Cafe", "Card purchase Blue Harbour Cafe Zurich", "BLUE HARBOUR CAFE", false},
		{"a description that adds only reference numbers is not longer",
			"Corner Market", "Corner Market 90210", "CORNER MARKET", false},
		{"a code with no description stays the code",
			"ZV01", "", "ZV01", false},
		{"a notice with no description stays the code",
			"DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS", "", "CRD1W", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.counterparty, tc.description)
			if got != tc.want {
				t.Fatalf("Normalize(%q, %q) = %q, want %q",
					tc.counterparty, tc.description, got, tc.want)
			}
			detailed, _, ok := RuleCategory(got, "", "", "")
			if ok != tc.cardRule || (ok && detailed != canonical.SpendDetailedCardSpend) {
				t.Errorf("RuleCategory(%q) on the signature alone = (%q, %v), want card rule = %v",
					got, detailed, ok, tc.cardRule)
			}
		})
	}
}

// TestSignatureVersion pins the stamp: a change that moves keys
// without bumping it would carry nothing and orphan everything. A
// change to Normalize is the usual cause; an adapter that changes what
// it hands Normalize moves keys just as surely (versions 4, 7, 8 and
// 9), so the stamp is not a version number for this file alone.
func TestSignatureVersion(t *testing.T) {
	if SignatureVersion != 9 {
		t.Errorf("SignatureVersion = %d, want 9", SignatureVersion)
	}
}

// TestNormalizeIgnoresMemo pins the memo contract SignatureVersion 4
// records: what gold stores after canonical.DescriptionMemoSeparator
// is the payer's words, and a payment is keyed the same whatever they
// were. The caption is the
// shape that motivates it — the UBS web adapter's, where the
// description extends the counterparty and is therefore the field the
// signature is built from, so without the cut every message would
// ride into the key. The memo is the payer's words to the rule tier
// too: no built-in fires on it, while a config rule — the holder's own
// input — may key on it. Every value is synthetic.
func TestNormalizeIgnoresMemo(t *testing.T) {
	const (
		cp      = "EXAMPLE PAYEE"
		caption = "EXAMPLE PAYEE EXAMPLE STREET 1 9999 EXAMPLETOWN"
	)
	want := Normalize(cp, caption)
	if want != "EXAMPLE PAYEE EXAMPLE STREET 1 EXAMPLETOWN" {
		t.Fatalf("fixture is wrong: the caption should win the signature, got %q", want)
	}
	for _, memo := range []string{"THANKS", "Rent March", "see you soon", "EXAMPLE PAYEE again"} {
		description := canonical.JoinDescriptionMemo(caption, memo)
		if got := Normalize(cp, description); got != want {
			t.Errorf("Normalize(%q, %q) = %q, want %q: the memo entered the signature",
				cp, description, got, want)
		}
	}
	// A memo with no narrative before it keys nothing; the counterparty
	// still wins when there is one.
	if got := Normalize("", canonical.JoinDescriptionMemo("", "THANKS")); got != "" {
		t.Errorf("a memo alone signed as %q, want no signature", got)
	}
	if got := Normalize(cp, canonical.JoinDescriptionMemo("", "THANKS")); got != cp {
		t.Errorf("a memo alone beside a counterparty signed as %q, want %q", got, cp)
	}
	// A built-in never fires on the memo: the words are the payer's,
	// and a delta placed from them would carry the rule's provenance.
	for _, memo := range []string{"card payment", "Bancomat", "Hypothekarzins Q3"} {
		if detailed, _, ok := RuleCategory("", "", canonical.JoinDescriptionMemo(caption, memo), ""); ok {
			t.Errorf("RuleCategory on memo %q placed %q, want no built-in to fire", memo, detailed)
		}
	}
	if detailed, _, ok := RuleCategory("", "", canonical.JoinDescriptionMemo("Bancomat Main Street", "THANKS"), ""); !ok ||
		detailed != canonical.SpendDetailedCashWithdrawal {
		t.Errorf("RuleCategory on a narrative that names the machine = (%q, %v), want the atm rule", detailed, ok)
	}
	// A config rule may key on the memo: it is the holder's own input.
	rules := []Rule{{Match: regexp.MustCompile(`(?i)hypothekarzins`), Category: canonical.SpendDetailedInternalTransfer}}
	if got, ok := ConfigRuleCategory(rules, RuleRow{Counterparty: cp,
		Description: canonical.JoinDescriptionMemo(caption, "Hypothekarzins Q3")}); !ok || got != canonical.SpendDetailedInternalTransfer {
		t.Errorf("ConfigRuleCategory on a memo = (%q, %v), want the rule to fire", got, ok)
	}
}

// TestNormalizeIsStable is the property the merchant store depends on:
// the same merchant reached through different narrative decorations
// collapses to one key, or every verdict is bought once per spelling.
func TestNormalizeIsStable(t *testing.T) {
	want := Normalize("Blue Harbour Cafe", "")
	for _, variant := range []string{
		"BLUE HARBOUR CAFE",
		"blue harbour cafe",
		"Blue  Harbour   Cafe",
		"SQ *BLUE HARBOUR CAFE",
		"Blue-Harbour-Cafe 7788",
		"DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; BLUE HARBOUR CAFE",
	} {
		if got := Normalize(variant, ""); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", variant, got, want)
		}
	}
}

// TestNormalizeStripsDirectDebitBoilerplate pins the one narrative
// shape Normalize rewrites rather than merely folds: the Swiss
// direct-debit mandate notice that precedes the creditor. Every value
// is synthetic; the STRUCTURE is the bank's. The cases after the first
// six are the boundary — the words without the shape are left alone.
// With stripDirectDebitBoilerplate returning its input unchanged, the
// first six fail and the rest still pass, so the test is not vacuous.
func TestNormalizeStripsDirectDebitBoilerplate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"a card issuer behind the notice",
			"DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; EXAMPLE CREDITOR AG; C/O EXAMPLE CARD CENTER; CREDIT CARD STATEMENT 03/2026",
			"EXAMPLE CREDITOR AG C O EXAMPLE CARD CENTER CREDIT CARD"},
		{"a telecom behind the notice",
			"DIRECT DEBIT; TEL1W OBJECTION TO UBS; WITHIN 30 DAYS; EXAMPLE TELECOM (SCHWEIZ) AG; 9999 MUSTERSTADT; ACCOUNT: 0123456789",
			"EXAMPLE TELECOM SCHWEIZ AG MUSTERSTADT ACCOUNT"},
		{"the marker is a booking type and may not travel with the text",
			"CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; EXAMPLE CREDITOR AG",
			"EXAMPLE CREDITOR AG"},
		{"a different objection window",
			"DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 60 DAYS; EXAMPLE CREDITOR AG",
			"EXAMPLE CREDITOR AG"},
		{"no window at all",
			"DIRECT DEBIT; CRD1W OBJECTION TO UBS; EXAMPLE CREDITOR AG",
			"EXAMPLE CREDITOR AG"},
		{"case folds before the match",
			"direct debit; crd1w objection to ubs; within 30 days; Example Creditor AG",
			"EXAMPLE CREDITOR AG"},
		{"the words without the shape: notice after the creditor",
			"EXAMPLE CREDITOR AG; OBJECTION TO UBS WITHIN 30 DAYS",
			"EXAMPLE CREDITOR AG OBJECTION TO UBS WITHIN 30 DAYS"},
		{"the words without the shape: marker but no mandate phrase",
			"DIRECT DEBIT; EXAMPLE TELECOM AG; WITHIN 30 DAYS",
			"DIRECT DEBIT EXAMPLE TELECOM AG WITHIN 30 DAYS"},
		{"the words without the shape: objection not led by a code",
			"NOTICE OF OBJECTION TO UBS FILED",
			"NOTICE OF OBJECTION TO UBS FILED"},
		{"the words without the shape: mandate phrase cut short",
			"DIRECT DEBIT; CRD1W OBJECTION TO",
			"DIRECT DEBIT CRD1W OBJECTION TO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in, ""); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeDirectDebitNoticeAlone pins what the notice folds to
// when there is no creditor behind it: not an empty signature, which
// would drop the row out of every store, but the mandate code — a
// signature Uninformative refuses, so it is never sent either.
func TestNormalizeDirectDebitNoticeAlone(t *testing.T) {
	for _, in := range []string{
		"DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS",
		"DIRECT DEBIT; CRD1W OBJECTION TO UBS",
		"CRD1W OBJECTION TO UBS; WITHIN 30 DAYS",
	} {
		got := Normalize(in, "")
		if got == "" {
			t.Errorf("Normalize(%q) = %q; the notice alone must not fold to nothing", in, got)
			continue
		}
		if !Uninformative(got) {
			t.Errorf("Normalize(%q) = %q; want a signature Uninformative refuses", in, got)
		}
		if TransferShaped(got) {
			t.Errorf("Normalize(%q) = %q; the fence is not what should catch it", in, got)
		}
	}
}

// TestNormalizeEbillMarkerIsNotAMerchant pins the change
// SignatureVersion 4 records. On the UBS adapter's statement era an
// e-bill's first narrative segment is the rail marker, promoted into
// the counterparty, and the description leads with the booking type:
// the counterparty holds words and is no prefix of the description,
// so under the ordinary precedence it would stand and every e-bill
// would share one signature per spelling of the marker. The
// signature must be the creditor, whichever spelling leads,
// whichever booking type, and whatever the payment order's own lines
// and a PDF page footer put around it, wherever the page break falls
// — before the creditor, after it, or after the last line; and the
// creditor's postal
// address must leave the row a candidate — neither fenced, on the
// signature or on the raw narrative the wider context levels send,
// nor uninformative. Every value is synthetic; the STRUCTURE is the
// bank's. The last group is the boundary: the marker's words inside
// a creditor's name, and the plain payment order whose counterparty
// is the payee, are keyed as before. With the marker case removed
// from Normalize every case in the first group fails and the last
// group still passes, so the test is not vacuous.
func TestNormalizeEbillMarkerIsNotAMerchant(t *testing.T) {
	const tail = "; CH EXAMPLETOWN 9999; QRR; 000000000000000000000000000; 1 times E-Banking domestic"
	const footer = "Form without signature Page 2 / 6; ABCDEF01 / 000000 / XXXXXXXXXXXXXXXX00000 01.01.2000"
	cases := []struct {
		name         string
		counterparty string
		description  string
		want         string
	}{
		{"the German spelling",
			"EBILL-RECHNUNG", "PAYNET ORDER; EBILL-RECHNUNG; EXAMPLE TELECOM AG" + tail,
			"EXAMPLE TELECOM AG CH EXAMPLETOWN"},
		{"the English spelling",
			"EBILL INVOICE", "PAYNET ORDER; EBILL INVOICE; EXAMPLE TELECOM AG" + tail,
			"EXAMPLE TELECOM AG CH EXAMPLETOWN"},
		{"the short spelling",
			"E-BILL", "PAYNET ORDER; E-BILL; EXAMPLE TELECOM AG" + tail,
			"EXAMPLE TELECOM AG CH EXAMPLETOWN"},
		{"hyphen and space are one separator",
			"EBILL RECHNUNG", "PAYNET ORDER; EBILL RECHNUNG; EXAMPLE TELECOM AG" + tail,
			"EXAMPLE TELECOM AG CH EXAMPLETOWN"},
		{"case folds before the match",
			"eBill-Rechnung", "Paynet Order; eBill-Rechnung; Example Telecom AG" + tail,
			"EXAMPLE TELECOM AG CH EXAMPLETOWN"},
		{"a different booking type",
			"E-BILL", "MULTI PAYNET ORDER; E-BILL; EXAMPLE FUEL AG" + tail,
			"EXAMPLE FUEL AG CH EXAMPLETOWN"},
		{"a second execution line",
			"E-BILL", "PAYNET ORDER; E-BILL; EXAMPLE INSURANCE AG; CH EXAMPLETOWN 9999; QRR; 2 times E-Banking domestic",
			"EXAMPLE INSURANCE AG CH EXAMPLETOWN"},
		{"a page footer between the marker and the creditor",
			"EBILL-RECHNUNG", "PAYNET ORDER; EBILL-RECHNUNG; " + footer + "; EXAMPLE ENERGY AG" + tail,
			"EXAMPLE ENERGY AG CH EXAMPLETOWN"},
		{"a page footer between the creditor and its address",
			"EBILL-RECHNUNG", "PAYNET ORDER; EBILL-RECHNUNG; EXAMPLE ENERGY AG; " + footer + tail,
			"EXAMPLE ENERGY AG CH EXAMPLETOWN"},
		{"a page footer after the last line",
			"EBILL-RECHNUNG", "PAYNET ORDER; EBILL-RECHNUNG; EXAMPLE ENERGY AG" + tail + "; " + footer,
			"EXAMPLE ENERGY AG CH EXAMPLETOWN"},
		{"a creditor without a word keeps its slot",
			"E-BILL", "PAYNET ORDER; E-BILL; X&Z AG" + tail,
			"X Z AG CH EXAMPLETOWN"},
		{"the marker with nothing behind it is no merchant",
			"EBILL-RECHNUNG", "PAYNET ORDER; EBILL-RECHNUNG", ""},
		{"the marker alone is no merchant",
			"E-BILL", "", ""},

		// The words without the shape: a creditor is a creditor.
		{"a creditor whose name carries the word",
			"EXAMPLE EBILL SERVICES AG", "PAYNET ORDER; EXAMPLE EBILL SERVICES AG" + tail,
			"EXAMPLE EBILL SERVICES AG"},
		{"the marker followed by a name is a name",
			"E-BILL EXAMPLE AG", "PAYNET ORDER; E-BILL EXAMPLE AG" + tail,
			"E BILL EXAMPLE AG"},
		{"a plain payment order keeps its payee",
			"EXAMPLE PAYEE AG", "E-BANKING PAYMENT ORDER; EXAMPLE PAYEE AG; CH; 1 times E-Banking domestic",
			"EXAMPLE PAYEE AG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := Normalize(tc.counterparty, tc.description)
			if sig != tc.want {
				t.Fatalf("Normalize(%q, %q) = %q, want %q", tc.counterparty, tc.description, sig, tc.want)
			}
			if sig == "" {
				return
			}
			if TransferShaped(sig) {
				t.Errorf("TransferShaped(%q) = true; a postal address is not a transfer", sig)
			}
			if TransferShaped(tc.description) {
				t.Errorf("TransferShaped(%q) = true on the raw narrative; the descriptor would be dropped at the wider context levels", tc.description)
			}
			if Uninformative(sig) {
				t.Errorf("Uninformative(%q) = true; a creditor is a word", sig)
			}
		})
	}
}

// TestNormalizeDropsPhoneNumbers pins the third reduction
// SignatureVersion 4 records: a phone number is never part of a
// merchant. A statement prints one as spaced groups — six digits or
// more in total, at least one group of three or more — each too short
// for the reference-number test, and on the web-export shape of a
// person-to-person mobile payment — the payee promoted as the
// counterparty, the description the payee, the number and the rail's
// reference — the description extends the counterparty and would win,
// so the row keyed on the payee and the number. The run is dropped
// from either field, and a description that is the counterparty
// followed by a number leaves the counterparty standing, so the
// web-export shape keys the payee alone, as the statement era — which
// leads with the booking type and stood already — keys the same lines.
// The third group is a number inside a merchant narrative: it goes,
// the rest stays, and where it directly follows the counterparty the
// counterparty stands whatever comes after it. The last group is the
// boundary: a single digit group between words is a name, a run short
// of minPhoneDigits is not a number and neither is a run over it with
// no group of minPhoneGroupDigits — a date with a two-digit year, a
// day and month with a time behind them once the year is gone, three
// house numbers, and, deliberately, a number written as five pairs —
// a house number in front of a postcode is a lone group once the
// postcode is gone, and a number printed unbroken is a reference
// number, gone before the precedence is read. Every value is
// synthetic; the STRUCTURE is the bank's. The parts of the change are
// pinned apart. With dropPhoneRuns returning its input, every case
// whose number would otherwise reach the key fails: the anchor-less
// web-export case, the web-export case whose counterparty carries the
// number, and the third group, less the e-bill case, whose number is
// a wordless segment the e-bill path drops on its own, and the
// merchant's number in front of its address, which the exception
// covers; the other web-export cases pass on the exception alone,
// since their counterparty carries no number. With rule 4's exception
// removed, every case whose number directly follows the counterparty
// with more behind it fails, on what is behind. With the
// counterparty's runs dropped after the contact line is read instead
// of before, the case whose counterparty carries the number fails.
// With phoneRunEnd's group test removed, the runs of pairs in the
// last group fail: each clears the digit floor. The statement-era
// cases and the rest of the last group pass every way and pin what
// must not move.
func TestNormalizeDropsPhoneNumbers(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		description  string
		want         string
	}{
		// A person-to-person payment, the web-export shape.
		{"web export: the payee, the number, the reference",
			"EXAMPLE, PERSON", "EXAMPLE, PERSON; +41 00 000 00 00; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"web export: no country code",
			"EXAMPLE, PERSON", "EXAMPLE, PERSON; 000 000 00 00; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"web export: a last group of four digits",
			"EXAMPLE, PERSON", "EXAMPLE, PERSON; +41 000 000 0000; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"web export: three groups, the last of four digits",
			"EXAMPLE, PERSON", "EXAMPLE, PERSON; 000 000 0000; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"web export: the number is the whole tail",
			"EXAMPLE, PERSON", "EXAMPLE, PERSON; +41 00 000 00 00",
			"EXAMPLE PERSON"},
		{"web export: the counterparty carries the number too",
			"EXAMPLE PERSON +41 00 000 00 00", "EXAMPLE PERSON +41 00 000 00 00; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"no counterparty to anchor to: the number alone goes",
			"", "EXAMPLE, PERSON; +41 00 000 00 00; TWINT-EXAMPLE",
			"EXAMPLE PERSON TWINT EXAMPLE"},

		// The statement era, as twint_test.go composes it.
		{"statement: a debit to a person",
			"EXAMPLE, PERSON", "DEBIT UBS TWINT; EXAMPLE, PERSON; +41 00 000 00 00; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"statement: a credit from a person",
			"EXAMPLE, PERSON", "CREDIT UBS TWINT; EXAMPLE, PERSON; +41 00 000 00 00; TWINT-EXAMPLE",
			"EXAMPLE PERSON"},
		{"statement: a payment to a merchant",
			"EXAMPLE CHOCOLATIER AG", "PAYMENT UBS TWINT; EXAMPLE CHOCOLATIER AG; EXAMPLE STREET 1; CH EXAMPLETOWN 9999",
			"EXAMPLE CHOCOLATIER AG"},
		{"statement: a merchant payment reversed",
			"EXAMPLE CHOCOLATIER AG", "REVERSAL UBS TWINT; EXAMPLE CHOCOLATIER AG; EXAMPLE STREET 1; CH EXAMPLETOWN 9999",
			"EXAMPLE CHOCOLATIER AG"},

		// A number inside a merchant narrative.
		{"in the middle of the counterparty",
			"EXAMPLE SHOP TEL +41 00 000 00 00 EXAMPLETOWN", "",
			"EXAMPLE SHOP TEL EXAMPLETOWN"},
		{"in the middle, no country code",
			"EXAMPLE SHOP TEL 000 000 00 00 EXAMPLETOWN", "",
			"EXAMPLE SHOP TEL EXAMPLETOWN"},
		{"at the head",
			"+41 00 000 00 00 EXAMPLE SHOP", "", "EXAMPLE SHOP"},
		{"the shortest run that is a number",
			"EXAMPLE SHOP 000 000", "", "EXAMPLE SHOP"},
		{"in the middle of the description",
			"", "EXAMPLE SHOP TEL +41 00 000 00 00 EXAMPLETOWN",
			"EXAMPLE SHOP TEL EXAMPLETOWN"},
		{"a number alone is no signature",
			"", "+41 00 000 00 00", ""},
		{"on the e-bill path",
			"E-BILL", "PAYNET ORDER; E-BILL; EXAMPLE TELECOM AG; +41 00 000 00 00; CH EXAMPLETOWN 9999; QRR; 1 times E-Banking domestic",
			"EXAMPLE TELECOM AG CH EXAMPLETOWN"},
		{"a merchant's number in front of its address: the name stands",
			"EXAMPLE SHOP AG", "EXAMPLE SHOP AG; 000 000 00 00; EXAMPLE STREET 12; 9999 EXAMPLETOWN",
			"EXAMPLE SHOP AG"},
		{"a merchant's number behind its address: the number goes",
			"EXAMPLE SHOP AG", "EXAMPLE SHOP AG EXAMPLE STREET 12 9999 EXAMPLETOWN 000 000 00 00",
			"EXAMPLE SHOP AG EXAMPLE STREET 12 EXAMPLETOWN"},

		// The boundary: a digit group is a name, a short run is not a
		// number and nor is a run of pairs, an unbroken number is a
		// reference number.
		//
		// These narratives run on without segment separators. The
		// separator is a different question, settled before this one
		// — the key stops at the first `;` on a structured narrative
		// (§4) — and a case written with one would be decided there
		// and never reach the run tests these pin.
		{"a house number",
			"EXAMPLE STREET 12", "", "EXAMPLE STREET 12"},
		{"a house number in an address behind the payee",
			"EXAMPLE PAYEE", "EXAMPLE PAYEE EXAMPLE STREET 12 9999 EXAMPLETOWN",
			"EXAMPLE PAYEE EXAMPLE STREET 12 EXAMPLETOWN"},
		{"a house number in front of a postcode",
			"EXAMPLE PAYEE", "EXAMPLE PAYEE EXAMPLE STREET 1 9999 EXAMPLETOWN",
			"EXAMPLE PAYEE EXAMPLE STREET 1 EXAMPLETOWN"},
		{"two house numbers",
			"", "EXAMPLE STREET 12 14 EXAMPLETOWN", "EXAMPLE STREET 12 14 EXAMPLETOWN"},
		{"a month in front of a year",
			"EXAMPLE PAYEE", "EXAMPLE PAYEE CREDIT CARD STATEMENT 03/2026",
			"EXAMPLE PAYEE CREDIT CARD STATEMENT 03"},
		{"a day and month once the year is gone",
			"EXAMPLE SHOP 12.03.2026", "", "EXAMPLE SHOP 12 03"},
		{"a time",
			"EXAMPLE SHOP 12:34", "", "EXAMPLE SHOP 12 34"},
		{"a price",
			"EXAMPLE SHOP CHF 12.50", "", "EXAMPLE SHOP CHF 12 50"},
		{"a date with a two-digit year",
			"EXAMPLE SHOP 03.09.26", "", "EXAMPLE SHOP 03 09 26"},
		{"a date and a time once the year is gone",
			"EXAMPLE SHOP 12.03.2026 12:34", "", "EXAMPLE SHOP 12 03 12 34"},
		{"three house numbers",
			"EXAMPLE STREET 12 14 16", "", "EXAMPLE STREET 12 14 16"},
		{"a number written as five pairs is below the bar",
			"EXAMPLE SHOP 00 00 00 00 00", "", "EXAMPLE SHOP 00 00 00 00 00"},
		{"a numbered chain",
			"Kiosk 24 Seven", "", "KIOSK 24 SEVEN"},
		{"a numbered chain spelt in digits",
			"Kiosk 24/7", "", "KIOSK 24 7"},
		{"the digits lead the name",
			"24/7 Kiosk", "", "24 7 KIOSK"},
		{"the digits are the name",
			"24/7", "", "24 7"},
		{"the longest pair short of a number",
			"EXAMPLE SHOP 000 00", "", "EXAMPLE SHOP 000 00"},
		{"a short pair behind the counterparty is no contact line",
			"UBS SWITZERLAND AG", "UBS SWITZERLAND AG 00 00 C/O UBS CARD CENTER",
			"UBS SWITZERLAND AG 00 00 C O UBS CARD CENTER"},
		{"a date behind the counterparty is no contact line",
			"EXAMPLE SHOP", "EXAMPLE SHOP 03.09.26 EXAMPLETOWN",
			"EXAMPLE SHOP 03 09 26 EXAMPLETOWN"},
		{"a number printed unbroken is a reference number",
			"EXAMPLE, PERSON", "EXAMPLE, PERSON 0000000000 TWINT-EXAMPLE",
			"EXAMPLE PERSON TWINT EXAMPLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.counterparty, tc.description)
			if got != tc.want {
				t.Errorf("Normalize(%q, %q) = %q, want %q", tc.counterparty, tc.description, got, tc.want)
			}
		})
	}
}

// TestTransferShaped pins the fence on the strings it is given. The
// ATM cases are the ones that must NOT fire: those rows are the rule
// tier's, and fencing them would hide a rule-tier regression behind a
// silence that looks deliberate. Every value is invented.
//
// A rail token is what fences a mobile person-to-person payment, and
// it sits in the booking type rather than in the payee — so these
// cases fence nothing for a caller that passes only the reduced
// signature, which no longer carries the rail. The rail has to be read
// where it still exists: the row's provider filing and its raw
// narrative. TestP2PRowCandidacy is the pin for that reading and
// TestCollectMerchantCandidatesFencesTheWholeRow (cmd/wealthdb) for
// candidacy actually using it; this list is not, and extending it
// fences no row on its own.
func TestTransferShaped(t *testing.T) {
	fenced := []string{
		"ONLINE TRANSFER TO SAVINGS",
		"WIRE TRANSFER OUTGOING",
		"ZELLE PAYMENT TO A RECIPIENT",
		"VENMO CASHOUT",
		"CASH APP TRANSFER",
		"AUTOPAY PAYMENT",
		"AUTO PAY SCHEDULED",
		"SEPA UEBERWEISUNG",
		"DIRECT DEBIT COLLECTION",
		"STANDING ORDER MONTHLY",
		"PAYMENT THANK YOU MOBILE",
		// The Swiss mobile rail as the BANK feed books it, in every
		// booking type both eras spell it in and in both cases. That
		// half is fenced whole, shops included: every row of it prints
		// the unmasked mobile of the account that INITIATED the
		// payment, so the personal data is the payer's and is there
		// whatever was bought.
		"PAYMENT UBS TWINT",
		"DEBIT UBS TWINT",
		"CREDIT UBS TWINT",
		"REVERSAL UBS TWINT",
		"Payment UBS TWINT",
		// The same rail as the CARD feed prints it, person-to-person
		// half: the payee is initials and a masked mobile, and either
		// half of that shape fences on its own. Both values invented.
		"TWINT * Sent to A.B.     079***1234   CHE",
		"079***1234",
		// A well-formed IBAN, spaced and unspaced. Both are invented:
		// the check digits are computed so the fence's mod-97 test
		// passes, over an account body no bank issues.
		"CH35 0000 0123 4567 8901 2",
		"DE21123456780000012345",
	}
	for _, s := range fenced {
		if !TransferShaped(s) {
			t.Errorf("TransferShaped(%q) = false, want true (person-bearing narratives must not reach the model tier)", s)
		}
	}

	open := []string{
		"ATM WITHDRAWAL MAIN STREET",
		"ATM CASH WITHDRAWAL",
		"BARGELDBEZUG BAHNHOFPLATZ",
		"CORNER MARKET",
		"WIRELESS SERVICES MONTHLY",
		"BLUE HARBOUR CAFE",
		"",
		// The same Swiss rail's OTHER half, which is most of it: a
		// consumer-to-business payment, printed by the card feed as a
		// trading name and a place. Fencing the rail's name fenced
		// this too, and a shop the model never sees is a shop that
		// gets lumped under a generic category instead of named.
		"TWINT * EXAMPLE SPORTS AG   EXAMPLE CITY   CHE",
		"TWINT * EXAMPLE RESTAURANT  EXAMPLETOWN    CHE",
		// Two letters and two digits at a token start, and no IBAN:
		// a Swiss legal form in front of a postal code, a canton in
		// front of an ESR reference, the word No. in front of an
		// invoice number, and the reference prefixes a bank writes on
		// an ordinary domestic payment. Each of these used to fence a
		// tradesman's bill out of candidacy. Every value is invented.
		"EXAMPLE VERSICHERUNG AG 9999 EXAMPLE 000000123456789012345",
		"EXAMPLE SCHULEN 9999 ZH 25 12345 00500 12345 00012 34567",
		"INVOICE NO. 12345678 EXAMPLE MINISTORAGE AG",
		"EXAMPLE MOTORS GMBH CH RN123456789",
		"EXAMPLE MINISTORAGE AG TN:012345678",
	}
	for _, s := range open {
		if TransferShaped(s) {
			t.Errorf("TransferShaped(%q) = true, want false", s)
		}
	}
}

// TestP2PRowCandidacy pins the whole-row fence on the shape the key
// alone cannot refuse: a mobile person-to-person payment, whose rail
// leads the narrative and whose payee wins the key. Every value is
// invented — the payee, the shop, the street and the town are
// placeholders that carry the SHAPE only.
//
// The key-only assertion is what keeps the rest from being vacuous:
// all three key-only predicates PASS a private individual's name, so
// candidacy decided on the key admits it. RowTransferShaped is what
// refuses the row. The bank-feed merchant case pins the cost where the
// line cannot tell a person from a shop — that merchant is refused too
// and stays placeable by a config rule or a pin — while the card-feed
// pair pins the split where the line CAN; the bill and memo cases pin
// the boundary the reading must not cross.
func TestP2PRowCandidacy(t *testing.T) {
	const (
		payee           = "EXAMPLE, PERSON"
		personFiling    = "DEBIT UBS TWINT"
		personNarrative = "DEBIT UBS TWINT; EXAMPLE, PERSON; TWINT-EXAMPLE"
		shop            = "EXAMPLE CHOCOLATIER AG"
		shopFiling      = "PAYMENT UBS TWINT"
		shopNarrative   = "PAYMENT UBS TWINT; EXAMPLE CHOCOLATIER AG; EXAMPLE STREET 1; CH EXAMPLETOWN 9999"
	)

	sig := Normalize(payee, personNarrative)
	if sig != "EXAMPLE PERSON" {
		t.Fatalf("Normalize(%q, %q) = %q, want %q", payee, personNarrative, sig, "EXAMPLE PERSON")
	}
	if TransferShaped(sig) || Uninformative(sig) || FilingOnly(sig, personFiling) {
		t.Fatalf("a key-only predicate refuses %q; the whole-row reading would be vacuous", sig)
	}
	if !RowTransferShaped(sig, personFiling, personNarrative) {
		t.Errorf("RowTransferShaped(%q, %q, %q) = false; a payee's name is not a merchant and must never be sent",
			sig, personFiling, personNarrative)
	}

	shopSig := Normalize(shop, shopNarrative)
	if !RowTransferShaped(shopSig, shopFiling, shopNarrative) {
		t.Errorf("RowTransferShaped(%q, %q, %q) = false; the BANK feed's booking type fences every row it books",
			shopSig, shopFiling, shopNarrative)
	}

	// The same rail's card feed, where the line DOES tell the two
	// apart, so the fence reads the shape instead of the rail's name:
	// the person stays out and the shop becomes a candidate. Without
	// this split the rail's own name fenced both, and in Switzerland
	// the shops are the larger half by far.
	const (
		cardFiling    = "TWINT"
		cardPerson    = "TWINT * Sent to A.B.     079***1234   CHE"
		cardShop      = "TWINT * EXAMPLE SPORTS AG   EXAMPLE CITY   CHE"
	)
	personCardSig := Normalize("", cardPerson)
	if !RowTransferShaped(personCardSig, cardFiling, cardPerson) {
		t.Errorf("RowTransferShaped(%q, %q, %q) = false; a masked mobile is the payee's contact details",
			personCardSig, cardFiling, cardPerson)
	}
	shopCardSig := Normalize("", cardShop)
	if RowTransferShaped(shopCardSig, cardFiling, cardShop) {
		t.Errorf("RowTransferShaped(%q, %q, %q) = true; a trading name and a place is a shop, and only the model can name it",
			shopCardSig, cardFiling, cardShop)
	}

	// A wire-paid bill: an address, a creditor, no rail. It is the
	// population only the model can place, and the reading must leave
	// it a candidate.
	const (
		billFiling    = "payment order"
		billNarrative = "Z44?GEMEINDE EXAMPLE; HAUPTSTRASSE 12; CH EXAMPLE 9999; ORDENTLICHE STEUERN 2026"
	)
	billSig := Normalize("", billNarrative)
	if RowTransferShaped(billSig, billFiling, billNarrative) {
		t.Errorf("RowTransferShaped(%q, %q, %q) = true; a postal address is not a rail",
			billSig, billFiling, billNarrative)
	}

	// The memo is the payer's own words and is not read: a note that
	// merely mentions a rail must not fence the merchant it sits
	// beside.
	memoNarrative := canonical.JoinDescriptionMemo("EXAMPLE CAFE", "settling the wire transfer we split")
	memoSig := Normalize("", memoNarrative)
	if memoSig != "EXAMPLE CAFE" {
		t.Fatalf("Normalize(%q, %q) = %q, want %q", "", memoNarrative, memoSig, "EXAMPLE CAFE")
	}
	if RowTransferShaped(memoSig, "", memoNarrative) {
		t.Errorf("RowTransferShaped(%q, %q, %q) = true; the memo is not read", memoSig, "", memoNarrative)
	}
}

// TestMT940PaymentNarrativeCandidacy pins candidacy for the narrative
// a Swiss MT940 :86: feed writes for a payment order: a transaction
// type code, a question mark, then the creditor and a postal address,
// sometimes ending in the QR-reference marker. The adapter carries no
// counterparty for these rows, so the signature is built from the
// description alone. Every value is synthetic; the STRUCTURE is the
// bank's.
//
// A row with a creditor word in it must be a candidate: not fenced,
// not uninformative, on the signature AND on the raw narrative the
// wider context levels send. A row that is the code alone is
// correctly refused by Uninformative, and by nothing else.
//
// The signature is the creditor and nothing else — the type code and
// the address are the bank's filing and are trimmed off it (§4) — so
// the two bills that differ only in a house number key alike, which
// is the point of the trim. That also closes on the signature side an
// old gap in the unanchored IBAN match: a two-digit house number in
// front of the country code used to read as an account number on the
// key itself (`...SE12CHEXAMPLE...`), and no address reaches the key
// now. The raw narrative still carries every one of those shapes, and
// the fence reads it whole — a postal code in front of more text is
// two letters and two digits at a token start there
// (`...LE9999ORDENTLICHE...`), and it is the registry length and the
// mod-97 check that refuse it. The last group is the boundary the
// narrowing must not cross: the same address shape carrying a real
// IBAN, spaced or not, stays fenced.
func TestMT940PaymentNarrativeCandidacy(t *testing.T) {
	cases := []struct {
		name          string
		narrative     string
		wantSig       string
		uninformative bool
	}{
		{"a tax bill paid to a municipality",
			"Z44?GEMEINDE EXAMPLE; HAUPTSTRASSE 1; CH EXAMPLE 9999; ORDENTLICHE STEUERN 2026",
			"GEMEINDE EXAMPLE", false},
		{"the same bill at a two-digit house number",
			"Z44?GEMEINDE EXAMPLE; HAUPTSTRASSE 12; CH EXAMPLE 9999; ORDENTLICHE STEUERN 2026",
			"GEMEINDE EXAMPLE", false},
		{"a dentist's invoice",
			"Z44?ZAHNARZTPRAXIS EXAMPLE AG; MUSTERSTRASSE 7; CH EXAMPLE 9999",
			"ZAHNARZTPRAXIS EXAMPLE AG", false},
		{"a utility paid by QR-bill",
			"Z59?EXAMPLE WERKE AG; INDUSTRIESTRASSE 3; CH EXAMPLE 9999; QRR",
			"EXAMPLE WERKE AG", false},
		{"a code and nothing else", "N21?", "N21", true},
		{"another code and nothing else", "D37?", "D37", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := Normalize("", tc.narrative)
			if sig != tc.wantSig {
				t.Errorf("Normalize(%q) = %q, want %q", tc.narrative, sig, tc.wantSig)
			}
			if TransferShaped(sig) {
				t.Errorf("TransferShaped(%q) = true; a postal address is not a transfer, and the fence is what would refuse candidacy", sig)
			}
			if TransferShaped(tc.narrative) {
				t.Errorf("TransferShaped(%q) = true on the raw narrative; the descriptor would be dropped at the wider context levels", tc.narrative)
			}
			if got := Uninformative(sig); got != tc.uninformative {
				t.Errorf("Uninformative(%q) = %v, want %v", sig, got, tc.uninformative)
			}
		})
	}

	// Both IBANs are invented, and both are well-formed: the check
	// digits are computed over an account body no bank issues.
	for _, narrative := range []string{
		"Z44?EXAMPLE PERSON; HAUPTSTRASSE 12; CH EXAMPLE 9999; CH35 0000 0123 4567 8901 2",
		"Z44?EXAMPLE PERSON; HAUPTSTRASSE 12; CH EXAMPLE 9999; DE21123456780000012345",
	} {
		if !TransferShaped(narrative) {
			t.Errorf("TransferShaped(%q) = false; an IBAN behind a postal address is still an IBAN", narrative)
		}
	}
}

// TestUninformative pins the wordless-signature refusal at candidacy.
// Every value is synthetic; only the SHAPES are real — they are what
// an MT940 :86: narrative reduces to when the bank wrote nothing but
// its own tag.
// The last two open cases are the boundary the predicate must not
// cross: it knows nothing about which words are merchants, so a
// three-letter abbreviation and an all-letter code both pass, and the
// gauntlet's echo check is what catches the code downstream.
func TestUninformative(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"bare four-character code", "ZV01", true},
		{"two-digit number", "42", true},
		{"two-letter code", "KH", true},
		{"code plus reference number", "ZV01 7788", true},
		{"digits split by separators", "12-345", true},
		{"nothing at all", "", true},
		{"a real merchant", "BLUE HARBOUR CAFE", false},
		{"a merchant with digits in it", "KIOSK 24 SEVEN", false},
		{"a non-ASCII merchant name", "Café Grünhafen", false},
		{"a three-letter abbreviation is a word", "NWH 12", false},
		{"an all-letter code cannot be told from a word", "NTRF", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Uninformative(tc.in); got != tc.want {
				t.Errorf("Uninformative(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// A statement narrative is written in segments — the payee, then the
// street, then the country and town, then what the payment was for —
// and an MT940 :86: narrative leads with the structured-field tag the
// payee was written into. Neither the tag nor the address is the
// merchant: left in the key, one merchant gets a separate signature
// per address spelling, and a model reads the town as part of the name.
func TestNormalizeDropsTheAddressBehindThePayee(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		description  string
		want         string
	}{
		{"the address behind the payee is not part of the name",
			"Blue Harbour Cafe", "Blue Harbour Cafe;CH 8000 Zurich",
			"BLUE HARBOUR CAFE"},
		{"the same payee at a differently-spelled address folds together",
			"Blue Harbour Cafe", "Blue Harbour Cafe;CH Zurich 8000",
			"BLUE HARBOUR CAFE"},
		{"a structured narrative reduces to its payee segment",
			"", "Z44?Blue Harbour Cafe;Hafenstrasse 1;CH 8000 Zurich;INVOICE 4471",
			"BLUE HARBOUR CAFE"},
		{"the field tag varies across bookings and is never the merchant",
			"", "Z59?Blue Harbour Cafe;Hafenstrasse 1;CH 8000 Zurich",
			"BLUE HARBOUR CAFE"},
		{"a description richer than the counterparty still wins, less its address",
			"Blue Harbour", "Blue Harbour Cafe;CH 8000 Zurich",
			"BLUE HARBOUR CAFE"},
		// The tag survives as the key when it is all there was, and
		// carries no word, so candidacy refuses it like any other
		// wordless narrative.
		{"a narrative that is nothing but a field tag names no merchant",
			"", "Z21?", "Z21"},
		// The guard on the rule above. Only a tagged narrative is
		// known to lead with the payee; the export feed composes its
		// description the other way round, and trimming that to the
		// first segment would file every direct debit under one key.
		{"an untagged narrative keeps the merchant behind the booking type",
			"", "Direct debit;Blue Harbour Cafe;bill of 03.2026",
			"DIRECT DEBIT BLUE HARBOUR CAFE BILL OF 03"},
		{"an untagged narrative is not trimmed even when it leads with a payee",
			"", "Blue Harbour Cafe;CH 8000 Zurich",
			"BLUE HARBOUR CAFE CH ZURICH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.counterparty, tc.description); got != tc.want {
				t.Errorf("Normalize(%q, %q) = %q, want %q",
					tc.counterparty, tc.description, got, tc.want)
			}
		})
	}
}
