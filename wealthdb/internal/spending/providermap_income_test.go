package spending

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestIncomeProviderVocabularies pins the income side of the provider
// maps: what each bank vocabulary translates, what it declines, and the
// two ways the income side differs from the outflow one.
func TestIncomeProviderVocabularies(t *testing.T) {
	// Every value both maps translate must be a value of its own
	// family's taxonomy — a typo here is a verdict gold would refuse.
	for value, detailed := range ubsIncomeBookingTypes {
		if !canonical.ValidIncomeDetailed(detailed) {
			t.Errorf("ubsIncomeBookingTypes[%q] = %q, which is not an income value", value, detailed)
		}
	}
	for value, detailed := range raiffeisenIncomeCategories {
		if !canonical.ValidIncomeDetailed(detailed) {
			t.Errorf("raiffeisenIncomeCategories[%q] = %q, which is not an income value", value, detailed)
		}
	}
	for value, detailed := range syntheticIncomeCategories {
		if !canonical.ValidIncomeDetailed(detailed) {
			t.Errorf("syntheticIncomeCategories[%q] = %q, which is not an income value", value, detailed)
		}
	}

	// A bank's booking type names the movement, so a translated value
	// CLAIMS the row even where it is a catch-all: there is no
	// counterparty name for a later tier to read.
	for _, tc := range []struct{ value, want string }{
		{"SALARY PAYMENT", "INCOME_WAGES"},
		{"salary payment", "INCOME_WAGES"}, // folded case
		{"DIVIDEND", "INCOME_DIVIDENDS"},
		{"INTEREST CALCULATION BALANCE", "INCOME_INTEREST_EARNED"},
		{"RETURN OF CAPITAL", canonical.IncomeDetailedCapitalReturn},
	} {
		detailed, ok, drift := ProviderIncomeCategory("ubs", "cash", tc.value)
		if !ok || detailed != tc.want {
			t.Errorf("ProviderIncomeCategory(ubs, %q) = (%q, %v), want %q", tc.value, detailed, ok, tc.want)
		}
		if drift {
			t.Errorf("%q counted as drift; a bank vocabulary is not categorical", tc.value)
		}
		if !ProviderIncomeCategoryClaims("ubs", "cash", detailed) {
			t.Errorf("%q did not claim; a booking type naming the movement is the most any tier will know", tc.value)
		}
	}

	// The same booking type means different things by direction. This
	// is the whole reason the two maps are separate.
	spendDetailed, _, _ := ProviderCategory("ubs", "cash", "INTEREST CALCULATION BALANCE")
	incomeDetailed, _, _ := ProviderIncomeCategory("ubs", "cash", "INTEREST CALCULATION BALANCE")
	if spendDetailed == incomeDetailed {
		t.Error("one booking type resolved the same on both sides; interest charged is not interest earned")
	}

	// A generic credit type names a rail and nothing else: left to the
	// model tier, and not counted as drift on a non-categorical map.
	for _, value := range []string{"e-banking credit", "CREDIT", "SEPA CREDIT"} {
		if _, ok, _ := ProviderIncomeCategory("ubs", "cash", value); ok {
			t.Errorf("%q was translated; a generic credit names no payer", value)
		}
	}

	// A CATEGORICAL vocabulary's catch-all is translated and then
	// DECLINED — recorded, and left for the tier that can read a payer.
	detailed, ok, drift := ProviderIncomeCategory("raiffeisen_at", "cash", "income_other")
	if !ok || detailed != "INCOME_OTHER_INCOME" {
		t.Fatalf("raiffeisen income_other = (%q, %v), want INCOME_OTHER_INCOME", detailed, ok)
	}
	if drift {
		t.Error("a translated value counted as drift")
	}
	if ProviderIncomeCategoryClaims("raiffeisen_at", "cash", detailed) {
		t.Error("a categorical vocabulary's catch-all claimed the row; it names no payer")
	}
	// ...and the values it reviewed and left alone are not drift.
	if _, ok, drift := ProviderIncomeCategory("raiffeisen_at", "cash", "not_categorized"); ok || drift {
		t.Errorf("not_categorized = (ok=%v, drift=%v), want reviewed and left alone", ok, drift)
	}
	// An unknown value on a categorical vocabulary IS drift.
	if _, ok, drift := ProviderIncomeCategory("raiffeisen_at", "cash", "brand_new_bucket"); ok || !drift {
		t.Errorf("an unmapped categorical value = (ok=%v, drift=%v), want drift", ok, drift)
	}

	// A card vocabulary carries no income map at all: an inbound card
	// row is a refund, which is spending's to net.
	if _, ok, drift := ProviderIncomeCategory("amex", "card", "Restaurant"); ok || drift {
		t.Errorf("a card vocabulary answered on the income side = (ok=%v, drift=%v)", ok, drift)
	}
}

// TestRaiffeisenIncomeDriftOnlyOnTheUnreviewed pins the income side's
// reviewed set against the spending one.
//
// The vocabulary is CATEGORICAL, so anything outside the reviewed set
// is reported as drift — "the issuer's vocabulary has moved". The
// income map translates one value by design, so a reviewed set of its
// own size would make every ordinary spend token on an admitted inflow
// row report as drift on every load, and the real signal would be
// unreadable underneath it. For example, a `deposit` the bank files
// `real_estate_other`.
func TestRaiffeisenIncomeDriftOnlyOnTheUnreviewed(t *testing.T) {
	// Reviewed on either side ⇒ not drift here.
	for _, v := range []string{
		"real_estate_other", // a deposit can carry it; reviewed on the spending side
		"not_categorized",   // the bank placed nothing
		"payment_other",     // a payment it could not read
		"supermarket",       // an ordinary spend token, on a refund
		"bank_fee",
		"atm_withdrawal",
	} {
		if _, ok, drift := ProviderIncomeCategory("raiffeisen_at", "cash", v); drift {
			t.Errorf("ProviderIncomeCategory(%q) counts as drift; it is reviewed on one side or the other", v)
		} else if ok {
			t.Errorf("ProviderIncomeCategory(%q) translated; a reviewed value is recorded, not placed", v)
		}
	}

	// The one value the income map translates, and it declines to claim
	// — the vocabulary's own catch-all leaves the payer to the model.
	detailed, ok, drift := ProviderIncomeCategory("raiffeisen_at", "cash", "income_other")
	if !ok || drift {
		t.Errorf("income_other: (ok=%v, drift=%v), want translated and not drift", ok, drift)
	}
	if detailed != "INCOME_OTHER_INCOME" {
		t.Errorf("income_other = %q, want INCOME_OTHER_INCOME", detailed)
	}
	if ProviderIncomeCategoryClaims("raiffeisen_at", "cash", detailed) {
		t.Error("the vocabulary's own catch-all claimed the row")
	}

	// A value NEITHER side has seen is drift, which is the signal the
	// set exists to keep readable.
	if _, _, drift := ProviderIncomeCategory("raiffeisen_at", "cash", "crypto_exchange_other"); !drift {
		t.Error("an unreviewed value did not count as drift; the vocabulary can now move unnoticed")
	}

	// And the derivation holds in the direction that matters: every
	// value reviewed for spending is reviewed here, save the one this
	// side translates.
	for v := range raiffeisenUncategorized {
		if v == "income_other" {
			continue
		}
		if !raiffeisenIncomeUncategorized[v] {
			t.Errorf("%q is reviewed for spending but drift for income", v)
		}
	}
}
