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

// TestDepositBankAccountsGrainBlanked locks the deposit-bank conduit policy
// end to end: a chase-kind account row keeps its start/end values but blanks
// TWR/MWR with accounts_grain_meaningless, carries no unknown_adapter_policy,
// and the sources grain still computes a real, ungated number.
func TestDepositBankAccountsGrainBlanked(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "chx", "chase")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "chx", "CHK", canonical.AccountKindCash, nil,
		[]snap{{a, 4000}, {b, 4100}},
		[]txn{{dy(2024, time.June, 3), canonical.TxKindDeposit, 100}})
	end := eod(2024, time.December, 30)

	acctRows, err := RunReturns(ctx, db, params("accounts", 0, end))
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	r, ok := summaryFor(acctRows, "CHK")
	if !ok {
		t.Fatal("no CHK accounts row")
	}
	if r.StartValue == nil || r.EndValue == nil {
		t.Errorf("account start/end must be populated (start=%v end=%v)", r.StartValue, r.EndValue)
	}
	if r.TWR != nil || r.MWR != nil {
		t.Errorf("account TWR/MWR must be n/a (got TWR=%v MWR=%v)", r.TWR, r.MWR)
	}
	if !qualityHas(r, "accounts_grain_meaningless") {
		t.Errorf("quality = %v, want accounts_grain_meaningless", r.Quality)
	}
	if qualityHas(r, "unknown_adapter_policy") {
		t.Errorf("quality = %v: chase registers a policy, unknown_adapter_policy must not fire", r.Quality)
	}

	srcRows, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("RunReturns sources: %v", err)
	}
	s, ok := summaryFor(srcRows, "chx")
	if !ok {
		t.Fatal("no chx source row")
	}
	if s.TWR == nil {
		t.Error("sources-grain TWR must be a real number")
	}
	if qualityHas(s, "accounts_grain_meaningless") || qualityHas(s, "unknown_adapter_policy") {
		t.Errorf("sources grain must be ungated and policy-known: %v", s.Quality)
	}
}
