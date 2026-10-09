package silver

import (
	"log"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// InKind is the basis private-market positions distributed in kind:
// Schedule K-1 Line 19(c), property distributions such as the shares a
// vehicle hands out at an exit. Such a distribution moves basis out of
// the vehicle with the asset, so it reduces the book value from the
// K-1's period end on. Cash paid back (Line 19(a)) does not: the book
// value stays the capital paid in, gross (docs/DESIGN.md §7.4). The
// zero value holds no distributions.
type InKind struct {
	byKey   map[string][]inKindOut
	clamped map[string]bool
}

// inKindOut is one K-1's Line 19(c) figure and the end of the period
// the K-1 covers.
type inKindOut struct {
	periodEnd int64
	amount    canonical.Decimal
}

// Add records a K-1's property distribution on the position key, from
// periodEnd (unix seconds) on. A figure that is not positive moves no
// basis.
func (k *InKind) Add(key string, periodEnd int64, amount canonical.Decimal) {
	if !amount.IsPositive() {
		return
	}
	if k.byKey == nil {
		k.byKey = map[string][]inKindOut{}
	}
	k.byKey[key] = append(k.byKey[key], inKindOut{periodEnd: periodEnd, amount: amount})
}

// PeriodEnds are the distinct period ends of the recorded
// distributions, oldest first: the days a book value steps down, so a
// snapshot lands on each.
func (k *InKind) PeriodEnds() []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, outs := range k.byKey {
		for _, d := range outs {
			if !seen[d.periodEnd] {
				seen[d.periodEnd] = true
				out = append(out, d.periodEnd)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// BookValue is the book value at t of the position key whose capital
// paid in is paidIn, a figure stamped stamp: paidIn less every
// distribution whose period ended on or before t, never below zero. A
// reduced value is arithmetic over two stated figures, so its stamp is
// derived, with the paid-in figure's method and fees, and extra holds
// both figures for the payload. A nil paidIn has no book value.
func (k *InKind) BookValue(key string, t int64, paidIn *canonical.Decimal, stamp canonical.Basis) (*canonical.Decimal, canonical.Basis, map[string]any) {
	if paidIn == nil {
		return nil, canonical.Basis{}, nil
	}
	var out canonical.Decimal
	for _, d := range k.byKey[key] {
		if d.periodEnd <= t {
			out = out.Add(d.amount)
		}
	}
	if out.IsZero() {
		return paidIn, stamp, nil
	}
	left := paidIn.Sub(out)
	if left.IsNegative() {
		left = canonical.Decimal{}
		if k.clamped == nil {
			k.clamped = map[string]bool{}
		}
		k.clamped[key] = true
	}
	stamp.Origin = canonical.BasisDerived
	return &left, stamp, map[string]any{
		"paid_in":              paidIn.String(),
		"property_distributed": out.String(),
	}
}

// LogClamped reports the positions whose K-1s state more basis
// distributed in kind than the capital paid in. Their book value is
// held at zero.
func (k *InKind) LogClamped(source string) {
	if n := len(k.clamped); n > 0 {
		log.Printf("%s adapter: %d position(s) distributed more basis in kind (K-1 Line 19(c)) "+
			"than the capital paid in; book value held at 0", source, n)
	}
}
