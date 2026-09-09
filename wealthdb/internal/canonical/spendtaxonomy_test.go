package canonical

import (
	"strings"
	"testing"
)

// droppedSpendPrimaries are the Plaid primaries the spend taxonomy
// deliberately leaves behind: they describe flows, not spending.
var droppedSpendPrimaries = []string{
	"INCOME", "TRANSFER_IN", "TRANSFER_OUT", "LOAN_PAYMENTS",
}

// TestSpendTaxonomyCounts pins the size of the vendored subset. The
// numbers are the reason the file can claim to be Plaid's taxonomy
// minus its four flow primaries: a silent addition or deletion during
// a taxonomy refresh shows up here first.
func TestSpendTaxonomyCounts(t *testing.T) {
	if got := len(vendoredSpendCategories); got != 80 {
		t.Errorf("vendored detailed values = %d, want 80", got)
	}
	if got := len(extensionSpendCategories); got != 1 {
		t.Errorf("extension values = %d, want 1", got)
	}
	if got := len(deltaSpendCategories); got != 6 {
		t.Errorf("delta values = %d, want 6", got)
	}
	if got := len(SpendCategories); got != 87 {
		t.Errorf("SpendCategories = %d, want 87", got)
	}
	if got := len(modelSpendCategories); got != 81 {
		t.Errorf("modelSpendCategories = %d, want 81", got)
	}

	primaries := map[string]struct{}{}
	for _, c := range vendoredSpendCategories {
		primaries[c.Primary] = struct{}{}
	}
	if got := len(primaries); got != 12 {
		t.Errorf("vendored primaries = %d, want 12", got)
	}

	seen := map[string]struct{}{}
	for _, c := range SpendCategories {
		if _, dup := seen[c.Detailed]; dup {
			t.Errorf("duplicate spend_detailed %q", c.Detailed)
		}
		seen[c.Detailed] = struct{}{}
	}
}

// TestSpendTaxonomyVendoredShape holds the vendored rows to Plaid's
// own conventions — a detailed value is its primary plus a suffix, and
// the whole vocabulary is uppercase — and confirms the four flow
// primaries left no residue behind, in either dimension.
func TestSpendTaxonomyVendoredShape(t *testing.T) {
	for _, c := range vendoredSpendCategories {
		if !strings.HasPrefix(c.Detailed, c.Primary+"_") {
			t.Errorf("detailed %q is not prefixed by its primary %q", c.Detailed, c.Primary)
		}
		if c.Primary != strings.ToUpper(c.Primary) || c.Detailed != strings.ToUpper(c.Detailed) {
			t.Errorf("vendored pair (%q, %q) is not uppercase", c.Primary, c.Detailed)
		}
		if c.Description == "" || c.Description != strings.TrimSpace(c.Description) {
			t.Errorf("description for %q is empty or untrimmed: %q", c.Detailed, c.Description)
		}
		for _, dropped := range droppedSpendPrimaries {
			if c.Primary == dropped || strings.HasPrefix(c.Detailed, dropped+"_") {
				t.Errorf("dropped primary %q survives in (%q, %q)", dropped, c.Primary, c.Detailed)
			}
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
	}
	for _, c := range deltaSpendCategories {
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
		t.Errorf("delta %q missing from deltaSpendCategories", d)
	}
}

// TestSpendDeltaMarkerIsExact pins the structural marker gold reads
// delta-ness off: a row is a delta if and only if its primary equals
// its detailed value. Migration 0048's spend_txn_categories() tests
// exactly that on the seeded dimension to blank the merchant column,
// and restates no list — so the marker must hold for every delta and
// for nothing vendored, or a vendored row would lose its merchant, or
// a delta keep one, with nothing on the SQL side to say so.
func TestSpendDeltaMarkerIsExact(t *testing.T) {
	deltas := map[string]struct{}{}
	for _, d := range deltaSpendCategories {
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
// vocabulary and refuses the near-misses: a bare primary (which is not
// a detailed value unless it is a delta) and wrong-cased spellings.
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
	} {
		if ValidSpendDetailed(s) {
			t.Errorf("ValidSpendDetailed(%q) = true, want false", s)
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
	for _, d := range deltaSpendCategories {
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
	got[0] = SpendCategory{"X", "Y", "Z"}
	if vendoredSpendCategories[0] != first {
		t.Error("VendoredSpendCategories must return a copy")
	}
	for _, c := range VendoredSpendCategories() {
		for _, d := range deltaSpendCategories {
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
	if len(got) != len(deltaSpendCategories) {
		t.Fatalf("got %d rows, want %d", len(got), len(deltaSpendCategories))
	}
	first := deltaSpendCategories[0]
	got[0] = SpendCategory{"X", "Y", "Z"}
	if deltaSpendCategories[0] != first {
		t.Error("DeltaSpendCategories must return a copy")
	}
}

// TestCatchAllSpendDetailed pins the predicate a tier declines on. Every
// primary has exactly one catch-all and it is the only value in that
// primary the predicate admits; a delta is never one, because a delta is
// a verdict about what the row IS rather than a shrug about a merchant.
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
