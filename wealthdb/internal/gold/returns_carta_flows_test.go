package gold

// Tests for the carta/equityzen flow-counting migration: their double-entry
// ledgers count only the deposit/withdrawal boundary legs, and the
// CapitalCallRisk knob keeps the honesty tag on flow-less windows.

import (
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestCartaBoundaryFlowsCounted pins the external set end to end: the
// deposit/withdrawal legs count, the holding legs (contribution here) do not,
// MWR computes for real, and no nav flag appears on a flow-bearing window.
func TestCartaBoundaryFlowsCounted(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "crt", "carta")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "crt", "PORT", canonical.AccountKindCustody, nil,
		[]snap{{a, 2000}, {b, 2600}},
		[]txn{
			// A mid-window capital call: the deposit leg counts, its paired
			// contribution leg must not (counting both would cancel the event).
			{dy(2024, time.June, 3), canonical.TxKindDeposit, 500},
			{dy(2024, time.June, 3), canonical.TxKindContribution, -500},
			// A partial distribution swept out: only the withdrawal leg counts.
			{dy(2024, time.September, 2), canonical.TxKindDistribution, 200},
			{dy(2024, time.September, 2), canonical.TxKindWithdrawal, -200},
		})
	end := eod(2024, time.December, 30)

	rows, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "crt"); nf != 300 {
		t.Errorf("net_flow = %.2f, want 300 (deposit 500 - withdrawal 200; holding legs ignored)", nf)
	}
	s, _ := summaryFor(rows, "crt")
	if s.MWR == nil {
		t.Error("a flow-bearing carta window must compute a real MWR")
	}
	if qualityHas(s, "nav_only") || qualityHas(s, "nav_only_capital_call_risk") {
		t.Errorf("flow-bearing window must carry no nav flags: %v", s.Quality)
	}
	if qualityHas(s, "unknown_adapter_policy") {
		t.Errorf("carta registers a policy: %v", s.Quality)
	}
}

// TestCapitalCallRiskFlowlessTagged pins the honesty fallback: a carta window
// with NO observed flows (a positions-only silver) keeps
// nav_only_capital_call_risk on its summary — value growth may embed
// unobserved capital calls — while a fully nav-only entity's nav_only flag
// does not appear (the regime is flow-counting).
func TestCapitalCallRiskFlowlessTagged(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "crt", "carta")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "crt", "PORT", canonical.AccountKindCustody, nil,
		[]snap{{a, 2000}, {b, 2600}}, nil)
	end := eod(2024, time.December, 30)

	rows, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "crt")
	if !ok {
		t.Fatal("no crt source row")
	}
	if !qualityHas(s, "nav_only_capital_call_risk") {
		t.Errorf("flow-less carta window must keep the capital-call tag: %v", s.Quality)
	}
	if qualityHas(s, "nav_only") {
		t.Errorf("nav_only is regime-driven and must not appear: %v", s.Quality)
	}
	if s.MWR != nil || !qualityHas(s, "mwr_no_flows") {
		t.Errorf("no flows ⇒ MWR n/a + mwr_no_flows (MWR=%v q=%v)", s.MWR, s.Quality)
	}
}

// TestCapitalCallRiskNotMaskedByOtherFlows pins that the flow-less check is
// per constituent: neither a sibling source's flows in the merged global
// entity nor a staggered constituent's own synthetic onboarding flow may
// suppress the tag for a flow-less carta account.
func TestCapitalCallRiskNotMaskedByOtherFlows(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "crt", "carta")
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	// Flow-less carta custody account, plus a SECOND carta account debuting
	// mid-window (its synthetic onboarding flow enters the entity flow series).
	seedAcct(t, db, ctx, "crt", "PORT", canonical.AccountKindCustody, nil,
		[]snap{{a, 2000}, {b, 2600}}, nil)
	seedAcct(t, db, ctx, "crt", "LATE", canonical.AccountKindCustody, nil,
		[]snap{{dy(2024, time.June, 3), 500}, {b, 700}}, nil)
	// A flow-bearing bank sibling for the global grain.
	seedAcct(t, db, ctx, "sq", "BRK", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1000}, {b, 1100}},
		[]txn{{dy(2024, time.June, 3), canonical.TxKindDeposit, 50}})
	end := eod(2024, time.December, 30)

	// Sources grain: LATE's synthetic onboarding must not mask the tag.
	srcRows, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("RunReturns sources: %v", err)
	}
	s, _ := summaryFor(srcRows, "crt")
	if !qualityHas(s, "nav_only_capital_call_risk") {
		t.Errorf("synthetic onboarding must not mask the flow-less tag: %v", s.Quality)
	}

	// Global grain: the swissquote deposit must not mask it either.
	globRows, err := RunReturns(ctx, db, params("global", 0, end))
	if err != nil {
		t.Fatalf("RunReturns global: %v", err)
	}
	g, ok := summaryFor(globRows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if !qualityHas(g, "nav_only_capital_call_risk") {
		t.Errorf("a sibling source's flows must not mask the flow-less tag: %v", g.Quality)
	}
}
