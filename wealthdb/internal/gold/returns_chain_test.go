package gold

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

func TestUnvaluedAt(t *testing.T) {
	t.Parallel()
	bounds := []int64{0, 5, 10, 15}
	cases := []struct {
		name  string
		moves []returns.Flow
		want  []float64
	}{
		{"nothing moved", nil, []float64{0, 0, 0, 0}},
		{"shown from its valuation on", []returns.Flow{{Day: 4, Amount: 7, ValuedOn: 10}}, []float64{0, 7, 0, 0}},
		{"not shown by the last boundary", []returns.Flow{{Day: 12, Amount: 7, ValuedOn: 20}}, []float64{0, 0, 0, 7}},
		{"two sides of a sweep cancel", []returns.Flow{{Day: 4, Amount: 7, ValuedOn: 10}, {Day: 4, Amount: -7, ValuedOn: 10}}, []float64{0, 0, 0, 0}},
		{"they add", []returns.Flow{{Day: 4, Amount: 7, ValuedOn: 10}, {Day: 5, Amount: 2, ValuedOn: 15}}, []float64{0, 9, 2, 0}},
	}
	for _, c := range cases {
		if got := unvaluedAt(bounds, c.moves); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestUnshownAt(t *testing.T) {
	t.Parallel()
	a := &accountData{snapDays: []int64{10, 20},
		nonTransfer:  []returns.Flow{{Day: 9, Amount: 1}, {Day: 10, Amount: 2}, {Day: 12, Amount: 3}, {Day: 16, Amount: 4}},
		transferLike: []returns.Flow{{Day: 14, Amount: 5}}}
	got := a.unshownAt(15)
	want := []returns.Flow{{Day: 12, Amount: 3, ValuedOn: 20}, {Day: 14, Amount: 5, ValuedOn: 20}}
	if !slices.Equal(got, want) {
		t.Errorf("unshownAt(15) = %+v, want %+v", got, want)
	}
	for _, day := range []int64{5, 10 - 1, 20, 25} {
		if got := a.unshownAt(day); len(got) != 0 {
			t.Errorf("unshownAt(%d) = %+v, want nothing", day, got)
		}
	}
}

func TestValuedOn(t *testing.T) {
	t.Parallel()
	a := &accountData{snapDays: []int64{10, 20}}
	for day, want := range map[int64]int64{5: 10, 10: 10, 11: 20, 20: 20, 21: 0} {
		if got := a.valuedOn(day); got != want {
			t.Errorf("valuedOn(%d) = %d, want %d", day, got, want)
		}
	}
}

// dailyFrom is one snapshot a day, value(d) on the d-th day of February 2023, and
// value(0) on the last of January.
func dailyFrom(value func(d int) int64) []snap {
	s := []snap{{dy(2023, time.January, 31), value(0)}}
	for d := 1; d <= 28; d++ {
		s = append(s, snap{dy(2023, time.February, d), value(d)})
	}
	return s
}

// from reads a stepped value series: before[d] is the value up to day d.
func from(first int64, steps ...[2]int64) func(int) int64 {
	return func(d int) int64 {
		v := first
		for _, s := range steps {
			if int64(d) >= s[0] {
				v = s[1]
			}
		}
		return v
	}
}

// TestChainRollsAnAccountForward: a daily account beside a month-end one, the
// month-end one valuing only at the month's ends. Each case states the return the
// chain must find, worked out by hand from the two accounts.
func TestChainRollsAnAccountForward(t *testing.T) {
	t.Parallel()
	jan31, feb28 := dy(2023, time.January, 31), dy(2023, time.February, 28)
	feb := func(d int) int64 { return dy(2023, time.February, d) }
	const (
		in, out = canonical.TxKindTransferIn, canonical.TxKindTransferOut
		dep, wd = canonical.TxKindDeposit, canonical.TxKindWithdrawal
	)
	type seed func(acct string, s []snap, x []txn)
	cases := []struct {
		name string
		to   int64
		seed func(add seed)
		twr  float64
	}{
		// Unrolled, the wire is a gain, the month's end a loss, and the chain
		// keeps the square of a third: a ninth.
		{"a wire out, nothing else moving", 0, func(add seed) {
			add("DAILY", dailyFrom(from(6000)), nil)
			add("MONTHLY", []snap{{jan31, 3000}, {feb28, 0}}, []txn{{feb(10), wd, -3000}})
		}, 0},
		{"a tiny flow leaves a large deposit's day alone", 0, func(add seed) {
			add("DAILY", dailyFrom(from(1000, [2]int64{10, 600}, [2]int64{11, 10000})), []txn{{feb(11), dep, 9400}})
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 999}}, []txn{{feb(5), wd, -1}})
		}, -400.0 / 1999},
		{"a deposit swept on to the daily account, which then gains", 0, func(add seed) {
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 1000}, {feb(5), out, -1000}})
			add("DAILY", dailyFrom(from(1000, [2]int64{5, 2000}, [2]int64{20, 2200})), []txn{{feb(5), in, 1000}})
		}, 200.0 / 3000},
		{"a gain, then money swept out through the month-end account", 0, func(add seed) {
			add("DAILY", dailyFrom(from(2000, [2]int64{3, 2200}, [2]int64{5, 1200})), []txn{{feb(5), out, -1000}})
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), in, 1000}, {feb(5), wd, -1000}})
		}, 200.0 / 3000},
		{"a deposit swept on and withdrawn the same day", 0, func(add seed) {
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 500}, {feb(5), out, -500}})
			add("DAILY", dailyFrom(from(1000)), []txn{{feb(5), in, 500}, {feb(5), wd, -500}})
		}, 0},
		{"two month-end accounts sweeping between them", 0, func(add seed) {
			add("A", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 500}, {feb(5), out, -500}})
			add("B", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), in, 500}, {feb(5), wd, -500}})
		}, 0},
		{"a large inflow onto a small base", 0, func(add seed) {
			add("DAILY", dailyFrom(from(100)), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 102010}}, []txn{{feb(5), dep, 100000}})
		}, 1010.0 / 101100},
		{"recurring funding of a late account", 0, func(add seed) {
			add("DAILY", dailyFrom(from(10000, [2]int64{5, 9000}, [2]int64{12, 8000}, [2]int64{19, 7000})),
				[]txn{{feb(5), out, -1000}, {feb(12), out, -1000}, {feb(19), out, -1000}})
			add("LATE", []snap{{feb28, 3000}}, []txn{{feb(5), in, 1000}, {feb(12), in, 1000}, {feb(19), in, 1000}})
		}, 0},
		// The money reaches the daily account by a path no flow records, a
		// purchase say, so the month-end account never shows it.
		{"a deposit its account never shows", 0, func(add seed) {
			add("DAILY", dailyFrom(from(1000, [2]int64{5, 2000})), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 1000}})
		}, 0},
		{"money in and out of an account that never shows it", 0, func(add seed) {
			add("DAILY", dailyFrom(from(1000, [2]int64{5, 4000}, [2]int64{20, 1000})), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 3000}, {feb(20), wd, -3000}})
		}, 0},
		{"more in than out of an account that never shows it", 0, func(add seed) {
			add("DAILY", dailyFrom(from(1000, [2]int64{5, 4000}, [2]int64{20, 2000})), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 3000}, {feb(20), wd, -2000}})
		}, 0},
		{"a flow past the window's end is what the valuation shows", feb(20), func(add seed) {
			add("DAILY", dailyFrom(from(1000, [2]int64{5, 4000})), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 4000}}, []txn{{feb(5), dep, 3000}, {feb(25), dep, 3000}})
		}, 0},
		{"a payout past the account's value", 0, func(add seed) {
			add("DAILY", dailyFrom(from(1000)), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 200}}, []txn{{feb(5), wd, -1500}})
		}, 0.05},
		{"a flow on the valuation day is part of what the value shows", 0, func(add seed) {
			add("DAILY", dailyFrom(from(1000, [2]int64{10, 4000})), nil)
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 4000}}, []txn{{feb(10), dep, 3000}, {feb28, dep, 3000}})
		}, 0},
		{"two wires, one on the valuation day", 0, func(add seed) {
			add("DAILY", dailyFrom(from(6000)), nil)
			add("MONTHLY", []snap{{jan31, 6000}, {feb28, 0}}, []txn{{feb(10), wd, -3000}, {feb28, wd, -3000}})
		}, 0},
		{"a flow dated after the valuation that shows it", 0, func(add seed) {
			add("DAILY", dailyFrom(from(6000)), nil)
			add("MONTHLY", []snap{{jan31, 6000}, {feb(10), 3000}, {feb28, 3000}}, []txn{{feb(12), wd, -3000}})
		}, 0},
		{"a wire out, then a real deposit before the month's end", 0, func(add seed) {
			add("DAILY", dailyFrom(from(6000, [2]int64{20, 9000})), []txn{{feb(20), dep, 3000}})
			add("MONTHLY", []snap{{jan31, 3000}, {feb28, 0}}, []txn{{feb(10), wd, -3000}})
		}, 0},
		// With no flow a chain of value ratios telescopes, so a mistimed move alone
		// cannot show. A real deposit while the entity reads the money twice can.
		{"a sweep to the daily account, then a real deposit", 0, func(add seed) {
			add("MONTHLY", []snap{{jan31, 3000}, {feb28, 0}}, []txn{{feb(10), out, -3000}})
			add("DAILY", dailyFrom(from(6000, [2]int64{10, 9000}, [2]int64{20, 12000})),
				[]txn{{feb(10), in, 3000}, {feb(20), dep, 3000}})
		}, 0},
		{"a sweep, the window ending before the valuation", feb(20), func(add seed) {
			add("MONTHLY", []snap{{jan31, 3000}, {feb28, 0}}, []txn{{feb(10), out, -3000}})
			add("DAILY", dailyFrom(from(6000, [2]int64{10, 9000})), []txn{{feb(10), in, 3000}})
		}, 0},
		{"a deposit swept on, the window ending before the valuation", feb(20), func(add seed) {
			add("MONTHLY", []snap{{jan31, 1000}, {feb28, 1000}}, []txn{{feb(5), dep, 1000}, {feb(5), out, -1000}})
			add("DAILY", dailyFrom(from(1000, [2]int64{5, 2000})), []txn{{feb(5), in, 1000}})
		}, 0},
		{"a flow before the window's start, shown inside it", 0, func(add seed) {
			add("DAILY", dailyFrom(from(6000)), nil)
			add("MONTHLY", []snap{{jan31, 3000}, {feb28, 0}}, []txn{{feb(10), wd, -3000}})
		}, 0},
		{"a flow the window's end comes before", feb(20), func(add seed) {
			add("DAILY", dailyFrom(from(6000)), nil)
			add("MONTHLY", []snap{{jan31, 3000}, {feb28, 0}}, []txn{{feb(10), wd, -3000}})
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "bankx", "fidelity")
			c.seed(func(acct string, s []snap, x []txn) {
				seedAcct(t, db, ctx, "bankx", acct, canonical.AccountKindBrokerage, nil, s, x)
			})
			to := eod(2023, time.February, 28)
			if c.to != 0 {
				to = c.to + 86399
			}
			start := int64(0)
			if strings.Contains(c.name, "before the window's start") {
				start = feb(15)
			}
			rows, err := RunReturns(ctx, db, params("sources", start, to))
			if err != nil {
				t.Fatalf("RunReturns: %v", err)
			}
			s, ok := summaryFor(rows, "bankx")
			if !ok || s.TWR == nil {
				t.Fatalf("no TWR: %+v", s)
			}
			if math.Abs(*s.TWR-c.twr) > 1e-9 {
				t.Errorf("TWR = %.6f, want %.6f", *s.TWR, c.twr)
			}
		})
	}
}

// TestChainRollsACrossMatchedPair: a wire from a month-end account in one source
// to a daily account in another. The matcher drops both legs, so neither is a
// flow, and the daily account shows the money at once. Unrolled, the entity reads
// the money twice until the month's end. That alone telescopes away, as a chain of
// value ratios does, but a real deposit while it lasts is read against the wrong
// base: here it would read +6.67% on money that earned nothing.
func TestChainRollsACrossMatchedPair(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "bank1", "fidelity")
	seedReturnsSource(t, db, ctx, "bank2", "fidelity")
	wire := dy(2023, time.February, 10)
	seedAcct(t, db, ctx, "bank1", "MONTHLY", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2023, time.January, 31), 3000}, {dy(2023, time.February, 28), 0}},
		[]txn{{wire, canonical.TxKindWithdrawal, -3000}})
	seedAcct(t, db, ctx, "bank2", "DAILY", canonical.AccountKindBrokerage, nil,
		dailyFrom(from(6000, [2]int64{10, 9000}, [2]int64{20, 12000})),
		[]txn{{wire, canonical.TxKindDeposit, 3000}, {dy(2023, time.February, 20), canonical.TxKindDeposit, 3000}})

	rows, err := RunReturns(ctx, db, tmParams("global", 0, eod(2023, time.February, 28)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "")
	if !ok || s.TWR == nil {
		t.Fatalf("no TWR: %+v", s)
	}
	if !qualityHas(s, "cross_source_netted=1") {
		t.Fatalf("the pair did not match: quality=%v", s.Quality)
	}
	if math.Abs(*s.TWR) > 1e-9 {
		t.Errorf("TWR = %.6f, want 0", *s.TWR)
	}
}
