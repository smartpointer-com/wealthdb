package gold

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// These regression tests lock the staggered-inception semantics: a constituent
// that joins (or leaves) the aggregate value spine mid-window has its pre-debut
// funding (or closure drain) SUBSUMED by the synthetic onboarding (or closure)
// amount, never double-counted. Subsumption (subsumesAtDebut / subsumesAtClosure
// in returns_compute.go) covers the whole pre-debut region at any distance from
// debut — an account fed by month-end snapshots can be funded months before its
// first one — so every fixture below dates the real funding well before debut, far outside
// the ±nettingWindowDay transfer-netting window. All data synthetic (AGENTS.md §4).

// netFlowOf returns the summary row's net_flow as a float for an entity.
func netFlowOf(t *testing.T, rows []ReturnRow, entity string) float64 {
	t.Helper()
	r, ok := summaryFor(rows, entity)
	if !ok {
		t.Fatalf("no summary row for %q", entity)
	}
	v, ok := parseFloatPtr(r.NetFlow)
	if !ok {
		t.Fatalf("%q has no net_flow", entity)
	}
	return v
}

// TestStaggeredPreDebutDepositSubsumed: a late constituent funded
// by an external deposit dated MONTHS before its first month-end snapshot. The
// deposit (outside the ±nettingWindowDay netting window) must be subsumed by onboarding,
// not counted on top of it, and the aggregate TWR must come out sane/positive
// rather than driven below −100% by a pre-spine deposit booked against ΔV≈0.
func TestStaggeredPreDebutDepositSubsumed(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "viacx", "viac")
	// Window opens 2023-01-02 with A alive and flat. B debuts only at a year-end
	// snapshot worth 5000, but was funded by a +3000 deposit ~100 days earlier
	// (far outside ±3 days of debut) plus ~2000 of growth before its first snapshot.
	wStart := dy(2023, time.January, 2)
	bFund := dy(2023, time.September, 20) // pre-debut external deposit
	bDebut := dy(2023, time.December, 31) // B's first snapshot
	end := dy(2024, time.June, 30)

	seedAcct(t, db, ctx, "viacx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 10000}, {end, 10000}}, nil)
	seedAcct(t, db, ctx, "viacx", "B", canonical.AccountKindBrokerage, nil,
		[]snap{{bDebut, 5000}, {end, 5400}},
		[]txn{{bFund, canonical.TxKindDeposit, 3000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.June, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "viacx")
	if !ok {
		t.Fatal("no viacx source row")
	}
	if !qualityHas(s, "staggered_inception") {
		t.Errorf("expected staggered_inception; quality=%v", s.Quality)
	}
	// Net flow = B's full debut value (5000) booked once as onboarding; the pre-debut
	// 3000 deposit is subsumed, NOT added on top (which would give 8000).
	if nf := netFlowOf(t, rows, "viacx"); math.Abs(nf-5000) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 5000 (pre-debut deposit subsumed, not 8000)", nf)
	}
	// A flat + B's ~8%% slice growth ⇒ a small sane TWR, never the pre-fix collapse.
	if s.TWR == nil {
		t.Fatal("viacx TWR should be defined")
	}
	if *s.TWR <= 0 || *s.TWR > 0.2 {
		t.Errorf("staggered TWR = %.4f, want a small positive value (onboarding absorbed the arrival)", *s.TWR)
	}
}

// TestStaggeredTwoLateAccounts: TWO constituents debut at the
// same later snapshot, each pre-funded by an external deposit dated before debut.
// Both deposits must be subsumed; net_flow is the sum of the two debut values
// once, not doubled.
func TestStaggeredTwoLateAccounts(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "relx", "relevate")
	wStart := dy(2025, time.January, 2)
	fund := dy(2025, time.July, 10) // both pre-debut deposits land here
	debut := dy(2025, time.September, 30)
	end := dy(2026, time.June, 30)

	// An anchor account alive from the window start keeps winFrom < debut.
	seedAcct(t, db, ctx, "relx", "ANCHOR", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 50000}, {end, 55000}}, nil)
	seedAcct(t, db, ctx, "relx", "P1", canonical.AccountKindBrokerage, nil,
		[]snap{{debut, 27000}, {end, 28000}}, []txn{{fund, canonical.TxKindDeposit, 26000}})
	seedAcct(t, db, ctx, "relx", "P2", canonical.AccountKindBrokerage, nil,
		[]snap{{debut, 27000}, {end, 28000}}, []txn{{fund, canonical.TxKindDeposit, 26000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2026, time.June, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// net_flow = 27000 + 27000 onboarding only; the two 26000 deposits are subsumed,
	// so the pre-fix 27000+26000 doubling (→ ~106000) must not appear.
	if nf := netFlowOf(t, rows, "relx"); math.Abs(nf-54000) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 54000 (two debut values once; deposits subsumed)", nf)
	}
	s, _ := summaryFor(rows, "relx")
	if s.TWR == nil || *s.TWR < 0 {
		t.Errorf("two-late-account TWR = %v, want a sane non-negative value", s.TWR)
	}
}

// TestStaggeredJournalFundedNoPhantom: a late account NEW is
// funded by a pre-debut journal-IN; its sibling journal-OUT sits on an alive
// account OLD. Two scenarios are exercised together via two NEW/OLD pairs:
//
//   - UNMATCHED (different magnitudes / >window apart): netting leaves both legs.
//     The pre-debut journal-IN is subsumed by NEW's onboarding; the journal-OUT
//     stays as OLD's real outflow (OLD's value genuinely dropped). This is NOT a
//     phantom — the −X is backed by OLD's value series — and capital is counted
//     once.
//   - MATCHED (same magnitude, within ±3 days): netting annihilates the internal
//     pair BEFORE subsumption, so neither leg appears, and onboarding books NEW's
//     debut value less that funding: the money moved inside the entity.
//
// The assertion is that no orphaned negative phantom is produced and the net_flow
// equals exactly the sum the boundary values justify.
func TestStaggeredJournalFundedNoPhantom(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	wStart := dy(2023, time.January, 2)
	jrnl := dy(2023, time.June, 1)       // pre-debut journal-in funds the NEW accounts
	drop := dy(2023, time.June, 1)       // OLD accounts' value drops on the same day
	debut := dy(2023, time.December, 31) // both NEW accounts debut here
	end := dy(2024, time.June, 30)

	// --- UNMATCHED pair: OLD1 ships 4000 out; NEW1 receives a DIFFERENT-sized
	// journal-in (4200) — the heuristic cannot net them, so both survive. ---
	seedAcct(t, db, ctx, "fidx", "OLD1", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 12000}, {drop, 8000}, {end, 8200}},
		[]txn{{drop, canonical.TxKindJournal, -4000}})
	seedAcct(t, db, ctx, "fidx", "NEW1", canonical.AccountKindBrokerage, nil,
		[]snap{{debut, 4200}, {end, 4300}},
		[]txn{{jrnl, canonical.TxKindJournal, 4200}})

	// --- MATCHED pair: OLD2 ships exactly 3000 out and NEW2 receives exactly 3000
	// within the window — the internal pair nets to zero. ---
	seedAcct(t, db, ctx, "fidx", "OLD2", canonical.AccountKindBrokerage, nil,
		[]snap{{wStart, 9000}, {drop, 6000}, {end, 6100}},
		[]txn{{drop, canonical.TxKindJournal, -3000}})
	seedAcct(t, db, ctx, "fidx", "NEW2", canonical.AccountKindBrokerage, nil,
		[]snap{{drop, 3000}, {end, 3050}},
		[]txn{{drop, canonical.TxKindJournal, 3000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.June, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "fidx")
	if !ok {
		t.Fatal("no fidx source row")
	}

	// Expected surviving flow series:
	//   OLD1 journal-out  -4000            (kept: OLD1 is alive, real value drop)
	//   NEW1 onboarding   +4200            (its pre-debut +4200 journal-in subsumed)
	//   OLD2/NEW2 3000 internal pair       (netted away — both gone)
	//   NEW2 debuts at the drop day with value 3000, all of it that funding, so
	//     onboarding books nothing.
	// So net_flow = -4000 + 4200 = 200.
	want := -4000.0 + 4200.0
	if nf := netFlowOf(t, rows, "fidx"); math.Abs(nf-want) > 1e-6 {
		t.Errorf("net_flow = %.2f, want %.2f (no phantom; one count per boundary dollar)", nf, want)
	}
	// The matched internal pair must register as fully netted (0 unmatched from it).
	// OLD1↔NEW1 cannot net (different magnitude), so exactly those two legs are
	// unmatched.
	if !qualityHas(s, "unmatched_transfers=2") {
		t.Errorf("want unmatched_transfers=2 (OLD1 out + NEW1 in survive; OLD2/NEW2 net); quality=%v", s.Quality)
	}
	// Sane, finite TWR — never the sub-(-100%) chain collapse.
	if s.TWR == nil || *s.TWR < -0.9 {
		t.Errorf("journal-funded TWR = %v, want a sane finite value (no phantom collapse)", s.TWR)
	}
}

// TestStaggeredClosureNoSyntheticAfterWindow guards the boundary case where a
// constituent's zero-carry closureDay (today) falls AFTER the requested window
// end: the closure machinery must stay out of it, so the only flow over the
// window is the single real drain — never a drain PLUS a synthetic closure
// outflow. (For the in-window subsumption itself, see
// TestStaggeredPostClosureFlowSubsumed.)
func TestStaggeredClosureNoSyntheticAfterWindow(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubsx", "ubs")
	t0 := dy(2024, time.January, 2)
	// C holds 1000 at t0 and t1, then a withdrawal of the full 1000 lands in March
	// (a snapshot-less month — value carried flat at 1000), and the next snapshot in
	// July reads 0. The drain precedes the zeroing snapshot by months.
	t1 := dy(2024, time.February, 1)
	drain := dy(2024, time.March, 15)
	tZero := dy(2024, time.July, 2)

	seedAcct(t, db, ctx, "ubsx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 2000}, {tZero, 2100}}, nil)
	seedAcct(t, db, ctx, "ubsx", "C", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, 1000}, {tZero, 0}},
		[]txn{{drain, canonical.TxKindWithdrawal, -1000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.July, 2)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// net_flow over the window is the single -1000 drain (A contributes no flows).
	// The closureDay is today — past this July window end — so no synthetic closure
	// outflow is added; counting both would give -2000.
	if nf := netFlowOf(t, rows, "ubsx"); math.Abs(nf-(-1000)) > 1e-6 {
		t.Errorf("net_flow = %.2f, want -1000 (real drain only; no out-of-window synthetic)", nf)
	}
	// A grows ~5%% and C exits ⇒ the source TWR stays close to A's return, not the
	// spuriously-negative double-count.
	s, _ := summaryFor(rows, "ubsx")
	if s.TWR == nil || *s.TWR < -0.1 || *s.TWR > 0.2 {
		t.Errorf("closure TWR = %v, want ~A's small positive return (no closure double-count)", s.TWR)
	}
}

// TestStaggeredPostClosureFlowSubsumed exercises the closure-drain branch of
// flowSubsumed (and lastNonzeroDay): with the window run out to today, a closed
// account's closureDay (today, via the 0-carry spine) is in-window, so a stray
// flow dated in the flat zero-carry gap — after the last non-zero day — is
// subsumed rather than booked as a spurious exit with no matching value move.
func TestStaggeredPostClosureFlowSubsumed(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "clx", "ubs")
	t0 := dy(2024, time.January, 2)
	zero := dy(2024, time.March, 1)
	post := dy(2024, time.April, 1)
	// A anchors the source alive to today; C reads 0 from March, then a stray
	// -500 withdrawal posts in April (after C already shows 0).
	seedAcct(t, db, ctx, "clx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 2000}, {dy(2024, time.June, 1), 2100}}, nil)
	seedAcct(t, db, ctx, "clx", "C", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {zero, 0}},
		[]txn{{post, canonical.TxKindWithdrawal, -500}})

	// Far-future end ⇒ clamped to today ⇒ C.closureDay (today) is in-window.
	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2027, time.January, 1)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "clx"); math.Abs(nf) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 0 (post-closure -500 is subsumed, not a phantom exit)", nf)
	}
}

// TestFundedDebutBooksNoNewMoney: netting cancels a pair, and the synthetic flows
// must then book only what is new to the entity. An inside move read as new money
// is a flow against a flat value: a loss of the whole amount, or a gain if the
// flow is lost instead. Every case states both the flow and the return.
func TestFundedDebutBooksNoNewMoney(t *testing.T) {
	t.Parallel()
	start := dy(2023, time.January, 31)
	move := dy(2023, time.February, 20)
	next := dy(2023, time.February, 21)
	debut := dy(2023, time.February, 28)
	later := dy(2023, time.March, 15)
	end := dy(2023, time.March, 31)
	out := []txn{{move, canonical.TxKindTransferOut, -6000}}
	in := []txn{{move, canonical.TxKindTransferIn, 6000}}
	type env struct {
		t   *testing.T
		db  *sql.DB
		ctx context.Context
	}
	acct := func(e env, src, name string, snaps []snap, txns []txn) {
		seedAcct(e.t, e.db, e.ctx, src, name, canonical.AccountKindBrokerage, nil, snaps, txns)
	}
	flat := func(e env, src string) {
		acct(e, src, "LIVE", []snap{{start, 10000}, {debut, 10000}, {end, 10000}}, nil)
	}
	one := func(e env) { seedReturnsSource(e.t, e.db, e.ctx, "fidx", "fidelity") }

	cases := []struct {
		name          string
		level, entity string
		seed          func(e env)
		flow, twr     float64
	}{
		{"siblings in one source", "sources", "fidx", func(e env) {
			one(e)
			acct(e, "fidx", "OLD", []snap{{start, 10000}, {debut, 4000}, {end, 4100}}, out)
			acct(e, "fidx", "NEW", []snap{{debut, 6000}, {end, 6100}}, in)
		}, 0, 0.02},
		{"a move between two sources", "global", "", func(e env) {
			seedReturnsSource(e.t, e.db, e.ctx, "fromx", "fidelity")
			seedReturnsSource(e.t, e.db, e.ctx, "tox", "fidelity")
			acct(e, "fromx", "OLD", []snap{{start, 10000}, {debut, 4000}, {end, 4100}}, out)
			acct(e, "tox", "NEW", []snap{{debut, 6000}, {end, 6100}}, in)
		}, 0, 0.02},
		{"growth before the debut stays onboarded", "sources", "fidx", func(e env) {
			one(e)
			acct(e, "fidx", "OLD", []snap{{start, 10000}, {debut, 4000}, {end, 4100}}, out)
			acct(e, "fidx", "NEW", []snap{{debut, 6100}, {end, 6200}}, in)
		}, 100, 200.0 / 10100},
		{"money passed on through an account that keeps nothing", "sources", "fidx", func(e env) {
			one(e)
			acct(e, "fidx", "OLD", []snap{{start, 10000}, {debut, 4000}, {end, 4100}}, out)
			acct(e, "fidx", "MID", []snap{{debut, 0}, {end, 0}},
				[]txn{{move, canonical.TxKindTransferIn, 6000}, {next, canonical.TxKindTransferOut, -6000}})
			acct(e, "fidx", "NEW", []snap{{debut, 6000}, {end, 6100}},
				[]txn{{next, canonical.TxKindTransferIn, 6000}})
		}, 0, 0.02},
		{"a late account paying into a live one brought that money too", "sources", "fidx", func(e env) {
			one(e)
			acct(e, "fidx", "OLD", []snap{{start, 10000}, {debut, 16000}, {end, 16100}}, in)
			acct(e, "fidx", "NEW", []snap{{debut, 2000}, {end, 2100}}, out)
		}, 8000, 200.0 / 18000},
		{"two debuts together, one paying the other", "sources", "fidx", func(e env) {
			one(e)
			flat(e, "fidx")
			acct(e, "fidx", "SAV", []snap{{debut, 9000}, {end, 9000}}, out)
			acct(e, "fidx", "CHK", []snap{{debut, 1000}, {end, 1000}},
				append([]txn{{next, canonical.TxKindWithdrawal, -5000}}, in...))
		}, 10000, 0},
		{"staggered debuts, the payer first", "sources", "fidx", func(e env) {
			one(e)
			flat(e, "fidx")
			acct(e, "fidx", "PAYER", []snap{{debut, 2000}, {end, 2000}}, out)
			acct(e, "fidx", "RECEIVER", []snap{{later, 5000}, {end, 5000}}, in)
		}, 7000, 0},
		{"a partner the window never holds", "sources", "fidx", func(e env) {
			one(e)
			flat(e, "fidx")
			acct(e, "fidx", "NEW", []snap{{debut, 6000}, {end, 6000}}, in)
			acct(e, "fidx", "AFTER", []snap{{dy(2023, time.April, 30), 3000}}, out)
		}, 6000, 0},
		{"a partner that has not shown the leg by the window's end", "sources", "fidx", func(e env) {
			one(e)
			acct(e, "fidx", "OLD", []snap{{start, 10000}, {dy(2023, time.April, 30), 4000}}, out)
			acct(e, "fidx", "NEW", []snap{{debut, 6000}, {end, 6000}}, in)
		}, 6000, 0},
		{"a per-entity-once payer before its own debut", "global", "", func(e env) {
			one(e)
			seedReturnsSource(e.t, e.db, e.ctx, "ubsx", "ubs")
			flat(e, "fidx")
			acct(e, "fidx", "NEW", []snap{{debut, 6000}, {end, 6000}}, in)
			acct(e, "ubsx", "PAYER", []snap{{later, 1000}, {end, 1000}}, out)
		}, 7000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			c.seed(env{t, db, ctx})
			rows, err := RunReturns(ctx, db, params(c.level, 0, eod(2023, time.March, 31)))
			if err != nil {
				t.Fatalf("RunReturns: %v", err)
			}
			if nf := netFlowOf(t, rows, c.entity); math.Abs(nf-c.flow) > 1e-6 {
				t.Errorf("net_flow = %.2f, want %.2f", nf, c.flow)
			}
			s, ok := summaryFor(rows, c.entity)
			if !ok || s.TWR == nil {
				t.Fatalf("no TWR: %+v", s)
			}
			if math.Abs(*s.TWR-c.twr) > 1e-9 {
				t.Errorf("TWR = %.6f, want %.6f", *s.TWR, c.twr)
			}
		})
	}
}

// TestClosureOnTheFinalDayBooksNoExit: an account zeroes on the spine's last day,
// today, the one case the synthetic closure books anything. Money that went to a
// sibling never left the entity, so the cancelled pair keeps the closure out of
// the flows. An account whose only row is that zero never held value on the
// spine, so a pair of its is read as a debut's.
func TestClosureOnTheFinalDayBooksNoExit(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	today := dy(now.Year(), now.Month(), now.Day())
	start, move := today-60*86400, today-2*86400
	out := []txn{{move, canonical.TxKindTransferOut, -6000}}
	in := []txn{{move, canonical.TxKindTransferIn, 6000}}
	cases := []struct {
		name          string
		closing, peer []snap
		closingLeg    []txn
		peerLeg       []txn
		flow          float64
	}{
		{"into a debut", []snap{{start, 6000}, {today, 0}}, []snap{{today, 6000}}, out, in, 0},
		{"into a live account", []snap{{start, 6000}, {today, 0}}, []snap{{start, 10000}, {today, 16000}}, out, in, 0},
		{"only a zero row, paying a live account", []snap{{today, 0}}, []snap{{start, 10000}, {today, 16000}}, out, in, 6000},
		{"only a zero row, paid by a live account", []snap{{today, 0}}, []snap{{start, 10000}, {today, 4000}}, in, out, -6000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "fidx", "fidelity")
			seedAcct(t, db, ctx, "fidx", "ANCHOR", canonical.AccountKindBrokerage, nil, []snap{{start, 10000}, {today, 10000}}, nil)
			seedAcct(t, db, ctx, "fidx", "CLOSING", canonical.AccountKindBrokerage, nil, c.closing, c.closingLeg)
			seedAcct(t, db, ctx, "fidx", "PEER", canonical.AccountKindBrokerage, nil, c.peer, c.peerLeg)
			rows, err := RunReturns(ctx, db, params("sources", 0, eod(now.Year()+1, time.January, 1)))
			if err != nil {
				t.Fatalf("RunReturns: %v", err)
			}
			if nf := netFlowOf(t, rows, "fidx"); math.Abs(nf-c.flow) > 1e-6 {
				t.Errorf("net_flow = %.2f, want %.2f", nf, c.flow)
			}
			s, ok := summaryFor(rows, "fidx")
			if !ok || s.TWR == nil {
				t.Fatalf("no TWR: %+v", s)
			}
			if math.Abs(*s.TWR) > 1e-9 {
				t.Errorf("TWR = %.6f, want 0", *s.TWR)
			}
		})
	}
}

// TestDrainBeforeAFinalDayClosure: a wire before the window out of an account that
// zeroes on the spine's last day. The synthetic closure stands for the account's
// whole carried value, so the opening value must not take the wire off as well.
func TestDrainBeforeAFinalDayClosure(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	today := dy(now.Year(), now.Month(), now.Day())
	start := today - 60*86400
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	seedAcct(t, db, ctx, "fidx", "ANCHOR", canonical.AccountKindBrokerage, nil, []snap{{start, 10000}, {today, 10000}}, nil)
	seedAcct(t, db, ctx, "fidx", "OLD", canonical.AccountKindBrokerage, nil, []snap{{start, 6000}, {today, 0}},
		[]txn{{start + 10*86400, canonical.TxKindWithdrawal, -6000}})
	rows, err := RunReturns(ctx, db, params("sources", start+20*86400, eod(now.Year()+1, time.January, 1)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "fidx")
	if !ok || s.TWR == nil {
		t.Fatalf("no TWR: %+v", s)
	}
	if math.Abs(*s.TWR) > 1e-9 {
		t.Errorf("TWR = %.6f, want 0", *s.TWR)
	}
}
