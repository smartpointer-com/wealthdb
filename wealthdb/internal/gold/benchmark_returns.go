package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// The quality flags of a benchmark's figures (docs/RETURNS-NOTES.md,
// "Benchmarks").
const (
	// benchmarkGap: a month of the span has no return for every fund of
	// the mix, so the span has none.
	benchmarkGap = "benchmark_gap"
	// benchmarkPartialPeriod: the benchmark's months start or end inside
	// the bucket.
	benchmarkPartialPeriod = "benchmark_partial_period"
	// benchmarkStartClipped: the window starts before the benchmark's
	// first month, and compares from there.
	benchmarkStartClipped = "benchmark_start_clipped"
	// benchmarkEndClipped: the window ends after the benchmark's last
	// month, and compares through it.
	benchmarkEndClipped = "benchmark_end_clipped"
	// benchmarkNoOverlap: the window and the benchmark's months share no day.
	benchmarkNoOverlap = "benchmark_no_overlap"
	// shadowNonPositive: the household's flows took out more than the
	// shadow holds, so the shadow has no money-weighted return.
	shadowNonPositive = "shadow_nonpositive"
)

// benchmarkFundCurrency is the currency of a benchmark fund's return.
// N-PORT states every fund in US dollars.
const benchmarkFundCurrency = "USD"

// Benchmark is one benchmark as gold reads it: its name and, per
// reporting currency, its monthly index.
type Benchmark struct {
	Name    string
	indexes map[string]*returns.Index
	// clamped are the month ends whose FX fell back to the day-0 clamp,
	// per currency.
	clamped map[string]map[int64]bool
}

// index is the benchmark's index in a currency, nil where it has no
// months.
func (b *Benchmark) index(ccy string) *returns.Index { return b.indexes[ccy] }

// benchmarkFundRow is one fund of a mix, its key read through the aliases.
type benchmarkFundRow struct {
	benchmark, key string
	weight         float64
}

// readBenchmarkMix is every benchmark's funds in name order, read through
// the aliases as a composition is.
func readBenchmarkMix(ctx context.Context, q querier) ([]benchmarkFundRow, error) {
	var out []benchmarkFundRow
	err := scanAll(ctx, q, `SELECT benchmark, exposure_key(fund_ref), weight_pct FROM benchmark_funds ORDER BY 1, 2`,
		func(rows *sql.Rows) error {
			var r benchmarkFundRow
			if err := rows.Scan(&r.benchmark, &r.key, &r.weight); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("benchmark funds: %w", err)
	}
	return out, nil
}

// readFundMonths is each fund's return per month end day, as a ratio. Two
// filings of one month read as the later one.
func readFundMonths(ctx context.Context, q querier) (map[string]map[int64]float64, error) {
	out := map[string]map[int64]float64{}
	err := scanAll(ctx, q, `
WITH r AS (SELECT exposure_key(fund_ref) AS k, month, as_of, total_return FROM fund_returns)
SELECT k, CAST(epoch(month) AS BIGINT) // 86400, CAST(total_return AS DOUBLE) / 100
  FROM r
QUALIFY row_number() OVER (PARTITION BY k, month ORDER BY as_of DESC) = 1`, func(rows *sql.Rows) error {
		var key string
		var day int64
		var r float64
		if err := rows.Scan(&key, &day, &r); err != nil {
			return err
		}
		if out[key] == nil {
			out[key] = map[int64]float64{}
		}
		out[key][day] = r
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fund returns: %w", err)
	}
	return out, nil
}

// monthEndsBetween are the last days of the months from first's through
// last's, both month ends.
func monthEndsBetween(first, last int64) []int64 {
	var out []int64
	t := returns.DayToTimeUTC(first)
	for {
		end := EpochDay(time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Unix())
		if end > last {
			return out
		}
		out = append(out, end)
		t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
}

// blendMonths is a mix's return per month end, r = Σ wᵢ rᵢ, from the
// latest first month of its funds through the earliest last month. A
// month that lacks a fund is a gap: an entry with OK false. A fund with
// no months leaves the mix with none.
func blendMonths(mix []benchmarkFundRow, funds map[string]map[int64]float64) []returns.BenchmarkMonth {
	var first, last int64
	for i, f := range mix {
		months := funds[f.key]
		if len(months) == 0 {
			return nil
		}
		lo, hi := int64(math.MaxInt64), int64(0)
		for d := range months {
			lo, hi = min(lo, d), max(hi, d)
		}
		if i == 0 {
			first, last = lo, hi
		}
		first, last = max(first, lo), min(last, hi)
	}
	if first > last {
		return nil
	}
	ends := monthEndsBetween(first, last)
	out := make([]returns.BenchmarkMonth, len(ends))
	for i, end := range ends {
		out[i].End, out[i].OK = end, true
		for _, f := range mix {
			r, ok := funds[f.key][end]
			if !ok {
				out[i].R, out[i].OK = 0, false
				break
			}
			out[i].R += f.weight / 100 * r
		}
	}
	return out
}

// readDollarRates is the dollar's rate in each currency on each day: the
// last on or before it, by the chain exposure_holdings values a holding
// by. A currency with no rate has none on the day.
func readDollarRates(ctx context.Context, q querier, days []int64, ccys []string) (map[string]map[int64]float64, error) {
	out := map[string]map[int64]float64{}
	for _, c := range ccys {
		out[c] = map[int64]float64{}
	}
	if len(days) == 0 {
		return out, nil
	}
	err := scanAll(ctx, q, `
WITH z AS (SELECT d.day, c.ccy
             FROM (SELECT unnest(CAST(? AS BIGINT[])) AS day) d,
                  (SELECT unnest(CAST(? AS VARCHAR[])) AS ccy) c)
SELECT z.ccy, z.day, COALESCE(o0.rate, o1.rate * o2.rate)
  FROM z
  ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = 'USD' AND o0.to_ccy = z.ccy AND o0.day <= z.day
  ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = 'USD' AND o1.to_ccy = 'CHF' AND o1.day <= z.day
  ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = z.ccy AND o2.day <= z.day`,
		func(rows *sql.Rows) error {
			var ccy string
			var day int64
			var rate sql.NullFloat64
			if err := rows.Scan(&ccy, &day, &rate); err != nil {
				return err
			}
			if rate.Valid {
				out[ccy][day] = rate.Float64
			}
			return nil
		}, days, ccys)
	if err != nil {
		return nil, fmt.Errorf("benchmark fx: %w", err)
	}
	return out, nil
}

// inCurrency is months read in another currency, unhedged, at month-end
// FX: (1 + r) × FX_end / FX_start − 1. rates holds the dollar's rate in
// the currency per day, the month end before the first among them. A
// month without either rate is a gap.
func inCurrency(months []returns.BenchmarkMonth, rates map[int64]float64) []returns.BenchmarkMonth {
	out := slices.Clone(months)
	for i := range out {
		end, ok1 := rates[out[i].End]
		start, ok0 := rates[monthEndBefore(out[i].End)]
		if !ok0 || !ok1 || start == 0 {
			out[i].R, out[i].OK = 0, false
			continue
		}
		out[i].R = (1+out[i].R)*end/start - 1
	}
	return out
}

// monthEndBefore is the last day of the month before the one that holds
// day.
func monthEndBefore(day int64) int64 {
	return day - int64(returns.DayToTimeUTC(day).Day())
}

// LoadBenchmarks reads every benchmark with its index in each of ccys.
// A benchmark none of whose funds have returns has no index.
func LoadBenchmarks(ctx context.Context, q querier, fx fxBounds, ccys []string) ([]*Benchmark, error) {
	mix, err := readBenchmarkMix(ctx, q)
	if err != nil || len(mix) == 0 {
		return nil, err
	}
	funds, err := readFundMonths(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []*Benchmark
	var all [][]returns.BenchmarkMonth
	for start := 0; start < len(mix); {
		end := start
		for end < len(mix) && mix[end].benchmark == mix[start].benchmark {
			end++
		}
		out = append(out, &Benchmark{Name: mix[start].benchmark, indexes: map[string]*returns.Index{},
			clamped: map[string]map[int64]bool{}})
		all = append(all, blendMonths(mix[start:end], funds))
		start = end
	}
	seen := map[int64]bool{}
	for _, months := range all {
		for _, m := range months {
			seen[m.End], seen[monthEndBefore(m.End)] = true, true
		}
	}
	days := make([]int64, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	slices.Sort(days)
	foreign := slices.DeleteFunc(slices.Clone(ccys), func(c string) bool { return c == benchmarkFundCurrency })
	rates, err := readDollarRates(ctx, q, days, foreign)
	if err != nil {
		return nil, err
	}
	for i, b := range out {
		for _, ccy := range ccys {
			months := all[i]
			if ccy != benchmarkFundCurrency {
				months = inCurrency(months, rates[ccy])
				b.clamped[ccy] = clampedMonths(months, fx, ccy)
			}
			b.indexes[ccy] = returns.NewIndex(months)
		}
	}
	return out, nil
}

// clampedMonths are the month ends of months read at an FX rate from
// before the pair's history: the rate at the month's start or end is the
// day-0 clamp.
func clampedMonths(months []returns.BenchmarkMonth, fx fxBounds, ccy string) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range months {
		start := monthEndBefore(m.End)
		if fx.clampedBefore(benchmarkFundCurrency, ccy, start) || fx.clampedBefore(ccy, benchmarkFundCurrency, start) {
			out[m.End] = true
		}
	}
	return out
}

// clampedIn says a month of the days after from through to was read at a
// clamped rate.
func (b *Benchmark) clampedIn(ccy string, from, to int64) bool {
	for end := range b.clamped[ccy] {
		if end > from && monthEndBefore(end) < to {
			return true
		}
	}
	return false
}

// BenchmarkRow is one row of benchmark_returns: the benchmark beside the
// household over the same days.
type BenchmarkRow struct {
	Benchmark, Currency, Granularity string
	WindowFromYear                   int
	Period                           string
	StartDay, EndDay                 int64 // epoch days
	TWR, TWRAnnualized               *float64
	PortfolioTWR                     *float64
	PortfolioTWRAnnualized           *float64
	Excess, ExcessAnnualized         *float64
	PortfolioMWR, MWR, MWRAnnualized *float64
	Quality                          []string
}

// minus is a − b, nil where either is.
func minus(a, b *float64) *float64 {
	if a == nil || b == nil {
		return nil
	}
	return f64(*a - *b)
}

// benchmarkBuckets are the benchmark's returns on the household's
// buckets `rows`, the per-period rows of one granularity at the global
// grain. A bucket before the benchmark's first month has no row. One that
// its months end inside, or that runs through a gap, has a row with no
// return and the flag that says why. One after its last month has no row.
func benchmarkBuckets(b *Benchmark, ccy, granularity string, rows []ReturnRow) []BenchmarkRow {
	ix := b.index(ccy)
	if ix == nil {
		return nil
	}
	var out []BenchmarkRow
	for _, r := range rows {
		if r.IsSummary || r.EndDay <= ix.Start() || r.StartDay >= ix.End() {
			continue
		}
		row := BenchmarkRow{Benchmark: b.Name, Currency: ccy, Granularity: granularity, Period: r.Period,
			StartDay: r.StartDay, EndDay: r.EndDay, PortfolioTWR: r.TWR, Quality: []string{}}
		switch {
		case r.StartDay < ix.Start():
			row.Quality = append(row.Quality, benchmarkPartialPeriod)
		case r.EndDay > ix.End():
			row.Quality = append(row.Quality, benchmarkPartialPeriod)
		default:
			if g, ok := ix.Growth(r.StartDay, r.EndDay); ok {
				row.TWR = f64(g - 1)
				row.Excess = minus(row.PortfolioTWR, row.TWR)
			} else {
				row.Quality = append(row.Quality, benchmarkGap)
			}
			if b.clampedIn(ccy, r.StartDay, r.EndDay) {
				row.Quality = append(row.Quality, "pre_fx_history")
			}
		}
		out = append(out, row)
	}
	return out
}

// benchmarkWindow is a window's days on both sides and what the
// household's flows are over them: the window compared.
type benchmarkWindow struct {
	assets   []*accountData
	from, to int64
	ownTo    int64 // the last day of the household's own window
	params   ReturnParams
	flows    []returns.Flow
	av       func(int64) (float64, bool)
	v0       float64
	row      ReturnRow // the household's summary over the days compared
	quality  []string
	ok       bool
}

// benchmarkWindows reads the household's side of the windows of one
// dataset. Benchmarks whose months span the same days share it, so each
// is computed once.
type benchmarkWindows struct {
	ds        *returnsDataset
	summaries map[[2]int64]*ReturnRow
	compared  map[[4]int64]*benchmarkWindow
}

func newBenchmarkWindows(ds *returnsDataset) *benchmarkWindows {
	return &benchmarkWindows{ds: ds, summaries: map[[2]int64]*ReturnRow{}, compared: map[[4]int64]*benchmarkWindow{}}
}

// summary is the household's summary row over p's window at the global
// grain, nil where it has none.
func (c *benchmarkWindows) summary(p ReturnParams) *ReturnRow {
	key := [2]int64{p.FromEpoch, p.ToEpoch}
	if r, ok := c.summaries[key]; ok {
		return r
	}
	var out *ReturnRow
	for _, r := range computeReturns(c.ds, p) {
		if r.IsSummary {
			out = &r
		}
	}
	c.summaries[key] = out
	return out
}

// compare narrows the household's window p to the days the index has, and
// computes the household over them. ok on the result is false where the
// two share no day or the household has no row there, the flags saying
// why.
func (c *benchmarkWindows) compare(ix *returns.Index, p ReturnParams) *benchmarkWindow {
	p.Level, p.Period, p.Method = "global", "total", "both"
	whole := c.summary(p)
	if whole == nil {
		return &benchmarkWindow{}
	}
	key := [4]int64{p.FromEpoch, p.ToEpoch, ix.Start(), ix.End()}
	if w, ok := c.compared[key]; ok {
		return w
	}
	w := &benchmarkWindow{from: whole.StartDay, to: whole.EndDay, ownTo: whole.EndDay}
	c.compared[key] = w
	if w.from < ix.Start() {
		w.from = ix.Start()
		w.quality = append(w.quality, benchmarkStartClipped)
	}
	if w.to > ix.End() {
		w.to = ix.End()
		w.quality = append(w.quality, benchmarkEndClipped)
	}
	if w.to <= w.from {
		w.quality = append(w.quality, benchmarkNoOverlap)
		return w
	}
	row := whole
	if w.from != whole.StartDay || w.to != whole.EndDay {
		p.FromEpoch, p.ToEpoch = w.from*SecondsPerDay, w.to*SecondsPerDay
		if row = c.summary(p); row == nil {
			return w
		}
	}
	w.row, w.from, w.to, w.params = *row, row.StartDay, row.EndDay, p
	groups, order := groupAccounts("global", c.ds.accts, p.ReturnsExclude)
	if len(order) == 0 {
		return w
	}
	_, w.assets = splitLiabilities(groups[order[0]])
	if len(w.assets) == 0 {
		return w
	}
	w.av = entityValue(w.assets)
	w.flows, _, _ = entityFlows(w.assets, p, w.from, w.to, true)
	w.v0, _ = w.av(w.from)
	w.ok = true
	return w
}

// flowQuality are the row's flags a benchmark's figures inherit: the
// household's flows are the benchmark's too.
func flowQuality(q []string) []string {
	return slices.DeleteFunc(slices.Clone(q), func(f string) bool {
		return f != "fx_clamped_flow" && f != "pre_fx_history"
	})
}

// benchmarkTotal is the benchmark's row for the window p, the household
// beside it over the same days, and its public market equivalent: the
// XIRR of the household's flows with the shadow's value at the end.
func benchmarkTotal(c *benchmarkWindows, b *Benchmark, p ReturnParams, year int) (BenchmarkRow, *benchmarkWindow) {
	row := BenchmarkRow{Benchmark: b.Name, Currency: p.OutCcy, Granularity: "total", WindowFromYear: year,
		Period: "total", Quality: []string{}}
	ix := b.index(p.OutCcy)
	if ix == nil {
		row.Quality = append(row.Quality, benchmarkNoOverlap)
		return row, nil
	}
	w := c.compare(ix, p)
	row.Quality = append(row.Quality, w.quality...)
	row.StartDay, row.EndDay = w.from, w.to
	if !w.ok {
		return row, nil
	}
	row.Period = w.row.Period
	row.PortfolioTWR, row.PortfolioTWRAnnualized, row.PortfolioMWR = w.row.TWR, w.row.TWRAnnualized, w.row.MWR
	row.Quality = append(row.Quality, flowQuality(w.row.Quality)...)
	if b.clampedIn(p.OutCcy, w.from, w.to) {
		row.Quality = append(row.Quality, "pre_fx_history")
	}
	g, grew := ix.Growth(w.from, w.to)
	if !grew {
		row.Quality = dedupeStrings(append(row.Quality, benchmarkGap))
		return row, w
	}
	days := float64(w.to - w.from)
	row.TWR = f64(g - 1)
	if returns.ShouldAnnualize(p.Annualize, days) {
		row.TWRAnnualized = f64(returns.Annualize(g-1, days))
	}
	row.Excess = minus(row.PortfolioTWR, row.TWR)
	row.ExcessAnnualized = minus(row.PortfolioTWRAnnualized, row.TWRAnnualized)
	shadow, _, short := ix.ShadowValues(w.v0, w.from, w.flows, []int64{w.to})
	var q []string
	if short[0] || shadow[0] <= valueTol {
		q = append(q, shadowNonPositive)
	} else {
		row.MWR, row.MWRAnnualized, q = computeMWR(w.v0, shadow[0], w.from, w.to, w.flows, w.assets, p, nil)
	}
	row.Quality = dedupeStrings(append(row.Quality, q...))
	return row, w
}

// BenchmarkMWRRow is one row of benchmark_mwr.
type BenchmarkMWRRow struct {
	Benchmark, Currency, Series string
	WindowFromYear              int
	EndDay                      int64 // epoch days
	Value, MWR                  *float64
	Quality                     []string
}

// The sides of benchmark_mwr.
const (
	seriesPortfolio = "portfolio"
	seriesBenchmark = "benchmark"
)

// lineEnds are the days of a window's money-weighted lines: each month
// end after its start, through its end.
func lineEnds(from, to int64) []int64 {
	var out []int64
	for _, end := range monthEndsBetween(from, to) {
		if end > from {
			out = append(out, end)
		}
	}
	if len(out) == 0 || out[len(out)-1] != to {
		out = append(out, to)
	}
	return out
}

// mwrLine is the money-weighted return from the window's start to each
// of ends, value(i) being what the side holds there. A day `blocked`
// flags has no return. Nor has a day XIRR finds no answer for, with the
// mwr_* flag. Each month end starts from the rate of the one before, over
// the flows netted by day. The last is solved cold over the flows
// themselves, as the summary row's figure is, so the line ends on the
// tile's number.
func mwrLine(w *benchmarkWindow, ends []int64, value func(i int) float64, blocked func(i int) []string) ([]*float64, [][]string) {
	mwr, flags := make([]*float64, len(ends)), make([][]string, len(ends))
	series := returns.NewFlowSeries(w.from, w.flows)
	guess := 0.1
	for i, end := range ends {
		if flags[i] = blocked(i); flags[i] != nil {
			continue
		}
		var rate *float64
		if i == len(ends)-1 {
			rate, flags[i] = solveMWR(returns.XIRR, w.v0, value(i), w.from, end, flowsIn(w.flows, w.from, end), w.assets, nil)
		} else {
			flows := series.Upto(end)
			rate, flags[i] = solveMWR(func(v0, v1 float64, _, to int64, _ []returns.Flow) (float64, error) {
				return series.Solve(guess, v0, v1, to, len(flows))
			}, w.v0, value(i), w.from, end, flows, w.assets, nil)
		}
		if rate != nil {
			guess = *rate
			mwr[i] = f64(returns.DeAnnualize(*rate, float64(end-w.from)))
		}
	}
	return mwr, flags
}

// portfolioLine is the household's side of a window's lines, through the
// end of the household's own window, so its last point is the Return
// (MWR) tile's where the window starts inside the benchmark's months.
func portfolioLine(w *benchmarkWindow) []BenchmarkMWRRow {
	whole := *w
	whole.to = w.ownTo
	whole.flows, _, _ = entityFlows(w.assets, w.params, w.from, w.ownTo, true)
	ends := lineEnds(whole.from, whole.to)
	mwr, flags := mwrLine(&whole, ends, func(i int) float64 {
		v, _ := w.av(ends[i])
		return v
	}, func(int) []string { return nil })
	out := []BenchmarkMWRRow{lineStart(seriesPortfolio, w)}
	for i, end := range ends {
		r := BenchmarkMWRRow{Series: seriesPortfolio, EndDay: end, MWR: mwr[i], Quality: flags[i]}
		if v, ok := w.av(end); ok {
			r.Value = f64(v)
		}
		out = append(out, r)
	}
	return out
}

// lineStart is a line's first row: the window's start value, with no
// return yet, so both sides start level.
func lineStart(series string, w *benchmarkWindow) BenchmarkMWRRow {
	return BenchmarkMWRRow{Series: series, EndDay: w.from, Value: f64(w.v0), Quality: []string{}}
}

// shadowLine is the benchmark's side of a window's lines: the same start
// value and flows in the benchmark, through its last month.
func shadowLine(ix *returns.Index, w *benchmarkWindow) []BenchmarkMWRRow {
	ends := lineEnds(w.from, w.to)
	shadow, ok, short := ix.ShadowValues(w.v0, w.from, w.flows, ends)
	mwr, flags := mwrLine(w, ends, func(i int) float64 { return shadow[i] }, func(i int) []string {
		switch {
		case !ok[i]:
			return []string{benchmarkGap}
		case short[i] || shadow[i] <= valueTol:
			return []string{shadowNonPositive}
		}
		return nil
	})
	out := []BenchmarkMWRRow{lineStart(seriesBenchmark, w)}
	for i, end := range ends {
		r := BenchmarkMWRRow{Series: seriesBenchmark, EndDay: end, MWR: mwr[i], Quality: flags[i]}
		if ok[i] {
			r.Value = f64(shadow[i])
		}
		out = append(out, r)
	}
	return out
}
