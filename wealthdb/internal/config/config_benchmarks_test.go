package config

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestBenchmarkDefsShipTheBuiltInSet(t *testing.T) {
	c := &Config{}
	defs := c.BenchmarkDefs()
	if len(defs) != 5 {
		t.Fatalf("defs = %+v", defs)
	}
	byName := map[string]canonical.Benchmark{}
	for _, d := range defs {
		byName[d.Name] = d
		if !d.Builtin {
			t.Errorf("%s is not built in", d.Name)
		}
		var sum float64
		for _, f := range d.Funds {
			sum += f.WeightPct
		}
		if sum != 100 {
			t.Errorf("%s sums to %v", d.Name, sum)
		}
	}
	mix := byName["Global 60/40"].Funds
	if len(mix) != 2 || mix[0] != (canonical.BenchmarkFund{FundRef: "symbol:ACWI", WeightPct: 60}) ||
		mix[1] != (canonical.BenchmarkFund{FundRef: "symbol:BND", WeightPct: 40}) {
		t.Errorf("60/40 = %+v", mix)
	}
}

func TestBenchmarksAddAndRedefineByName(t *testing.T) {
	c := &Config{Benchmarks: map[string]map[string]float64{
		"World 70/30": {"symbol:acwi": 70, "symbol:BND": 30},
		"S&P 500":     {"symbol:QQQ": 100},
	}}
	if err := c.validateBenchmarks(nil); err != nil {
		t.Fatal(err)
	}
	defs := c.BenchmarkDefs()
	if len(defs) != 6 {
		t.Fatalf("defs = %+v", defs)
	}
	for _, d := range defs {
		switch d.Name {
		case "S&P 500":
			if d.Builtin || len(d.Funds) != 1 || d.Funds[0].FundRef != "symbol:QQQ" {
				t.Errorf("redefined = %+v", d)
			}
		case "World 70/30":
			if d.Builtin || d.Funds[0].FundRef != "symbol:ACWI" {
				t.Errorf("added = %+v", d)
			}
		case "Global 60/40":
			if !d.Builtin {
				t.Errorf("untouched = %+v", d)
			}
		}
	}
}

func TestBenchmarksRefuseABadBlockAndNameTheFix(t *testing.T) {
	for name, c := range map[string]struct {
		block map[string]map[string]float64
		want  string
	}{
		"sum":    {map[string]map[string]float64{"A": {"symbol:X": 60, "symbol:Y": 30}}, "make them sum to 100"},
		"empty":  {map[string]map[string]float64{"A": {}}, "name at least one fund"},
		"ref":    {map[string]map[string]float64{"A": {"X": 100}}, "want isin:"},
		"weight": {map[string]map[string]float64{"A": {"symbol:X": 110, "symbol:Y": -10}}, "want a percentage above 0"},
		"twice":  {map[string]map[string]float64{"A": {"symbol:x": 50, "symbol:X": 50}}, "list it once"},
		"none":   {map[string]map[string]float64{"none": {"symbol:X": 100}}, "choose another name"},
		"blank":  {map[string]map[string]float64{" ": {"symbol:X": 100}}, "give the benchmark a name"},
		"spaced": {map[string]map[string]float64{" A": {"symbol:X": 100}}, "give the benchmark a name"},
		"case":   {map[string]map[string]float64{"s&p 500": {"symbol:X": 100}}, "by case alone"},
		"each":   {map[string]map[string]float64{"Abc": {"symbol:X": 100}, "ABC": {"symbol:X": 100}}, "by case alone"},
	} {
		cfg := &Config{Benchmarks: c.block}
		err := cfg.validateBenchmarks(nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}
