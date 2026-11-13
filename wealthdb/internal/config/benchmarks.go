package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// builtinBenchmarks is the set the engine ships, on with no config. Each
// fund stands in for an index: a fund's total return is public, and an
// index's own series is licensed.
var builtinBenchmarks = map[string]map[string]float64{
	"S&P 500":              {"symbol:IVV": 100},
	"World (ACWI)":         {"symbol:ACWI": 100},
	"US total bond market": {"symbol:BND": 100},
	"Global 60/40":         {"symbol:ACWI": 60, "symbol:BND": 40},
	"Global 80/20":         {"symbol:ACWI": 80, "symbol:BND": 20},
}

// BuiltinBenchmarkNames are the built-in set's names, sorted.
func BuiltinBenchmarkNames() []string { return slices.Sorted(maps.Keys(builtinBenchmarks)) }

// benchmarkWeightTol is how far a mix's weights may sum from 100: float
// noise, not a typo.
const benchmarkWeightTol = 1e-9

// validateBenchmarks checks the benchmarks block and compiles it with
// the built-in set (compileBenchmarks).
func (c *Config) validateBenchmarks(map[string]bool) error {
	defs, err := compileBenchmarks(c.Benchmarks)
	if err != nil {
		return err
	}
	c.benchmarkDefs = defs
	return nil
}

// compileBenchmarks checks the configured mixes and returns the
// built-in set with them over it, sorted by name. A configured name
// replaces the built-in one whole. Every name is usable and differs
// from the others by more than its case, every fund reference parses
// and names a fund once, every weight is positive, and each mix sums
// to 100.
func compileBenchmarks(configured map[string]map[string]float64) ([]canonical.Benchmark, error) {
	folded := map[string]string{}
	for name := range builtinBenchmarks {
		folded[strings.ToLower(name)] = name
	}
	funds := map[string][]canonical.BenchmarkFund{}
	for _, name := range slices.Sorted(maps.Keys(configured)) {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return nil, fmt.Errorf("config: benchmarks[%q]: give the benchmark a name with no spaces around it", name)
		}
		if strings.EqualFold(name, canonical.NoBenchmark) {
			return nil, fmt.Errorf("config: benchmarks[%q]: %q is the picker's value for no benchmark; choose another name",
				name, canonical.NoBenchmark)
		}
		if prev, dup := folded[strings.ToLower(name)]; dup && prev != name {
			return nil, fmt.Errorf("config: benchmarks[%q]: it differs from %q by case alone; use that exact name to redefine it, or another name",
				name, prev)
		}
		folded[strings.ToLower(name)] = name
		mix, err := compileBenchmarkMix(name, configured[name])
		if err != nil {
			return nil, err
		}
		funds[name] = mix
	}
	for name, mix := range builtinBenchmarks {
		if _, redefined := funds[name]; redefined {
			continue
		}
		compiled, err := compileBenchmarkMix(name, mix)
		if err != nil {
			return nil, err
		}
		funds[name] = compiled
	}
	out := make([]canonical.Benchmark, 0, len(funds))
	for _, name := range slices.Sorted(maps.Keys(funds)) {
		_, own := configured[name]
		out = append(out, canonical.Benchmark{Name: name, Builtin: !own, Funds: funds[name]})
	}
	return out, nil
}

// compileBenchmarkMix checks one mix and returns its funds, sorted by
// fund reference.
func compileBenchmarkMix(name string, mix map[string]float64) ([]canonical.BenchmarkFund, error) {
	if len(mix) == 0 {
		return nil, fmt.Errorf("config: benchmarks[%q]: name at least one fund, such as \"symbol:ACWI\": 100", name)
	}
	seen := map[string]string{}
	var sum float64
	var out []canonical.BenchmarkFund
	for _, key := range slices.Sorted(maps.Keys(mix)) {
		ref, err := canonical.ParseFundRef(key)
		if err != nil {
			return nil, fmt.Errorf("config: benchmarks[%q]: %w", name, err)
		}
		if prev, dup := seen[ref]; dup {
			return nil, fmt.Errorf("config: benchmarks[%q]: %q and %q name the same fund; list it once", name, prev, key)
		}
		seen[ref] = key
		w := mix[key]
		if !(w > 0) || math.IsInf(w, 0) {
			return nil, fmt.Errorf("config: benchmarks[%q][%q]: weight %v: want a percentage above 0", name, key, w)
		}
		sum += w
		out = append(out, canonical.BenchmarkFund{FundRef: ref, WeightPct: w})
	}
	if math.Abs(sum-100) > benchmarkWeightTol {
		return nil, fmt.Errorf("config: benchmarks[%q]: the weights sum to %v; make them sum to 100", name, sum)
	}
	slices.SortFunc(out, func(x, y canonical.BenchmarkFund) int { return strings.Compare(x.FundRef, y.FundRef) })
	return out, nil
}

// BenchmarkDefs is the built-in set with the config's benchmarks over
// it, sorted by name, as Validate compiled it. A Config never validated
// reads as one with no benchmarks block: the built-in set alone.
func (c *Config) BenchmarkDefs() []canonical.Benchmark {
	if c.benchmarkDefs != nil {
		return c.benchmarkDefs
	}
	defs, err := compileBenchmarks(nil)
	if err != nil {
		panic(err) // the built-in set is valid by construction
	}
	return defs
}
