package gold

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestPostExitResidue: once outflows take nearly all of an entity's value, the
// sub-periods on what is left add nothing to the chain until the value is
// material again, and the summary says so with post_exit_residue. Income on the
// residue still counts in gain. Each case holds at the accounts and the sources
// grain, and states the TWR and whether the tag is there.
func TestPostExitResidue(t *testing.T) {
	t.Parallel()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	wd, dep := canonical.TxKindWithdrawal, canonical.TxKindDeposit
	daily := func(first, last int64, value func(day int64) int64) []snap {
		var s []snap
		for x := first; x <= last; x += 86400 {
			s = append(s, snap{x, value(x)})
		}
		return s
	}
	cases := []struct {
		name   string
		snaps  []snap
		txns   []txn
		twr    float64
		tagged bool
		gain   float64 // checked when not NaN
	}{
		// A sale leaves 10, and the sold positions' dividend lands on it a month
		// later. Unskipped, that month reads +2,000%.
		{"a sale, the residue, then the income", []snap{{d(time.January, 31), 10000}, {d(time.February, 29), 11000},
			{d(time.March, 31), 10}, {d(time.April, 30), 210}}, []txn{{d(time.March, 10), wd, -10990}}, 0.1, true, 1200},
		{"a small first value growing 5000x", []snap{{d(time.January, 31), 1}, {d(time.February, 29), 100},
			{d(time.March, 31), 5000}}, nil, 4999, false, math.NaN()},
		{"a withdrawal of nine tenths", []snap{{d(time.January, 31), 10000}, {d(time.February, 29), 1000},
			{d(time.March, 31), 1100}}, []txn{{d(time.February, 10), wd, -9000}}, 0.1, false, math.NaN()},
		// Value lost with no outflow is a loss, not an exit: 99.5% down, then
		// five-fold up.
		{"a write-off with no outflow", []snap{{d(time.January, 31), 10000}, {d(time.February, 29), 50},
			{d(time.March, 31), 250}}, nil, 0.005*5 - 1, false, math.NaN()},
		{"a withdrawal of 95 percent that leaves 5 percent", []snap{{d(time.January, 31), 10000}, {d(time.February, 29), 500},
			{d(time.March, 31), 550}}, []txn{{d(time.February, 10), wd, -9500}}, 0.1, false, math.NaN()},
		// The known edge across sub-periods: 95% withdrawn, then a fall with no
		// flow leaves 0.5%. The run counts both, so it reads as an exit, and the
		// recovery after it adds nothing.
		{"a withdrawal of 95 percent, then a fall to half a percent", []snap{{d(time.January, 31), 10000},
			{d(time.February, 29), 500}, {d(time.March, 31), 50}, {d(time.April, 30), 100}},
			[]txn{{d(time.February, 10), wd, -9500}}, -0.9, true, math.NaN()},
		// A known edge: 90% withdrawn and a market fall leave 0.5%, which reads
		// as an exit, so the recovery after it adds nothing.
		{"a 90 percent withdrawal and a fall that leave half a percent", []snap{{d(time.January, 31), 10000},
			{d(time.February, 29), 50}, {d(time.March, 31), 100}}, []txn{{d(time.February, 10), wd, -9000}},
			-950.0 / (10000 - 9000*19.0/29), true, math.NaN()},
		// The residue earns, then new money makes the value material and the
		// chain runs again: +10% after the deposit.
		{"an exit, then material new money", []snap{{d(time.January, 31), 10000}, {d(time.February, 29), 10},
			{d(time.March, 31), 60}, {d(time.April, 30), 5000}, {d(time.May, 31), 5500}},
			[]txn{{d(time.February, 10), wd, -9990}, {d(time.April, 10), dep, 4940}}, 0.1, true, math.NaN()},
		// Three days of sales take 8,950 of 9,000. None is an exit alone: the
		// last leaves 1.7% of what it held. The 50 left earns 200 on the 20th,
		// and 250 is material again.
		{"an exit spread over three days", daily(d(time.January, 31), d(time.February, 28), func(x int64) int64 {
			switch {
			case x < d(time.February, 1):
				return 8000
			case x < d(time.February, 10):
				return 9000
			case x < d(time.February, 11):
				return 6000
			case x < d(time.February, 12):
				return 3000
			case x < d(time.February, 20):
				return 50
			}
			return 250
		}), []txn{{d(time.February, 10), wd, -3000}, {d(time.February, 11), wd, -3000}, {d(time.February, 12), wd, -2950}},
			0.125, true, math.NaN()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "fidx", "fidelity")
			seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil, c.snaps, c.txns)
			to := c.snaps[len(c.snaps)-1].at + 43199
			for _, level := range []string{"accounts", "sources"} {
				rows, err := RunReturns(ctx, db, params(level, 0, to))
				if err != nil {
					t.Fatalf("RunReturns %s: %v", level, err)
				}
				entity := map[string]string{"accounts": "A", "sources": "fidx"}[level]
				s, ok := summaryFor(rows, entity)
				if !ok || s.TWR == nil {
					t.Fatalf("%s: no TWR: %+v", level, s)
				}
				if math.Abs(*s.TWR-c.twr) > 1e-9*math.Max(1, math.Abs(c.twr)) {
					t.Errorf("%s: TWR = %.9f, want %.9f", level, *s.TWR, c.twr)
				}
				if got := qualityHas(s, "post_exit_residue"); got != c.tagged {
					t.Errorf("%s: post_exit_residue = %v, want %v: %v", level, got, c.tagged, s.Quality)
				}
				if !math.IsNaN(c.gain) {
					if g, _ := parseFloatPtr(s.Gain()); math.Abs(g-c.gain) > 1e-6 {
						t.Errorf("%s: gain = %.2f, want %.2f", level, g, c.gain)
					}
				}
			}
		})
	}
}

// seedExitNearTheFloor is one account whose sale takes 90.5% of the value in
// dollars and leaves 0.5%, and 88.7% in francs, the franc having moved 2%
// between the valuation and the sale; the residue earns 200 in April.
func seedExitNearTheFloor(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	seedFX(t, db, d(time.January, 1), "USD", "CHF", "1.00")
	seedFX(t, db, d(time.March, 1), "USD", "CHF", "1.02")
	seedFX(t, db, d(time.January, 1), "USD", "EUR", "0.90")
	seedFX(t, db, d(time.January, 1), "USD", "GBP", "0.80")
	seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d(time.January, 31), 10000}, {d(time.February, 29), 10000}, {d(time.March, 31), 50}, {d(time.April, 30), 250}},
		[]txn{{d(time.March, 10), canonical.TxKindWithdrawal, -9050}})
	return db, ctx
}

// TestPostExitResidueSameInEveryCurrency: a sale takes 90.5% of the value in
// dollars and leaves 0.5%. The rate moves 2% between the valuation and the sale,
// so in francs the same sale is 88.7% of the value, under the outflow floor.
// Which sub-periods are residue is one decision, made in dollars, so every
// reporting currency leaves out the same income month and carries the same tag.
func TestPostExitResidueSameInEveryCurrency(t *testing.T) {
	t.Parallel()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	db, ctx := seedExitNearTheFloor(t)
	for _, level := range []string{"accounts", "sources"} {
		entity := map[string]string{"accounts": "A", "sources": "fidx"}[level]
		for _, ccy := range []string{"USD", "CHF", "EUR", "GBP"} {
			p := params(level, 0, d(time.April, 30)+43199)
			p.OutCcy = ccy
			rows, err := RunReturns(ctx, db, p)
			if err != nil {
				t.Fatalf("RunReturns %s %s: %v", level, ccy, err)
			}
			s, ok := summaryFor(rows, entity)
			if !ok || s.TWR == nil {
				t.Fatalf("%s %s: no TWR: %+v", level, ccy, s)
			}
			if !qualityHas(s, "post_exit_residue") {
				t.Errorf("%s %s: no post_exit_residue: %v", level, ccy, s.Quality)
			}
			// Up to the exit the sale lost about a quarter, a little more in
			// francs. The residue's +400% is in no currency.
			if *s.TWR > 0 || *s.TWR < -0.35 {
				t.Errorf("%s %s: TWR = %.4f, want the return up to the exit", level, ccy, *s.TWR)
			}
		}
	}
}

// TestMaterializedResidueSameInEveryCurrency: report_returns, written for every
// currency from one load, carries the same residue decision in each.
func TestMaterializedResidueSameInEveryCurrency(t *testing.T) {
	t.Parallel()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	db, ctx := seedExitNearTheFloor(t)
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: d(time.April, 30) + 43199, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT currency, quality FROM report_returns
		WHERE grain = 'sources' AND granularity = 'total' AND is_summary AND window_from_year = 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var ccy, q string
		if err := rows.Scan(&ccy, &q); err != nil {
			t.Fatal(err)
		}
		seen++
		if !strings.Contains(q, "post_exit_residue") {
			t.Errorf("%s: no post_exit_residue: %s", ccy, q)
		}
	}
	if seen != 4 {
		t.Errorf("%d currencies, want 4", seen)
	}
}

// TestPostExitResidueRunDoesNotHideAnExit: a small withdrawal while the value
// is higher opens a run; a later sale then takes all but 0.5% of what is left.
// Measured from the run's opening value its outflows are 89.6%, under the
// floor, but the sale alone is an exit, so the income on the residue still
// adds nothing.
func TestPostExitResidueRunDoesNotHideAnExit(t *testing.T) {
	t.Parallel()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d(time.January, 31), 10000}, {d(time.February, 29), 8000}, {d(time.March, 31), 40}, {d(time.April, 30), 240}},
		[]txn{{d(time.February, 10), canonical.TxKindWithdrawal, -1000}, {d(time.March, 10), canonical.TxKindWithdrawal, -7960}})
	rows, err := RunReturns(ctx, db, params("sources", 0, d(time.April, 30)+43199))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := summaryFor(rows, "fidx")
	if !ok || s.TWR == nil || !qualityHas(s, "post_exit_residue") {
		t.Fatalf("want the residue left out: %+v", s)
	}
	if *s.TWR > 0 || *s.TWR < -0.2 {
		t.Errorf("TWR = %.4f, want the return up to the exit", *s.TWR)
	}
}

// TestPostExitResidueValueLeft: a sale's outflows take 96% of the value, and
// fees and rates take nearly all the rest: 0.06% is left. That is an exit by
// the value left, so the dividend a month later adds nothing, in every
// currency.
func TestPostExitResidueValueLeft(t *testing.T) {
	t.Parallel()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	seedFX(t, db, d(time.January, 1), "USD", "CHF", "0.90")
	seedFX(t, db, d(time.January, 1), "USD", "EUR", "0.90")
	seedFX(t, db, d(time.January, 1), "USD", "GBP", "0.80")
	seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d(time.January, 31), 10000}, {d(time.February, 29), 10000}, {d(time.March, 31), 6}, {d(time.April, 30), 206}},
		[]txn{{d(time.March, 10), canonical.TxKindWithdrawal, -9600}})
	want := -394.0 / (10000 - 9600*21.0/31)
	for _, ccy := range []string{"USD", "CHF", "EUR", "GBP"} {
		p := params("sources", 0, d(time.April, 30)+43199)
		p.OutCcy = ccy
		rows, err := RunReturns(ctx, db, p)
		if err != nil {
			t.Fatal(err)
		}
		s, ok := summaryFor(rows, "fidx")
		if !ok || s.TWR == nil {
			t.Fatalf("%s: no TWR: %+v", ccy, s)
		}
		if !qualityHas(s, "post_exit_residue") || math.Abs(*s.TWR-want) > 1e-6 {
			t.Errorf("%s: TWR %.6f, quality %v; want %.6f and post_exit_residue", ccy, *s.TWR, s.Quality, want)
		}
	}
}

// TestPostExitResidueWithoutATwin: where the dollar dataset could not be loaded
// the accounts have no twin, and the reporting currency decides alone. The sale
// takes 89.5% in dollars, under the outflow floor, but 91.3% in francs: an exit
// only where francs decide.
func TestPostExitResidueWithoutATwin(t *testing.T) {
	t.Parallel()
	d := func(m time.Month, day int) int64 { return dy(2024, m, day) }
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	seedFX(t, db, d(time.January, 1), "USD", "CHF", "1.00")
	seedFX(t, db, d(time.March, 1), "USD", "CHF", "0.98")
	seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d(time.January, 31), 10000}, {d(time.February, 29), 10000}, {d(time.March, 31), 50}, {d(time.April, 30), 250}},
		[]txn{{d(time.March, 10), canonical.TxKindWithdrawal, -8950}})
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	p := params("sources", 0, d(time.April, 30)+43199)
	p.OutCcy = "CHF"
	for _, linked := range []bool{true, false} {
		ds, err := loadReturnsDataset(ctx, db, "CHF", fx, ReturnsSettings{})
		if err != nil {
			t.Fatal(err)
		}
		if !linked {
			ds.twins = nil
		}
		s, _ := summaryFor(computeReturns(ds, p), "fidx")
		if got := qualityHas(s, "post_exit_residue"); got == linked {
			t.Errorf("linked %v: post_exit_residue = %v: %v", linked, got, s.Quality)
		}
	}
}

// TestPostExitResidueInEveryWindow: the residue is decided on the entity's
// since-inception chain and applied to every window. A window that opens after
// a December exit, or inside a three-day exit, skips the same sub-periods and
// carries the tag; one that opens after the residue is material again does
// not.
func TestPostExitResidueInEveryWindow(t *testing.T) {
	t.Parallel()
	wd := canonical.TxKindWithdrawal
	feb := func(day int) int64 { return dy(2024, time.February, day) }
	daily := func(first, last int64, value func(day int64) int64) []snap {
		var s []snap
		for x := first; x <= last; x += 86400 {
			s = append(s, snap{x, value(x)})
		}
		return s
	}
	cases := []struct {
		name     string
		snaps    []snap
		txns     []txn
		from, to int64
		twr      float64
		tagged   bool
	}{
		// A sale on 10 December leaves 10, which earns 50 in January. The
		// window from January holds only the residue: a factor of 1 throughout.
		{"a window that opens after a December exit", []snap{{dy(2023, time.November, 30), 10000},
			{dy(2023, time.December, 31), 10}, {dy(2024, time.January, 31), 60}, {dy(2024, time.February, 29), 60}},
			[]txn{{dy(2023, time.December, 10), wd, -9990}}, dy(2024, time.January, 1), dy(2024, time.February, 29), 0, true},
		// The third day of a three-day sale opens the window: alone it leaves
		// 1.7% of what it held, no exit, but the inception chain's run is one.
		{"a window that opens inside an exit", daily(dy(2024, time.January, 31), feb(28), func(x int64) int64 {
			switch {
			case x < feb(1):
				return 8000
			case x < feb(10):
				return 9000
			case x < feb(11):
				return 6000
			case x < feb(12):
				return 3000
			case x < feb(20):
				return 50
			}
			return 250
		}), []txn{{feb(10), wd, -3000}, {feb(11), wd, -3000}, {feb(12), wd, -2950}}, feb(11), feb(28), 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "fidx", "fidelity")
			seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil, c.snaps, c.txns)
			for _, level := range []string{"accounts", "sources"} {
				rows, err := RunReturns(ctx, db, params(level, c.from, c.to+43199))
				if err != nil {
					t.Fatal(err)
				}
				s, ok := summaryFor(rows, map[string]string{"accounts": "A", "sources": "fidx"}[level])
				if !ok || s.TWR == nil {
					t.Fatalf("%s: no TWR: %+v", level, s)
				}
				if math.Abs(*s.TWR-c.twr) > 1e-9 || qualityHas(s, "post_exit_residue") != c.tagged {
					t.Errorf("%s: TWR %.6f, quality %v; want %.6f, tagged %v", level, *s.TWR, s.Quality, c.twr, c.tagged)
				}
			}
		})
	}
}

// TestPostExitResidueCumulativeLine: the Returns chart's cumulative line divides
// since-year windows, (1 + TWR since 2023) / (1 + TWR since 2024). With the
// residue read the same in both, the line holds its level after a December
// exit instead of falling by the residue's income.
func TestPostExitResidueCumulativeLine(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fidx", "fidelity")
	seedAcct(t, db, ctx, "fidx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2023, time.January, 31), 10000}, {dy(2023, time.November, 30), 11000},
			{dy(2023, time.December, 31), 10}, {dy(2024, time.January, 31), 60}, {dy(2024, time.February, 29), 60}},
		[]txn{{dy(2023, time.December, 10), canonical.TxKindWithdrawal, -10990}})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: dy(2024, time.February, 29) + 43199, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	twr := func(year int) (float64, string) {
		var v float64
		var q string
		if err := db.QueryRowContext(ctx, `SELECT twr, quality FROM report_returns WHERE grain = 'sources'
			AND granularity = 'total' AND is_summary AND currency = 'USD' AND window_from_year = ?`, year).Scan(&v, &q); err != nil {
			t.Fatalf("window %d: %v", year, err)
		}
		return v, q
	}
	y23, _ := twr(2023)
	y24, q24 := twr(2024)
	if !strings.Contains(q24, "post_exit_residue") || math.Abs(y24) > 1e-12 {
		t.Errorf("since 2024: TWR %v, quality %s; want 0 and post_exit_residue", y24, q24)
	}
	if cum := (1+y23)/(1+y24) - 1; math.Abs(cum-y23) > 1e-12 || y23 < 0.09 {
		t.Errorf("the line at 2024 reads %v, since 2023 %v: it must hold its level", cum, y23)
	}
}

// TestBridgeReadsTheResidueInDollars: the bridge's months carry the same
// residue decision as returns, made in dollars, from the CLI as from the
// materialized table. In francs the sale is under the outflow floor, so a
// bridge that decided in francs would not tag April.
func TestBridgeReadsTheResidueInDollars(t *testing.T) {
	t.Parallel()
	db, ctx := seedExitNearTheFloor(t)
	rows := runBridge(t, db, ctx, bridgeParams("CHF", "monthly"))
	checkIdentity(t, "CHF", rows)
	found := false
	for _, r := range rows {
		if r.Period == "2024-04" {
			found = slices.Contains(r.Quality, "post_exit_residue")
		}
	}
	if !found {
		t.Errorf("April in francs has no post_exit_residue: %+v", rows)
	}
}
