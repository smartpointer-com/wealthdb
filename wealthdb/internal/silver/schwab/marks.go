package schwab

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// markGuard finds the bad quotes in the api feed's overnight marks
// (docs/adapters/schwab.md §4.1). A thinly traded security's mark can be
// far off one night and right again a night or two later, and the spike
// moves the account's value while it lasts.
//
// A position's mark is its value per unit. A mark that moves more than
// SpikePct from the previous mark, and comes back within RevertPct of that
// previous mark at one of the next two observations, is a spike: the
// observations in between take the previous mark, quantity times mark. A
// move that does not come back is real and stands, whatever its size and
// the vehicle: a real move is never held. The rules at the edges:
//
//   - A position's run of observations is the snapshots in a row that
//     state it. A snapshot that leaves it out ends the run, and its next
//     observation starts a new one. A spike is read inside a run only.
//   - The newest observations, with no observation after them yet, stand
//     as stated. The next load reads them again with what came after
//     (apiReader.ChangeWindow reaches back two snapshots for it).
//   - A split moves quantity and mark together and never comes back, so
//     it stands like any real move.
//   - An observation without a value or a quantity states no mark. It
//     stands as it is and is not counted among the next two.
//
// The guard reads the api positions only, and not the options and futures
// among them, whose marks move that far as a matter of course. Statement
// marks (the web subsource) are period-end closes a month apart and stand
// as printed.
type markGuard struct {
	// spike and revert are fractions of a mark.
	spike, revert canonical.Decimal
	// index numbers the source's snapshots in order, so a run can tell a
	// snapshot that left a position out.
	index  map[int64]int
	series map[markSeries][]markObs
}

// markSeries is one position of one account.
type markSeries struct{ account, position string }

// markObs is one observation of a position. One without a value or a
// quantity has no mark.
type markObs struct {
	snap     int64
	at       int // the snapshot's index
	quantity canonical.Decimal
	mark     *canonical.Decimal
}

// markFix is what a spiked observation reads: its value at the previous
// mark, and the snapshot that mark is from.
type markFix struct {
	value canonical.Decimal
	from  int64
}

func newMarkGuard(g silver.MarkGuard) *markGuard {
	g = g.WithDefaults()
	hundred := canonical.NewDecimalFromInt(100)
	return &markGuard{
		spike:  canonical.NewDecimalFromFloat(g.SpikePct).Div(hundred),
		revert: canonical.NewDecimalFromFloat(g.RevertPct).Div(hundred),
		index:  map[int64]int{},
		series: map[markSeries][]markObs{},
	}
}

// guardsMarks reports whether the guard reads a position of this vehicle.
func guardsMarks(v canonical.Vehicle) bool {
	return v != canonical.VehicleOption && v != canonical.VehicleFuture
}

// snapshot registers one of the source's snapshots. The caller registers
// every snapshot it reads, in order, cash-only ones included.
func (g *markGuard) snapshot(snap int64) {
	if _, ok := g.index[snap]; !ok {
		g.index[snap] = len(g.index)
	}
}

// observe records a position's quantity and value at snapshot snap, which
// the caller has registered.
func (g *markGuard) observe(k markSeries, snap int64, quantity canonical.Decimal, value *canonical.Decimal) {
	o := markObs{snap: snap, at: g.index[snap], quantity: quantity}
	if value != nil && !quantity.IsZero() {
		mark := value.Div(quantity)
		o.mark = &mark
	}
	g.series[k] = append(g.series[k], o)
}

// fixes returns the spiked observations, by position and snapshot, with
// what each reads instead.
func (g *markGuard) fixes() map[markSeries]map[int64]markFix {
	out := map[markSeries]map[int64]markFix{}
	for k, obs := range g.series {
		for start := 0; start < len(obs); {
			end := start + 1
			for end < len(obs) && obs[end].at == obs[end-1].at+1 {
				end++
			}
			var marked []markObs
			for _, o := range obs[start:end] {
				if o.mark != nil {
					marked = append(marked, o)
				}
			}
			g.fixRun(k, marked, out)
			start = end
		}
	}
	return out
}

// fixRun reads the observations of one run that state a mark. ref is the
// observation whose mark the next is measured against.
func (g *markGuard) fixRun(k markSeries, run []markObs, out map[markSeries]map[int64]markFix) {
	ref := 0
	for i := 1; i < len(run); i++ {
		prev := *run[ref].mark
		if prev.IsZero() || within(*run[i].mark, prev, g.spike) {
			ref = i
			continue
		}
		back := 0
		for j := i + 1; j < len(run) && j <= i+2; j++ {
			if within(*run[j].mark, prev, g.revert) {
				back = j
				break
			}
		}
		if back == 0 {
			ref = i
			continue
		}
		if out[k] == nil {
			out[k] = map[int64]markFix{}
		}
		for s := i; s < back; s++ {
			out[k][run[s].snap] = markFix{value: run[s].quantity.Mul(prev).Round(4), from: run[ref].snap}
		}
		ref, i = back, back
	}
}

// within reports whether mark lies within frac of level.
func within(mark, level, frac canonical.Decimal) bool {
	return mark.Sub(level).Abs().LessThanOrEqual(level.Abs().Mul(frac))
}
