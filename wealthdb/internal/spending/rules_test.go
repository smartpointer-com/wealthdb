package spending

import (
	"regexp"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestRuleCategory pins the built-in tier pattern by pattern, on a
// narrative written around each one rather than on the pattern itself:
// a narrative is what fails when a pattern is misspelled or dropped,
// which is the regression the tier is exposed to — a whole class of
// card bills or ATM withdrawals falling into the backlog, or a
// mortgage payment re-admitted to the spending base. Every narrative
// is invented; only the SHAPES are drawn from what card and deposit
// exports look like.
func TestRuleCategory(t *testing.T) {
	cases := []struct {
		signature string
		detailed  string
	}{
		// Card payments with no counter-leg in gold: a card wealthdb
		// does not itemise, so the bill is generic card spend, kept in
		// the base. (A bill whose card IS in gold never reaches this
		// rule's verdict — the matcher outranks it; enrich_test pins
		// that.)
		{"AUTOPAY 7788 PAYMENT", canonical.SpendDetailedCardSpend},
		{"AUTO PAY SCHEDULED", canonical.SpendDetailedCardSpend},
		{"PAYMENT THANK YOU MOBILE", canonical.SpendDetailedCardSpend},
		{"ONLINE PAYMENT TO CARD", canonical.SpendDetailedCardSpend},
		{"CARDMEMBER SERVICES EPAY", canonical.SpendDetailedCardSpend},
		{"CREDIT CRD EPAY", canonical.SpendDetailedCardSpend},
		{"AUTOPMT SCHEDULED WEB", canonical.SpendDetailedCardSpend},
		{"AUTOMATIC PAYMENT RECEIVED", canonical.SpendDetailedCardSpend},
		{"ELECTRONIC PAYMENT SCHEDULED", canonical.SpendDetailedCardSpend},
		{"PAYMENT TO CARD ACCOUNT", canonical.SpendDetailedCardSpend},
		{"CARD PMT WEB", canonical.SpendDetailedCardSpend},
		{"CC PAYMENT MOBILE", canonical.SpendDetailedCardSpend},
		// The direct-debit shape with the mandate notice LEFT IN, fed
		// straight to the rule: it never anchors to the head of the
		// signature, so it matches whether or not Normalize stripped
		// the boilerplate.
		{"DIRECT DEBIT CRD1W OBJECTION TO UBS WITHIN 30 DAYS UBS CARD CENTER CREDIT CARD STATEMENT",
			canonical.SpendDetailedCardSpend},

		// ATM: unattributable by construction, and its own primary
		// rather than a plausible guess.
		{"ATM WITHDRAWAL MAIN STREET", canonical.SpendDetailedCashWithdrawal},
		{"CASH WITHDRAWAL BRANCH", canonical.SpendDetailedCashWithdrawal},
		{"BARGELDBEZUG BAHNHOFPLATZ", canonical.SpendDetailedCashWithdrawal},
		{"CASH ADVANCE FEE FREE", canonical.SpendDetailedCashWithdrawal},
		{"GELDAUTOMAT EXAMPLEPLATZ", canonical.SpendDetailedCashWithdrawal},
		{"CASHPOINT EXAMPLE HIGH STREET", canonical.SpendDetailedCashWithdrawal},
		{"WITHDRAWAL AT COUNTER", canonical.SpendDetailedCashWithdrawal},
		{"CASH DISBURSEMENT AT COUNTER", canonical.SpendDetailedCashWithdrawal},

		// Mortgage: an own-account move because the mortgage is itself
		// a tracked account.
		{"MORTGAGE PAYMENT", canonical.SpendDetailedInternalTransfer},
		{"HYPOTHEK ZINS", canonical.SpendDetailedInternalTransfer},
		{"HYPOTHEKARZINS QUARTAL", canonical.SpendDetailedInternalTransfer},
		{"HOME LOAN SERVICING", canonical.SpendDetailedInternalTransfer},
	}
	for _, tc := range cases {
		t.Run(tc.signature, func(t *testing.T) {
			detailed, _, ok := RuleCategory(tc.signature, "", "", "")
			if !ok {
				t.Fatalf("RuleCategory(%q) did not fire, want %q", tc.signature, tc.detailed)
			}
			if detailed != tc.detailed {
				t.Errorf("RuleCategory(%q) = %q, want %q", tc.signature, detailed, tc.detailed)
			}
		})
	}
}

// TestRuleCategoryCardIssuers pins the issuer table against the real
// export formats it was built from, digits masked, and takes each one
// THROUGH Normalize first: the rule runs on the signature, so the
// punctuation (`AG;C/O`), the trailing card or reference numbers and
// the leading direct-debit notice all have to survive — or fall away —
// the way they do on a real row. Every descriptor in the table must
// also fire on its own, so a phrase added to the table without a
// format behind it is caught here.
//
// The label is pinned with the verdict, because a table entry is now
// two claims: that the format IS a card bill, and whose card it is.
// The bare masked-card descriptor is the entry that names no issuer —
// it says a card was topped up and nothing about who issued it — so a
// bill it places is labelled with nothing.
func TestRuleCategoryCardIssuers(t *testing.T) {
	for _, tc := range []struct{ raw, label string }{
		{"PAYMENT TO CHASE CARD ENDING IN ####", "Chase"},
		{"CHASE CREDIT CRD AUTOPAY", "Chase"},
		{"AMERICAN EXPRESS ACH PMT    M### / A###", "American Express"},
		{"AMERICAN EXPRESS CREDIT CARD", "American Express"},
		{"CITI CARD ONLINE PAYMENT    ####", "Citi"},
		{"CITI AUTOPAY     PAYMENT    ####", "Citi"},
		{"CITI CREDIT CARD PAYMENT", "Citi"},
		{"UBS SWITZERLAND AG;C/O UBS CARD CENTER", "UBS Card Center"},
		{"UBS AG;C/O UBS CARD CENTER AG", "UBS Card Center"},
		// The Swiss direct-debit shape: mandate notice first, then the
		// creditor. Normalize strips the notice (SignatureVersion 2);
		// TestRuleCategory covers the un-stripped form.
		{"DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; UBS CARD CENTER; CREDIT CARD STATEMENT ##/####",
			"UBS Card Center"},
		// The web adapter carries DIRECT DEBIT as a booking type, so
		// the free text can begin at the notice.
		{"CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; UBS CARD CENTER; CARD PAYMENT",
			"UBS Card Center"},
		// A masked card number as the whole counterparty: a top-up,
		// and no issuer named anywhere in it.
		{"XXXX XXXX XXXX ####", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			sig := Normalize(tc.raw, "")
			detailed, label, ok := RuleCategory(sig, "", "", "")
			if !ok {
				t.Fatalf("RuleCategory(%q) [from %q] did not fire", sig, tc.raw)
			}
			if detailed != canonical.SpendDetailedCardSpend {
				t.Errorf("RuleCategory(%q) = %q, want %q", sig, detailed, canonical.SpendDetailedCardSpend)
			}
			if label != tc.label {
				t.Errorf("RuleCategory(%q) label = %q, want %q", sig, label, tc.label)
			}
		})
	}
	for _, iss := range cardIssuers {
		for _, d := range iss.phrases {
			if detailed, label, ok := RuleCategory(d, "", "", ""); !ok ||
				detailed != canonical.SpendDetailedCardSpend || label != iss.name {
				t.Errorf("descriptor %q alone = (%q, %q, %v), want the card rule labelled %q",
					d, detailed, label, ok, iss.name)
			}
			// The same descriptor named only in the description, behind
			// a signature and a counterparty that are nothing but the
			// mandate code: the rule reads the narrative, not just the
			// key, and it labels from whichever field carried the
			// issuer.
			if detailed, label, ok := RuleCategory("CRD1W", "CRD1W OBJECTION TO UBS", d, ""); !ok ||
				detailed != canonical.SpendDetailedCardSpend || label != iss.name {
				t.Errorf("descriptor %q in the description alone = (%q, %q, %v), want the card rule labelled %q",
					d, detailed, label, ok, iss.name)
			}
		}
	}
}

// TestRuleLabelIsTheCardRuleAlone pins the label's boundary: it is the
// issuer a card bill was paid to, and nothing else in the tier
// produces one. A bill recognised by a generic card-payment phrase or
// by a masked-card shape is a bill whose issuer the narrative does not
// name; an ATM withdrawal and a mortgage payment are not bills at all.
// A label leaking onto any of those would put a name in the merchant
// column of a line that has no merchant.
func TestRuleLabelIsTheCardRuleAlone(t *testing.T) {
	for _, sig := range []string{
		// Card bills whose narrative names no issuer.
		"AUTOPAY SCHEDULED PAYMENT", "PAYMENT THANK YOU MOBILE",
		"CARD PMT WEB", "1234XXXXXXXX5678 03.09.26",
		// The other two built-ins, which have no issuer table at all.
		"ATM WITHDRAWAL MAIN STREET", "BARGELDBEZUG BAHNHOFPLATZ",
		"MORTGAGE PAYMENT", "HYPOTHEKARZINS QUARTAL",
	} {
		detailed, label, ok := RuleCategory(sig, "", "", "")
		if !ok {
			t.Fatalf("RuleCategory(%q) did not fire", sig)
		}
		if label != "" {
			t.Errorf("RuleCategory(%q) = %q labelled %q, want no label", sig, detailed, label)
		}
	}
	// A row nothing places carries no label either.
	if _, label, ok := RuleCategory("CORNER MARKET", "", "", ""); ok || label != "" {
		t.Errorf("RuleCategory on a merchant = (%q, %v), want no match and no label", label, ok)
	}
}

// TestBuiltinRulePatternsAreReachable walks every token and phrase in
// builtinRules and asserts each one, on its own, places its own rule's
// category. That is a narrow claim, and it is worth being clear about
// what it does and does not buy: it proves a pattern is spelled in a
// form tokenize can actually produce — no punctuation, no lower case,
// no double space — and that no earlier rule shadows it, so a pattern
// that could never fire is caught the day it is added. It cannot see a
// typo or a deletion, because the pattern is both the input and the
// expectation; TestRuleCategory's narratives are what catch those.
//
// Shapes are left out: a matching string cannot be generated from a
// compiled regexp, and TestRuleCategoryMaskedCard pins the one shape
// against the formats it was built from.
func TestBuiltinRulePatternsAreReachable(t *testing.T) {
	for i, r := range builtinRules {
		for _, pattern := range append(append([]string(nil), r.tokens...), r.phrases...) {
			detailed, _, ok := RuleCategory(pattern, "", "", "")
			if !ok {
				t.Errorf("rule %d: pattern %q fires nothing; tokenize cannot produce it as written", i, pattern)
				continue
			}
			if detailed != r.detailed {
				t.Errorf("rule %d: pattern %q = %q, want %q; an earlier rule shadows it",
					i, pattern, detailed, r.detailed)
			}
		}
	}
}

// TestRuleCategoryReadsNarrative pins the built-in tier's three
// inputs. The signature is where the merchant store looks, but it is
// not always where the creditor is: on a Swiss direct debit the
// adapter's counterparty is the mandate notice and the signature it
// yields is the bare code, and on a PDF-era transfer the description
// leads with the booking type so the counterparty is not a truncation
// of it — in both the creditor is in the description and nowhere
// else. Each field is folded and tested on its own, whole; rule order
// outranks field order; nothing fires on no text. With RuleCategory
// reading the signature alone, every positive case here but the
// first fails. Every value is synthetic.
func TestRuleCategoryReadsNarrative(t *testing.T) {
	cases := []struct {
		name                                 string
		signature, counterparty, description string
		detailed                             string
	}{
		{"the signature alone still fires",
			"UBS CARD CENTER CREDIT CARD STATEMENT", "", "", canonical.SpendDetailedCardSpend},
		{"a direct debit whose signature is the bare mandate code",
			"CRD1W", "CRD1W OBJECTION TO UBS",
			"CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; UBS CARD CENTER; CREDIT CARD STATEMENT 03/2026",
			canonical.SpendDetailedCardSpend},
		{"a PDF-era transfer whose description leads with the booking type",
			"UBS SWITZERLAND AG", "UBS SWITZERLAND AG",
			"E-BANKING PAYMENT ORDER; UBS SWITZERLAND AG; C/O UBS CARD CENTER",
			canonical.SpendDetailedCardSpend},
		{"an issuer in the counterparty with no signature at all",
			"", "UBS AG;C/O UBS CARD CENTER AG", "", canonical.SpendDetailedCardSpend},
		{"a direct debit whose creditor line lost the card centre",
			"EXAMPLE BANK AG EXAMPLE CREDIT CARD STATEMENT OF", "VIS1W OBJECTION TO UBS",
			"DIRECT DEBIT; VIS1W OBJECTION TO UBS; WITHIN 30 DAYS; EXAMPLE BANK AG; #### EXAMPLE; CREDIT CARD STATEMENT; OF ##.##.####",
			canonical.SpendDetailedCardSpend},
		{"a masked card number in the description",
			"TOP UP", "", "Top up 1234XXXXXXXX5678 03.09.26", canonical.SpendDetailedCardSpend},
		{"an ATM in the description behind a place-name signature",
			"MAIN STREET BRANCH", "", "ATM withdrawal Main Street branch", canonical.SpendDetailedCashWithdrawal},
		{"a mortgage in the description behind the bank's name",
			"EXAMPLE BANK AG", "Example Bank AG", "Hypothek Zins 1. Quartal", canonical.SpendDetailedInternalTransfer},
		{"rule order outranks field order",
			"MORTGAGE PAYMENT", "", "CITI CREDIT CARD PAYMENT", canonical.SpendDetailedCardSpend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detailed, _, ok := RuleCategory(tc.signature, tc.counterparty, tc.description, "")
			if !ok {
				t.Fatalf("RuleCategory(%q, %q, %q) did not fire, want %q",
					tc.signature, tc.counterparty, tc.description, tc.detailed)
			}
			if detailed != tc.detailed {
				t.Errorf("RuleCategory(%q, %q, %q) = %q, want %q",
					tc.signature, tc.counterparty, tc.description, detailed, tc.detailed)
			}
		})
	}

	// The negatives: no text anywhere, and a phrase split across two
	// fields — CREDIT at the end of one, CRD at the start of the next
	// — which would spell CREDIT CRD only if the fields were joined.
	for _, tc := range []struct{ signature, counterparty, description string }{
		{"", "", ""},
		{"", "EXAMPLE CREDIT", "CRD SERVICES"},
		{"EXAMPLE CREDIT", "", "CRD SERVICES"},
	} {
		if detailed, _, ok := RuleCategory(tc.signature, tc.counterparty, tc.description, ""); ok {
			t.Errorf("RuleCategory(%q, %q, %q) placed %q, want no match",
				tc.signature, tc.counterparty, tc.description, detailed)
		}
	}
}

// TestRuleCategoryMaskedCard pins the two masked-card-number
// conventions the card rule reads as an issuer: the spaced
// `XXXX XXXX XXXX ####` phrase, and the one-token `####XXXXXXXX####`
// shape — first and last four digits kept, the middle eight masked —
// with and without the date fragment that follows it on real rows,
// in either case of X. Every case goes to the rule both raw and
// through Normalize: the one-token form is a mixed token that has to
// SURVIVE the reference-number strip to be seen at all. The plain
// 16-digit number and the wrong-width mask are the boundary — not the
// shape, and never a card. Digits are synthetic.
func TestRuleCategoryMaskedCard(t *testing.T) {
	cases := []struct {
		raw   string
		fires bool
	}{
		{"XXXX XXXX XXXX 5678", true},
		{"1234XXXXXXXX5678", true},
		{"1234XXXXXXXX5678 03.09.26", true},
		{"1234xxxxxxxx5678 03.09.26", true},
		{"1234123412345678", false},
		{"1234XXXXXXX5678", false}, // seven X's: not the shape
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			for _, sig := range []string{tc.raw, Normalize(tc.raw, "")} {
				detailed, _, ok := RuleCategory(sig, "", "", "")
				if ok != tc.fires {
					t.Fatalf("RuleCategory(%q) fired=%v (→ %q), want fired=%v",
						sig, ok, detailed, tc.fires)
				}
				if ok && detailed != canonical.SpendDetailedCardSpend {
					t.Errorf("RuleCategory(%q) = %q, want %q",
						sig, detailed, canonical.SpendDetailedCardSpend)
				}
			}
		})
	}
}

// TestRuleCategoryLeavesMerchantsAlone is the other half of the
// contract: the rule tier is three structural cases, not a merchant
// classifier. Anything it fires on that is really a merchant is a row
// permanently mis-categorised with no model tier to correct it, so the
// patterns must match on whole tokens rather than substrings — for
// phrases as much as for tokens.
//
// The store-card cases are the card rule's own boundary: a fuel card
// or a retailer's card names its merchant, and its bill is that
// merchant's spend, not generic card spend. The issuer table holds
// issuers only, and an issuer's bare name is not a bill either.
//
// Every string is tried in each of the three positions the rule tier
// reads — as the signature, as the counterparty, as the description —
// with the other two blank, and once more as the description behind a
// direct debit's bare mandate code: the narrative fields are read
// with the same whole-token matching as the signature and must
// respect the same boundary.
func TestRuleCategoryLeavesMerchantsAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"CORNER MARKET",
		"BLUE HARBOUR CAFE",
		"ATMOSPHERE CLIMBING GYM", // not the ATM token
		"MORTGAGEHUB ONLINE",      // not the MORTGAGE token
		"EPAYMENTS UNLIMITED",     // not the EPAY token
		"SCORECARD PAYMENTS LTD",  // not the CARD PAYMENT phrase: whole words only
		"NORTHSIDE PHARMACY",
		// Store cards that name their merchant: left to the merchant
		// tiers, never re-filed as generic card spend.
		"NORTHWIND PETROL FUEL CARD INVOICE",
		"EXAMPLE DEPARTMENT STORE CARD ACCOUNT",
		"DIRECT DEBIT CRD1W OBJECTION TO UBS WITHIN 30 DAYS NORTHWIND PETROL AG FUEL CARD INVOICE 07",
		// An issuer's name where a merchant would be is a merchant.
		"AMERICAN EXPRESS TRAVEL DESK",
		"CHASE BANK BRANCH FEE",
		"CITI PLAZA PARKING",
		"UBS SWITZERLAND AG SAFE DEPOSIT",
	} {
		for _, in := range [][3]string{
			{s, "", ""},
			{"", s, ""},
			{"", "", s},
			{"CRD1W", "CRD1W OBJECTION TO UBS", "WITHIN 30 DAYS; " + s},
		} {
			if detailed, _, ok := RuleCategory(in[0], in[1], in[2], ""); ok {
				t.Errorf("RuleCategory(%q, %q, %q) placed %q, want no match",
					in[0], in[1], in[2], detailed)
			}
		}
	}
}

// TestRuleCategoriesAreInTheTaxonomy stops a rule from assigning a
// value gold's spend_categories dimension has never heard of, which
// would resolve to a NULL primary and silently vanish from every
// grouped report.
func TestRuleCategoriesAreInTheTaxonomy(t *testing.T) {
	for i, r := range builtinRules {
		if !canonical.ValidSpendDetailed(r.detailed) {
			t.Errorf("built-in rule %d assigns %q, which is not a recognised spend_detailed value",
				i, r.detailed)
		}
	}
}

func TestConfigRuleScopeNarrowsWhereARuleMayFire(t *testing.T) {
	// The point of a scope: a pattern right for one booking on one
	// account in one month must not sweep up a later row that merely
	// reads the same.
	const day = 1734307200 // 2024-12-16T00:00:00Z
	scoped := []Rule{{
		Match:    regexp.MustCompile(`(?i)^\s*closing\s*$`),
		Category: "internal_transfer",
		Scope: RuleScope{
			Source: "ubs", Account: "ACCT-1",
			From: day - 15*86400, To: day + 15*86400,
		},
	}}
	row := func(src, acct string, at int64) RuleRow {
		return RuleRow{Counterparty: "Closing", Source: src, Account: acct, OccurredAt: at}
	}
	for _, tc := range []struct {
		name string
		row  RuleRow
		want bool
	}{
		{"inside every dimension", row("ubs", "ACCT-1", day), true},
		{"another source", row("chase", "ACCT-1", day), false},
		{"another account", row("ubs", "ACCT-2", day), false},
		{"before the range", row("ubs", "ACCT-1", day-30*86400), false},
		{"after the range", row("ubs", "ACCT-1", day+30*86400), false},
		{"on the first admitted second", row("ubs", "ACCT-1", day-15*86400), true},
		{"on the last admitted second", row("ubs", "ACCT-1", day+15*86400), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := ConfigRuleCategory(scoped, tc.row)
			if ok != tc.want {
				t.Errorf("fired = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestAnUnscopedRuleStillFiresEverywhere(t *testing.T) {
	// Every rule written before scopes existed carries the zero scope,
	// and must keep matching exactly as it did.
	rules := []Rule{{Match: regexp.MustCompile(`(?i)acme`), Category: "gift"}}
	got, ok := ConfigRuleCategory(rules, RuleRow{
		Counterparty: "ACME LTD", Source: "anywhere", Account: "any", OccurredAt: 1})
	if !ok || got.Category != "gift" {
		t.Errorf("got (%q, %v), want (gift, true)", got.Category, ok)
	}
}

// TestConfigRuleCategory pins the helper's contract: either narrative
// field fires a rule on its own, the match is case-insensitive (the
// loader compiles with (?i)), the first rule written wins, and no
// rules means nothing fires. Names and entities are invented.
func TestConfigRuleCategory(t *testing.T) {
	rules := []Rule{
		{Match: regexp.MustCompile(`(?i)SAMPLE HOLDER`), Category: canonical.SpendDetailedInternalTransfer},
		{Match: regexp.MustCompile(`(?i)EXAMPLE EXCHANGE LTD`), Category: canonical.SpendDetailedInternalTransfer},
		{Match: regexp.MustCompile(`(?i)EXAMPLE VENTURES FUND`), Category: canonical.SpendDetailedInvestment},
	}
	cases := []struct {
		counterparty, description string
		want                      string
		ok                        bool
	}{
		{"", "Wire transfer to Sample Holder", canonical.SpendDetailedInternalTransfer, true},
		{"Example Exchange Ltd", "", canonical.SpendDetailedInternalTransfer, true},
		{"SAMPLE HOLDER", "Corner Market", canonical.SpendDetailedInternalTransfer, true},
		{"", "Subscription Example Ventures Fund II", canonical.SpendDetailedInvestment, true},
		// Both fire: the first rule written wins.
		{"Example Ventures Fund", "Sample Holder", canonical.SpendDetailedInternalTransfer, true},
		// The description is read whole, memo included: what the payer
		// wrote a payment was for is the holder's own input.
		{"", canonical.JoinDescriptionMemo("Wire transfer", "for Sample Holder"), canonical.SpendDetailedInternalTransfer, true},
		{"Corner Market", "Blue Harbour Cafe", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		got, ok := ConfigRuleCategory(rules, RuleRow{
			Counterparty: tc.counterparty, Description: tc.description})
		if got.Category != tc.want || ok != tc.ok {
			t.Errorf("ConfigRuleCategory(%q, %q) = (%q, %v), want (%q, %v)",
				tc.counterparty, tc.description, got.Category, ok, tc.want, tc.ok)
		}
	}
	if _, ok := ConfigRuleCategory(nil, RuleRow{
		Counterparty: "Sample Holder", Description: "Sample Holder"}); ok {
		t.Error("no rules must fire on nothing")
	}
}

// TestCardRuleRefusesACashWithdrawal pins the card rule's one
// refusal. Cash taken at a machine is booked against the card that
// opened the drawer, so the counterparty is the masked card number the
// card rule reads as a bill, and the card rule leads the table — which
// would file cash out of an ATM as spend on a card, inflating the
// card_spend placeholder and emptying the cash_withdrawal line. Where
// the booking type names the machine the card rule stands down.
//
// The refusal reads the provider's own filing as well as the narrative
// fields, because the bank may name the machine only there — the
// narrative on such a row can be the masked number and nothing else.
// A masked number with no machine anywhere is still a card bill: that
// is the pairing this test turns on. Every value is synthetic.
func TestCardRuleRefusesACashWithdrawal(t *testing.T) {
	const maskedPAN = "0000XXXXXXXX0000"
	cases := []struct {
		name                                                   string
		signature, counterparty, description, providerCategory string
		detailed                                               string
		fires                                                  bool
	}{
		{"a masked card number with no machine named is a card bill",
			maskedPAN, maskedPAN, "", "Direct debit",
			canonical.SpendDetailedCardSpend, true},
		{"the booking type names an ATM withdrawal",
			maskedPAN, maskedPAN, "", "ATM Withdrawal", "", false},
		{"the booking type names a Bancomat withdrawal",
			maskedPAN, maskedPAN, "", "UBS Bancomat Withdrawal", "", false},
		{"the booking type in either case",
			maskedPAN, maskedPAN, "", "atm withdrawal", "", false},
		{"the narrative names the machine, so the cash rule places it",
			maskedPAN, maskedPAN, "ATM withdrawal; EXAMPLETOWN", "",
			canonical.SpendDetailedCashWithdrawal, true},
		{"a card bill whose booking type merely mentions a card",
			"UBS CARD CENTER", "UBS CARD CENTER", "", "Card payment",
			canonical.SpendDetailedCardSpend, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detailed, _, ok := RuleCategory(tc.signature, tc.counterparty, tc.description, tc.providerCategory)
			if ok != tc.fires {
				t.Fatalf("RuleCategory(…, %q) fired=%v (→ %q), want fired=%v",
					tc.providerCategory, ok, detailed, tc.fires)
			}
			if ok && detailed != tc.detailed {
				t.Errorf("RuleCategory(…, %q) = %q, want %q", tc.providerCategory, detailed, tc.detailed)
			}
		})
	}
}

// TestRuleRefusalReadsTheFilingOnlyToDecline pins the boundary the
// refusal lives on. The provider's booking type may stop a built-in
// from firing; it may never place a verdict, which would be the
// provider tier wearing the rule tier's provenance and outranking it.
// So a row whose ONLY cash-withdrawal marker is the booking type gets
// no rule verdict at all — the provider tier below places it.
func TestRuleRefusalReadsTheFilingOnlyToDecline(t *testing.T) {
	if detailed, _, ok := RuleCategory("EXAMPLE SHOP", "EXAMPLE SHOP", "", "ATM Withdrawal"); ok {
		t.Errorf("the booking type placed %q; a built-in must never fire on the provider's filing", detailed)
	}
}

// A bank's bill-pay line is "Online Payment <ref> To <payee>", and the payee
// is whoever the holder addressed it to. Read as a card bill it files real
// spending under a card, ignoring the payee written in the narrative. The
// issuers that announce themselves that way keep their own phrase.
func TestOnlinePaymentAloneIsNotACardBill(t *testing.T) {
	cases := []struct {
		name      string
		narrative string
		wantCard  bool
	}{
		{"bill-pay to a landlord", "01/02 Online Payment 9000000001 To Example Person", false},
		{"bill-pay to a firm", "01/03 Online Payment 90000000002 To Example Appliance Co", false},
		{"an issuer that names itself still matches", "CITI CARD ONLINE PAYMENT 1234", true},
		{"as do the other card wordings", "AUTOMATIC PAYMENT THANK YOU", true},
		{"and paying a named card", "01 31 PAYMENT TO CHASE CARD ENDING IN", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := Normalize("", tc.narrative)
			_, _, ok := RuleCategory(sig, "", tc.narrative, "")
			if ok != tc.wantCard {
				t.Errorf("RuleCategory(%q) fired = %v, want %v", sig, ok, tc.wantCard)
			}
		})
	}
}

// TestConfigRuleMatchesTheIssuersOwnFiling pins the field that makes the
// issuer an input to our classification rather than a tier above it: a
// rule may key on what the issuer called a row, which is the only way to
// write one about a class of merchant the descriptor never names.
func TestConfigRuleMatchesTheIssuersOwnFiling(t *testing.T) {
	rules := []Rule{{
		Match:    regexp.MustCompile(`(?i)^Club Membership$`),
		Category: "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS",
	}}
	// The descriptor says nothing a rule could use; the issuer's filing does.
	got, ok := ConfigRuleCategory(rules, RuleRow{
		Counterparty: "EXAMPLE ASSOCIATION", Description: "",
		ProviderCategory: "Club Membership",
	})
	if !ok || got.Category != "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS" {
		t.Errorf("= (%q, %v), want the rule keyed on the issuer's value to fire", got.Category, ok)
	}
	// It is one field among three, not a special case: the same rule
	// must not fire on a row the issuer filed differently.
	if _, ok := ConfigRuleCategory(rules, RuleRow{
		Counterparty: "EXAMPLE ASSOCIATION", ProviderCategory: "Groceries",
	}); ok {
		t.Error("fired on a row the issuer filed as something else")
	}
	// And a row the issuer never filed still matches on the narrative.
	if _, ok := ConfigRuleCategory(rules, RuleRow{Counterparty: "Club Membership"}); !ok {
		t.Error("the counterparty field must keep working when the issuer is silent")
	}
}

// TestRuleCategoryBrokerageNarratives pins the rules a brokerage
// account brought into scope (migration 0064) needs.
//
// The account kind widened, and the narratives that arrive with it are
// new to this tier. Four shapes, three verdicts: a custodian's
// SECURITY-level pass-through and the account's own management fee
// both file as investment fees — they are the same cost, of holding
// the assets — while tax withheld at source is its own thing. What the
// rules must NOT do is confuse any of them with a fee for banking,
// which is what the wire-fee case below pins.
//
// Because two of the rules share a verdict, the value alone does not
// say which fired; TestEachBrokerageNarrativeMatchesItsOwnRule below
// pins that separately.
func TestRuleCategoryBrokerageNarratives(t *testing.T) {
	for _, tc := range []struct{ narrative, detailed string }{
		// A depositary charge names the security, never a payee.
		{"FEE CHARGED EXAMPLE INDUSTRIAL AG SPON ADR EACH REP 1 ORD (Cash)",
			canonical.SpendDetailedInvestmentFees},
		{"ADR FEE", canonical.SpendDetailedInvestmentFees},

		// Withholding at source on foreign dividend income. The
		// household never sees it, but the gross dividend is booked
		// as income, so the withholding is the tax it paid.
		{"FOREIGN TAX PAID EXAMPLE ENERGY ASA SPON ADR EACH REP 1 ORD",
			canonical.SpendDetailedWithholdingTax},
		{"WITHHOLDING TAX", canonical.SpendDetailedWithholdingTax},
		{"NRA Tax EXAMPLE RESTAURANT INC", canonical.SpendDetailedWithholdingTax},

		// The fee for being managed — a service, not a pass-through.
		{"ADVISOR FEE DEDUCTED Advisor Fee (Cash)",
			canonical.SpendDetailedInvestmentFees},
		{"ADVISOR FEE DEDUCTED Investment Mgr Fee (Cash)",
			canonical.SpendDetailedInvestmentFees},
	} {
		got, _, ok := RuleCategory("", "", tc.narrative, "")
		if !ok || got != tc.detailed {
			t.Errorf("RuleCategory(%q) = (%q, %v), want %q",
				tc.narrative, got, ok, tc.detailed)
		}
	}
}

// TestRuleCategoryLeavesAWireAlone: a wire out of a brokerage account
// is NOT the rule tier's to place. It is either an own-account move —
// which only the matcher can know, by finding the receiving leg — or
// it is spend on something the narrative does not name. Guessing here
// would outrank the matcher's evidence for the first case and invent a
// merchant for the second.
func TestRuleCategoryLeavesAWireAlone(t *testing.T) {
	for _, narrative := range []string{
		"WIRE TRANSFER TO BANK (Cash)",
		"WIRE TRANSFER TO BANK",
	} {
		if _, _, ok := RuleCategory("", "", narrative, ""); ok {
			t.Errorf("RuleCategory(%q) placed a verdict; a wire is the "+
				"matcher's to pair or nobody's to guess", narrative)
		}
	}
}

// TestRuleCategoryWireFee: the charge for SENDING a wire is a bank
// fee, and is not to be confused with the wire itself — which no rule
// places (TestRuleCategoryLeavesAWireAlone).
func TestRuleCategoryWireFee(t *testing.T) {
	for _, n := range []string{"WIRED FUNDS FEE", "WIRE TRANSFER FEE"} {
		got, _, ok := RuleCategory("", "", n, "")
		if !ok || got != "BANK_FEES_OTHER_BANK_FEES" {
			t.Errorf("RuleCategory(%q) = (%q, %v), want a bank fee", n, got, ok)
		}
	}
	if _, _, ok := RuleCategory("", "", "WIRED FUNDS DISBURSED", ""); ok {
		t.Error("the wire itself must stay unplaced; only its fee is a bank fee")
	}
}

// TestEachBrokerageNarrativeMatchesItsOwnRule is the assertion the
// shared verdict costs the test above.
//
// A security-level pass-through and an account's management fee both
// file as investment fees, so asserting the category cannot catch a
// phrase that moved from one rule's list to the other's — the value
// would be right and the reason wrong. matchRule returns the rule, so
// the phrase lists stay pinned where they belong.
func TestEachBrokerageNarrativeMatchesItsOwnRule(t *testing.T) {
	ruleOf := func(t *testing.T, narrative string) spendRule {
		t.Helper()
		r, _, ok := matchRule("", "", narrative, "")
		if !ok {
			t.Fatalf("matchRule(%q) placed nothing", narrative)
		}
		return r
	}
	passThrough := ruleOf(t, "ADR FEE")
	managed := ruleOf(t, "ADVISOR FEE DEDUCTED Advisor Fee (Cash)")

	if passThrough.detailed != managed.detailed {
		t.Fatalf("the two rules no longer share a verdict (%q vs %q); this test's"+
			" reason to exist is gone and TestRuleCategoryBrokerageNarratives covers it",
			passThrough.detailed, managed.detailed)
	}
	if &passThrough.phrases[0] == &managed.phrases[0] {
		t.Error("both narratives matched the SAME rule; the pass-through and the" +
			" management fee must stay separate rules with separate phrase lists")
	}
	// And each keeps its own side of the split.
	for _, n := range []string{"FEE CHARGED EXAMPLE INDUSTRIAL AG SPON ADR", "DEPOSITARY FEE"} {
		if &ruleOf(t, n).phrases[0] != &passThrough.phrases[0] {
			t.Errorf("%q left the pass-through rule", n)
		}
	}
	for _, n := range []string{"MANAGEMENT FEE", "INVESTMENT MGR FEE", "ADVISORY FEE"} {
		if &ruleOf(t, n).phrases[0] != &managed.phrases[0] {
			t.Errorf("%q left the management-fee rule", n)
		}
	}
}
