package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// benchmarkTables are what MaterializeReturns writes beside report_returns.
type benchmarkTables struct {
	returns []BenchmarkRow
	mwr     []BenchmarkMWRRow
}

const (
	benchmarkReturnCols = `computed_at, benchmark, currency, granularity, window_from_year, period, start_day, end_day,
    twr, twr_annualized, portfolio_twr, portfolio_twr_annualized, excess, excess_annualized,
    portfolio_mwr, mwr, mwr_annualized, quality`
	benchmarkMWRCols = `computed_at, benchmark, currency, window_from_year, series, end_day, value, mwr, quality`
)

// computeBenchmarks is every benchmark beside the household in every
// reporting currency: the buckets of globals, the household's rows at the
// global grain by currency and granularity; one total per window, since
// inception and since each year from minYear through maxYear; and each
// window's two money-weighted lines. The household's line of a window
// holds for every benchmark that starts it on the same day.
func computeBenchmarks(ctx context.Context, db *sql.DB, datasets map[string]*returnsDataset, fx fxBounds,
	p MaterializeParams, minYear, maxYear int, globals map[[2]string][]ReturnRow) (*benchmarkTables, error) {
	out := &benchmarkTables{}
	if ok, err := hasTable(ctx, db, "benchmark_funds"); err != nil || !ok {
		return out, err
	}
	benchmarks, err := LoadBenchmarks(ctx, db, fx, materializeCurrencies)
	if err != nil {
		return nil, fmt.Errorf("MaterializeReturns benchmarks: %w", err)
	}
	years := []int{0}
	for y := minYear; y <= maxYear; y++ {
		years = append(years, y)
	}
	// Each currency has a dataset of its own, so they run side by side.
	parts := make([]benchmarkTables, len(materializeCurrencies))
	var wg sync.WaitGroup
	for i, ccy := range materializeCurrencies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parts[i] = benchmarksInCurrency(benchmarks, ccy, datasets[ccy], p, years, globals)
		}()
	}
	wg.Wait()
	for i := range parts {
		out.returns = append(out.returns, parts[i].returns...)
		out.mwr = append(out.mwr, parts[i].mwr...)
	}
	return out, nil
}

// benchmarksInCurrency is computeBenchmarks in one currency.
func benchmarksInCurrency(benchmarks []*Benchmark, ccy string, ds *returnsDataset, p MaterializeParams, years []int,
	globals map[[2]string][]ReturnRow) benchmarkTables {
	var out benchmarkTables
	windows := newBenchmarkWindows(ds)
	lines := map[[2]int64][]BenchmarkMWRRow{}
	for _, b := range benchmarks {
		if b.index(ccy) == nil {
			continue
		}
		for _, granularity := range []string{"monthly", "quarterly", "annual"} {
			out.returns = append(out.returns, benchmarkBuckets(b, ccy, granularity, globals[[2]string{ccy, granularity}])...)
		}
		for _, year := range years {
			var from int64
			if year > 0 {
				from = time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
			}
			row, w := benchmarkTotal(windows, b, ReturnParams{
				FromEpoch: from, ToEpoch: p.ToEpoch, OutCcy: ccy, Annualize: "auto", Netting: true, Inception: "full",
				InceptionOverrides: p.InceptionOverrides, ReturnsExclude: p.ReturnsExclude, ReturnsHide: p.ReturnsHide,
			}, year)
			if row.EndDay > 0 {
				out.returns = append(out.returns, row)
			}
			// A window with no opening value has no line to start level.
			if w == nil || w.v0 <= valueTol {
				continue
			}
			key := [2]int64{w.from, w.ownTo}
			if _, ok := lines[key]; !ok {
				lines[key] = portfolioLine(w)
			}
			for _, side := range [][]BenchmarkMWRRow{lines[key], shadowLine(b.index(ccy), w)} {
				for _, r := range side {
					r.Benchmark, r.Currency, r.WindowFromYear = b.Name, ccy, year
					out.mwr = append(out.mwr, r)
				}
			}
		}
	}
	return out
}

// write replaces the benchmark tables' rows and returns how many it
// wrote. A gold before migration 0176 has neither.
func (t *benchmarkTables) write(ctx context.Context, tx *sql.Tx, computedAt int64) (int, error) {
	if ok, err := hasTable(ctx, tx, "benchmark_returns"); err != nil || !ok {
		return 0, err
	}
	for _, table := range []string{"benchmark_returns", "benchmark_mwr"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return 0, fmt.Errorf("%s: delete: %w", table, err)
		}
	}
	if len(t.returns) > 0 {
		if err := insertColumns(ctx, tx, "benchmark_returns", "benchmark_returns", benchmarkReturnCols, len(t.returns),
			func(i int, args []any) []any {
				r := &t.returns[i]
				return append(args, computedAt, r.Benchmark, r.Currency, r.Granularity, r.WindowFromYear, r.Period,
					r.StartDay*SecondsPerDay, r.EndDay*SecondsPerDay, r.TWR, r.TWRAnnualized, r.PortfolioTWR,
					r.PortfolioTWRAnnualized, r.Excess, r.ExcessAnnualized, r.PortfolioMWR, r.MWR, r.MWRAnnualized,
					strings.Join(r.Quality, ";"))
			}); err != nil {
			return 0, err
		}
	}
	if len(t.mwr) > 0 {
		if err := insertColumns(ctx, tx, "benchmark_mwr", "benchmark_mwr", benchmarkMWRCols, len(t.mwr),
			func(i int, args []any) []any {
				r := &t.mwr[i]
				return append(args, computedAt, r.Benchmark, r.Currency, r.WindowFromYear, r.Series,
					r.EndDay*SecondsPerDay, r.Value, r.MWR, strings.Join(r.Quality, ";"))
			}); err != nil {
			return 0, err
		}
	}
	return len(t.returns) + len(t.mwr), nil
}
