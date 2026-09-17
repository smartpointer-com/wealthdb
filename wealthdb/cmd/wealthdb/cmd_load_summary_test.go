package main

import (
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
)

// TestLoadPrintsBothFamilyBlocks pins what `load` says about the pass.
//
// The pass writes two overlays in one transaction and the summary is
// how a reader learns either happened, but the income block was
// unpinned: delete the second printFamilySummary call and every other
// test in the suite stays green. So does moving the transfer-override
// line, which belongs under spending because there is ONE matcher and
// one override ledger, and read under income's block it is a remark
// about the family it is not about.
func TestLoadPrintsBothFamilyBlocks(t *testing.T) {
	var out strings.Builder
	spend := spending.FamilyResult{
		Enriched: 10, MatcherRows: 1, RuleRows: 2, ProviderRows: 3,
		PinRows: 1, SignatureOnlyRows: 3,
		UnmatchedPins: 1, UnresolvedScopeAccounts: 1,
		UnmappedProviderCategories: 1, RekeyedVerdicts: 2, SplitVerdicts: 1,
	}
	income := spending.FamilyResult{
		Enriched: 4, MatcherRows: 1, RuleRows: 1, ProviderRows: 0,
		PinRows: 0, SignatureOnlyRows: 2,
		UnmatchedPins: 2, UnresolvedScopeAccounts: 3,
		UnmappedProviderCategories: 4, RekeyedVerdicts: 5, SplitVerdicts: 6,
	}
	// Driven through printPassSummary rather than through two direct
	// calls: the defect this pins is a missing CALL, not a broken
	// printer, so the test has to reach the place the calls are made.
	printPassSummary(&out, &spending.Result{
		FamilyResult:               spend,
		Income:                     income,
		UnmatchedTransferOverrides: 2,
	})
	got := out.String()

	// Each family's own numbers, under its own name, in its own nouns.
	for _, want := range []string{
		"spending: 10 row(s) enriched — 1 matcher, 2 rule, 3 provider, 1 pinned, 3 unplaced",
		"income: 4 row(s) enriched — 1 matcher, 1 rule, 0 provider, 0 pinned, 2 unplaced",
		"income: 2 pin(s) matched no transaction",
		"`income.accounts` keys on the account id",
		"income: 5 payer verdict(s) carried forward",
		"income: 6 payer verdict(s) left behind",
		"spending: 2 merchant verdict(s) carried forward",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the load summary is missing %q:\n%s", want, got)
		}
	}
	// The income block never says "merchant", and vice versa.
	incomeBlock := got[strings.Index(got, "income: 4 row(s)"):]
	if strings.Contains(incomeBlock, "merchant") {
		t.Errorf("the income block says \"merchant\":\n%s", incomeBlock)
	}
	if strings.Contains(got[:strings.Index(got, "income: 4 row(s)")], "payer") {
		t.Errorf("the spending block says \"payer\":\n%s", got)
	}
	// Spending first, then income — the order the pass runs them in and
	// the order every other surface names them in.
	if strings.Index(got, "spending:") > strings.Index(got, "income:") {
		t.Errorf("income is reported before spending:\n%s", got)
	}
	// The override ledger is spending's: one matcher, one ledger, both
	// families reading its verdicts. Its line belongs under spending's
	// block, not after income's, where it reads as a remark about the
	// family it is not about.
	override := strings.Index(got, "transfer override(s) matched no leg")
	if override < 0 {
		t.Fatalf("the override line is missing:\n%s", got)
	}
	if override > strings.Index(got, "income: 4 row(s)") {
		t.Errorf("the transfer-override line prints after the income block:\n%s", got)
	}

	// A quiet run says one line per family and nothing else: the
	// counters that are zero stay silent, which is what makes a
	// non-zero one worth reading.
	var quiet strings.Builder
	printFamilySummary(&quiet, "income", "payer", spending.FamilyResult{Enriched: 2})
	if n := strings.Count(strings.TrimSpace(quiet.String()), "\n"); n != 0 {
		t.Errorf("a quiet income run printed %d extra line(s):\n%s", n, quiet.String())
	}
}

// TestLoadPrintsTheCashflowBoundary pins the third block, and the same
// defect the two family blocks are pinned against: delete the call and
// nothing else in the suite notices.
//
// The wrapper-coverage line is the one worth having. An unset wrapper
// reads as household, so a retirement or health account among the
// unset ones sits INSIDE the cash pool — its trades counted as the
// household's investing, its contributions absent rather than wrong.
// No reconciliation downstream can see a crossing that never happened.
func TestLoadPrintsTheCashflowBoundary(t *testing.T) {
	var out strings.Builder
	printPassSummary(&out, &spending.Result{Cashflow: spending.CashflowResult{
		ScopeRows: 2, UnresolvedScopeAccounts: 1,
		WrapperRows: 23, WrapperOverrides: 1, FarAccounts: 17,
		PooledAccountsWithoutWrapper: 4,
	}})
	got := out.String()
	for _, want := range []string{
		"cashflow: household boundary stamped — 23 wrapper(s), 1 overridden, 2 account(s) " +
			"out of the pool; 17 own-account move(s) carry a far account",
		"cashflow: 4 pooled account(s) have no tax wrapper",
		"`cashflow.accounts` keys on the account id",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the load summary is missing %q:\n%s", want, got)
		}
	}
	// After both families: the boundary is a stamp rather than an
	// enrichment, and the pass writes it last.
	if strings.Index(got, "cashflow:") < strings.Index(got, "income:") {
		t.Errorf("the cashflow block prints before income's:\n%s", got)
	}

	// A deployment whose wrappers are all set says one line.
	var quiet strings.Builder
	printCashflowSummary(&quiet, spending.CashflowResult{WrapperRows: 23, FarAccounts: 17})
	if n := strings.Count(strings.TrimSpace(quiet.String()), "\n"); n != 0 {
		t.Errorf("a covered deployment printed %d extra line(s):\n%s", n, quiet.String())
	}

	// The pass that ENDS the migrated-but-unloaded state says so, once.
	// Before it the pool is every account and every matched own-account
	// move resolves to `vehicles · Untracked accounts`, which reads as a
	// finding and is not one — so the load that fixes it is the place a
	// reader learns that any earlier report was wrong.
	var first strings.Builder
	printCashflowSummary(&first, spending.CashflowResult{WrapperRows: 23, FirstPass: true})
	if !strings.Contains(first.String(), "FIRST pass") {
		t.Errorf("the first pass did not announce itself:\n%s", first.String())
	}
	if strings.Contains(quiet.String(), "FIRST pass") {
		t.Errorf("a later pass repeated the first-pass notice:\n%s", quiet.String())
	}
}
