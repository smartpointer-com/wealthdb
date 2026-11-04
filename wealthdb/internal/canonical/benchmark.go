package canonical

// Benchmark is a named mix of funds with target weights, rebalanced
// monthly (docs/RETURNS-NOTES.md, "Benchmarks"). The weights sum to 100.
// Builtin says the engine ships it and no config block redefines it.
type Benchmark struct {
	Name    string
	Builtin bool
	Funds   []BenchmarkFund
}

// BenchmarkFund is one fund of a benchmark's mix.
type BenchmarkFund struct {
	FundRef   string
	WeightPct float64
}

// NoBenchmark is the Benchmark picker's value that picks none.
const NoBenchmark = "None"
