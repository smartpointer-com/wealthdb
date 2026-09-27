package gold

// Structural guard for the returns-policy registry: every adapter kind the
// gold silver_sources CHECK constraint admits must ship a registered
// ReturnsPolicy, so `unknown_adapter_policy` stays unreachable through the
// real pipeline (RETURNS-NOTES.md "Quality flags"). The CHECK list exists
// only in the newest silver_sources migration (the rename-recreate pattern
// rewrites it wholesale), so it is parsed out of the embedded SQL and
// cross-checked against the silver adapter registry (populated by the blank
// imports in returns_policy_import_test.go) and the policy registry.

import (
	"io/fs"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

var (
	checkClauseRe = regexp.MustCompile(`(?s)silver_kind\s+TEXT\s+NOT\s+NULL\s+CHECK\s*\(silver_kind\s+IN\s*\(([^)]*)\)`)
	quotedKindRe  = regexp.MustCompile(`'([^']+)'`)
)

// silverKindCheckList extracts the silver_kind whitelist from the
// highest-numbered migration that declares the CHECK constraint.
func silverKindCheckList(t *testing.T) []string {
	t.Helper()
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files) // numeric prefixes sort lexically; the last match wins
	var kinds []string
	for _, name := range files {
		b, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		m := checkClauseRe.FindSubmatch(b)
		if m == nil {
			continue
		}
		kinds = kinds[:0]
		for _, q := range quotedKindRe.FindAllStringSubmatch(string(m[1]), -1) {
			kinds = append(kinds, q[1])
		}
	}
	if len(kinds) == 0 {
		t.Fatal("no migration declares the silver_sources silver_kind CHECK constraint")
	}
	return kinds
}

// TestEveryCheckKindHasAdapterAndPolicy pins the three-way invariant between
// the migration CHECK list, the adapter registry, and the policy registry: a
// kind admitted into gold must have an adapter, and every adapter kind must
// register a ReturnsPolicy — except fred, a pure FX reference source that
// emits no accounts/positions/transactions, so nothing of it ever reaches the
// returns engine (its Known=false fallback is pinned by
// TestRegisteredFlowPolicies).
func TestEveryCheckKindHasAdapterAndPolicy(t *testing.T) {
	checkKinds := silverKindCheckList(t)
	registered := silver.Kinds()

	inCheck := make(map[string]bool, len(checkKinds))
	for _, k := range checkKinds {
		inCheck[k] = true
	}
	inRegistry := make(map[string]bool, len(registered))
	for _, k := range registered {
		inRegistry[k] = true
	}

	for _, k := range checkKinds {
		if !inRegistry[k] {
			t.Errorf("kind %q: in the silver_sources CHECK but no silver adapter registers it", k)
		}
	}
	for _, k := range registered {
		if !inCheck[k] {
			t.Errorf("kind %q: adapter registered but missing from the silver_sources CHECK (add a whitelist migration)", k)
		}
	}

	for _, k := range checkKinds {
		if k == "fred" {
			continue
		}
		if rp, ok := returns.ReturnsPolicyFor(k); !ok || !rp.Flow.Known {
			t.Errorf("kind %q: no registered ReturnsPolicy — add internal/silver/%s/policy.go and the test blank imports", k, k)
		}
	}
}

// TestDepositBankPlumbingHidden locks the deposit-bank conduit policy end to
// end: a chase-kind source emits NO rows of its own at the accounts or
// sources grain (AccountsGrainHidden plumbing), while its balances and flows
// still enter the global aggregate.
func TestDepositBankPlumbingHidden(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "chx", "chase")
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "chx", "CHK", canonical.AccountKindCash, nil,
		[]snap{{a, 4000}, {b, 4100}},
		[]txn{{dy(2024, time.June, 3), canonical.TxKindDeposit, 100}})
	seedAcct(t, db, ctx, "sq", "BRK", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1000}, {b, 1100}}, nil)
	end := eod(2024, time.December, 30)

	acctRows, err := RunReturns(ctx, db, params("accounts", 0, end))
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	if _, ok := summaryFor(acctRows, "CHK"); ok {
		t.Error("hidden plumbing must emit no accounts-grain row")
	}
	if _, ok := summaryFor(acctRows, "BRK"); !ok {
		t.Error("the non-plumbing account must still emit its row")
	}

	srcRows, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("RunReturns sources: %v", err)
	}
	if _, ok := summaryFor(srcRows, "chx"); ok {
		t.Error("an all-plumbing source must emit no sources-grain row")
	}

	// Global: the chase balance and its deposit are inside the math — start
	// 5000, end 5200, net flow 100.
	globRows, err := RunReturns(ctx, db, params("global", 0, end))
	if err != nil {
		t.Fatalf("RunReturns global: %v", err)
	}
	g, ok := summaryFor(globRows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if v, ok := parseFloatPtr(g.StartValue); !ok || v != 5000 {
		t.Errorf("global start = %v, want 5000 (plumbing balance included)", g.StartValue)
	}
	if nf := netFlowOf(t, globRows, ""); nf != 100 {
		t.Errorf("global net_flow = %.2f, want 100 (plumbing deposit counted)", nf)
	}
	if g.TWR == nil || qualityHas(g, "unknown_adapter_policy") {
		t.Errorf("global must compute with a known policy (TWR=%v q=%v)", g.TWR, g.Quality)
	}
}

// TestCashAccountsHiddenWhateverTheSource pins the kind rule: a cash account
// emits no rows of its own even on a source whose policy shows its accounts
// (fidelity-kind, as a statement archive's deposit accounts are), so a
// portfolio or source made only of cash accounts emits none either. A
// portfolio holding anything else still shows, and the cash still counts in
// every aggregate that contains it.
func TestCashAccountsHiddenWhateverTheSource(t *testing.T) {
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

	g, ok := summaryFor(run("global"), "")
	if !ok {
		t.Fatal("no global row")
	}
	if v, ok := parseFloatPtr(g.StartValue); !ok || v != 5200 {
		t.Errorf("global start = %v, want 5200 (every cash account included)", g.StartValue)
	}
}
