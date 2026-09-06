package spending

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestProviderMapsAreInTheTaxonomy pins every translation to a value
// gold's spend_categories dimension actually holds. A typo here would
// resolve to a NULL primary and drop the row out of every grouped
// report without any error to notice. It also pins that no two keys of
// one map fold to the same lookup key: two spellings of one value kept
// as two entries could drift to two verdicts.
func TestProviderMapsAreInTheTaxonomy(t *testing.T) {
	for kind, v := range providerVocabularies {
		if len(v.translations) == 0 {
			t.Errorf("silver kind %q has an empty provider map; omit the entry instead", kind)
		}
		folded := map[string]string{}
		for value, detailed := range v.translations {
			if !canonical.ValidSpendDetailed(detailed) {
				t.Errorf("%s provider map: %q → %q, which is not a recognised spend_detailed value",
					kind, value, detailed)
			}
			key := providerCategoryKey(value)
			if other, dup := folded[key]; dup {
				t.Errorf("%s provider map: %q and %q fold to the same key", kind, value, other)
			}
			folded[key] = value
		}
	}
}

func TestProviderCategoryTranslates(t *testing.T) {
	cases := map[string]string{
		"Groceries":         "FOOD_AND_DRINK_GROCERIES",
		"Food & Drink":      "FOOD_AND_DRINK_RESTAURANT",
		"Health & Wellness": "MEDICAL_OTHER_MEDICAL",
		// Case and surrounding space fold, so a cosmetic change on the
		// provider's side does not silently unmap a whole category.
		"  groceries ": "FOOD_AND_DRINK_GROCERIES",
	}
	for category, want := range cases {
		detailed, ok, drift := ProviderCategory("chase", "", category)
		if !ok || drift || detailed != want {
			t.Errorf("ProviderCategory(chase, %q) = (%q, %v, %v), want (%q, true, false)",
				category, detailed, ok, drift, want)
		}
	}
}

// TestProviderCategoryUnmappedFallsThrough is the load-bearing case
// for a categorical vocabulary. The issuer publishes more values than
// this build has seen, and a value nobody reviewed must reach the model
// tier and be COUNTED — never be guessed into a plausible neighbour,
// which would be invisible in every report that consumed it.
func TestProviderCategoryUnmappedFallsThrough(t *testing.T) {
	detailed, ok, drift := ProviderCategory("chase", "", "Automotive & Transit")
	if ok || detailed != "" {
		t.Errorf("ProviderCategory(chase, unseen) = (%q, %v), want no guess", detailed, ok)
	}
	if !drift {
		t.Error("ProviderCategory(chase, unseen) reported nothing to count; " +
			"an unseen value in a categorical vocabulary must count as vocabulary drift")
	}
}

// TestProviderCategoryUnmappedSource separates "this build does not
// understand the issuer's word" from "this source publishes no
// categories at all". Only the first is drift worth counting.
func TestProviderCategoryUnmappedSource(t *testing.T) {
	for _, tc := range []struct{ kind, category string }{
		{"schwab", "Groceries"}, // no map for this kind
		{"chase", ""},           // mapped kind, but the row carries no category
		{"chase", "   "},
		{"ubs", ""},
	} {
		if detailed, ok, drift := ProviderCategory(tc.kind, "", tc.category); ok || drift {
			t.Errorf("ProviderCategory(%q, %q) = (%q, %v, %v), want ('', false, false)",
				tc.kind, tc.category, detailed, ok, drift)
		}
	}
}

// TestProviderCategoryUBSBookingTypes pins the UBS vocabulary across
// its three eras — the statement PDF's upper-case types, the MT940 :61:
// codes, the web export's mixed-case labels — and that the fold makes
// one entry serve every spelling. The deltas are the point: the bank's
// own word names the movement, and the tier places it.
func TestProviderCategoryUBSBookingTypes(t *testing.T) {
	cases := map[string]string{
		// Fees, per era.
		"UBS ADVICE":                        "BANK_FEES_OTHER_BANK_FEES",
		"CUSTODY PRICE":                     "BANK_FEES_OTHER_BANK_FEES",
		"RENTAL FEE SAFE BOX":               "BANK_FEES_OTHER_BANK_FEES",
		"ADR/GDR HANDLING FEES":             "BANK_FEES_OTHER_BANK_FEES",
		"THIRD-PARTY CHARGES":               "BANK_FEES_OTHER_BANK_FEES",
		"BALANCE CLOSING OF SERVICE PRICES": "BANK_FEES_OTHER_BANK_FEES",
		"NCHG":                              "BANK_FEES_OTHER_BANK_FEES",
		"NCOM":                              "BANK_FEES_OTHER_BANK_FEES",
		"INTEREST CALCULATION BALANCE":      "BANK_FEES_INTEREST_CHARGE",
		// Cash, in the PDF and the web spelling.
		"ATM WITHDRAWAL":          canonical.SpendDetailedCashWithdrawal,
		"ATM Withdrawal":          canonical.SpendDetailedCashWithdrawal,
		"UBS BANCOMAT WITHDRAWAL": canonical.SpendDetailedCashWithdrawal,
		"UBS Bancomat Withdrawal": canonical.SpendDetailedCashWithdrawal,
		// FX between own currency accounts, per era.
		"NFEX":                  canonical.SpendDetailedInternalTransfer,
		"FOREX SALE":            canonical.SpendDetailedInternalTransfer,
		"FOREX PURCHASE":        canonical.SpendDetailedInternalTransfer,
		"Sale FX Spot":          canonical.SpendDetailedInternalTransfer,
		"Purchase FX Spot":      canonical.SpendDetailedInternalTransfer,
		"Sale FX Forward":       canonical.SpendDetailedInternalTransfer,
		"Purchase FX Forward":   canonical.SpendDetailedInternalTransfer,
		"Sale from FX Swap":     canonical.SpendDetailedInternalTransfer,
		"Purchase from FX Swap": canonical.SpendDetailedInternalTransfer,
		// Card bills, in the PDF and the web spelling.
		"PAYMENT TO CARD": canonical.SpendDetailedCardSpend,
		"Payment to card": canonical.SpendDetailedCardSpend,
	}
	for value, want := range cases {
		detailed, ok, drift := ProviderCategory("ubs", "", value)
		if !ok || drift || detailed != want {
			t.Errorf("ProviderCategory(ubs, %q) = (%q, %v, %v), want (%q, true, false)",
				value, detailed, ok, drift, want)
		}
	}
}

// TestProviderCategoryRailIsNotDrift pins the booking-type
// vocabulary's unmapped case: a payment rail names how the money
// moved, not what it bought, so a value the map does not translate is
// the normal case and must fall through WITHOUT being counted — or the
// drift canary would fire on every payment order at the bank. The same
// miss on a categorical vocabulary still counts (the test above), so
// the flag, not the miss, is what decides.
func TestProviderCategoryRailIsNotDrift(t *testing.T) {
	for _, value := range []string{
		"e-banking payment order", "E-BANKING PAYMENT ORDER", "PAYNET ORDER",
		"DIRECT DEBIT", "credit", "SALARY PAYMENT", "NTRF", "NMSC", "NSTO",
		"order", "ATM Withdrawal;Reversal",
	} {
		if detailed, ok, drift := ProviderCategory("ubs", "", value); ok || drift || detailed != "" {
			t.Errorf("ProviderCategory(ubs, %q) = (%q, %v, %v), want ('', false, false)",
				value, detailed, ok, drift)
		}
	}
}

// TestUBSVocabularyIsPerProduct pins the reason the registry is keyed by
// product rather than by source: the same source publishes a booking
// type on a bank account and a merchant category on a card, and the two
// shapes disagree about what an unmapped value MEANS.
func TestUBSVocabularyIsPerProduct(t *testing.T) {
	// A card's MCC description is translated only under the card key.
	if got, ok, _ := ProviderCategory("ubs", "card", "Grocery stores"); !ok ||
		got != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("ProviderCategory(ubs, card, Grocery stores) = (%q, %v), "+
			"want the groceries value", got, ok)
	}
	// The same value read as a bank booking type is not one, and a
	// booking-type miss is a rail rather than drift.
	if got, ok, drift := ProviderCategory("ubs", "cash", "Grocery stores"); ok || drift {
		t.Errorf("ProviderCategory(ubs, cash, Grocery stores) = (%q, %v, %v), "+
			"want no verdict and no drift", got, ok, drift)
	}
	// And a bank booking type still translates on the cash side.
	if got, ok, _ := ProviderCategory("ubs", "cash", "ATM WITHDRAWAL"); !ok ||
		got != canonical.SpendDetailedCashWithdrawal {
		t.Errorf("ProviderCategory(ubs, cash, ATM WITHDRAWAL) = (%q, %v), "+
			"want cash_withdrawal", got, ok)
	}
}

// TestUBSCardVocabularyIsCategorical: every card row carries a category,
// so a value the map lacks is one this build has not reviewed — the
// drift canary, exactly as for a card issuer.
func TestUBSCardVocabularyIsCategorical(t *testing.T) {
	detailed, ok, drift := ProviderCategory("ubs", "card", "Llama grooming")
	if ok || detailed != "" {
		t.Errorf("an unreviewed MCC description must not be guessed, got %q", detailed)
	}
	if !drift {
		t.Error("an unmapped card category must count as drift")
	}
}

// TestUBSCardMoneyMovementIsLeftUnmapped: the bank's catch-all for a
// card row that moved money rather than bought something names no line
// of business, so placing it would file transfers as shopping.
func TestUBSCardMoneyMovementIsLeftUnmapped(t *testing.T) {
	if detailed, ok, _ := ProviderCategory(
		"ubs", "card", "Banks - merchandise and services"); ok {
		t.Errorf("got %q, want no verdict for the bank's own catch-all", detailed)
	}
}

// TestUnknownAccountKindFallsBackToTheSourceVocabulary keeps a source
// with one vocabulary working without an entry per account kind.
func TestUnknownAccountKindFallsBackToTheSourceVocabulary(t *testing.T) {
	if got, ok, _ := ProviderCategory("chase", "card", "Groceries"); !ok ||
		got != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("ProviderCategory(chase, card, Groceries) = (%q, %v), "+
			"want the source-wide chase map", got, ok)
	}
	if got, ok, _ := ProviderCategory("ubs", "safekeeping", "ATM WITHDRAWAL"); !ok ||
		got != canonical.SpendDetailedCashWithdrawal {
		t.Errorf("an account kind with no vocabulary of its own must inherit "+
			"the source's, got (%q, %v)", got, ok)
	}
}

// TestUBSCardValuesAreVendored: the provider tier may place a delta, but
// every value in this map names a line of business, so all of them must
// be real vendored taxonomy values.
func TestUBSCardValuesAreVendored(t *testing.T) {
	for category, detailed := range ubsCardCategories {
		if !canonical.VendoredSpendDetailed(detailed) {
			t.Errorf("%q -> %q is not a vendored taxonomy value",
				category, detailed)
		}
	}
}
