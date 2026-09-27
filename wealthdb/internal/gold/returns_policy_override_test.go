package gold

// Tests for the config-side returns_policy_overrides plumbing: the override
// composes on top of the registered policy at the single resolution point
// (newAccountData), so a regime swap provably changes what RunReturns counts,
// in both loader paths.

import (
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// TestPolicyOverrideFlowRegime pins both override directions: a nav-only
// source (manual) overridden to flow_complete counts its deposit and computes
// a real MWR; a flow-complete bank (swissquote) overridden to nav_only stops
// counting flows and picks up the nav flags. The unoverridden baselines are
// asserted first, so the deltas are attributable to the override alone.
func TestPolicyOverrideFlowRegime(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "man", "manual")
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	dep := []txn{{dy(2024, time.June, 3), canonical.TxKindDeposit, 500}}
	seedAcct(t, db, ctx, "man", "RE", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 2000}, {b, 2100}}, dep)
	seedAcct(t, db, ctx, "sq", "BRK", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 2000}, {b, 2100}}, dep)
	end := eod(2024, time.December, 30)

	// Baselines: manual ignores the deposit (nav-only), swissquote counts it.
	base, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("RunReturns baseline: %v", err)
	}
	if nf := netFlowOf(t, base, "man"); nf != 0 {
		t.Errorf("baseline manual net_flow = %.2f, want 0 (nav-only ignores flows)", nf)
	}
	m, _ := summaryFor(base, "man")
	if !qualityHas(m, "nav_only") || m.MWR != nil {
		t.Errorf("baseline manual must be nav_only with MWR n/a (q=%v MWR=%v)", m.Quality, m.MWR)
	}
	if nf := netFlowOf(t, base, "sq"); nf != 500 {
		t.Errorf("baseline swissquote net_flow = %.2f, want 500", nf)
	}

	// manual → flow_complete: the deposit becomes a counted external flow.
	fc := returns.RegimeFlowComplete
	p := params("sources", 0, end)
	p.PolicyOverrides = map[string]ReturnsPolicyOverride{"man": {FlowRegime: &fc}}
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns override man: %v", err)
	}
	if nf := netFlowOf(t, rows, "man"); nf != 500 {
		t.Errorf("overridden manual net_flow = %.2f, want 500", nf)
	}
	m, _ = summaryFor(rows, "man")
	if qualityHas(m, "nav_only") || qualityHas(m, "nav_only_capital_call_risk") {
		t.Errorf("overridden manual must lose the nav flags: %v", m.Quality)
	}
	if qualityHas(m, "unknown_adapter_policy") {
		t.Errorf("an explicit override is a known policy: %v", m.Quality)
	}
	if m.MWR == nil {
		t.Error("overridden manual must compute a real MWR")
	}

	// swissquote → nav_only: flows stop counting, nav flags appear.
	nav := returns.RegimeNavOnly
	p = params("sources", 0, end)
	p.PolicyOverrides = map[string]ReturnsPolicyOverride{"sq": {FlowRegime: &nav}}
	rows, err = RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns override sq: %v", err)
	}
	if nf := netFlowOf(t, rows, "sq"); nf != 0 {
		t.Errorf("nav-overridden swissquote net_flow = %.2f, want 0", nf)
	}
	s, _ := summaryFor(rows, "sq")
	if !qualityHas(s, "nav_only") || s.MWR != nil || !qualityHas(s, "mwr_no_flows") {
		t.Errorf("nav-overridden swissquote must be nav_only / mwr_no_flows (q=%v MWR=%v)", s.Quality, s.MWR)
	}
}

// TestPolicyOverrideAccountsGrain pins the accounts_grain override in the
// restoring direction: chase registers hidden plumbing by default (see
// internal/silver/chase/policy.go), so its rows are absent everywhere — and
// the "normal" override brings back an account the policy hid. It cannot
// bring back a cash account, which hides whatever the policy says.
func TestPolicyOverrideAccountsGrain(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "chx", "chase")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "chx", "SWEEP", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 4000}, {b, 4100}}, nil)
	seedAcct(t, db, ctx, "chx", "CHK", canonical.AccountKindCash, nil,
		[]snap{{a, 900}, {b, 950}}, nil)
	end := eod(2024, time.December, 30)

	rows, err := RunReturns(ctx, db, params("accounts", 0, end))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if _, ok := summaryFor(rows, "SWEEP"); ok {
		t.Fatal("hidden plumbing must emit no accounts row without the override")
	}

	normal := returns.AccountsGrainNormal
	p := params("accounts", 0, end)
	p.PolicyOverrides = map[string]ReturnsPolicyOverride{"chx": {AccountsGrain: &normal}}
	rows, err = RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	r, ok := summaryFor(rows, "SWEEP")
	if !ok {
		t.Fatal("no SWEEP accounts row under the normal override")
	}
	if r.TWR == nil {
		t.Error("the normal override must restore a computed accounts-grain TWR")
	}
	if qualityHas(r, "accounts_grain_meaningless") {
		t.Errorf("the normal override must not blank: %v", r.Quality)
	}
	if _, ok := summaryFor(rows, "CHK"); ok {
		t.Error("a cash account must stay hidden under the normal override")
	}
}

// TestPolicyOverrideMultiLoader pins that the multi-currency loader threads
// the override map identically to the single-currency path — otherwise the
// materialized report_returns would silently diverge from the CLI.
func TestPolicyOverrideMultiLoader(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "man", "manual")
	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "man", "RE", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 2000}, {b, 2100}}, nil)

	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		t.Fatalf("loadFxBounds: %v", err)
	}
	fc := returns.RegimeFlowComplete
	ov := map[string]ReturnsPolicyOverride{"man": {FlowRegime: &fc}}
	multi, err := loadReturnsDatasetsMulti(ctx, db, fx, ov, nil)
	if err != nil {
		t.Fatalf("loadReturnsDatasetsMulti: %v", err)
	}
	acct := multi["USD"].accts[acctKey("man", "RE")]
	if acct == nil {
		t.Fatal("multi loader missing man/RE")
	}
	if acct.policy.Regime != returns.RegimeFlowComplete || !acct.policy.IsExternal(canonical.TxKindDeposit) {
		t.Errorf("multi loader must apply the override (regime=%v)", acct.policy.Regime)
	}
}
