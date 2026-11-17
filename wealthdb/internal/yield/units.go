package yield

import (
	"math"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// UnitTrade is a transaction that moves a key's quantity: a trade, or
// with Corp a corporate action's leg, which delivers or takes back units
// as a change of units does.
type UnitTrade struct {
	At   int64
	Qty  float64
	Corp bool
}

// UnitStep is a key's quantity and value at two consecutive snapshots of
// its source, and the transactions between them, oldest first.
type UnitStep struct {
	Before, After           float64
	ValueBefore, ValueAfter *float64
	Trades                  []UnitTrade
}

// unitChangeMin is the least change of units, either way, that a price
// or a corporate action tells from a trade: a three-for-two split. A
// smaller corporate delivery, such as a scrip dividend, is a trade.
const unitChangeMin = 1.5

// Change is the change of units a step shows: each old unit became k new
// ones, 1 where its trades explain its quantities. A change they do not
// explain is one of units where a corporate action's leg names it, or
// where the price per unit moved by its inverse (priced); else it is a
// trade no transaction records, and k is 1. Its factor is the quantities'
// (wholeFactor), with the step's trades before the change or after it.
// The legs count once each, as the lot engine dedupes them (LOTS.md
// §3.5), and must state the change: their sum is the units delivered, or
// a leg states the new total. ok is false where no whole factor fits, or
// the legs state another change.
func (s UnitStep) Change() (k float64, ok bool) {
	if !(s.Before > lots.Tolerance(s.Before)) || !(s.After > lots.Tolerance(s.After)) {
		// A position opened or closed tells nothing of its units.
		return 1, true
	}
	traded := 0.0
	var legs []float64
	for _, t := range s.Trades {
		switch {
		case !t.Corp:
			traded += t.Qty
		case !slices.Contains(legs, t.Qty):
			legs = append(legs, t.Qty)
		}
	}
	if math.Abs(s.After-s.Before-traded) <= lots.Tolerance(s.After) {
		return 1, true
	}
	small := false
	for _, q := range [][2]float64{{s.Before + traded, s.After}, {s.Before, s.After - traded}} {
		if q[0] <= 0 || q[1] <= 0 {
			continue
		}
		r := q[1] / q[0]
		small = small || (r < unitChangeMin && r > 1/unitChangeMin)
		k, whole := wholeFactor(q[0], r)
		switch {
		case !whole:
		case len(legs) > 0 && stated(legs, q[0], q[1]):
			return k, true
		case len(legs) == 0 && s.priced(k):
			return k, true
		}
	}
	switch {
	case small:
		return 1, true
	case len(legs) > 0:
		return 1, false
	}
	if k := s.After / (s.Before + traded); k >= unitChangeMin || k <= 1/unitChangeMin {
		return 1, !s.priced(k)
	}
	return 1, true
}

// stated says corporate action legs state the change of a quantity from
// before to after: they sum to the units delivered, or one states the new
// total, short at most of a fraction paid in cash.
func stated(legs []float64, before, after float64) bool {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1+lots.Tolerance(b) }
	sum := 0.0
	for _, l := range legs {
		sum += l
	}
	return near(sum, after-before) || slices.ContainsFunc(legs, func(l float64) bool { return l > 0 && near(l, after) })
}

// priced says the price per unit moved by the inverse of k between the
// step's snapshots: in ratio, nearer to k than to staying put. false
// where a value is unknown.
func (s UnitStep) priced(k float64) bool {
	if s.ValueBefore == nil || s.ValueAfter == nil || !(*s.ValueBefore > 0) || !(*s.ValueAfter > 0) {
		return false
	}
	moved := (*s.ValueBefore / s.Before) / (*s.ValueAfter / s.After)
	return math.Abs(math.Log(moved/k)) < math.Abs(math.Log(moved))
}

// wholeFactorMost bounds the factors wholeFactor tries: past it, so many
// fit that none tells.
const wholeFactorMost = 1000

// wholeFactor reads ratio, what each of before units became, as a whole
// number n of at least two new units for each old one, or as one new
// unit for n old ones: the new quantity is n·before, or before/n, exactly
// or short of the fraction of a unit paid in cash. false where neither
// fits, or where more than one does, as on a small position whose new
// quantity rounds down to the same whole units under several.
func wholeFactor(before, ratio float64) (float64, bool) {
	if !(before > 0) || !(ratio > 0) {
		return 0, false
	}
	after := before * ratio
	fits := func(target float64) bool {
		tol := lots.Tolerance(target)
		return math.Abs(after-target) <= tol || math.Abs(after-math.Floor(target)) <= tol
	}
	var found []float64
	try := func(lo, hi float64, factor, target func(n float64) float64) bool {
		if hi-lo > wholeFactorMost {
			return false
		}
		for n := max(lo, 2); n <= hi; n++ {
			if fits(target(n)) {
				found = append(found, factor(n))
			}
		}
		return true
	}
	if !try(math.Floor(after/before), math.Ceil((after+1)/before),
		func(n float64) float64 { return n }, func(n float64) float64 { return n * before }) ||
		!try(math.Floor(before/(after+1)), math.Ceil(before/after),
			func(n float64) float64 { return 1 / n }, func(n float64) float64 { return before / n }) ||
		len(found) != 1 {
		return 0, false
	}
	return found[0], true
}
