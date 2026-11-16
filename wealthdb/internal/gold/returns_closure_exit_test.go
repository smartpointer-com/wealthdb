package gold

import (
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestFinalDayClosureBooksWhatIsLeft: an account zeroes on the spine's last day,
// today, and the synthetic closure books its carried value. Money that left
// as a flow since its last valuation is not in that value any more, so the
// closure books only the rest, and the drain counts once.
func TestFinalDayClosureBooksWhatIsLeft(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	today := dy(now.Year(), now.Month(), now.Day())
	start, drain, shown := today-60*86400, today-5*86400, today-3*86400
	wd := canonical.TxKindWithdrawal
	cases := []struct {
		name  string
		old   []snap
		txns  []txn
		match bool
		flow  float64
	}{
		{"a drain, then the zeroing", []snap{{start, 6000}, {today, 0}}, []txn{{drain, wd, -6000}}, false, -6000},
		{"part drained, then the zeroing", []snap{{start, 6000}, {today, 0}}, []txn{{drain, wd, -2000}}, false, -6000},
		{"no drain", []snap{{start, 6000}, {today, 0}}, nil, false, -6000},
		{"a drain a valuation shows before the zeroing", []snap{{start, 6000}, {shown, 4000}, {today, 0}},
			[]txn{{drain, wd, -2000}}, false, -6000},
		// A stored link drops both legs of a wire to another source, so the
		// money stays inside at the global grain and the closure books nothing.
		{"a matched drain to another source", []snap{{start, 6000}, {today, 0}}, []txn{{drain, wd, -6000}}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "fidx", "fidelity")
			seedAcct(t, db, ctx, "fidx", "ANCHOR", canonical.AccountKindBrokerage, nil, []snap{{start, 10000}, {today, 10000}}, nil)
			seedAcct(t, db, ctx, "fidx", "OLD", canonical.AccountKindBrokerage, nil, c.old, c.txns)
			level, entity, p := "sources", "fidx", params("sources", 0, eod(now.Year()+1, time.January, 1))
			if c.match {
				seedReturnsSource(t, db, ctx, "bankx", "fidelity")
				seedAcct(t, db, ctx, "bankx", "PEER", canonical.AccountKindBrokerage, nil,
					[]snap{{start, 1000}, {drain, 7000}, {today, 7000}}, []txn{{drain, canonical.TxKindDeposit, 6000}})
				seedTransferLink(t, db, ctx, "fidx", "OLD-tx0", "bankx", "PEER-tx0")
				level, entity = "global", ""
				p = tmParams(level, 0, eod(now.Year()+1, time.January, 1))
			}
			rows, err := RunReturns(ctx, db, p)
			if err != nil {
				t.Fatalf("RunReturns %s: %v", level, err)
			}
			if nf := netFlowOf(t, rows, entity); math.Abs(nf-c.flow) > 1e-6 {
				t.Errorf("net_flow = %.2f, want %.2f", nf, c.flow)
			}
			s, ok := summaryFor(rows, entity)
			if !ok || s.TWR == nil {
				t.Fatalf("no TWR: %+v", s)
			}
			if math.Abs(*s.TWR) > 1e-9 {
				t.Errorf("TWR = %.6f, want 0", *s.TWR)
			}
		})
	}
}

// TestClosedEntityEndsOnItsReturn: every account of a source or portfolio
// empties. Its exit legs fall in the zero tail, where the value shows no
// change, so they leave as one closure flow on the zeroing day, the day the
// value shows the money gone. The entity's chain then ends on its return up
// to the exit. A move to another member cancels at a grain that holds both,
// so those grains do not change. An account that zeroes with no exit leg is
// a write-off and keeps its loss.
func TestClosedEntityEndsOnItsReturn(t *testing.T) {
	t.Parallel()
	t0, t1 := dy(2024, time.January, 31), dy(2024, time.February, 29)
	zero := dy(2024, time.March, 15)
	later := zero + 3*86400
	const (
		in, out = canonical.TxKindTransferIn, canonical.TxKindTransferOut
		wd      = canonical.TxKindWithdrawal
	)
	pa, pb := "PA", "PB"
	type acct struct {
		src, id string
		pf      *string
		snaps   []snap
		txns    []txn
	}
	type want struct {
		level, entity string
		flow, twr     float64
	}
	apr20, apr30 := dy(2024, time.April, 20), dy(2024, time.April, 30)
	cases := []struct {
		name  string
		accts []acct
		wants []want
		split bool // the flows fall in more than one month
	}{
		{"a source empties into another on the zeroing day", []acct{
			{"srca", "A1", nil, []snap{{t0, 10000}, {t1, 11000}, {zero, 0}}, []txn{{zero, out, -11000}}},
			{"srcb", "B1", nil, []snap{{t0, 5000}, {t1, 5000}, {zero, 16000}}, []txn{{zero, in, 11000}}},
		}, []want{
			{"sources", "srca", -11000, 0.1},
			{"sources", "srcb", 11000, 0},
			{"global", "", 0, 16000.0/15000 - 1},
		}, false},
		{"the exit leg days after the zeroing", []acct{
			{"srca", "A1", nil, []snap{{t0, 10000}, {t1, 11000}, {zero, 0}}, []txn{{later, out, -11000}}},
			{"srcb", "B1", nil, []snap{{t0, 5000}, {t1, 5000}, {later, 16000}}, []txn{{later, in, 11000}}},
		}, []want{
			{"sources", "srca", -11000, 0.1},
			{"sources", "srcb", 11000, 0},
			{"global", "", 0, 16000.0/15000 - 1},
		}, false},
		{"zero valuations after the exit", []acct{
			{"srca", "A1", nil, []snap{{t0, 10000}, {t1, 11000}, {zero, 0}, {later, 0}, {later + 10*86400, 0}},
				[]txn{{later, out, -11000}}},
			{"srcb", "B1", nil, []snap{{t0, 5000}, {t1, 5000}, {later, 16000}}, []txn{{later, in, 11000}}},
		}, []want{
			{"sources", "srca", -11000, 0.1},
			{"global", "", 0, 16000.0/15000 - 1},
		}, false},
		// A sibling pays money through an account that zeroed earlier. The
		// zeroing never showed that money, so it leaves on the day it passed,
		// and the zeroing day books only the account's own exit.
		{"money passed through an emptied account", []acct{
			{"srca", "A1", nil, []snap{{t0, 10000}, {t1, 11000}, {apr30, 0}}, []txn{{apr20, out, -11000}}},
			{"srca", "C1", nil, []snap{{t0, 5000}, {t1, 0}},
				[]txn{{t1, wd, -5000}, {apr20, in, 11000}, {apr20, wd, -11000}}},
			{"srcb", "B1", nil, []snap{{t0, 5000}, {t1, 5000}, {apr30, 5000}}, nil},
		}, []want{
			{"sources", "srca", -16000, 1.0 / 15},
			{"global", "", -16000, 0.05},
		}, true},
		{"an exit to outside the household", []acct{
			{"srca", "A1", nil, []snap{{t0, 10000}, {t1, 11000}, {zero, 0}}, []txn{{later, wd, -11000}}},
			{"srcb", "B1", nil, []snap{{t0, 5000}, {t1, 5000}, {later, 5000}}, nil},
		}, []want{
			{"sources", "srca", -11000, 0.1},
			{"global", "", -11000, 16000.0/15000 - 1},
		}, false},
		{"a write-off keeps its loss", []acct{
			{"srca", "A1", nil, []snap{{t0, 10000}, {t1, 11000}, {zero, 0}}, nil},
			{"srcb", "B1", nil, []snap{{t0, 5000}, {t1, 5000}, {later, 5000}}, nil},
		}, []want{
			{"sources", "srca", 0, -1},
			{"global", "", 0, 5000.0/15000 - 1},
		}, false},
		{"a portfolio empties into another", []acct{
			{"srca", "A1", &pa, []snap{{t0, 4000}, {t1, 4400}, {zero, 0}}, []txn{{later, out, -4400}}},
			{"srca", "A2", &pa, []snap{{t0, 6000}, {t1, 6600}, {zero, 0}}, []txn{{later, out, -6600}}},
			{"srca", "B1", &pb, []snap{{t0, 1000}, {t1, 1000}, {later, 12000}},
				[]txn{{later, in, 4400}, {later, in, 6600}}},
		}, []want{
			{"portfolios", pa, -11000, 0.1},
			{"portfolios", pb, 11000, 0},
			{"sources", "srca", 0, 12000.0/11000 - 1},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "srca", "fidelity")
			seedReturnsSource(t, db, ctx, "srcb", "fidelity")
			for _, a := range c.accts {
				seedAcct(t, db, ctx, a.src, a.id, canonical.AccountKindBrokerage, a.pf, a.snaps, a.txns)
			}
			// A window to today, and one that ends after the exit but before the
			// spine's last day, read the same. The month of the zeroing holds the
			// whole net flow: the exit is dated where the value shows it.
			for _, to := range []int64{eod(2027, time.January, 1), eod(2024, time.April, 30)} {
				byLevel := map[string][]ReturnRow{}
				for _, level := range []string{"accounts", "portfolios", "sources", "global"} {
					rows, err := RunReturns(ctx, db, params(level, 0, to))
					if err != nil {
						t.Fatalf("RunReturns %s: %v", level, err)
					}
					byLevel[level] = rows
				}
				for _, w := range c.wants {
					rows := byLevel[w.level]
					if nf := netFlowOf(t, rows, w.entity); math.Abs(nf-w.flow) > 1e-6 {
						t.Errorf("to %d, %s %q: net_flow = %.2f, want %.2f", to, w.level, w.entity, nf, w.flow)
					}
					s, _ := summaryFor(rows, w.entity)
					if s.TWR == nil {
						t.Errorf("to %d, %s %q: no TWR: %+v", to, w.level, w.entity, s)
					} else if math.Abs(*s.TWR-w.twr) > 1e-9 {
						t.Errorf("to %d, %s %q: TWR = %.6f, want %.6f", to, w.level, w.entity, *s.TWR, w.twr)
					}
					p := params(w.level, 0, to)
					p.Period = "monthly"
					monthly, err := RunReturns(ctx, db, p)
					if err != nil {
						t.Fatalf("RunReturns monthly: %v", err)
					}
					found := false
					for _, r := range monthly {
						if c.split {
							found = true
							break
						}
						if r.IsSummary || r.EntityID != w.entity || r.Period != "2024-03" {
							continue
						}
						found = true
						if nf, _ := parseFloatPtr(r.NetFlow); math.Abs(nf-w.flow) > 1e-6 {
							t.Errorf("to %d, %s %q: March net_flow = %.2f, want %.2f", to, w.level, w.entity, nf, w.flow)
						}
					}
					if !found {
						t.Errorf("to %d, %s %q: no March row", to, w.level, w.entity)
					}
				}
				// The value identity: every grain's end values sum to the global's.
				sum := func(level string) (total float64) {
					for _, r := range byLevel[level] {
						if v, ok := parseFloatPtr(r.EndValue); ok && r.IsSummary {
							total += v
						}
					}
					return total
				}
				g := sum("global")
				for _, level := range []string{"accounts", "portfolios", "sources"} {
					if s := sum(level); math.Abs(s-g) > 1e-6 {
						t.Errorf("to %d, %s end values sum to %.2f, global %.2f", to, level, s, g)
					}
				}
			}
		})
	}
}

// TestBridgeReadsAnExitAsAFlow: an account empties, and its exit leg to
// outside the household is dated after the zeroing. The closure books that
// leg, so the bridge reads it as money that left, in net_flow, not as value
// that left tracking.
func TestBridgeReadsAnExitAsAFlow(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "srca", "fidelity")
	t0, zero := dy(2024, time.January, 31), dy(2024, time.March, 15)
	seedAcct(t, db, ctx, "srca", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 10000}, {zero, 0}}, []txn{{zero + 3*86400, canonical.TxKindWithdrawal, -10000}})
	seedAcct(t, db, ctx, "srca", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 5000}, {dy(2024, time.June, 30), 5000}}, nil)
	rows := runBridge(t, db, ctx, bridgeParams("USD", "monthly"))
	checkIdentity(t, "USD", rows)
	for _, r := range rows {
		if r.Period != "2024-03" {
			continue
		}
		if cents(t, r.NetFlow) != -1000000 || cents(t, r.Onboarded) != 0 || cents(t, r.Gain) != 0 {
			t.Errorf("March: net_flow %s, onboarded %s, gain %s; want -10000, 0, 0", *r.NetFlow, *r.Onboarded, *r.Gain)
		}
		return
	}
	t.Errorf("no March row: %+v", rows)
}

// TestClosureBeforeItsExitLeg: a window that ends after the zeroing but before
// the exit leg still holds the exit. The account's own tail legs past the
// window's end count towards the closure flow on the zeroing day, so the
// window reads the source's return up to the exit, not a write-off.
func TestClosureBeforeItsExitLeg(t *testing.T) {
	t.Parallel()
	t0, t1 := dy(2024, time.January, 31), dy(2024, time.February, 29)
	zero := dy(2024, time.March, 15)
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "srca", "fidelity")
	seedReturnsSource(t, db, ctx, "srcb", "fidelity")
	seedAcct(t, db, ctx, "srca", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 10000}, {t1, 11000}, {zero, 0}}, []txn{{zero + 3*86400, canonical.TxKindTransferOut, -11000}})
	seedAcct(t, db, ctx, "srcb", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 5000}, {t1, 5000}, {zero + 3*86400, 16000}}, []txn{{zero + 3*86400, canonical.TxKindTransferIn, 11000}})
	to := zero + 86400 + 43199
	for _, w := range []struct {
		level, entity string
		flow, twr     float64
	}{
		{"sources", "srca", -11000, 0.1},
		{"accounts", "A1", 0, -1},
		{"global", "", -11000, 16000.0/15000 - 1},
	} {
		rows, err := RunReturns(ctx, db, params(w.level, 0, to))
		if err != nil {
			t.Fatal(err)
		}
		if nf := netFlowOf(t, rows, w.entity); math.Abs(nf-w.flow) > 1e-6 {
			t.Errorf("%s: net_flow = %.2f, want %.2f", w.level, nf, w.flow)
		}
		s, _ := summaryFor(rows, w.entity)
		if s.TWR == nil || math.Abs(*s.TWR-w.twr) > 1e-9 {
			t.Errorf("%s: TWR = %v, want %.6f", w.level, s.TWR, w.twr)
		}
	}
}
