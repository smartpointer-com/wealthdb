package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// printBenchmarks is `wealthdb help benchmarks`: the built-in set and the
// config's, each with its funds and weights.
func printBenchmarks(g globalFlags, w io.Writer) error {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	fmt.Fprint(w, `Benchmarks are named fund mixes, rebalanced monthly. Name one with
--benchmark on 'wealthdb returns global' and 'wealthdb exposure', or pick
one on the dashboards. The benchmarks block of wealthdb.cfg adds one, or
redefines a built-in one by its name.

`)
	for _, b := range cfg.BenchmarkDefs() {
		funds := make([]string, len(b.Funds))
		for i, f := range b.Funds {
			funds[i] = fmt.Sprintf("%s %g", f.FundRef, f.WeightPct)
		}
		origin := "configured"
		if b.Builtin {
			origin = "built-in"
		}
		fmt.Fprintf(w, "  %-24s %-10s %s\n", b.Name, origin, strings.Join(funds, ", "))
	}
	return nil
}

// benchmarkParamDoc is an MCP benchmark parameter's text: what it does on
// the tool, then the built-in names. A config may add others.
func benchmarkParamDoc(doc string) string {
	names := config.BuiltinBenchmarkNames()
	return doc + " One of " + strings.Join(names, ", ") + ", or a name from the config's benchmarks block."
}

// exposureBenchmarkViews are the views --benchmark applies to, each with
// the dimension of benchmark_exposure its rows read.
var exposureBenchmarkViews = map[string]string{
	"classes": gold.BenchmarkDimClass, "sectors": gold.BenchmarkDimClassSector, "sizes": gold.BenchmarkDimSize,
	"countries": gold.BenchmarkDimCountry, "currencies": gold.BenchmarkDimCurrencyKind,
	"credit": "credit", "maturities": "maturity", "coupons": "coupon", "cycles": "cycle",
	"securities": gold.BenchmarkDimSecurity, "issuers": gold.BenchmarkDimIssuer,
}

// checkExposureBenchmark refuses --benchmark where it describes nothing:
// a view with no such dimension, or a series, since a benchmark's
// look-through is today's alone. A benchmark places a security in its
// country of risk, so the domicile basis has nothing to compare with.
func checkExposureBenchmark(view, benchmark, country string, series []seriesPoint) error {
	if benchmark == "" {
		return nil
	}
	if country != exposureCountryBases[0] {
		return errs.Newf(errs.ExitUsage, "exposure: a benchmark reads the country of risk; --benchmark takes no --country %s", country)
	}
	if _, ok := exposureBenchmarkViews[view]; !ok {
		names := make([]string, 0, len(exposureBenchmarkViews))
		for v := range exposureBenchmarkViews {
			names = append(names, v)
		}
		slices.Sort(names)
		return errs.Newf(errs.ExitUsage, "exposure: --benchmark belongs to the %s views", strings.Join(names, ", "))
	}
	if len(series) > 0 {
		return errs.Newf(errs.ExitUsage, "exposure: a benchmark's look-through is today's; --benchmark takes no --period")
	}
	return nil
}

// benchmarkWeights are a benchmark's shares in one dimension, by bucket.
// Only gold knows them: the report runs load before any row renders
// (withLoads).
type benchmarkWeights map[string]float64

// load reads the shares the last `web refresh` wrote. A benchmark with
// none is an error: it says what to run.
func (w benchmarkWeights) load(name, dimension string) func(context.Context, *sql.DB) error {
	return func(ctx context.Context, db *sql.DB) error {
		read, err := gold.BenchmarkWeights(ctx, db, name, dimension)
		if err != nil {
			return err
		}
		if len(read) == 0 {
			names, err := gold.BenchmarkNames(ctx, db)
			if err != nil {
				return err
			}
			if !slices.Contains(names, name) {
				return errs.Newf(errs.ExitUsage, "exposure: no benchmark named %q; 'wealthdb help benchmarks' lists them", name)
			}
			return errs.Newf(errs.ExitUsage, "exposure: %q has no look-through yet; run 'wealthdb web refresh'", name)
		}
		for k, v := range read {
			w[k] = v
		}
		return nil
	}
}

// of is the weight of the row's bucket: a key the benchmark does not hold
// weighs zero.
func (w benchmarkWeights) of(key string) *float64 {
	v := w[key]
	return &v
}

// benchmarkView is seriesView with, under req.benchmark, the benchmark's
// weight of each row's bucket and the row's share less it. key names the
// row's bucket of the view's dimension.
func benchmarkView[T any](req request, registry []columnSpec[T], defaults []string, key func(T) string,
	share func(T) *float64, fetch func(context.Context, *sql.DB, int64) ([]T, error)) *report {
	if req.benchmark == "" {
		return seriesView(req, registry, defaults, fetch)
	}
	weights := benchmarkWeights{}
	weight := pctCol("benchmark", func(r T) *float64 { return weights.of(key(r)) })
	difference := pctCol("difference", func(r T) *float64 {
		s := share(r)
		if s == nil {
			return nil
		}
		d := *s - *weights.of(key(r))
		return &d
	})
	cols := append(slices.Clone(registry), weight, difference)
	shown := slices.Clone(defaults)
	at := len(shown)
	if i := slices.Index(shown, "quality"); i >= 0 {
		at = i
	}
	shown = slices.Insert(shown, at, "benchmark", "difference")
	return withLoads(seriesView(req, cols, shown, fetch),
		weights.load(req.benchmark, exposureBenchmarkViews[req.view]))
}
