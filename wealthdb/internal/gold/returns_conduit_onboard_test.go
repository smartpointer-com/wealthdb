package gold

import (
	"math"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// These tests lock the UBS per-entity-once onboarding semantics (OnboardScope=
// OnboardPerEntityOnce + ConduitKinds:[cash] + Inception=first-real-snapshot),
// which the DEFAULT-policy staggered tests never exercise (isConduit()==false,
// perEntityGroups empty). Source id -> silver_kind "ubs" selects that policy.
// All data synthetic / placeholder (CLAUDE.md §4).

// TestUBSConduitOnboardStepNoSameDayContamination is the regression: the
// per-entity-once step-up must be the newly-debuting constituents' first value NET
// OF same-day sibling FUNDING drops only — NOT the raw aggregate calendar delta,
// which sweeps in same-day external deposits (already booked as their own flow) and
// organic market appreciation on existing holdings (genuine RETURN misread as an
// inflow).
//
// Fixture (window anchored at SEC0's first real snapshot = wStart):
//   - SEC0 (brokerage) alive from wStart; on the debut day it rises +1000, of which
//     +500 is a genuine external DEPOSIT (a real flow) and +500 is MARKET gain.
//   - CASH (cash conduit) drops exactly 800 on the debut day — internal funding of
//     the new securities account.
//   - SEC1 (brokerage) debuts on the debut day worth 800 (funded by CASH's drop).
//
// The ONLY dollar crossing the entity boundary in-window is the +500 deposit, so
// net_flow must be 500. The raw-delta formula would book onboarding = ΔAggregate =
// (11000+4200+800) - (10000+5000+0) = 1000 ON TOP OF the 500 deposit = 1500,
// fabricating 1000 (market +500 + the deposit counted again +500, minus the netted
// cash/securities move). groupOnboardStep books 800 - 800 = 0, so net_flow = 500.
func TestUBSConduitOnboardStepNoSameDayContamination(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubsx", "ubs")

	wStart := dy(2023, time.January, 2)
	debut := dy(2023, time.September, 30)
	end := dy(2024, time.June, 30)

	// SEC0: +1000 on debut day = +500 deposit (real flow) + 500 market gain.
	seedAcct(t, db, ctx, "ubsx", "SEC0", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 10000}, {debut, 11000}, {end, 11000}},
		[]txn{{debut, canonical.TxKindDeposit, 500}})
	// CASH conduit: drops exactly 800 on debut day to fund SEC1 (internal — no txn).
	seedAcct(t, db, ctx, "ubsx", "CASH", canonical.AccountKindCash, nil,
		[]snap{{wStart, 5000}, {debut, 4200}, {end, 4200}}, nil)
	// SEC1: debuts worth 800, funded entirely by CASH's drop (internal).
	seedAcct(t, db, ctx, "ubsx", "SEC1", canonical.AccountKindBrokerage, nil,
		[]snap{{debut, 800}, {end, 800}}, nil)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.June, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// Exactly the +500 external deposit crosses the boundary; the internal cash->
	// securities move nets to 0 onboarding and the +500 market gain is NOT a flow.
	if nf := netFlowOf(t, rows, "ubsx"); math.Abs(nf-500) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 500 (raw-delta onboarding would give 1500: "+
			"same-day market gain + deposit double-swept into the step-up)", nf)
	}
	s, ok := summaryFor(rows, "ubsx")
	if !ok {
		t.Fatal("no ubsx source row")
	}
	if s.TWR == nil {
		t.Fatal("ubsx TWR should be defined")
	}
	// Sane positive TWR: real growth is the +500 market gain over a ~15000 base.
	// A fabricated +1000 phantom inflow would depress TWR well below this.
	if *s.TWR <= 0 || *s.TWR > 0.15 {
		t.Errorf("conduit TWR = %.4f, want a small positive value (no phantom inflow)", *s.TWR)
	}
}

// TestUBSConduitInternalMoveNetsToZeroOnboarding is the pure conduit case with NO
// same-day contamination: a cash->securities move that debuts a securities account
// while its funding cash conduit drops by the same amount must onboard NOTHING (the
// capital was already booked once as the inception base at wStart). Under the
// per-CONSTITUENT default this same shape would phantom-onboard the new account's
// full value; per-entity-once nets it away.
func TestUBSConduitInternalMoveNetsToZeroOnboarding(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubsy", "ubs")

	wStart := dy(2023, time.January, 2)
	debut := dy(2023, time.September, 30)
	end := dy(2024, time.June, 30)

	// SEC0 anchors the window and stays flat (no market move, no deposit).
	seedAcct(t, db, ctx, "ubsy", "SEC0", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 10000}, {debut, 10000}, {end, 10000}}, nil)
	// CASH drops 2000 to fund SEC1 (internal); SEC1 debuts worth exactly 2000.
	seedAcct(t, db, ctx, "ubsy", "CASH", canonical.AccountKindCash, nil,
		[]snap{{wStart, 6000}, {debut, 4000}, {end, 4000}}, nil)
	seedAcct(t, db, ctx, "ubsy", "SEC1", canonical.AccountKindBrokerage, nil,
		[]snap{{debut, 2000}, {end, 2000}}, nil)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.June, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// No external capital crossed the boundary in-window: net_flow must be 0. A
	// per-constituent (or raw-delta) onboarding of SEC1's +2000 would appear here.
	if nf := netFlowOf(t, rows, "ubsy"); math.Abs(nf) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 0 (internal cash->securities move, nothing onboarded)", nf)
	}
}

// TestUBSConduitGenuineExternalDebutOnboards is the must-NOT-over-correct guard: a
// securities account that debuts mid-window from GENUINELY NEW external capital
// (no sibling funding drop that day) still onboards its full value. Otherwise the
// fix would swing the other way and understate capital.
func TestUBSConduitGenuineExternalDebutOnboards(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubsz", "ubs")

	wStart := dy(2023, time.January, 2)
	debut := dy(2023, time.September, 30)
	end := dy(2024, time.June, 30)

	// SEC0 anchors and stays flat; CASH stays flat (no funding drop on debut day).
	seedAcct(t, db, ctx, "ubsz", "SEC0", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 10000}, {debut, 10000}, {end, 10000}}, nil)
	seedAcct(t, db, ctx, "ubsz", "CASH", canonical.AccountKindCash, nil,
		[]snap{{wStart, 3000}, {debut, 3000}, {end, 3000}}, nil)
	// SEC1 debuts worth 4000 of brand-new external capital; no sibling drop.
	seedAcct(t, db, ctx, "ubsz", "SEC1", canonical.AccountKindBrokerage, nil,
		[]snap{{debut, 4000}, {end, 4000}}, nil)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.June, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// SEC1's full 4000 debut value is onboarded (no offsetting sibling drop).
	if nf := netFlowOf(t, rows, "ubsz"); math.Abs(nf-4000) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 4000 (genuine external debut onboards its full value)", nf)
	}
}
