package canonical

import (
	"strings"
	"testing"
)

// droppedPrimaries are the Plaid primaries the taxonomy leaves behind
// whichever family is reading: they describe movements that are the
// matcher's or a tracked account's, not a category of spend or of
// income. INCOME is NOT among them any more — it is the income
// family's whole vendored vocabulary — and TestSpendTaxonomyVendoredShape
// keeps it out of the outflow half by name.
var droppedPrimaries = []string{"TRANSFER_IN", "TRANSFER_OUT", "LOAN_PAYMENTS"}

// TestSpendTaxonomyCounts pins the size of each class of the table.
// The numbers are the reason the file can claim to be Plaid's taxonomy
// minus three flow primaries, split into two families: a silent
// addition or deletion during a taxonomy refresh shows up here first.
func TestSpendTaxonomyCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"vendoredSpendCategories", len(vendoredSpendCategories), 80},
		{"vendoredIncomeCategories", len(vendoredIncomeCategories), 7},
		{"extensionSpendCategories", len(extensionSpendCategories), 3},
		{"extensionIncomeCategories", len(extensionIncomeCategories), 9},
		{"deltaCategories", len(deltaCategories), 11},
		{"SpendCategories", len(SpendCategories), 110},
		{"modelSpendCategories", len(modelSpendCategories), 83},
		{"modelIncomeCategories", len(modelIncomeCategories), 16},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// The membership sets the two families' predicates answer from.
	// Spending's 89 is the count from before the income side existed
	// and must not move: every income value added here is fenced out
	// of it.
	if got := len(spendDetailedValues); got != 89 {
		t.Errorf("spending vocabulary = %d values, want 89", got)
	}
	if got := len(incomeDetailedValues); got != 24 {
		t.Errorf("income vocabulary = %d values, want 24", got)
	}

	primaries := map[string]struct{}{}
	for _, c := range vendoredSpendCategories {
		primaries[c.Primary] = struct{}{}
	}
	if got := len(primaries); got != 12 {
		t.Errorf("vendored outflow primaries = %d, want 12", got)
	}

	seen := map[string]struct{}{}
	for _, c := range SpendCategories {
		if _, dup := seen[c.Detailed]; dup {
			t.Errorf("duplicate detailed value %q", c.Detailed)
		}
		seen[c.Detailed] = struct{}{}
	}
}

// TestSpendTaxonomyVendoredShape holds the vendored rows of both
// families to Plaid's own conventions — a detailed value is its primary
// plus a suffix, and the whole vocabulary is uppercase — and confirms
// the three dropped primaries left no residue behind, in either
// dimension. INCOME is checked the other way round: the income table is
// nothing but INCOME rows, and the outflow table holds none.
func TestSpendTaxonomyVendoredShape(t *testing.T) {
	for _, c := range append(VendoredSpendCategories(), VendoredIncomeCategories()...) {
		if !strings.HasPrefix(c.Detailed, c.Primary+"_") {
			t.Errorf("detailed %q is not prefixed by its primary %q", c.Detailed, c.Primary)
		}
		if c.Primary != strings.ToUpper(c.Primary) || c.Detailed != strings.ToUpper(c.Detailed) {
			t.Errorf("vendored pair (%q, %q) is not uppercase", c.Primary, c.Detailed)
		}
		if c.Description == "" || c.Description != strings.TrimSpace(c.Description) {
			t.Errorf("description for %q is empty or untrimmed: %q", c.Detailed, c.Description)
		}
		for _, dropped := range droppedPrimaries {
			if c.Primary == dropped || strings.HasPrefix(c.Detailed, dropped+"_") {
				t.Errorf("dropped primary %q survives in (%q, %q)", dropped, c.Primary, c.Detailed)
			}
		}
	}
	for _, c := range vendoredSpendCategories {
		if c.Primary == "INCOME" {
			t.Errorf("%q is an INCOME row in the outflow half of the table", c.Detailed)
		}
	}
	for _, c := range vendoredIncomeCategories {
		if c.Primary != "INCOME" {
			t.Errorf("vendored income row %q has primary %q, want INCOME", c.Detailed, c.Primary)
		}
	}
}

// TestEveryCategoryCarriesAFamily pins the column every predicate is
// derived from. A row seeded without one would be admitted by neither
// family — invisible to the gauntlet, refused by every rule and pin —
// which is exactly the failure a taxonomy refresh pasting rows in would
// produce. FamilyBoth is the deltas' alone: a vendored value or an
// extension names a merchant judgement or a payer judgement, never the
// same judgement from both sides.
func TestEveryCategoryCarriesAFamily(t *testing.T) {
	for _, c := range SpendCategories {
		switch c.Family {
		case FamilySpending, FamilyIncome:
		case FamilyBoth:
			if c.Primary != c.Detailed {
				t.Errorf("%q is FamilyBoth but is not a delta", c.Detailed)
			}
		default:
			t.Errorf("%q carries family %q, which is not a family", c.Detailed, c.Family)
		}
	}
	for _, tc := range []struct {
		name  string
		cats  []SpendCategory
		want  Family
		count int
	}{
		{"vendoredSpendCategories", vendoredSpendCategories, FamilySpending, 80},
		{"vendoredIncomeCategories", vendoredIncomeCategories, FamilyIncome, 7},
		{"extensionSpendCategories", extensionSpendCategories, FamilySpending, 3},
		{"extensionIncomeCategories", extensionIncomeCategories, FamilyIncome, 9},
	} {
		n := 0
		for _, c := range tc.cats {
			if c.Family != tc.want {
				t.Errorf("%s: %q carries family %q, want %q", tc.name, c.Detailed, c.Family, tc.want)
			}
			n++
		}
		if n != tc.count {
			t.Errorf("%s = %d rows, want %d", tc.name, n, tc.count)
		}
	}
	// The three deltas both families read, by name: they are what
	// FamilyBoth exists for, and one of them going one-sided would
	// take a value out of a vocabulary without anything else saying so.
	both := map[string]bool{}
	for _, c := range deltaCategories {
		if c.Family == FamilyBoth {
			both[c.Detailed] = true
		}
	}
	for _, d := range []string{SpendDetailedInternalTransfer, SpendDetailedGift, SpendDetailedOther} {
		if !both[d] {
			t.Errorf("%q must be read from either side", d)
		}
	}
	if len(both) != 3 {
		t.Errorf("shared deltas = %d, want 3", len(both))
	}
}

// TestInFamily pins the one rule the membership sets are built on.
func TestInFamily(t *testing.T) {
	for _, tc := range []struct {
		f    Family
		want Family
		ok   bool
	}{
		{FamilySpending, FamilySpending, true},
		{FamilySpending, FamilyIncome, false},
		{FamilyIncome, FamilyIncome, true},
		{FamilyIncome, FamilySpending, false},
		{FamilyBoth, FamilySpending, true},
		{FamilyBoth, FamilyIncome, true},
	} {
		if got := tc.f.InFamily(tc.want); got != tc.ok {
			t.Errorf("%s.InFamily(%s) = %v, want %v", tc.f, tc.want, got, tc.ok)
		}
	}
}

// TestSpendDeltasAreSelfDetailed pins the deltas' defining property:
// they are primary-level, so they roll up to themselves and a report
// grouped by primary shows them as their own bucket. It also pins them
// to the repo's lowercase enum idiom, which is what distinguishes them
// from the vendored vocabulary at a glance.
func TestSpendDeltasAreSelfDetailed(t *testing.T) {
	want := map[string]struct{}{
		SpendDetailedInternalTransfer: {},
		SpendDetailedCashWithdrawal:   {},
		SpendDetailedCardSpend:        {},
		SpendDetailedGift:             {},
		SpendDetailedInvestment:       {},
		SpendDetailedOther:            {},
		IncomeDetailedCapitalReturn:   {},
		IncomeDetailedLoanProceeds:    {},
		IncomeDetailedReimbursement:   {},
		IncomeDetailedInheritance:     {},
		IncomeDetailedCashDeposit:     {},
	}
	for _, c := range deltaCategories {
		if c.Primary != c.Detailed {
			t.Errorf("delta %q has primary %q, want them equal", c.Detailed, c.Primary)
		}
		if c.Detailed != strings.ToLower(c.Detailed) {
			t.Errorf("delta %q is not lowercase", c.Detailed)
		}
		if _, ok := want[c.Detailed]; !ok {
			t.Errorf("unexpected delta %q", c.Detailed)
		}
		delete(want, c.Detailed)
	}
	for d := range want {
		t.Errorf("delta %q missing from deltaCategories", d)
	}
}

// TestSpendDeltaMarkerIsExact pins the structural marker gold reads
// delta-ness off: a row is a delta if and only if its primary equals
// its detailed value. Migration 0048's spend_txn_categories() tests
// exactly that on the seeded dimension to blank the merchant column,
// the income resolution does the same for the payer, and neither
// restates a list — so the marker must hold for every delta and for
// nothing vendored, or a vendored row would lose its counterparty, or
// a delta keep one, with nothing on the SQL side to say so.
func TestSpendDeltaMarkerIsExact(t *testing.T) {
	deltas := map[string]struct{}{}
	for _, d := range deltaCategories {
		deltas[d.Detailed] = struct{}{}
	}
	for _, c := range SpendCategories {
		_, isDelta := deltas[c.Detailed]
		if marked := c.Primary == c.Detailed; marked != isDelta {
			t.Errorf("(%q, %q): primary == detailed is %v, but delta is %v", c.Primary, c.Detailed, marked, isDelta)
		}
	}
}

// TestValidSpendDetailed checks membership across both halves of the
// spending vocabulary and refuses the near-misses: a bare primary
// (which is not a detailed value unless it is a delta), wrong-cased
// spellings, and — the fence that arrived with the income family —
// every income value. A spending rule or pin naming one must fail at
// config load rather than write a value no spending report can show.
func TestValidSpendDetailed(t *testing.T) {
	for _, s := range []string{
		"FOOD_AND_DRINK_GROCERIES", "TRAVEL_FLIGHTS", "BANK_FEES_ATM_FEES",
		SpendDetailedInternalTransfer, SpendDetailedCashWithdrawal,
		SpendDetailedCardSpend, SpendDetailedGift, SpendDetailedInvestment,
		SpendDetailedOther,
	} {
		if !ValidSpendDetailed(s) {
			t.Errorf("ValidSpendDetailed(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"", "FOOD_AND_DRINK", "TRAVEL", "food_and_drink_groceries",
		"INTERNAL_TRANSFER", "INVESTMENT", "CARD_SPEND", "GIFT", "INCOME_WAGES",
		"LOAN_PAYMENTS_CAR_PAYMENT", "TRANSFER_OUT_WITHDRAWAL", "groceries",
		"INCOME", "INCOME_OTHER_INCOME", IncomeDetailedRent, IncomeDetailedStaking,
		IncomeDetailedCapitalReturn, IncomeDetailedLoanProceeds,
		IncomeDetailedReimbursement, IncomeDetailedInheritance,
		IncomeDetailedCashDeposit,
	} {
		if ValidSpendDetailed(s) {
			t.Errorf("ValidSpendDetailed(%q) = true, want false", s)
		}
	}
}

// TestValidIncomeDetailed is the mirror over the income vocabulary:
// the seven vendored values, the eight extensions, and the eight
// deltas the income side reads — the three shared ones included, since
// a movement between tracked accounts and a cash gift mean the same
// thing whichever way the money went. Spending-only values are refused
// for the same reason income values are refused there.
func TestValidIncomeDetailed(t *testing.T) {
	for _, s := range []string{
		"INCOME_WAGES", "INCOME_DIVIDENDS", "INCOME_OTHER_INCOME",
		IncomeDetailedRent, IncomeDetailedStaking, IncomeDetailedDistributions,
		IncomeDetailedAlimonyAndChildSupport,
		IncomeDetailedCapitalReturn, IncomeDetailedLoanProceeds,
		IncomeDetailedReimbursement, IncomeDetailedInheritance,
		IncomeDetailedCashDeposit,
		SpendDetailedInternalTransfer, SpendDetailedGift, SpendDetailedOther,
	} {
		if !ValidIncomeDetailed(s) {
			t.Errorf("ValidIncomeDetailed(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"", "INCOME", "income_wages", "INCOME_RENTAL", "CAPITAL_RETURN",
		"FOOD_AND_DRINK_GROCERIES", "BANK_FEES_ATM_FEES",
		SpendDetailedCashWithdrawal, SpendDetailedCardSpend,
		SpendDetailedInvestment, SpendDetailedDigitalServices,
		SpendDetailedWithholdingTax,
		"TRANSFER_IN_DEPOSIT", "LOAN_PAYMENTS_CAR_PAYMENT",
	} {
		if ValidIncomeDetailed(s) {
			t.Errorf("ValidIncomeDetailed(%q) = true, want false", s)
		}
	}
}

// TestModelSpendDetailed pins the split ValidSpendDetailed and
// ModelSpendDetailed express. The distinction is what stops a
// merchant-keyed model verdict from carrying a delta value, so it has
// to hold in both directions: every delta recognised by one and
// refused by the other, every vendored value accepted by both. An
// extension sits with the vendored rows on both counts — it is ours,
// but it is an ordinary merchant judgement and a model may emit it.
func TestModelSpendDetailed(t *testing.T) {
	for _, d := range DeltaSpendCategories() {
		if !ValidSpendDetailed(d.Detailed) {
			t.Errorf("%q must be a valid stored value", d.Detailed)
		}
		if ModelSpendDetailed(d.Detailed) {
			t.Errorf("%q is a delta and must not pass the model check", d.Detailed)
		}
	}
	for _, c := range vendoredSpendCategories {
		if !ModelSpendDetailed(c.Detailed) {
			t.Errorf("%q is vendored and must pass the model check", c.Detailed)
		}
	}
	for _, c := range extensionSpendCategories {
		if !ModelSpendDetailed(c.Detailed) {
			t.Errorf("%q is an extension and must pass the model check: the model is what places it", c.Detailed)
		}
		if !ValidSpendDetailed(c.Detailed) {
			t.Errorf("%q must be a valid stored value", c.Detailed)
		}
	}
	for _, s := range []string{"", "FOOD_AND_DRINK", "NOT_A_CATEGORY", "food_and_drink_coffee"} {
		if ModelSpendDetailed(s) {
			t.Errorf("%q must not pass the model check", s)
		}
	}
	// card_spend by name, over and above the loop: it is the one delta
	// that sits IN the spending base, so it is the one a model verdict
	// could most plausibly reach for — and it is decided by the absence
	// of a counter-leg, which a merchant-keyed verdict cannot know. The
	// gauntlet's delta check is derived from this predicate.
	if ModelSpendDetailed(SpendDetailedCardSpend) {
		t.Errorf("%q is a delta and must be refused as model output", SpendDetailedCardSpend)
	}
	if !ValidSpendDetailed(SpendDetailedCardSpend) {
		t.Errorf("%q must be a valid stored value: the rule tier, config rules and pins place it", SpendDetailedCardSpend)
	}
	// gift by name, for the same reason: the other delta that sits IN
	// the spending base. It is decided by who the counterparty is to
	// the holder, which no narrative says — only a config rule or a pin
	// carries that knowledge.
	if ModelSpendDetailed(SpendDetailedGift) {
		t.Errorf("%q is a delta and must be refused as model output", SpendDetailedGift)
	}
	if !ValidSpendDetailed(SpendDetailedGift) {
		t.Errorf("%q must be a valid stored value: config rules and pins place it", SpendDetailedGift)
	}
}

// TestModelIncomeDetailed is the same split over the income
// vocabulary. The gauntlet the income conversation runs is the
// spending one with this predicate in place of its sibling.
func TestModelIncomeDetailed(t *testing.T) {
	for _, d := range DeltaIncomeCategories() {
		if !ValidIncomeDetailed(d.Detailed) {
			t.Errorf("%q must be a valid stored value", d.Detailed)
		}
		if ModelIncomeDetailed(d.Detailed) {
			t.Errorf("%q is a delta and must not pass the model check", d.Detailed)
		}
	}
	for _, c := range append(VendoredIncomeCategories(), extensionIncomeCategories...) {
		if !ModelIncomeDetailed(c.Detailed) {
			t.Errorf("%q is vendored or an extension and must pass the model check", c.Detailed)
		}
	}
	for _, s := range []string{"", "INCOME", "NOT_A_CATEGORY", "income_wages"} {
		if ModelIncomeDetailed(s) {
			t.Errorf("%q must not pass the model check", s)
		}
	}
	// capital_return by name: it is the delta the floor itself places
	// on a private fund's distribution, and it is OUT of the income
	// base. A payer-keyed verdict carrying it would not show a wrong
	// figure — it would remove that payer from every income report,
	// which is the one error a reader cannot see.
	if ModelIncomeDetailed(IncomeDetailedCapitalReturn) {
		t.Errorf("%q is a delta and must be refused as model output", IncomeDetailedCapitalReturn)
	}
	if !ValidIncomeDetailed(IncomeDetailedCapitalReturn) {
		t.Errorf("%q must be a valid stored value: a rule and a pin place it", IncomeDetailedCapitalReturn)
	}
}

// TestSpendPredicatesRefuseTheIncomeVocabulary is the regression the
// income family had to be added without breaking: every spending
// predicate answers exactly what it answered before, for every input.
// The values that did not exist then all fall into one set — the
// income vocabulary less the three deltas both families read — and
// every one of them must read as unknown on the spending side.
func TestSpendPredicatesRefuseTheIncomeVocabulary(t *testing.T) {
	shared := map[string]bool{
		SpendDetailedInternalTransfer: true,
		SpendDetailedGift:             true,
		SpendDetailedOther:            true,
	}
	n := 0
	for _, c := range SpendCategories {
		if !c.Family.InFamily(FamilyIncome) || shared[c.Detailed] {
			continue
		}
		n++
		if ValidSpendDetailed(c.Detailed) {
			t.Errorf("ValidSpendDetailed(%q) = true: an income value is not storable as a spend category", c.Detailed)
		}
		if ModelSpendDetailed(c.Detailed) {
			t.Errorf("ModelSpendDetailed(%q) = true: the merchant gauntlet must not accept an income value", c.Detailed)
		}
		if CatchAllSpendDetailed(c.Detailed) {
			t.Errorf("CatchAllSpendDetailed(%q) = true: the provider tier declines on spending catch-alls only", c.Detailed)
		}
	}
	if n != 21 {
		t.Errorf("checked %d income-only values, want 21", n)
	}
}

// TestVendoredSpendCategoriesIsACopy guards the accessor the prompt
// builder uses: it hands out the vocabulary a model may choose from,
// and a caller reordering it must not disturb the table the seed
// migrations are pinned to.
func TestVendoredSpendCategoriesIsACopy(t *testing.T) {
	got := VendoredSpendCategories()
	if len(got) != len(vendoredSpendCategories) {
		t.Fatalf("got %d rows, want %d", len(got), len(vendoredSpendCategories))
	}
	first := vendoredSpendCategories[0]
	got[0] = SpendCategory{"X", "Y", "Z", FamilySpending}
	if vendoredSpendCategories[0] != first {
		t.Error("VendoredSpendCategories must return a copy")
	}
	for _, c := range VendoredSpendCategories() {
		for _, d := range deltaCategories {
			if c.Detailed == d.Detailed {
				t.Errorf("delta %q must not appear in the model's vocabulary", c.Detailed)
			}
		}
	}
}

// TestDeltaSpendCategoriesIsACopy is the mirror for the delta accessor
// the prompt's prohibition sentence reads.
func TestDeltaSpendCategoriesIsACopy(t *testing.T) {
	got := DeltaSpendCategories()
	if len(got) != 6 {
		t.Fatalf("got %d rows, want 6", len(got))
	}
	first := deltaCategories[0]
	got[0] = SpendCategory{"X", "Y", "Z", FamilySpending}
	if deltaCategories[0] != first {
		t.Error("DeltaSpendCategories must return a copy")
	}
}

// TestIncomeAccessorsAreCopiesOfTheirFamily holds the income
// accessors to the same bar and to the boundary that matters more:
// each hands out its own family's rows and nothing of the other's, so
// the income conversation cannot be handed a merchant category and the
// spending one cannot be handed an income type.
func TestIncomeAccessorsAreCopiesOfTheirFamily(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  []SpendCategory
		want int
	}{
		{"VendoredIncomeCategories", VendoredIncomeCategories(), 7},
		{"ModelIncomeCategories", ModelIncomeCategories(), 16},
		{"DeltaIncomeCategories", DeltaIncomeCategories(), 8},
	} {
		if len(tc.got) != tc.want {
			t.Errorf("%s = %d rows, want %d", tc.name, len(tc.got), tc.want)
		}
		for _, c := range tc.got {
			if !c.Family.InFamily(FamilyIncome) {
				t.Errorf("%s hands out %q, which is %s's", tc.name, c.Detailed, c.Family)
			}
		}
	}
	// Each accessor mutated in turn, and the table it reads from checked
	// afterwards: a copy by construction today, but the contract is the
	// accessor's, and an accessor rewritten to hand out its backing slice
	// must fail here rather than let a prompt builder reorder the table
	// the seed migrations are pinned to.
	for _, tc := range []struct {
		name  string
		got   []SpendCategory
		table []SpendCategory
	}{
		{"VendoredIncomeCategories", VendoredIncomeCategories(), vendoredIncomeCategories},
		{"ModelIncomeCategories", ModelIncomeCategories(), modelIncomeCategories},
		{"DeltaIncomeCategories", DeltaIncomeCategories(), deltaCategories},
	} {
		first := tc.table[0]
		tc.got[0] = SpendCategory{"X", "Y", "Z", FamilyIncome}
		if tc.table[0] != first {
			t.Errorf("%s must return a copy", tc.name)
		}
	}
	// The model's income vocabulary carries no delta, as the spending
	// one carries none: the prohibition sentence and the choosable list
	// are built from the same table and must not overlap.
	deltas := map[string]struct{}{}
	for _, d := range DeltaIncomeCategories() {
		deltas[d.Detailed] = struct{}{}
	}
	for _, c := range ModelIncomeCategories() {
		if _, bad := deltas[c.Detailed]; bad {
			t.Errorf("delta %q must not appear in the model's vocabulary", c.Detailed)
		}
	}
}

// TestCatchAllSpendDetailed pins the predicate a tier declines on. Every
// spending primary has exactly one catch-all and it is the only value in
// that primary the predicate admits; a delta is never one, because a delta
// is a verdict about what the row IS rather than a shrug about a merchant.
func TestCatchAllSpendDetailed(t *testing.T) {
	byPrimary := map[string][]string{}
	for _, c := range SpendCategories {
		if CatchAllSpendDetailed(c.Detailed) {
			byPrimary[c.Primary] = append(byPrimary[c.Primary], c.Detailed)
		}
	}
	for _, c := range VendoredSpendCategories() {
		if _, ok := byPrimary[c.Primary]; !ok {
			t.Errorf("primary %s has no catch-all; a tier cannot decline in it", c.Primary)
		}
	}
	for prim, vals := range byPrimary {
		if len(vals) != 1 {
			t.Errorf("primary %s has %d catch-alls (%v), want exactly 1", prim, len(vals), vals)
		}
	}
	for _, d := range DeltaSpendCategories() {
		if CatchAllSpendDetailed(d.Detailed) {
			t.Errorf("delta %s reads as a catch-all; a delta is a verdict, not a shrug", d.Detailed)
		}
	}
	for _, s := range []string{"", "NOT_A_VALUE", "GENERAL_MERCHANDISE"} {
		if CatchAllSpendDetailed(s) {
			t.Errorf("%q must not read as a catch-all", s)
		}
	}
	// The two shapes the convention takes, both admitted.
	for _, s := range []string{"GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "RENT_AND_UTILITIES_OTHER_UTILITIES"} {
		if !CatchAllSpendDetailed(s) {
			t.Errorf("%s is a catch-all", s)
		}
	}
	if CatchAllSpendDetailed(SpendDetailedDigitalServices) {
		t.Error("an extension that names a real category is not a catch-all")
	}
}

// TestCatchAllIncomeDetailed is the income side's, where the vocabulary
// has one vendored primary and therefore exactly one catch-all. It is
// what `categorize income --refine` re-asks and what the income
// provider tier declines on.
func TestCatchAllIncomeDetailed(t *testing.T) {
	var found []string
	for _, c := range SpendCategories {
		if CatchAllIncomeDetailed(c.Detailed) {
			found = append(found, c.Detailed)
		}
	}
	if len(found) != 1 || found[0] != "INCOME_OTHER_INCOME" {
		t.Errorf("income catch-alls = %v, want [INCOME_OTHER_INCOME]", found)
	}
	for _, d := range DeltaIncomeCategories() {
		if CatchAllIncomeDetailed(d.Detailed) {
			t.Errorf("delta %s reads as a catch-all; a delta is a verdict, not a shrug", d.Detailed)
		}
	}
	// A spending catch-all is not the income family's to decline on,
	// and the reverse holds in TestSpendPredicatesRefuseTheIncomeVocabulary.
	for _, s := range []string{"GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "BANK_FEES_OTHER_BANK_FEES", "", "NOT_A_VALUE", "INCOME"} {
		if CatchAllIncomeDetailed(s) {
			t.Errorf("%q must not read as an income catch-all", s)
		}
	}
}

// TestSpendLabelOverrides pins the corrections to the mechanical rule.
// `card_spend` is the one: the rule reads it "Card spend", which is
// true of every card purchase in the product, so it read as a KIND of
// spending among the merchant categories rather than as the placeholder
// it is. Both levels take the correction — a delta is its own primary —
// and nothing else moves with it.
func TestSpendLabelOverrides(t *testing.T) {
	const want = "Uncategorized card spend"
	if got := SpendLabel(SpendDetailedCardSpend); got != want {
		t.Errorf("SpendLabel(%s) = %q, want %q", SpendDetailedCardSpend, got, want)
	}
	if got := SpendPrimaryLabel(SpendDetailedCardSpend); got != want {
		t.Errorf("SpendPrimaryLabel(%s) = %q, want %q", SpendDetailedCardSpend, got, want)
	}
	// The rule still runs everywhere else, initialisms included.
	for _, tc := range []struct{ value, want string }{
		{"FOOD_AND_DRINK_GROCERIES", "Groceries"},
		{"GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "Other general merchandise"},
		{"BANK_FEES_ATM_FEES", "ATM fees"},
		{SpendDetailedInternalTransfer, "Internal transfer"},
		{SpendDetailedOther, "Other"},
	} {
		if got := SpendLabel(tc.value); got != tc.want {
			t.Errorf("SpendLabel(%s) = %q, want %q", tc.value, got, tc.want)
		}
	}
	// Every override names a value the taxonomy actually holds: an
	// override on a value nothing stores is a correction that never
	// fires, and would drift unnoticed.
	for value := range spendLabelOverrides {
		if !ValidSpendDetailed(value) && !ValidIncomeDetailed(value) {
			t.Errorf("override for %q, which is not a taxonomy value", value)
		}
	}
}

// TestIncomeLabels reads every income value through the labeller, which
// is the rule migration 0069's seed was generated from. The mechanical
// rule takes the INCOME_ prefix off and opens the underscores out; no
// income value needs a correction, and the one place that would show is
// here.
func TestIncomeLabels(t *testing.T) {
	want := map[string]string{
		"INCOME_DIVIDENDS":                   "Dividends",
		"INCOME_INTEREST_EARNED":             "Interest earned",
		"INCOME_RETIREMENT_PENSION":          "Retirement pension",
		"INCOME_TAX_REFUND":                  "Tax refund",
		"INCOME_UNEMPLOYMENT":                "Unemployment",
		"INCOME_WAGES":                       "Wages",
		"INCOME_OTHER_INCOME":                "Other income",
		IncomeDetailedSelfEmployment:         "Self employment",
		IncomeDetailedGovernmentBenefits:     "Government benefits",
		IncomeDetailedRent:                   "Rent",
		IncomeDetailedRoyalties:              "Royalties",
		IncomeDetailedAlimonyAndChildSupport: "Alimony and child support",
		IncomeDetailedStaking:                "Staking",
		IncomeDetailedRewards:                "Rewards",
		IncomeDetailedDistributions:          "Distributions",
		IncomeDetailedCapitalReturn:          "Capital return",
		IncomeDetailedInsurancePayout:        "Insurance payout",
		IncomeDetailedLoanProceeds:           "Loan proceeds",
		IncomeDetailedReimbursement:          "Reimbursement",
		IncomeDetailedInheritance:            "Inheritance",
		IncomeDetailedCashDeposit:            "Cash deposit",
		SpendDetailedInternalTransfer:        "Internal transfer",
		SpendDetailedGift:                    "Gift",
		SpendDetailedOther:                   "Other",
	}
	for _, c := range SpendCategories {
		if !c.Family.InFamily(FamilyIncome) {
			continue
		}
		w, ok := want[c.Detailed]
		if !ok {
			t.Errorf("no expected label for income value %q", c.Detailed)
			continue
		}
		if got := IncomeLabel(c.Detailed); got != w {
			t.Errorf("IncomeLabel(%s) = %q, want %q", c.Detailed, got, w)
		}
		delete(want, c.Detailed)
	}
	for v := range want {
		t.Errorf("expected a label for %q, which the taxonomy does not hold", v)
	}
	if got := IncomePrimaryLabel("INCOME"); got != "Income" {
		t.Errorf("IncomePrimaryLabel(INCOME) = %q, want %q", got, "Income")
	}
	// The three shared deltas are expected above by the labels they read
	// BEFORE the income family existed, which is the contract: one row in
	// the dimension, one label, whichever side reads it.
}
