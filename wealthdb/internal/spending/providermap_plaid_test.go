package spending

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestPlaidVocabularyCoversVersion2 pins the reviewed list: each side
// translates every Plaid value or leaves it untranslated, never both, and
// only a value outside version 2 is drift.
func TestPlaidVocabularyCoversVersion2(t *testing.T) {
	if len(plaidPFCv2) != 127 {
		t.Errorf("plaidPFCv2 holds %d values, want Plaid's 127", len(plaidPFCv2))
	}
	seen := map[string]bool{}
	for _, v := range plaidPFCv2 {
		if seen[v] {
			t.Errorf("plaidPFCv2 lists %q twice", v)
		}
		seen[v] = true
		_, spend := plaidCategories[v]
		_, income := plaidIncomeCategories[v]
		if spend == plaidUncategorized[v] {
			t.Errorf("%q: spending side translated=%v and reviewed-untranslated=%v", v, spend, plaidUncategorized[v])
		}
		if income == plaidIncomeUncategorized[v] {
			t.Errorf("%q: income side translated=%v and reviewed-untranslated=%v", v, income, plaidIncomeUncategorized[v])
		}
		for _, account := range []string{"cash", "card"} {
			if _, _, drift := ProviderCategory("plaid", account, v); drift {
				t.Errorf("ProviderCategory(plaid, %s, %q) is drift", account, v)
			}
			if _, _, drift := ProviderIncomeCategory("plaid", account, v); drift {
				t.Errorf("ProviderIncomeCategory(plaid, %s, %q) is drift", account, v)
			}
		}
	}
	for k := range plaidCategories {
		if !seen[k] {
			t.Errorf("plaidCategories translates %q, which is not a version 2 value", k)
		}
	}
	for k := range plaidIncomeCategories {
		if !seen[k] {
			t.Errorf("plaidIncomeCategories translates %q, which is not a version 2 value", k)
		}
	}
	// The version 1 spellings mean the version 2 request failed; an
	// invented value means the taxonomy moved. Both are drift.
	for _, v := range []string{"INCOME_WAGES", "TRANSFER_IN_CASH_ADVANCES_AND_LOANS", "FOOD_AND_DRINK_SYNTHETIC"} {
		_, _, spendDrift := ProviderCategory("plaid", "cash", v)
		_, _, incomeDrift := ProviderIncomeCategory("plaid", "cash", v)
		if !spendDrift || !incomeDrift {
			t.Errorf("%q: drift spend=%v income=%v, want both", v, spendDrift, incomeDrift)
		}
	}
}

// TestPlaidPrimariesAreVendoredWhole pins how close the taxonomy stays to
// Plaid's. Every primary of version 2 is either vendored whole, value for
// value, or left out whole. The five left out name a movement, which the
// matcher and the deltas place. A vendored value Plaid does not publish,
// or a version 2 value missing under a vendored primary, fails here.
func TestPlaidPrimariesAreVendoredWhole(t *testing.T) {
	leftOut := map[string]bool{"TRANSFER_IN": true, "TRANSFER_OUT": true,
		"LOAN_PAYMENTS": true, "LOAN_DISBURSEMENTS": true, "OTHER": true}

	vendored := map[string]string{} // detailed value → its primary
	for _, c := range append(canonical.VendoredSpendCategories(), canonical.VendoredIncomeCategories()...) {
		vendored[c.Detailed] = c.Primary
	}
	primaries := map[string]bool{}
	for _, p := range vendored {
		primaries[p] = true
	}
	if len(primaries) != 13 {
		t.Errorf("the taxonomy vendors %d primaries, want 13", len(primaries))
	}
	for p := range leftOut {
		if primaries[p] {
			t.Errorf("%s is both vendored and left out", p)
		}
		primaries[p] = true
	}

	published := map[string]bool{}
	for _, v := range plaidPFCv2 {
		published[v] = true
		var under []string
		for p := range primaries {
			if strings.HasPrefix(v, p+"_") {
				under = append(under, p)
			}
		}
		if len(under) != 1 {
			t.Errorf("%q falls under %v, want exactly one of Plaid's primaries", v, under)
			continue
		}
		ours, isVendored := vendored[v]
		switch {
		case leftOut[under[0]] && isVendored:
			t.Errorf("%q is vendored, but its primary %s is left out", v, under[0])
		case !leftOut[under[0]] && !isVendored:
			t.Errorf("%q is missing from the taxonomy, but its primary %s is vendored", v, under[0])
		case isVendored && ours != under[0]:
			t.Errorf("%q is vendored under %s, want Plaid's %s", v, ours, under[0])
		}
	}
	for v := range vendored {
		if !published[v] {
			t.Errorf("vendored %q is not a version 2 value", v)
		}
	}
}

// TestPlaidTranslationsAreVendoredOrNamedDeltas pins what each side may
// translate into: a value the model tier also knows, or one of the deltas
// the provider tier is licensed to place.
func TestPlaidTranslationsAreVendoredOrNamedDeltas(t *testing.T) {
	spendDeltas := map[string]bool{canonical.SpendDetailedCardSpend: true,
		canonical.SpendDetailedDebtRepayment: true, canonical.DetailedMortgageTransfer: true}
	for v, detailed := range plaidCategories {
		if !canonical.ModelSpendDetailed(detailed) && !spendDeltas[detailed] {
			t.Errorf("plaidCategories[%q] = %q", v, detailed)
		}
	}
	incomeDeltas := map[string]bool{canonical.IncomeDetailedLoanProceeds: true,
		canonical.DetailedMortgageTransfer: true}
	for v, detailed := range plaidIncomeCategories {
		if !canonical.ModelIncomeDetailed(detailed) && !incomeDeltas[detailed] {
			t.Errorf("plaidIncomeCategories[%q] = %q", v, detailed)
		}
	}
}

// TestPlaidProviderTier pins both sides of the map (docs/adapters/plaid.md
// §10). Every vendored value translates to itself, and claims the row
// unless it is a catch-all. The loan values translate to the deltas,
// every one of them listed here. The rest is left untranslated on purpose.
func TestPlaidProviderTier(t *testing.T) {
	// The spending identities. Every value claims but a catch-all, which
	// is recorded and declined so the model reads the merchant.
	for _, c := range canonical.VendoredSpendCategories() {
		detailed, ok, drift := ProviderCategory("plaid", "card", c.Detailed)
		if !ok || drift || detailed != c.Detailed {
			t.Errorf("ProviderCategory(plaid, %q) = (%q, %v, %v), want itself", c.Detailed, detailed, ok, drift)
			continue
		}
		if got, want := ProviderCategoryClaims("plaid", "card", detailed), !canonical.CatchAllSpendDetailed(detailed); got != want {
			t.Errorf("%q claims = %v, want %v", c.Detailed, got, want)
		}
	}
	// A late fee and a cash-advance fee are specific fees, not
	// catch-alls, so each claims the row as itself.
	for _, v := range []string{"BANK_FEES_LATE_FEES", "BANK_FEES_CASH_ADVANCE"} {
		if d, ok, _ := ProviderCategory("plaid", "card", v); !ok || d != v || !ProviderCategoryClaims("plaid", "card", d) {
			t.Errorf("%q = (%q, %v), want itself, claimed", v, d, ok)
		}
	}
	if ProviderCategoryClaims("plaid", "card", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE") {
		t.Error("a merchandise catch-all claimed the row; the merchant name is the model's to read")
	}

	// The spending deltas, all of them: each loan payment whose value
	// names the kind of lender.
	deltas := map[string]string{
		"LOAN_PAYMENTS_CREDIT_CARD_PAYMENT":   canonical.SpendDetailedCardSpend,
		"LOAN_PAYMENTS_MORTGAGE_PAYMENT":      canonical.DetailedMortgageTransfer,
		"LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT":  canonical.SpendDetailedDebtRepayment,
		"LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT": canonical.SpendDetailedDebtRepayment,
		"LOAN_PAYMENTS_CASH_ADVANCES":         canonical.SpendDetailedDebtRepayment,
		"LOAN_PAYMENTS_CAR_PAYMENT":           canonical.SpendDetailedDebtRepayment,
	}
	for v, want := range deltas {
		detailed, ok, drift := ProviderCategory("plaid", "card", v)
		if !ok || drift || detailed != want {
			t.Errorf("ProviderCategory(plaid, %q) = (%q, %v, %v), want %q", v, detailed, ok, drift, want)
		}
		if !ProviderCategoryClaims("plaid", "card", detailed) {
			t.Errorf("%q did not claim; a delta is a verdict", v)
		}
	}
	for v, detailed := range plaidCategories {
		if v != detailed && deltas[v] != detailed {
			t.Errorf("the spending translation %q → %q is neither an identity nor a delta listed here", v, detailed)
		}
	}
	if want := len(canonical.VendoredSpendCategories()) + len(deltas); len(plaidCategories) != want {
		t.Errorf("plaidCategories holds %d translations, want the %d identities and deltas", len(plaidCategories), want)
	}
	// Whose account a transfer reaches is the matcher's to find. A wage
	// advance's repayment offsets the advance, which stays a receipt.
	for _, v := range []string{"TRANSFER_OUT_ACCOUNT_TRANSFER", "TRANSFER_OUT_WIRE", "LOAN_PAYMENTS_BNPL",
		"LOAN_PAYMENTS_OTHER_PAYMENT", "LOAN_PAYMENTS_EWA", "OTHER_OTHER", "INCOME_SALARY"} {
		if _, ok, drift := ProviderCategory("plaid", "cash", v); ok || drift {
			t.Errorf("ProviderCategory(plaid, %q) = (ok=%v, drift=%v), want reviewed and untranslated", v, ok, drift)
		}
	}

	// The income identities, all thirteen. INCOME_OTHER is income's one
	// catch-all and the one value that declines, so the model reads the
	// payer.
	for _, c := range canonical.VendoredIncomeCategories() {
		detailed, ok, drift := ProviderIncomeCategory("plaid", "cash", c.Detailed)
		if !ok || drift || detailed != c.Detailed {
			t.Errorf("ProviderIncomeCategory(plaid, %q) = (%q, %v, %v), want itself", c.Detailed, detailed, ok, drift)
			continue
		}
		if got, want := ProviderIncomeCategoryClaims("plaid", "cash", detailed), c.Detailed != "INCOME_OTHER"; got != want {
			t.Errorf("%q claims = %v, want %v", c.Detailed, got, want)
		}
	}

	// The income deltas, all of them: money borrowed arriving.
	incomeDeltas := map[string]string{
		"LOAN_DISBURSEMENTS_AUTO":               canonical.IncomeDetailedLoanProceeds,
		"LOAN_DISBURSEMENTS_CASH_ADVANCES":      canonical.IncomeDetailedLoanProceeds,
		"LOAN_DISBURSEMENTS_PERSONAL":           canonical.IncomeDetailedLoanProceeds,
		"LOAN_DISBURSEMENTS_STUDENT":            canonical.IncomeDetailedLoanProceeds,
		"LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT": canonical.IncomeDetailedLoanProceeds,
		"LOAN_DISBURSEMENTS_MORTGAGE":           canonical.DetailedMortgageTransfer,
	}
	for v, want := range incomeDeltas {
		detailed, ok, drift := ProviderIncomeCategory("plaid", "cash", v)
		if !ok || drift || detailed != want {
			t.Errorf("ProviderIncomeCategory(plaid, %q) = (%q, %v, %v), want %q", v, detailed, ok, drift, want)
		}
		if !ProviderIncomeCategoryClaims("plaid", "cash", detailed) {
			t.Errorf("%q did not claim; a delta is a verdict", v)
		}
	}
	for v, detailed := range plaidIncomeCategories {
		if v != detailed && incomeDeltas[v] != detailed {
			t.Errorf("the income translation %q → %q is neither an identity nor a delta listed here", v, detailed)
		}
	}
	if want := len(canonical.VendoredIncomeCategories()) + len(incomeDeltas); len(plaidIncomeCategories) != want {
		t.Errorf("plaidIncomeCategories holds %d translations, want the %d identities and deltas", len(plaidIncomeCategories), want)
	}
	// A transfer in may be the holder's own money, or savings interest
	// Plaid filed as a transfer: the matcher and the kind floor know more.
	// A wage advance may be wages or a loan, so it stays a visible receipt.
	for _, v := range []string{"TRANSFER_IN_ACCOUNT_TRANSFER", "TRANSFER_IN_OTHER_TRANSFER_IN",
		"LOAN_DISBURSEMENTS_EWA", "FOOD_AND_DRINK_GROCERIES"} {
		if _, ok, drift := ProviderIncomeCategory("plaid", "cash", v); ok || drift {
			t.Errorf("ProviderIncomeCategory(plaid, %q) = (ok=%v, drift=%v), want reviewed and untranslated", v, ok, drift)
		}
	}
}
