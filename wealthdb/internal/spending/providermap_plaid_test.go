package spending

import (
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

// TestPlaidProviderTier pins every hand-decided translation of either side
// (docs/adapters/plaid.md §10), a sample of the identities, and the values
// left untranslated on purpose.
func TestPlaidProviderTier(t *testing.T) {
	pinned := map[string]bool{}
	for _, tc := range []struct {
		value, want string
		claims      bool
	}{
		// The identities.
		{"FOOD_AND_DRINK_GROCERIES", "FOOD_AND_DRINK_GROCERIES", true},
		{"GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", false},
		// The overrides, all of them.
		{"BANK_FEES_LATE_FEES", "BANK_FEES_OTHER_BANK_FEES", false},
		{"BANK_FEES_CASH_ADVANCE", "BANK_FEES_OTHER_BANK_FEES", false},
		{"LOAN_PAYMENTS_CREDIT_CARD_PAYMENT", canonical.SpendDetailedCardSpend, true},
		{"LOAN_PAYMENTS_MORTGAGE_PAYMENT", canonical.DetailedMortgageTransfer, true},
		{"LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT", canonical.SpendDetailedDebtRepayment, true},
		{"LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT", canonical.SpendDetailedDebtRepayment, true},
		{"LOAN_PAYMENTS_CASH_ADVANCES", canonical.SpendDetailedDebtRepayment, true},
		{"LOAN_PAYMENTS_CAR_PAYMENT", canonical.SpendDetailedDebtRepayment, true},
	} {
		pinned[tc.value] = true
		detailed, ok, drift := ProviderCategory("plaid", "card", tc.value)
		if !ok || drift || detailed != tc.want {
			t.Errorf("ProviderCategory(plaid, %q) = (%q, %v, %v), want %q", tc.value, detailed, ok, drift, tc.want)
		}
		if got := ProviderCategoryClaims("plaid", "card", detailed); got != tc.claims {
			t.Errorf("%q claims = %v, want %v (a catch-all leaves the merchant to the model)", tc.value, got, tc.claims)
		}
	}
	for v, detailed := range plaidCategories {
		if v != detailed && !pinned[v] {
			t.Errorf("the spending override %q → %q is not pinned here", v, detailed)
		}
	}
	// Whose account a transfer reaches is the matcher's to find. A wage
	// advance's repayment offsets the advance, which stays a receipt.
	for _, v := range []string{"TRANSFER_OUT_ACCOUNT_TRANSFER", "TRANSFER_OUT_WIRE", "LOAN_PAYMENTS_BNPL",
		"LOAN_PAYMENTS_OTHER_PAYMENT", "LOAN_PAYMENTS_EWA", "OTHER_OTHER", "INCOME_SALARY"} {
		if _, ok, drift := ProviderCategory("plaid", "cash", v); ok || drift {
			t.Errorf("ProviderCategory(plaid, %q) = (ok=%v, drift=%v), want reviewed and untranslated", v, ok, drift)
		}
	}

	pinnedIncome := map[string]bool{}
	for _, tc := range []struct {
		value, want string
		claims      bool
	}{
		// The identities.
		{"INCOME_DIVIDENDS", "INCOME_DIVIDENDS", true},
		{"INCOME_RETIREMENT_PENSION", "INCOME_RETIREMENT_PENSION", true},
		// The overrides, all of them.
		{"INCOME_SALARY", "INCOME_WAGES", true},
		{"INCOME_GIG_ECONOMY", "INCOME_WAGES", true},
		{"INCOME_CONTRACTOR", canonical.IncomeDetailedSelfEmployment, true},
		{"INCOME_CHILD_SUPPORT", canonical.IncomeDetailedAlimonyAndChildSupport, true},
		{"INCOME_RENTAL", canonical.IncomeDetailedRent, true},
		{"INCOME_MILITARY", canonical.IncomeDetailedGovernmentBenefits, true},
		{"INCOME_LONG_TERM_DISABILITY", canonical.IncomeDetailedGovernmentBenefits, true},
		{"INCOME_OTHER", "INCOME_OTHER_INCOME", false},
		{"LOAN_DISBURSEMENTS_AUTO", canonical.IncomeDetailedLoanProceeds, true},
		{"LOAN_DISBURSEMENTS_CASH_ADVANCES", canonical.IncomeDetailedLoanProceeds, true},
		{"LOAN_DISBURSEMENTS_PERSONAL", canonical.IncomeDetailedLoanProceeds, true},
		{"LOAN_DISBURSEMENTS_STUDENT", canonical.IncomeDetailedLoanProceeds, true},
		{"LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT", canonical.IncomeDetailedLoanProceeds, true},
		{"LOAN_DISBURSEMENTS_MORTGAGE", canonical.DetailedMortgageTransfer, true},
	} {
		pinnedIncome[tc.value] = true
		detailed, ok, drift := ProviderIncomeCategory("plaid", "cash", tc.value)
		if !ok || drift || detailed != tc.want {
			t.Errorf("ProviderIncomeCategory(plaid, %q) = (%q, %v, %v), want %q", tc.value, detailed, ok, drift, tc.want)
		}
		if got := ProviderIncomeCategoryClaims("plaid", "cash", detailed); got != tc.claims {
			t.Errorf("%q claims = %v, want %v", tc.value, got, tc.claims)
		}
	}
	for v, detailed := range plaidIncomeCategories {
		if v != detailed && !pinnedIncome[v] {
			t.Errorf("the income override %q → %q is not pinned here", v, detailed)
		}
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
