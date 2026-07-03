package gold

import (
	"math"
	"testing"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
)

// These tests lock the cointracking migration onto the pluggable ReturnsPolicy:
// OnboardScope=OnboardNone (no phantom per-constituent onboarding for cold-storage
// wallets that debut mid-window funded by within-entity crypto transfers already
// excluded from flows) and AccountsGrainMeaningless=true (per-wallet return rows
// are meaningless — coins sweep between wallets on arrival — so the accounts grain
// is blanked while portfolios/sources/global stay valid). Source id -> silver_kind
// "cointracking" selects that policy. All data synthetic / placeholder (CLAUDE.md §4).

// TestCointrackingOnboardNoneNoPhantomInflow is the root-cause regression: two
// exchange wallets funded by fiat Deposits sweep their coins (crypto transfer_out)
// into two cold-storage wallets that debut mid-window. Under OnboardNone the
// aggregate flow set must contain ONLY the fiat deposits — no synthetic onboarding
// for the debuting cold wallets — so net_flow == the fiat sum and the aggregate TWR
// is positive and strictly below the (flow-honest) MWR. Under the pre-migration
// per-constituent default the cold wallets' +debut-value would be onboarded as
// phantom inflows, inflating net_flow and dragging TWR negative.
func TestCointrackingOnboardNoneNoPhantomInflow(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ctx", "cointracking")

	wStart := dy(2023, time.January, 2)
	fund := dy(2023, time.March, 31) // in-window fiat deposits
	sweep := dy(2023, time.June, 30)
	end := dy(2023, time.December, 29)

	// Two exchange wallets alive from wStart already holding coin (a real opening
	// base), each funded in-window by an additional fiat Deposit, then swept out
	// (crypto transfer_out, excluded from flows under RegimeCryptoPartial) so their
	// value drains to ~0 on the sweep day while the cold wallets debut same-day.
	// EXCH_A: base 6000 -> +6000 deposit = 12000, swept to ~0.
	seedAcct(t, db, ctx, "ctx", "EXCH_A", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 6000}, {fund, 12000}, {sweep, 1}, {end, 1}},
		[]txn{{fund, canonical.TxKindDeposit, 6000}, {sweep, canonical.TxKindTransferOut, -12000}})
	// EXCH_B: base 4000 -> +4000 deposit = 8000, swept to ~0.
	seedAcct(t, db, ctx, "ctx", "EXCH_B", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 4000}, {fund, 8000}, {sweep, 1}, {end, 1}},
		[]txn{{fund, canonical.TxKindDeposit, 4000}, {sweep, canonical.TxKindTransferOut, -8000}})
	// Two cold-storage wallets debuting on the sweep day, funded by the internal
	// sweep (crypto transfer_in, excluded), value-neutral against the exchange drop:
	// COLD_A debuts at 12000, COLD_B at 8000, then both appreciate ~10% by end.
	seedAcct(t, db, ctx, "ctx", "COLD_A", canonical.AccountKindBrokerage, nil,
		[]snap{{sweep, 12000}, {end, 13200}},
		[]txn{{sweep, canonical.TxKindTransferIn, 12000}})
	seedAcct(t, db, ctx, "ctx", "COLD_B", canonical.AccountKindBrokerage, nil,
		[]snap{{sweep, 8000}, {end, 8800}},
		[]txn{{sweep, canonical.TxKindTransferIn, 8000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}

	// Only the two fiat deposits (6000 + 4000) cross the boundary; the sweep legs
	// are crypto transfer_in/out (excluded) and the cold-wallet debuts must NOT be
	// onboarded. net_flow == the fiat sum, exactly.
	if nf := netFlowOf(t, rows, "ctx"); math.Abs(nf-10000) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 10000 (only the fiat deposits; no phantom "+
			"onboarding of the debuting cold wallets)", nf)
	}
	s, ok := summaryFor(rows, "ctx")
	if !ok {
		t.Fatal("no ctx source row")
	}
	if s.TWR == nil || s.MWR == nil {
		t.Fatalf("ctx TWR/MWR should both be defined (TWR=%v MWR=%v)", s.TWR, s.MWR)
	}
	// Real growth (10000 -> ~12000) is a positive return. With no phantom inflow the
	// aggregate TWR is positive, and MWR (XIRR on the same 10000 in) exceeds it
	// because the capital was in for less than the full window.
	if *s.TWR <= 0 {
		t.Errorf("aggregate TWR = %.4f, want positive (phantom onboarding would drag it negative)", *s.TWR)
	}
	if !(*s.TWR < *s.MWR) {
		t.Errorf("want TWR (%.4f) strictly below MWR (%.4f)", *s.TWR, *s.MWR)
	}
}

// TestCointrackingOnboardNoneKeepsLateDebutDeposit guards the debut-region
// subsumption fix. Under OnboardNone there is no synthetic onboarding to replace a
// subsumed pre-debut flow, so a real fiat deposit funding a wallet that debuts
// AFTER winFrom must be KEPT. Before the fix it was subsumed and vanished, so the
// funded value read as pure performance on the tiny opening base.
func TestCointrackingOnboardNoneKeepsLateDebutDeposit(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ctx", "cointracking")

	wStart := dy(2023, time.January, 2)
	late := dy(2023, time.June, 30) // a second wallet debuts here, funded by fiat
	end := dy(2023, time.December, 29)

	// SEED anchors winFrom with a tiny opening base, alive across the window.
	seedAcct(t, db, ctx, "ctx", "SEED", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 20}, {late, 20}, {end, 20}}, nil)
	// LATE debuts after winFrom with a fiat Deposit on its debut day; that deposit
	// is external capital and must survive (not be subsumed as if onboarded).
	seedAcct(t, db, ctx, "ctx", "LATE", canonical.AccountKindBrokerage, nil,
		[]snap{{late, 5000}, {end, 5000}},
		[]txn{{late, canonical.TxKindDeposit, 5000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// The late fiat deposit must appear in net_flow. Before the fix it was subsumed
	// (net_flow 0), so the +5000 value jump on the 20 base read as a huge gain.
	if nf := netFlowOf(t, rows, "ctx"); math.Abs(nf-5000) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 5000 (late-debut deposit kept under OnboardNone)", nf)
	}
}

// TestCointrackingDeadCoinClosureSurvivesOnboardNone proves genuine drain-to-zero
// losses survive OnboardNone: a wallet draining to ~0 while STILL emitting a real
// (zeroing) snapshot books a ClosureFlow outflow (closureDay != 0). OnboardNone
// only suppresses synthetic INFLOWS at debut; the closure path is untouched.
func TestCointrackingDeadCoinClosureSurvivesOnboardNone(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ctdead", "cointracking")

	wStart := dy(2023, time.January, 2)
	mid := dy(2023, time.June, 30)
	end := dy(2023, time.December, 29)

	// ANCHOR stays flat so the entity has a healthy positive base throughout (both
	// wallets are in V0 at wStart, so there are no in-window fiat flows at all).
	seedAcct(t, db, ctx, "ctdead", "ANCHOR", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 10000}, {mid, 10000}, {end, 10000}}, nil)
	// DEADCOIN: alive from wStart worth 2000, then the token goes to ~0 (a real
	// zeroing snapshot at end) — a genuine total loss, NOT a feed drop. The closure
	// path books a ClosureFlow outflow (closureDay != 0) regardless of OnboardNone.
	seedAcct(t, db, ctx, "ctdead", "DEADCOIN", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 2000}, {mid, 2000}, {end, 0}}, nil)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "ctdead")
	if !ok {
		t.Fatal("no ctdead source row")
	}
	// No fiat flows crossed the boundary in-window: net_flow is 0. The closure
	// outflow is a synthetic zeroing marker (net_flow is the sum of the real flow
	// set; the closure flow's sign makes it net to 0 against nothing else here) —
	// the point is the drain-to-zero LOSS surfaces in TWR, not a phantom inflow.
	if nf := netFlowOf(t, rows, "ctdead"); math.Abs(nf) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 0 (no in-window fiat flows)", nf)
	}
	// The dead coin is a real ~-17% drag: base 12000 at wStart, 10000 at end, no
	// flows, so TWR must be negative — proving the closure loss survived OnboardNone.
	if s.TWR == nil || *s.TWR >= 0 {
		t.Errorf("TWR = %v, want a negative real drain-to-zero loss", s.TWR)
	}
}

// TestCointrackingFeedDropContributesZeroNoFabricatedOutflow proves a feed-drop
// artifact (a wallet whose series ENDS while still nonzero — dropped out of a later
// same-source snapshot without a zeroing row) contributes 0 thereafter with NO
// fabricated outflow, and sets the droppedNonzero DISPLAY flag only. OnboardNone
// does not touch this path.
func TestCointrackingFeedDropContributesZeroNoFabricatedOutflow(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ctdrop", "cointracking")

	wStart := dy(2023, time.January, 2)
	mid := dy(2023, time.June, 30)
	end := dy(2023, time.December, 29)

	// LIVE runs the full window (defines globalMax = end), both wallets alive from
	// wStart (in V0). LIVE takes one in-window fiat deposit so there IS a real flow
	// to compare against — the feed-dropped wallet must add nothing to it.
	seedAcct(t, db, ctx, "ctdrop", "LIVE", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 10000}, {mid, 12000}, {end, 13000}},
		[]txn{{mid, canonical.TxKindDeposit, 2000}})
	// DROPPED: still worth 3000 at its last emitted snapshot (mid), then feed-dropped
	// — its series ends before globalMax while nonzero (no zeroing row).
	seedAcct(t, db, ctx, "ctdrop", "DROPPED", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 3000}, {mid, 3000}}, nil)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "ctdrop")
	if !ok {
		t.Fatal("no ctdrop source row")
	}
	// net_flow == the single in-window +2000 deposit; NO fabricated outflow was
	// booked for the feed-dropped wallet (staleness must not synthesize a divestment).
	if nf := netFlowOf(t, rows, "ctdrop"); math.Abs(nf-2000) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 2000 (deposit only; no fabricated drop outflow)", nf)
	}
	if !qualityHas(s, "dropped_while_nonzero") {
		t.Errorf("quality = %v, want dropped_while_nonzero display flag", s.Quality)
	}
}

// TestCointrackingAccountsGrainMeaningless locks AccountsGrainMeaningless: on the
// accounts (per-wallet) grain a cointracking wallet row keeps start/end values but
// blanks TWR/MWR to n/a plus the accounts_grain_meaningless flag, while the
// portfolios, sources, and global grains all still compute real numbers.
func TestCointrackingAccountsGrainMeaningless(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ctag", "cointracking")

	wStart := dy(2023, time.January, 2)
	end := dy(2023, time.December, 29)

	pf := "PF1"
	seedAcct(t, db, ctx, "ctag", "WALLET", canonical.AccountKindBrokerage, &pf,
		[]snap{{wStart, 5000}, {end, 6000}},
		[]txn{{wStart, canonical.TxKindDeposit, 5000}})

	// Accounts grain: the wallet row keeps start/end but TWR/MWR go n/a + flag.
	acctRows, err := RunReturns(ctx, db, params("accounts", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	w, ok := summaryFor(acctRows, "WALLET")
	if !ok {
		t.Fatal("no WALLET accounts row")
	}
	if w.StartValue == nil || w.EndValue == nil {
		t.Errorf("wallet start/end must be populated (start=%v end=%v)", w.StartValue, w.EndValue)
	}
	if w.TWR != nil || w.TWRAnnualized != nil || w.MWR != nil || w.MWRAnnualized != nil {
		t.Errorf("wallet TWR/MWR must be n/a (got TWR=%v MWR=%v)", w.TWR, w.MWR)
	}
	if !qualityHas(w, "accounts_grain_meaningless") {
		t.Errorf("quality = %v, want accounts_grain_meaningless", w.Quality)
	}

	// Portfolios grain: a coherent unit, so it computes a real number and is NOT gated.
	pfRows, err := RunReturns(ctx, db, params("portfolios", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns portfolios: %v", err)
	}
	pfr, ok := summaryFor(pfRows, "PF1")
	if !ok {
		t.Fatal("no PF1 portfolios row")
	}
	if pfr.TWR == nil {
		t.Errorf("portfolios-grain TWR must be a real number, got nil")
	}
	if qualityHas(pfr, "accounts_grain_meaningless") {
		t.Errorf("portfolios grain must NOT be gated accounts_grain_meaningless: %v", pfr.Quality)
	}

	// Sources grain: the aggregate is valid, so a real number is computed.
	srcRows, err := RunReturns(ctx, db, params("sources", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns sources: %v", err)
	}
	s, ok := summaryFor(srcRows, "ctag")
	if !ok {
		t.Fatal("no ctag source row")
	}
	if s.TWR == nil {
		t.Errorf("sources-grain TWR must be a real number, got nil")
	}
	if qualityHas(s, "accounts_grain_meaningless") {
		t.Errorf("sources grain must NOT be gated accounts_grain_meaningless: %v", s.Quality)
	}

	// Global grain: also never gated.
	globRows, err := RunReturns(ctx, db, params("global", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns global: %v", err)
	}
	g, ok := summaryFor(globRows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if g.TWR == nil || qualityHas(g, "accounts_grain_meaningless") {
		t.Errorf("global grain must compute a real number and not be gated (TWR=%v q=%v)", g.TWR, g.Quality)
	}
}
