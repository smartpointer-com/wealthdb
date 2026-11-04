package gold

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// benchmarkLinesPct is the share of a fund its lines must cover for what
// they imply to stand for it, as for a bond fund's implied yield
// (docs/YIELD.md §4).
const benchmarkLinesPct = 50

// benchmarkNoDebtPct is the share of debt under which a fund's duration
// reads zero where nothing states one.
const benchmarkNoDebtPct = 1

// benchmarkFundFigures are a fund's yield and duration on day, as a
// benchmark reads them. The yield is the one its page or filing states:
// distribution, SEC, trailing, to worst, to maturity. The duration is the
// one its page states, else what its filing's DV01 makes of its net
// assets. Where nothing states one, its lines of that day or before imply
// it, over the whole fund, where they cover enough of it: a line's stated
// maturity overstates a mortgage pool's, which is why the filing comes
// first. A fund with next to no debt has a duration of zero. Either is
// nil where the fund gives none.
func benchmarkFundFigures(f *yield.Fund, day time.Time) (yieldPct, duration *float64) {
	for _, d := range []*yield.Dated{f.DistributionYield, f.SECYield, f.TrailingYield, f.YieldToWorst, f.YieldToMaturity} {
		if d != nil {
			yieldPct = f64(d.Value)
			break
		}
	}
	if f.Duration != nil {
		duration = f64(f.Duration.Value)
	} else if f.NetAssets != nil && *f.NetAssets > 0 && len(f.Risk) > 0 {
		var dv01 float64
		for _, tenors := range f.Risk {
			for _, v := range tenors {
				dv01 += v
			}
		}
		duration = f64(dv01 * 10_000 / *f.NetAssets)
	}
	l := f.Lines
	if l == nil {
		return yieldPct, duration
	}
	current := !l.AsOf.After(day)
	if yieldPct == nil && current && l.Yield != nil && l.CoveredPct >= benchmarkLinesPct {
		yieldPct = l.Yield
	}
	switch {
	case duration != nil:
	case l.DebtPct < benchmarkNoDebtPct:
		duration = f64(0)
	case current && l.Duration != nil && l.DurationPct >= benchmarkLinesPct:
		duration = f64(*l.Duration * l.DebtPct / 100)
	}
	return yieldPct, duration
}

// weighted is Σ wᵢ xᵢ over a mix, nil where a fund has no figure.
func weighted(mix []benchmarkFundRow, figure func(key string) *float64) *float64 {
	var sum float64
	for _, f := range mix {
		x := figure(f.key)
		if x == nil {
			return nil
		}
		sum += f.weight / 100 * *x
	}
	return &sum
}

// benchmarks writes every benchmark's yield and duration at the
// yieldInstants of toEpoch. in is today's inputs. A fund's lines are read
// today alone: an unheld fund has one composition, and a
// bond fund's thousands of lines cost more than the rest of the fund. An
// earlier instant takes today's lines, which say nothing of it but its
// share of debt.
func (m *yieldMaterializer) benchmarks(ctx context.Context, mix []benchmarkFundRow, in *yield.Inputs, toEpoch int64) error {
	if len(mix) == 0 {
		return nil
	}
	seen := map[string]bool{}
	byName := map[string][]benchmarkFundRow{}
	for _, f := range mix {
		seen[f.key] = true
		byName[f.benchmark] = append(byName[f.benchmark], f)
	}
	keys := slices.Sorted(maps.Keys(seen))
	today, err := readYieldFunds(ctx, m.tx, in, keys)
	if err != nil {
		return fmt.Errorf("MaterializeYield benchmarks: %w", err)
	}
	names := slices.Sorted(maps.Keys(byName))
	for _, at := range yieldInstants(toEpoch) {
		read, day := today, in.Day
		if at != toEpoch {
			t := time.Unix(at, 0).UTC()
			day = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			if read, err = readFundFigures(ctx, m.tx, &yield.Inputs{AsOf: at, Day: day}, keys); err != nil {
				return fmt.Errorf("MaterializeYield benchmarks: %w", err)
			}
			for k, r := range read {
				r.fund.Lines = today[k].fund.Lines
			}
		}
		type figures struct{ yieldPct, duration *float64 }
		funds := make(map[string]figures, len(read))
		for k, r := range read {
			y, d := benchmarkFundFigures(r.fund, day)
			funds[k] = figures{y, d}
		}
		if err := m.insert("benchmark_yield", `computed_at, benchmark, as_of, yield_pct, duration`, len(names),
			func(i int, args []any) []any {
				f := byName[names[i]]
				return append(args, m.computedAt, names[i], at,
					weighted(f, func(k string) *float64 { return funds[k].yieldPct }),
					weighted(f, func(k string) *float64 { return funds[k].duration }))
			}); err != nil {
			return err
		}
	}
	return nil
}
