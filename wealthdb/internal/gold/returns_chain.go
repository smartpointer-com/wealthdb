package gold

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// The post-exit residue rule (RETURNS-NOTES, "Post-exit residue"). An exit is
// a sub-period, or a run of them, after which the value left is below
// residueShare of the value before it, outflows having taken at least
// exitOutflowShare of that value. The residue is every later sub-period that
// opens below residueShare of it. Which sub-periods are residue is decided in
// residueCurrency alone and applied in every reporting currency: flows and
// values convert at their own days' rates, so a share near a threshold could
// fall on either side of it in another currency.
const (
	residueShare     = 0.01
	exitOutflowShare = 0.90
	residueCurrency  = "USD"
)

// openingValue is an entity's value at from as the chain and MWR read it: what
// its accounts moved before from and do not show yet is in it.
func openingValue(av func(int64) (float64, bool), from int64, rolled []returns.Flow) float64 {
	v, _ := av(from)
	return v + unshownValue(unvaluedAt([]int64{from}, rolled)[0])
}

// unshownValue is an unvaluedAt figure as money: float dust from moves that
// cancel reads as nothing, so a value with nothing unshown stays as carried.
func unshownValue(v float64) float64 {
	if math.Abs(v) <= valueTol {
		return 0
	}
	return v
}

// subPeriod is one sub-period of the chain: its edges' values with the moves
// rolled in, and its flows.
type subPeriod struct {
	from, to int64
	v0, v1   float64
	flows    []returns.Flow
	// empty: the entity holds nothing and moves nothing, so there is no return
	// to chain, and a closed account's later zero valuations leave the chain
	// where it ended.
	empty bool
}

// chainSubPeriods are the sub-periods between bounds, read from the entity's
// value av, its moves rolled and its flows.
func chainSubPeriods(bounds []int64, av func(int64) (float64, bool), rolled, flows []returns.Flow) []subPeriod {
	unvalued := unvaluedAt(bounds, rolled)
	out := make([]subPeriod, 0, len(bounds))
	for i := 0; i+1 < len(bounds); i++ {
		sp := subPeriod{from: bounds[i], to: bounds[i+1], flows: flowsIn(flows, bounds[i], bounds[i+1])}
		sp.v0, _ = av(bounds[i])
		sp.v1, _ = av(bounds[i+1])
		sp.v0, sp.v1 = sp.v0+unvalued[i], sp.v1+unvalued[i+1]
		sp.empty = math.Abs(sp.v0) <= valueTol && math.Abs(sp.v1) <= valueTol && len(sp.flows) == 0
		out = append(out, sp)
	}
	return out
}

// residueSkips is, for each sub-period, whether it sits on the residue of an
// exit, by the day the sub-period ends.
func residueSkips(sps []subPeriod) map[int64]bool {
	skips := map[int64]bool{}
	var w exitWatch
	for _, sp := range sps {
		if !sp.empty && w.residue(sp.v0, sp.v1, sp.flows) {
			skips[sp.to] = true
		}
	}
	return skips
}

// residueEnds is the entity's residue decision: the end days of the
// sub-periods that sit on the residue of an exit. It is read once, on the
// entity's since-inception chain, so a window that opens after an exit or
// inside one skips the same sub-periods: a window's first sub-period ends on
// the same day as the inception chain's. It is read in residueCurrency on the
// accounts' twins, so every reporting currency applies one decision. An
// account with no twin, where that currency's dataset could not be loaded,
// leaves the reporting currency to decide alone.
func residueEnds(assets []*accountData, p ReturnParams, toDay int64, aggregate bool) map[int64]bool {
	dec := assets
	if p.OutCcy != residueCurrency {
		dec = make([]*accountData, len(assets))
		for i, a := range assets {
			if dec[i] = a.twin; dec[i] == nil {
				dec = assets
				break
			}
		}
	}
	since := p
	since.FromEpoch = 0
	compute := func() map[int64]bool {
		from, to, _ := entityWindow(dec, since, toDay)
		if to <= from {
			return nil
		}
		flows, _, rolled := entityFlows(dec, since, from, to, aggregate)
		return residueSkips(chainSubPeriods(canonicalChainBounds(from, to, unionSnapshotDays(dec)), entityValue(dec), rolled, flows))
	}
	if dec[0].residue == nil {
		return compute()
	}
	keys := make([]string, len(dec))
	for i, a := range dec {
		keys[i] = acctKey(a.src, a.acct)
	}
	slices.Sort(keys)
	key := fmt.Sprintf("%s|%d|%t|%t|%s|%p|%s", p.Level, toDay, aggregate, p.Netting, p.Inception,
		p.InceptionOverrides, strings.Join(keys, "|"))
	return dec[0].residue.get(key, compute)
}

// exitWatch follows a chain's sub-periods in order and says which sit on the
// residue of an exit, so they add nothing to the chain. A run is consecutive
// sub-periods that each move money out, or move none; it holds the opening
// value of its first plus its inflows, and a sub-period with a net inflow ends
// it. A sale spread over several days is one exit. A sub-period is also
// measured alone, so a run that opened higher cannot hide an exit.
type exitWatch struct {
	running      bool
	held, out    float64 // the run's opening value plus its inflows, and its outflows
	residueBelow float64 // > 0 after an exit: the opening value below which a sub-period is residue
}

// residue reports whether the sub-period from v0 to v1 with flows sits on an
// exit's residue, and otherwise records it towards the next exit.
func (w *exitWatch) residue(v0, v1 float64, flows []returns.Flow) bool {
	if w.residueBelow > 0 {
		if v0 < w.residueBelow {
			return true
		}
		w.residueBelow = 0
	}
	var in, out float64
	for _, f := range flows {
		if f.Amount > 0 {
			in += f.Amount
		} else {
			out -= f.Amount
		}
	}
	if in > out {
		w.running = false
		return false
	}
	if !w.running {
		w.running, w.held, w.out = true, v0, 0
	}
	w.held += in
	w.out += out
	exit := func(held, out float64) bool {
		return held > valueTol && v1 < residueShare*held && out >= exitOutflowShare*held
	}
	switch {
	case exit(w.held, w.out):
		w.residueBelow = residueShare * w.held
	case exit(v0+in, out):
		w.residueBelow = residueShare * (v0 + in)
	default:
		return false
	}
	w.running = false
	return false
}

func unionSnapshotDays(assets []*accountData) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, a := range assets {
		for _, d := range a.snapDays {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// canonicalChainBounds returns the valuation-day boundaries for the canonical
// cumulative TWR: winFrom, then every snapshot day in (winFrom, winTo]. Each
// sub-period therefore spans one real valuation interval, so a flow is always
// chained against the snapshot-to-snapshot value change it belongs to — never
// against a carried-flat calendar bucket, which is what let a flow in a sparse
// snapshot gap poison the chain with a sub-(-100%) sub-period. A carried tail
// past the last snapshot (winTo with no fresh valuation) is not a sub-period and
// is excluded; a window with no interior valuation falls back to one
// [winFrom, winTo] bucket. snaps must be ascending (unionSnapshotDays).
func canonicalChainBounds(winFrom, winTo int64, snaps []int64) []int64 {
	bounds := []int64{winFrom}
	for _, d := range snaps {
		if d > winFrom && d <= winTo {
			bounds = append(bounds, d)
		}
	}
	if len(bounds) == 1 {
		bounds = append(bounds, winTo)
	}
	return bounds
}

// unvaluedAt is, for each of bounds, what moves has rolled forward there: what the
// entity's accounts moved since each last valued, which their carried values do
// not show yet. A coarse grain's valuation days are the union of its accounts', so
// a boundary can fall between a flow into a sparsely valued account and the
// valuation that shows it. Without this, one sub-period holds the flow against a
// flat value and a later one the value change against no flow, and the chain keeps
// the difference, about the square of the flow's share of the entity. On an
// account's own valuation days this is nothing. bounds must be ascending.
func unvaluedAt(bounds []int64, moves []returns.Flow) []float64 {
	type step struct {
		day    int64
		amount float64
	}
	steps := make([]step, 0, 2*len(moves))
	for _, m := range moves {
		steps = append(steps, step{m.Day, m.Amount}, step{m.ValuedOn, -m.Amount})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].day < steps[j].day })
	out := make([]float64, len(bounds))
	sum, next := 0.0, 0
	for i, b := range bounds {
		for ; next < len(steps) && steps[next].day <= b; next++ {
			sum += steps[next].amount
		}
		out[i] = sum
	}
	return out
}

// valueBefore is a's value at the valuation before day, nothing before its first.
func (a *accountData) valueBefore(day int64) float64 {
	i := sort.Search(len(a.snapDays), func(k int) bool { return a.snapDays[k] >= day })
	if i == 0 {
		return 0
	}
	v, _ := a.valueAt(a.snapDays[i-1])
	return v
}

// confirms reports whether a's valuation on `on` shows moves, the flows and legs
// of the gap that ends there. It does if the value change matches what moved, to
// within half of that, and the moves never take the account below zero. Money can
// also leave an account by a path no flow records, such as a purchase, and a flow
// can be dated after the valuation that shows it. The value then says the account
// never held what the flows imply. A gap that nets to nothing says nothing either
// way. Flows past winTo are in the value change, so they count towards what moved.
func (a *accountData) confirms(on, winTo int64, moves []returns.Flow) bool {
	net := sumFlows(moves)
	if on > winTo {
		net += a.netIn(winTo, on)
	}
	v, _ := a.valueAt(on)
	before := a.valueBefore(on)
	if math.Abs(net) <= valueTol || math.Abs(v-before-net) >= math.Abs(net)/2 {
		return false
	}
	byDay := slices.Clone(moves)
	sort.SliceStable(byDay, func(i, j int) bool { return byDay[i].Day < byDay[j].Day })
	held := before
	for i, f := range byDay {
		held += f.Amount
		if held < -valueTol && (i+1 == len(byDay) || byDay[i+1].Day != f.Day) {
			return false
		}
	}
	return true
}

// unshownAt is a's flows on or before day that its value does not show yet, those
// after its last valuation, each with the valuation that will show it. Nil where
// no valuation comes before day or after it.
func (a *accountData) unshownAt(day int64) []returns.Flow {
	i := sort.Search(len(a.snapDays), func(k int) bool { return a.snapDays[k] > day })
	if i == 0 || i == len(a.snapDays) {
		return nil
	}
	var out []returns.Flow
	for f := range a.legs(a.snapDays[i-1], day) {
		f.ValuedOn = a.snapDays[i]
		out = append(out, f)
	}
	return out
}
