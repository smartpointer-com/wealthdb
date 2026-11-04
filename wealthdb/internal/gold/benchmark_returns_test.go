package gold

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// seedFundMonths writes a fund's returns, in percent, for consecutive
// months from `first`, one filing per month, a NaN leaving the month out.
func seedFundMonths(t *testing.T, db *sql.DB, ctx context.Context, ref string, first time.Time, pct ...float64) {
	t.Helper()
	for i, p := range pct {
		if math.IsNaN(p) {
			continue
		}
		end := time.Date(first.Year(), first.Month()+time.Month(i)+1, 0, 0, 0, 0, 0, time.UTC)
		if _, err := db.ExecContext(ctx, `INSERT INTO fund_returns (silver_source_id, fund_ref, as_of, month, total_return)
VALUES ('test-src', ?, ?, ?, ?)`, ref, end.Unix(), end.Format(time.DateOnly), p); err != nil {
			t.Fatalf("seed fund month: %v", err)
		}
	}
}

func seedBenchmark(t *testing.T, db *sql.DB, ctx context.Context, name string, funds ...canonical.BenchmarkFund) {
	t.Helper()
	if _, err := SyncBenchmarks(ctx, db, []canonical.Benchmark{{Name: name, Funds: funds}}); err != nil {
		t.Fatalf("SyncBenchmarks: %v", err)
	}
}

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", name, want)
		return
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", name, *got, want)
	}
}

// day0 is a day as the benchmark tables store it: its UTC midnight.
func day0(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

func month(y int, m time.Month) int64 {
	return EpochDay(time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Unix())
}

func TestBlendMonthsWeighsEachMonth(t *testing.T) {
	t.Parallel()
	jan, feb, mar := month(2024, time.January), month(2024, time.February), month(2024, time.March)
	funds := map[string]map[int64]float64{
		"a": {jan: 0.10, feb: 0.20, mar: 0.30},
		"b": {feb: -0.10, mar: 0.10},
	}
	got := blendMonths([]benchmarkFundRow{{key: "a", weight: 60}, {key: "b", weight: 40}}, funds)
	if len(got) != 2 || got[0].End != feb || got[1].End != mar {
		t.Fatalf("months = %+v, want February and March, the months both funds span", got)
	}
	if math.Abs(got[0].R-(0.6*0.20+0.4*-0.10)) > 1e-12 || math.Abs(got[1].R-(0.6*0.30+0.4*0.10)) > 1e-12 {
		t.Errorf("blend = %+v", got)
	}
}

func TestBlendMonthsLeavesAGapWhereAFundLacksTheMonth(t *testing.T) {
	t.Parallel()
	jan, feb, mar := month(2024, time.January), month(2024, time.February), month(2024, time.March)
	funds := map[string]map[int64]float64{
		"a": {jan: 0.10, feb: 0.20, mar: 0.30},
		"b": {jan: 0.10, mar: 0.10},
	}
	got := blendMonths([]benchmarkFundRow{{key: "a", weight: 50}, {key: "b", weight: 50}}, funds)
	if len(got) != 3 || !got[0].OK || got[1].OK || !got[2].OK || got[1].R != 0 {
		t.Errorf("months = %+v, want February a gap", got)
	}
}

func TestBlendMonthsOfAFundWithNoReturnsIsEmpty(t *testing.T) {
	t.Parallel()
	funds := map[string]map[int64]float64{"a": {month(2024, time.January): 0.10}}
	if got := blendMonths([]benchmarkFundRow{{key: "a", weight: 50}, {key: "b", weight: 50}}, funds); got != nil {
		t.Errorf("months = %+v, want none", got)
	}
}

func TestInCurrencyConvertsAtMonthEndFX(t *testing.T) {
	t.Parallel()
	dec, jan, feb := month(2023, time.December), month(2024, time.January), month(2024, time.February)
	months := []returns.BenchmarkMonth{{End: jan, R: 0.10, OK: true}, {End: feb, R: 0.05, OK: true}}
	got := inCurrency(months, map[int64]float64{dec: 0.80, jan: 0.88})
	if math.Abs(got[0].R-(1.10*0.88/0.80-1)) > 1e-12 || !got[0].OK {
		t.Errorf("January = %+v", got[0])
	}
	if got[1].OK {
		t.Errorf("February has no end rate, so it should be a gap: %+v", got[1])
	}
	if months[0].R != 0.10 {
		t.Error("the dollar months should stay as they were")
	}
}

func TestMonthEndBefore(t *testing.T) {
	t.Parallel()
	if got := monthEndBefore(month(2024, time.March)); got != month(2024, time.February) {
		t.Errorf("before March = %d", got)
	}
	if got := monthEndBefore(month(2024, time.January)); got != month(2023, time.December) {
		t.Errorf("before January = %d", got)
	}
}

func TestLineEnds(t *testing.T) {
	t.Parallel()
	from := EpochDay(dy(2024, time.January, 15))
	to := EpochDay(dy(2024, time.March, 10))
	got := lineEnds(from, to)
	want := []int64{month(2024, time.January), month(2024, time.February), to}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("ends = %v, want %v", got, want)
	}
	// A window from a month end starts with the next one, and one that
	// ends on a month end has no extra point.
	got = lineEnds(month(2024, time.January), month(2024, time.February))
	if len(got) != 1 || got[0] != month(2024, time.February) {
		t.Errorf("ends = %v", got)
	}
}

// seedBenchmarkHousehold is one brokerage account over the first half of
// 2024: 1000 at the start of January, 1210 by the end of June with a 100
// deposit on March 31, month-end snapshots, in dollars and francs.
func seedBenchmarkHousehold(t *testing.T, db *sql.DB, ctx context.Context) int64 {
	t.Helper()
	seedReturnsSource(t, db, ctx, "src-a", "schwab")
	seedAcct(t, db, ctx, "src-a", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2023, time.December, 31), 1000}, {dy(2024, time.January, 31), 1020}, {dy(2024, time.February, 29), 1040},
			{dy(2024, time.March, 31), 1160}, {dy(2024, time.April, 30), 1170}, {dy(2024, time.May, 31), 1190},
			{dy(2024, time.June, 30), 1210}},
		[]txn{{dy(2024, time.March, 31), canonical.TxKindDeposit, 100}})
	seedFX(t, db, dy(2023, time.December, 1), "USD", "CHF", "0.90")
	seedFX(t, db, dy(2023, time.December, 1), "USD", "EUR", "0.90")
	seedFX(t, db, dy(2023, time.December, 1), "USD", "GBP", "0.80")
	return time.Date(2024, time.June, 30, 23, 59, 59, 0, time.UTC).Unix()
}

func benchmarkRow(t *testing.T, db *sql.DB, ctx context.Context, where string, args ...any) (twr, portfolio, excess, mwr sql.NullFloat64, start, end int64, quality string) {
	t.Helper()
	err := db.QueryRowContext(ctx, `SELECT twr, portfolio_twr, excess, mwr, start_day, end_day, quality FROM benchmark_returns WHERE `+where, args...).
		Scan(&twr, &portfolio, &excess, &mwr, &start, &end, &quality)
	if err != nil {
		t.Fatalf("benchmark row %s: %v", where, err)
	}
	return
}

func TestMaterializeBenchmarkComparesOverTheMonthsBothSidesHave(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	// The fund's months stop at April: the household runs on to June.
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1, 2, 3, 4)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})

	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	twr, portfolio, excess, _, _, last, q := benchmarkRow(t, db, ctx,
		`benchmark = 'Test' AND currency = 'USD' AND granularity = 'total' AND window_from_year = 0`)
	if last != day0(2024, time.April, 30) {
		t.Errorf("the window ends %d, want the benchmark's last month", last)
	}
	if want := 1.01*1.02*1.03*1.04 - 1; math.Abs(twr.Float64-want) > 1e-9 {
		t.Errorf("twr = %v, want %v", twr.Float64, want)
	}
	if math.Abs(excess.Float64-(portfolio.Float64-twr.Float64)) > 1e-12 {
		t.Errorf("excess = %v, portfolio %v", excess.Float64, portfolio.Float64)
	}
	// The household over the same days, not through June.
	var june float64
	if err := db.QueryRowContext(ctx, `SELECT twr FROM report_returns WHERE grain = 'global' AND granularity = 'total'
AND is_summary AND currency = 'USD' AND window_from_year = 0`).Scan(&june); err != nil {
		t.Fatal(err)
	}
	if portfolio.Float64 == june {
		t.Errorf("the household's return %v should stop at April", june)
	}
	if q != benchmarkEndClipped+";after_tax" && q != benchmarkEndClipped {
		t.Errorf("quality = %q", q)
	}
}

func TestMaterializeBenchmarkMonthlyBucketsAndTheirGaps(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1, math.NaN(), 3, 4)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	month := func(period string) (sql.NullFloat64, string) {
		twr, _, _, _, _, _, q := benchmarkRow(t, db, ctx,
			`currency = 'USD' AND granularity = 'monthly' AND period = ?`, period)
		return twr, q
	}
	if twr, q := month("2024-01"); !twr.Valid || math.Abs(twr.Float64-0.01) > 1e-12 || q != "" {
		t.Errorf("January = %v %q", twr, q)
	}
	if twr, q := month("2024-02"); twr.Valid || q != benchmarkGap {
		t.Errorf("February = %v %q, want a gap", twr, q)
	}
	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_returns WHERE granularity = 'monthly'
AND period IN ('2024-05', '2024-06')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Errorf("%d rows past the benchmark's last month", after)
	}
	// A quarter that holds the gap has no return; so has the window.
	twr, _, excess, _, _, _, q := benchmarkRow(t, db, ctx, `currency = 'USD' AND granularity = 'quarterly' AND period = '2024-Q1'`)
	if twr.Valid || excess.Valid || q != benchmarkGap {
		t.Errorf("Q1 = %v %v %q", twr, excess, q)
	}
	twr, _, _, _, _, _, q = benchmarkRow(t, db, ctx, `currency = 'USD' AND granularity = 'total' AND window_from_year = 0`)
	if twr.Valid {
		t.Errorf("the window through the gap has return %v (%q)", twr.Float64, q)
	}
	// The quarter its months end inside has no return.
	twr, _, _, _, _, _, q = benchmarkRow(t, db, ctx, `currency = 'USD' AND granularity = 'quarterly' AND period = '2024-Q2'`)
	if twr.Valid || q != benchmarkPartialPeriod {
		t.Errorf("Q2 = %v %q", twr, q)
	}
}

func TestMaterializeBenchmarkStartsAtItsFirstMonthAndSaysSo(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC), 1, 1, 1, 1)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	twr, _, _, _, start, last, q := benchmarkRow(t, db, ctx,
		`currency = 'USD' AND granularity = 'total' AND window_from_year = 0`)
	if start != day0(2024, time.February, 29) || last != day0(2024, time.June, 30) {
		t.Errorf("window %d to %d", start, last)
	}
	if math.Abs(twr.Float64-(math.Pow(1.01, 4)-1)) > 1e-9 {
		t.Errorf("twr = %v", twr.Float64)
	}
	if q != benchmarkStartClipped {
		t.Errorf("quality = %q", q)
	}
}

func TestMaterializeBenchmarkReadsInEachCurrency(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	// A franc buys a tenth fewer dollars by the end of February: 0.90, then
	// 0.81. A dollar fund gains that much read in francs.
	seedFX(t, db, dy(2024, time.February, 29), "USD", "CHF", "0.81")
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1, 2)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	twr, _, _, _, _, _, _ := benchmarkRow(t, db, ctx, `currency = 'CHF' AND granularity = 'monthly' AND period = '2024-02'`)
	if want := 1.02*0.90/0.81 - 1; math.Abs(twr.Float64-want) > 1e-9 {
		t.Errorf("February in francs = %v, want %v", twr.Float64, want)
	}
	usd, _, _, _, _, _, _ := benchmarkRow(t, db, ctx, `currency = 'USD' AND granularity = 'monthly' AND period = '2024-02'`)
	if math.Abs(usd.Float64-0.02) > 1e-12 {
		t.Errorf("February in dollars = %v", usd.Float64)
	}
}

func TestMaterializeBenchmarkFlagsAClampedRate(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	// EUR's first rate comes after the fund's first month.
	if _, err := db.ExecContext(ctx, `DELETE FROM fx_rates WHERE quote_currency = 'EUR' OR base_currency = 'EUR'`); err != nil {
		t.Fatal(err)
	}
	seedFX(t, db, dy(2024, time.February, 15), "USD", "EUR", "0.90")
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1, 2, 3)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, _, q := benchmarkRow(t, db, ctx, `currency = 'EUR' AND granularity = 'monthly' AND period = '2024-01'`)
	if q != "pre_fx_history" {
		t.Errorf("January in euros: quality = %q", q)
	}
	_, _, _, _, _, _, q = benchmarkRow(t, db, ctx, `currency = 'EUR' AND granularity = 'monthly' AND period = '2024-03'`)
	if q != "" {
		t.Errorf("March in euros: quality = %q", q)
	}
}

func TestMaterializeBenchmarkShadowAndItsMWR(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1, 1, 1, 1, 1, 1)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	line := func(series string, day int64) (value, mwr sql.NullFloat64) {
		if err := db.QueryRowContext(ctx, `SELECT value, mwr FROM benchmark_mwr WHERE currency = 'USD' AND window_from_year = 0
AND series = ? AND end_day = ?`, series, day).Scan(&value, &mwr); err != nil {
			t.Fatalf("%s at %d: %v", series, day, err)
		}
		return
	}
	june := day0(2024, time.June, 30)
	// 1000 grows six months and the 100 of March 31 three.
	want := 1000*math.Pow(1.01, 6) + 100*math.Pow(1.01, 3)
	shadow, shadowMWR := line("benchmark", june)
	if math.Abs(shadow.Float64-want) > 1e-6 {
		t.Errorf("shadow = %v, want %v", shadow.Float64, want)
	}
	actual, actualMWR := line("portfolio", june)
	if actual.Float64 != 1210 {
		t.Errorf("actual = %v", actual.Float64)
	}
	// The lines end on the tiles.
	var tile float64
	if err := db.QueryRowContext(ctx, `SELECT mwr FROM report_returns WHERE grain = 'global' AND granularity = 'total'
AND is_summary AND currency = 'USD' AND window_from_year = 0`).Scan(&tile); err != nil {
		t.Fatal(err)
	}
	if math.Abs(actualMWR.Float64-tile) > 1e-12 {
		t.Errorf("the household's line ends at %v, the tile says %v", actualMWR.Float64, tile)
	}
	_, _, _, mwr, _, _, _ := benchmarkRow(t, db, ctx, `currency = 'USD' AND granularity = 'total' AND window_from_year = 0`)
	if math.Abs(shadowMWR.Float64-mwr.Float64) > 1e-12 {
		t.Errorf("the benchmark's line ends at %v, its tile says %v", shadowMWR.Float64, mwr.Float64)
	}
	// Six months at 1%: the same flows in the benchmark earn about 6.15%.
	if mwr.Float64 < 0.055 || mwr.Float64 > 0.065 {
		t.Errorf("benchmark mwr = %v", mwr.Float64)
	}
}

func TestMaterializeBenchmarkLineHasAGapWhereTheShadowHasNoValue(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1, math.NaN(), 3, 4, 5, 6)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	var value, mwr sql.NullFloat64
	var q string
	if err := db.QueryRowContext(ctx, `SELECT value, mwr, quality FROM benchmark_mwr WHERE currency = 'USD'
AND window_from_year = 0 AND series = 'benchmark' AND end_day = ?`, day0(2024, time.June, 30)).Scan(&value, &mwr, &q); err != nil {
		t.Fatal(err)
	}
	if value.Valid || mwr.Valid || q != benchmarkGap {
		t.Errorf("past the gap: %v %v %q", value, mwr, q)
	}
}

func TestMaterializeBenchmarkWindowPastItsMonthsSaysSo(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	// The fund's months stop in 2023: the household's 2024 window shares
	// no day with them, and the row says so rather than leaving none.
	seedFundMonths(t, db, ctx, "symbol:IDX", time.Date(2023, time.October, 1, 0, 0, 0, 0, time.UTC), 1, 1, 1)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	twr, _, _, _, _, last, q := benchmarkRow(t, db, ctx,
		`currency = 'USD' AND granularity = 'total' AND window_from_year = 2024`)
	if twr.Valid || last != day0(2023, time.December, 31) || !strings.Contains(q, benchmarkNoOverlap) {
		t.Errorf("twr %v, last %d, quality %q", twr, last, q)
	}
}

func TestMaterializeBenchmarkWithNoReturnsWritesNothing(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	end := seedBenchmarkHousehold(t, db, ctx)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM benchmark_returns) + (SELECT count(*) FROM benchmark_mwr)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows", n)
	}
}

func TestLoadBenchmarksReadsFundsThroughTheirAliases(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO security_aliases (alias, security_key) VALUES ('symbol:IDX', 'isin:XX0000000001')`); err != nil {
		t.Skipf("security_aliases shape: %v", err)
	}
	seedFundMonths(t, db, ctx, "isin:XX0000000001", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 2)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	got, err := LoadBenchmarks(ctx, db, fxBounds{}, []string{"USD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].index("USD") == nil {
		t.Fatalf("benchmarks = %+v", got)
	}
}

func TestWantedNamesABenchmarksFunds(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBenchmark(t, db, ctx, "Test", canonical.BenchmarkFund{FundRef: "symbol:IDX", WeightPct: 100})
	if _, err := db.ExecContext(ctx, `INSERT INTO fund_returns (silver_source_id, fund_ref, as_of, month, total_return)
VALUES ('test-src', 'symbol:IDX', ?, '2024-03-31', 1)`, day0(2024, time.March, 31)); err != nil {
		t.Fatal(err)
	}
	w, err := RefdataWanted(ctx, db, time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Funds) != 1 {
		t.Fatalf("funds = %+v", w.Funds)
	}
	f := w.Funds[0]
	if f.Ref != "symbol:IDX" || *f.Symbol != "IDX" || strings.Join(f.Roles, ",") != "benchmark" {
		t.Errorf("fund = %+v", f)
	}
	if *f.HeldFrom != "2024-06-01" || f.HeldTo != nil || *f.HistoryFrom != benchmarkHistoryFrom {
		t.Errorf("days: held from %v to %v, history from %v", *f.HeldFrom, f.HeldTo, *f.HistoryFrom)
	}
	if strings.Join(f.Wants, ",") != "monthly_returns,characteristics" {
		t.Errorf("wants = %v", f.Wants)
	}
	if got := f.Facts["monthly_returns"]; len(got) != 1 || got[0] != "2024-03-31" {
		t.Errorf("facts = %v", f.Facts)
	}
}

func TestTwoFilingsOfAMonthReadAsTheLater(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, f := range []struct {
		asOf int64
		pct  float64
	}{{dy(2024, time.March, 31), 1}, {dy(2024, time.April, 30), 2}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO fund_returns (silver_source_id, fund_ref, as_of, month, total_return)
VALUES ('test-src', 'symbol:IDX', ?, '2024-03-31', ?)`, f.asOf, f.pct); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readFundMonths(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["symbol:IDX"][month(2024, time.March)]; math.Abs(r-0.02) > 1e-12 {
		t.Errorf("March = %v, want the later filing's", r)
	}
}
