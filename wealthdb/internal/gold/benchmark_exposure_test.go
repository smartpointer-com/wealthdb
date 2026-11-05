package gold

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

func TestBenchmarkFundFigures(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	dated := func(v float64) *yield.Dated { return &yield.Dated{Value: v, Day: day} }
	lines := func(y, d float64, covered, debt float64, asOf time.Time) *yield.LineSummary {
		return &yield.LineSummary{AsOf: asOf, Yield: &y, Duration: &d, CoveredPct: covered, DurationPct: covered, DebtPct: debt}
	}
	for name, c := range map[string]struct {
		fund          yield.Fund
		yield, length *float64
	}{
		"a stated yield wins, in the report's order": {
			yield.Fund{SECYield: dated(2), TrailingYield: dated(3), Lines: lines(9, 9, 100, 0, day)}, f64(2), f64(0)},
		"a distribution yield beats a SEC yield": {
			yield.Fund{DistributionYield: dated(1), SECYield: dated(2)}, f64(1), nil},
		"a stated duration wins": {
			yield.Fund{Duration: dated(6), Risk: map[string]map[string]float64{"USD": {"5Y": 1}}, NetAssets: f64(100)}, nil, f64(6)},
		"a filing's DV01 over its net assets beats the lines": {
			yield.Fund{Risk: map[string]map[string]float64{"USD": {"5Y": 30, "10Y": 20}}, NetAssets: f64(100_000),
				Lines: lines(4, 9, 100, 100, day)}, f64(4), f64(5)},
		"the lines stand in where they cover enough": {
			yield.Fund{Lines: lines(4, 8, 60, 50, day)}, f64(4), f64(4)},
		"lines that cover too little say nothing": {
			yield.Fund{Lines: lines(4, 8, 40, 50, day)}, nil, nil},
		"lines dated after the day say nothing of it": {
			yield.Fund{Lines: lines(4, 8, 100, 100, day.AddDate(0, 1, 0))}, nil, nil},
		"a fund with next to no debt has no duration to speak of": {
			yield.Fund{Lines: &yield.LineSummary{AsOf: day.AddDate(0, 1, 0), DebtPct: 0.5}}, nil, f64(0)},
		"no record, no figure": {yield.Fund{}, nil, nil},
	} {
		y, d := benchmarkFundFigures(&c.fund, day)
		same := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || math.Abs(*a-*b) < 1e-9) }
		if !same(y, c.yield) || !same(d, c.length) {
			t.Errorf("%s: yield %v duration %v", name, y, d)
		}
	}
}

func TestWeightedNeedsEveryFund(t *testing.T) {
	t.Parallel()
	mix := []benchmarkFundRow{{key: "a", weight: 60}, {key: "b", weight: 40}}
	got := weighted(mix, func(k string) *float64 { return map[string]*float64{"a": f64(1), "b": f64(2)}[k] })
	if got == nil || math.Abs(*got-1.4) > 1e-12 {
		t.Errorf("weighted = %v", got)
	}
	if weighted(mix, func(k string) *float64 { return map[string]*float64{"a": f64(1)}[k] }) != nil {
		t.Error("a fund with no figure leaves the mix with none")
	}
}

func exposureTotals(t *testing.T, db *sql.DB, ctx context.Context) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	if err := scanAll(ctx, db, `SELECT dimension, sum(share_pct) FROM benchmark_exposure WHERE benchmark = 'Test' GROUP BY 1`,
		func(rows *sql.Rows) error {
			var d string
			var v float64
			if err := rows.Scan(&d, &v); err != nil {
				return err
			}
			out[d] = v
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	return out
}

// syncTestBenchmark names the mix exposureTotals reads: a fund of the
// exposure fixture and one gold holds nothing of.
func syncTestBenchmark(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := SyncBenchmarks(ctx, db, []canonical.Benchmark{{Name: "Test", Funds: []canonical.BenchmarkFund{
		{FundRef: "symbol:FUNDX", WeightPct: 60}, {FundRef: "symbol:NOPE", WeightPct: 40}}}}); err != nil {
		t.Fatal(err)
	}
}

// The look-through alone. TestMaterializeExposureMatchesTheLiveViews runs
// it inside the whole pass and checks the household's rows beside it.
func TestMaterializeBenchmarkExposureLooksThroughTheMix(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	syncTestBenchmark(t, db, ctx)
	tx, commit, err := beginPinned(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after commit
	if _, err := materializeBenchmarkExposure(ctx, tx, exAsOf, 2); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	totals := exposureTotals(t, db, ctx)
	for _, d := range []string{BenchmarkDimClass, BenchmarkDimSector, BenchmarkDimRegion, BenchmarkDimCountry,
		BenchmarkDimCurrency, BenchmarkDimFundCurrency, debtDimensionCredit, debtDimensionMaturity,
		BenchmarkDimSecurity, BenchmarkDimIssuer} {
		if math.Abs(totals[d]-100) > 1e-6 && d != BenchmarkDimIssuer {
			t.Errorf("%s sums to %v, want the whole mix: %v", d, totals[d], totals)
		}
	}
	var leaked int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM report_exposure`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("%d report_exposure rows: the virtual holdings leaked", leaked)
	}
	// The macro's shadow is gone with the pass: the live view reads the
	// household's holdings again, real estate among them, which no
	// benchmark holds.
	classes, err := ExposureClasses(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(classes, func(c ExposureClassRow) bool { return c.AssetClass == "real_estate" }) {
		t.Errorf("the live view reads %+v, no real estate: the shadow outlived the pass", classes)
	}
}
