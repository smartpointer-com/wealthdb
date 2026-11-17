package gold

import (
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// lossBeyondRows are the sources-grain rows of a month-end account whose
// value never shows a deposit: 100 at both month ends, 3000 paid in on
// February 5. Its one sub-period reads (100 - 100 - 3000) / (100 + 3000 ×
// 23/28), below -100%. extra adds the source's other accounts.
func lossBeyondRows(t *testing.T, method, period string, extra func(add func(acct string, kind canonical.AccountKind, s []snap))) []ReturnRow {
	t.Helper()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "bankx", "fidelity")
	jan31, feb28 := dy(2023, time.January, 31), dy(2023, time.February, 28)
	seedAcct(t, db, ctx, "bankx", "LATE", canonical.AccountKindBrokerage, nil,
		[]snap{{jan31, 100}, {feb28, 100}}, []txn{{dy(2023, time.February, 5), canonical.TxKindDeposit, 3000}})
	if extra != nil {
		extra(func(acct string, kind canonical.AccountKind, s []snap) {
			seedAcct(t, db, ctx, "bankx", acct, kind, nil, s, nil)
		})
	}
	p := params("sources", 0, eod(2023, time.February, 28))
	p.Method, p.Period = method, period
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	return rows
}

// TestLossBeyondTotal: a sub-period that loses more than everything, while
// the value the chain compounds stays at zero or above, makes the
// window's TWR n/a and says why. It changes no other figure: the row reads
// as a run without TWR does, and the per-period rows keep their own
// figures. The chain compounds the entity's assets, so a loan beside them,
// which it does not value, leaves the guard on, and so does an overdrawn
// account the rest outweighs. Where the entity's own value reads below
// zero, the arithmetic stands.
func TestLossBeyondTotal(t *testing.T) {
	t.Parallel()
	jan31, feb28 := dy(2023, time.January, 31), dy(2023, time.February, 28)

	t.Run("no debt", func(t *testing.T) {
		t.Parallel()
		both, ok := summaryFor(lossBeyondRows(t, "both", "total", nil), "bankx")
		if !ok {
			t.Fatal("no summary row")
		}
		if both.TWR != nil || both.TWRAnnualized != nil || !qualityHas(both, "loss_beyond_total") {
			t.Fatalf("TWR = %v quality=%v, want n/a with loss_beyond_total", both.TWR, both.Quality)
		}
		mwr, _ := summaryFor(lossBeyondRows(t, "mwr", "total", nil), "bankx")
		want := slices.DeleteFunc(slices.Clone(both.Quality), func(q string) bool { return q == "loss_beyond_total" })
		if !slices.Equal(want, mwr.Quality) {
			t.Errorf("quality = %v, want the MWR-only run's %v and the tag", both.Quality, mwr.Quality)
		}
		both.Quality, mwr.Quality = nil, nil
		if !reflect.DeepEqual(both, mwr) {
			t.Errorf("row = %+v, want every other figure as the MWR-only run's %+v", both, mwr)
		}
		for _, r := range lossBeyondRows(t, "both", "monthly", nil) {
			if !r.IsSummary && (r.TWR == nil || qualityHas(r, "loss_beyond_total")) {
				t.Errorf("bucket %s: TWR = %v quality=%v, want its own Dietz figure", r.Period, r.TWR, r.Quality)
			}
		}
	})

	type adder = func(string, canonical.AccountKind, []snap)
	for _, c := range []struct {
		name  string
		extra func(add adder)
		fires bool
		twr   float64
	}{
		{"a loan beside it", func(add adder) {
			add("LOAN", canonical.AccountKindMortgage, []snap{{jan31, -500}, {feb28, -500}})
		}, true, 0},
		{"an overdrawn account the rest outweighs", func(add adder) {
			add("OVERDRAWN", canonical.AccountKindBrokerage, []snap{{jan31, -1}, {feb28, -1}})
		}, true, 0},
		// The entity opens at 100 - 1 and ends at 100 - 200.
		{"the entity below zero at the step's end", func(add adder) {
			add("OVERDRAWN", canonical.AccountKindBrokerage, []snap{{jan31, -1}, {feb28, -200}})
		}, false, (-100 - 99 - 3000) / (99 + 3000*23.0/28)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, ok := summaryFor(lossBeyondRows(t, "both", "total", c.extra), "bankx")
			if !ok {
				t.Fatal("no summary row")
			}
			if c.fires {
				if s.TWR != nil || !qualityHas(s, "loss_beyond_total") {
					t.Errorf("TWR = %v quality=%v, want n/a with loss_beyond_total", s.TWR, s.Quality)
				}
				return
			}
			if s.TWR == nil || qualityHas(s, "loss_beyond_total") {
				t.Fatalf("TWR = %v quality=%v, want the chain's figure", s.TWR, s.Quality)
			}
			if math.Abs(*s.TWR-c.twr) > 1e-9 {
				t.Errorf("TWR = %.6f, want %.6f", *s.TWR, c.twr)
			}
		})
	}
}

// The entity owes over a span where its assets' summed value reads below
// zero on some day of it, the value carried into the span included.
func TestEntityOwesOver(t *testing.T) {
	t.Parallel()
	a := &accountData{series: []dayVal{{10, 5}, {20, -3}, {30, 0}}}
	b := &accountData{series: []dayVal{{10, 1}, {25, 10}, {40, 10}}}
	e := &entity{assets: []*accountData{a, b}}
	e.value = entityValue(e.assets)
	cases := []struct {
		from, to int64
		want     bool
	}{
		{0, 9, false},   // before the series
		{10, 19, false}, // above zero throughout
		{15, 20, true},  // the sum turns negative inside
		{22, 24, true},  // the value carried into the span
		{25, 26, false}, // b outweighs a's overdraft
		{30, 40, false},
	}
	for _, c := range cases {
		if got := e.owesOver(c.from, c.to); got != c.want {
			t.Errorf("owesOver(%d, %d) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}
