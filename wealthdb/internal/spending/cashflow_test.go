package spending

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The boundary stamp: configuration turned into two tables, re-stamped
// whole by every pass so that removing an entry removes its effect.

// wrapperSides reads the stamped boundary as comparable text.
func wrapperSides(t *testing.T, db *sql.DB, ctx context.Context) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT tax_wrapper, side, COALESCE(class, '') FROM cashflow_wrapper_sides`)
	if err != nil {
		t.Fatalf("read wrapper sides: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var w, side, class string
		if err := rows.Scan(&w, &side, &class); err != nil {
			t.Fatalf("scan wrapper sides: %v", err)
		}
		out[w] = side + "/" + class
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate wrapper sides: %v", err)
	}
	return out
}

// poolAccounts reads the cash pool as "source/account" strings.
func poolAccounts(t *testing.T, db *sql.DB, ctx context.Context) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT silver_source_id, account_external_id FROM cashflow_pool_accounts()`)
	if err != nil {
		t.Fatalf("read pool: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, acct string
		if err := rows.Scan(&src, &acct); err != nil {
			t.Fatalf("scan pool: %v", err)
		}
		out = append(out, src+"/"+acct)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pool: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestTheBoundaryIsStampedWhole pins the decision that keeps
// canonical.DefaultWrapperSide the single place a wrapper's side is
// decided: every wrapper reaches gold, defaults included, so no macro
// re-derives the boundary and a wrapper added for a new jurisdiction
// takes one edit to reach the statement.
func TestTheBoundaryIsStampedWhole(t *testing.T) {
	db, ctx := openGold(t)
	res := runPass(t, db, ctx, Options{})

	want := canonical.WrapperBoundaries()
	if res.Cashflow.WrapperRows != len(want) {
		t.Errorf("stamped %d wrappers, the enum has %d", res.Cashflow.WrapperRows, len(want))
	}
	if res.Cashflow.WrapperOverrides != 0 {
		t.Errorf("stamped %d overrides with no config", res.Cashflow.WrapperOverrides)
	}
	got := wrapperSides(t, db, ctx)
	if len(got) != len(want) {
		t.Fatalf("the table holds %d wrappers, want %d", len(got), len(want))
	}
	for _, b := range want {
		if g, w := got[string(b.Wrapper)], string(b.Side)+"/"+string(b.Class); g != w {
			t.Errorf("%s stamped %q, want %q", b.Wrapper, g, w)
		}
	}
}

// TestAWrapperOverrideMovesTheBoundary pins the config knob in both
// directions — a vehicle brought into the household, and a household
// wrapper sent out to a pool — and that removing the entry removes the
// effect, which is the whole of the stamp-whole contract.
func TestAWrapperOverrideMovesTheBoundary(t *testing.T) {
	db, ctx := openGold(t)
	res := runPass(t, db, ctx, Options{Cashflow: CashflowOptions{Wrappers: map[string]string{
		string(canonical.TaxWrapper529):          canonical.WrapperDestHousehold,
		string(canonical.TaxWrapperTrustGrantor): canonical.WrapperDestTrusts,
	}}})
	if res.Cashflow.WrapperOverrides != 2 {
		t.Errorf("counted %d overrides, want 2", res.Cashflow.WrapperOverrides)
	}
	got := wrapperSides(t, db, ctx)
	if got["529"] != "household/" {
		t.Errorf("529 = %q, want household/", got["529"])
	}
	if got["trust_grantor"] != "vehicle/trusts" {
		t.Errorf("trust_grantor = %q, want vehicle/trusts", got["trust_grantor"])
	}
	// An unlisted wrapper keeps the engine default.
	if got["hsa"] != "vehicle/health" {
		t.Errorf("hsa = %q, want the engine default vehicle/health", got["hsa"])
	}

	runPass(t, db, ctx, Options{})
	got = wrapperSides(t, db, ctx)
	if got["529"] != "vehicle/education" || got["trust_grantor"] != "household/" {
		t.Errorf("removing the entries left %q / %q behind",
			got["529"], got["trust_grantor"])
	}
}

// TestThePoolIsTheHouseholdsAccounts pins what the boundary is FOR:
// the pool holds the accounts on the household side of it and nothing
// else, and an unset wrapper reads as household — the safe direction,
// and the one the coverage counter exists to make visible.
func TestThePoolIsTheHouseholdsAccounts(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        UPDATE accounts SET tax_wrapper = 'taxable_personal'
         WHERE silver_source_id = 'bank' AND account_external_id = 'CASH1';
        UPDATE accounts SET tax_wrapper = 'roth_ira'
         WHERE silver_source_id = 'bank' AND account_external_id = 'BRK1';
        UPDATE accounts SET tax_wrapper = 'charitable'
         WHERE silver_source_id = 'bank' AND account_external_id = 'CUST1';
        UPDATE accounts SET tax_wrapper = 'hsa'
         WHERE silver_source_id = 'other-bank' AND account_external_id = 'CUST2';
    `); err != nil {
		t.Fatalf("set wrappers: %v", err)
	}
	res := runPass(t, db, ctx, Options{})

	want := []string{"bank/CARD1", "bank/CASH1", "other-bank/CASH2"}
	if got := poolAccounts(t, db, ctx); !equalStrings(got, want) {
		t.Errorf("pool = %v, want %v", got, want)
	}
	// CARD1 and CASH2 carry no wrapper at all: in the pool, and counted.
	if res.Cashflow.PooledAccountsWithoutWrapper != 2 {
		t.Errorf("coverage gap = %d, want 2", res.Cashflow.PooledAccountsWithoutWrapper)
	}
}

// TestThePoolScopeInheritsNothing is the correction the design makes to
// its own earlier draft, and the one a reader is most likely to assume
// away: an account either family excludes stays in the pool, and only
// `cashflow.accounts` takes one out.
func TestThePoolScopeInheritsNothing(t *testing.T) {
	db, ctx := openGold(t)
	res := runPass(t, db, ctx, Options{
		Exclude:  map[string][]string{"bank": {"CASH1"}},
		Income:   IncomeOptions{Exclude: map[string][]string{"bank": {"CARD1"}}},
		Cashflow: CashflowOptions{Exclude: map[string][]string{"other-bank": {"CUST2"}}},
	})
	if res.Cashflow.ScopeRows != 1 {
		t.Errorf("stamped %d pool exclusions, want 1", res.Cashflow.ScopeRows)
	}
	got := poolAccounts(t, db, ctx)
	for _, want := range []string{"bank/CASH1", "bank/CARD1"} {
		if !contains(got, want) {
			t.Errorf("%s left the pool because a family excluded it", want)
		}
	}
	if contains(got, "other-bank/CUST2") {
		t.Error("cashflow.accounts did not take an account out of the pool")
	}
}

// TestAnUnresolvedPoolExclusionIsCounted pins the same failure mode the
// families' scopes have: an entry naming an account gold does not hold
// fences nothing, so it is counted rather than silently stamped and
// forgotten.
func TestAnUnresolvedPoolExclusionIsCounted(t *testing.T) {
	db, ctx := openGold(t)
	res := runPass(t, db, ctx, Options{Cashflow: CashflowOptions{
		Exclude: map[string][]string{"bank": {"CASH1", "NO-SUCH-ACCOUNT"}},
	}})
	if res.Cashflow.ScopeRows != 2 {
		t.Errorf("stamped %d rows, want 2 — every entry is stamped whatever it resolves to", res.Cashflow.ScopeRows)
	}
	if res.Cashflow.UnresolvedScopeAccounts != 1 {
		t.Errorf("counted %d unresolved entries, want 1", res.Cashflow.UnresolvedScopeAccounts)
	}
}

// TestTheBoundaryStampIsIdempotent pins the re-assert: two passes over
// the same config leave the same two tables, so a nightly run cannot
// accumulate rows.
func TestTheBoundaryStampIsIdempotent(t *testing.T) {
	db, ctx := openGold(t)
	opts := Options{Cashflow: CashflowOptions{
		Exclude:  map[string][]string{"bank": {"CASH1"}},
		Wrappers: map[string]string{string(canonical.TaxWrapperHSA): canonical.WrapperDestHousehold},
	}}
	runPass(t, db, ctx, opts)
	first := fmt.Sprint(wrapperSides(t, db, ctx), poolAccounts(t, db, ctx))
	runPass(t, db, ctx, opts)
	if second := fmt.Sprint(wrapperSides(t, db, ctx), poolAccounts(t, db, ctx)); second != first {
		t.Errorf("a second pass changed the boundary:\n%s\n%s", first, second)
	}
}

// TestTheBoundaryStampChangesNoFamilyReport is the regression the stage
// has to clear. The stamp writes two tables neither family reads; if a
// spending or income figure moved, something is reading the boundary
// that should not be.
func TestTheBoundaryStampChangesNoFamilyReport(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-SHOP", account: "CASH1", kind: "purchase",
			occurredAt: day(1), amount: -40, description: "CORNER MARKET"},
		txn{source: "bank", id: "T-WAGE", account: "CASH1", kind: "deposit",
			occurredAt: day(2), amount: 5000, description: "SALARY"},
	)
	runPass(t, db, ctx, Options{})
	before := reportShape(t, db, ctx)

	runPass(t, db, ctx, Options{Cashflow: CashflowOptions{
		Exclude:  map[string][]string{"bank": {"CASH1"}},
		Wrappers: map[string]string{string(canonical.TaxWrapperTaxablePersonal): canonical.WrapperDestGiving},
	}})
	after := reportShape(t, db, ctx)
	if len(before) != len(after) {
		t.Fatalf("report rows = %d before the boundary moved, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d moved: %q vs %q", i, before[i], after[i])
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
