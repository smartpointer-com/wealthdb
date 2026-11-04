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

// validateBenchmarks checks the benchmarks block: every name is usable
// and differs from the others by more than its case, every fund
// reference parses and names a fund once, every weight is positive, and
// each mix sums to 100.
func (c *Config) validateBenchmarks(map[string]bool) error {
	folded := map[string]string{}
	for name := range builtinBenchmarks {
		folded[strings.ToLower(name)] = name
	}
	for _, name := range slices.Sorted(maps.Keys(c.Benchmarks)) {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("config: benchmarks[%q]: give the benchmark a name with no spaces around it", name)
		}
		if strings.EqualFold(name, canonical.NoBenchmark) {
			return fmt.Errorf("config: benchmarks[%q]: %q is the picker's value for no benchmark; choose another name",
				name, canonical.NoBenchmark)
		}
		if prev, dup := folded[strings.ToLower(name)]; dup && prev != name {
			return fmt.Errorf("config: benchmarks[%q]: it differs from %q by case alone; use that exact name to redefine it, or another name",
				name, prev)
		}
		folded[strings.ToLower(name)] = name
		if err := validateBenchmarkMix(name, c.Benchmarks[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateBenchmarkMix(name string, mix map[string]float64) error {
	if len(mix) == 0 {
		return fmt.Errorf("config: benchmarks[%q]: name at least one fund, such as \"symbol:ACWI\": 100", name)
	}
	seen := map[string]string{}
	var sum float64
	for _, key := range slices.Sorted(maps.Keys(mix)) {
		ref, err := canonical.ParseFundRef(key)
		if err != nil {
			return fmt.Errorf("config: benchmarks[%q]: %w", name, err)
		}
		if prev, dup := seen[ref]; dup {
			return fmt.Errorf("config: benchmarks[%q]: %q and %q name the same fund; list it once", name, prev, key)
		}
		seen[ref] = key
		w := mix[key]
		if !(w > 0) || math.IsInf(w, 0) {
			return fmt.Errorf("config: benchmarks[%q][%q]: weight %v: want a percentage above 0", name, key, w)
		}
		sum += w
	}
	if math.Abs(sum-100) > benchmarkWeightTol {
		return fmt.Errorf("config: benchmarks[%q]: the weights sum to %v; make them sum to 100", name, sum)
	}
	return nil
}

// BenchmarkDefs is the built-in set with the config's benchmarks over
// it, sorted by name. A configured name replaces the built-in one whole.
// Only call after Validate.
func (c *Config) BenchmarkDefs() []canonical.Benchmark {
	mixes := maps.Clone(builtinBenchmarks)
	maps.Copy(mixes, c.Benchmarks)
	out := make([]canonical.Benchmark, 0, len(mixes))
	for _, name := range slices.Sorted(maps.Keys(mixes)) {
		_, configured := c.Benchmarks[name]
		b := canonical.Benchmark{Name: name, Builtin: !configured}
		for _, key := range slices.Sorted(maps.Keys(mixes[name])) {
			ref, _ := canonical.ParseFundRef(key)
			b.Funds = append(b.Funds, canonical.BenchmarkFund{FundRef: ref, WeightPct: mixes[name][key]})
		}
		slices.SortFunc(b.Funds, func(x, y canonical.BenchmarkFund) int { return strings.Compare(x.FundRef, y.FundRef) })
		out = append(out, b)
	}
	return out
}
