package gold

// Structural guard for the returns-policy registry: every registered
// adapter kind that emits accounts must ship a registered ReturnsPolicy,
// so `unknown_adapter_policy` stays unreachable through the real pipeline
// (RETURNS-NOTES.md "Quality flags"). The adapter registry is populated
// by the blank import of silver/all in returns_policy_import_test.go.

import (
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// referenceKinds are the adapter kinds that emit no accounts: curves and
// fred (published rates, fred's FX rates too) and refdata (fund
// compositions and security facts). Nothing of theirs reaches the
// returns engine; TestRegisteredFlowPolicies pins their Known=false
// fallback.
var referenceKinds = []string{"curves", "fred", "refdata"}

// TestEveryAccountKindHasPolicy pins the invariant between the adapter
// registry and the policy registry: every registered kind but the
// reference ones registers a ReturnsPolicy, and each reference kind is
// a registered one, so the exemption cannot outlive its adapter.
func TestEveryAccountKindHasPolicy(t *testing.T) {
	t.Parallel()
	registered := silver.Kinds()
	if len(registered) <= len(referenceKinds) {
		t.Fatalf("registered kinds %v: the blank import of silver/all is missing", registered)
	}
	reference := make(map[string]bool, len(referenceKinds))
	for _, k := range referenceKinds {
		reference[k] = true
		if _, err := silver.Get(k); err != nil {
			t.Errorf("reference kind %q: %v", k, err)
		}
	}
	for _, k := range registered {
		if reference[k] {
			continue
		}
		if rp, ok := returns.ReturnsPolicyFor(k); !ok || !rp.Flow.Known {
			t.Errorf("kind %q: no registered ReturnsPolicy: set Returns in its silver.Source", k)
		}
	}
}

// TestCashAccountsHiddenWhateverTheSource pins the kind rule: a cash account
// emits no rows of its own even on a source whose policy shows its accounts
// (fidelity-kind, as a statement archive's deposit accounts are), so a
// portfolio or source made only of cash accounts emits none either. A
// portfolio holding anything else still shows, and the cash still counts in
// every aggregate that contains it.
func TestCashAccountsHiddenWhateverTheSource(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "dep", "fidelity")
	seedReturnsSource(t, db, ctx, "brk", "fidelity")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	sleeves, mixed := "SLEEVES", "MIXED"
	seedAcct(t, db, ctx, "dep", "CHK", canonical.AccountKindCash, &sleeves,
		[]snap{{a, 3000}, {b, 50}},
		[]txn{{dy(2024, time.June, 3), canonical.TxKindWithdrawal, -2950}})
	seedAcct(t, db, ctx, "dep", "SAV", canonical.AccountKindCash, &sleeves,
		[]snap{{a, 1000}, {b, 1010}}, nil)
	seedAcct(t, db, ctx, "brk", "CORE", canonical.AccountKindCash, &mixed,
		[]snap{{a, 200}, {b, 200}}, nil)
	seedAcct(t, db, ctx, "brk", "BRK", canonical.AccountKindBrokerage, &mixed,
		[]snap{{a, 1000}, {b, 1100}}, nil)
	end := eod(2024, time.December, 30)

	run := func(level string) []ReturnRow {
		t.Helper()
		rows, err := RunReturns(ctx, db, params(level, 0, end))
		if err != nil {
			t.Fatalf("RunReturns %s: %v", level, err)
		}
		return rows
	}
	acctRows := run("accounts")
	for _, id := range []string{"CHK", "SAV", "CORE"} {
		if _, ok := summaryFor(acctRows, id); ok {
			t.Errorf("cash account %s must emit no accounts-grain row", id)
		}
	}
	if _, ok := summaryFor(acctRows, "BRK"); !ok {
		t.Error("the brokerage account must keep its row")
	}

	pfRows := run("portfolios")
	if _, ok := summaryFor(pfRows, "SLEEVES"); ok {
		t.Error("an all-cash portfolio must emit no portfolios-grain row")
	}
	m, ok := summaryFor(pfRows, "MIXED")
	if !ok {
		t.Fatal("a portfolio holding more than cash must keep its row")
	}
	if v, ok := parseFloatPtr(m.StartValue); !ok || v != 1200 {
		t.Errorf("MIXED start = %v, want 1200 (its cash account included)", m.StartValue)
	}

	srcRows := run("sources")
	if _, ok := summaryFor(srcRows, "dep"); ok {
		t.Error("an all-cash source must emit no sources-grain row")
	}
	if _, ok := summaryFor(srcRows, "brk"); !ok {
		t.Error("a source holding more than cash must keep its row")
	}

	globRows := run("global")
	g, ok := summaryFor(globRows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if v, ok := parseFloatPtr(g.StartValue); !ok || v != 5200 {
		t.Errorf("global start = %v, want 5200 (every cash account included)", g.StartValue)
	}
	if nf := netFlowOf(t, globRows, ""); nf != -2950 {
		t.Errorf("global net_flow = %.2f, want -2950 (the cash account's withdrawal counted)", nf)
	}
}
