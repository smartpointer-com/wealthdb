package yield

import (
	"math"
	"testing"
)

// A whole factor is n new units for each old one, or one for n old ones,
// exactly or short of a fraction paid in cash; anything else is none, and
// so is a quantity several factors round to.
func TestAWholeFactor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		before, after, want float64
	}{
		{33, 660, 20},
		{100, 1000, 10},
		{10.5, 31, 3},
		{105, 10, 0.1},
		{1000, 50, 0.05},
		{100, 150, 0},
		{10, 19, 0},
		{0.05, 0.08, 0},
		{100, 100, 0},
		{7, 1, 0},
		{0.5, 1, 0},
	} {
		k, ok := wholeFactor(c.before, c.after/c.before)
		if ok != (c.want != 0) || (ok && math.Abs(k-c.want) > 1e-12) {
			t.Errorf("%v to %v: %v %v, want %v", c.before, c.after, k, ok, c.want)
		}
	}
}

// A step's trades explain its quantities, or its change is one of units:
// a corporate action's leg names it, or the price per unit moved by its
// inverse. A change neither names is a trade no transaction records, and
// so is a small delivery. The quantities give the factor; the legs count
// once each and must state it. A change of units no whole factor
// explains, or one the legs state otherwise, is not ok.
func TestAChangeOfUnits(t *testing.T) {
	t.Parallel()
	v := func(x float64) *float64 { return &x }
	for _, c := range []struct {
		what  string
		s     UnitStep
		k     float64
		units bool
	}{
		{"a buy", UnitStep{Before: 100, After: 120, Trades: []UnitTrade{{At: 1, Qty: 20}}}, 1, false},
		{"a 20-for-1 change with its leg", UnitStep{Before: 100, After: 2000,
			Trades: []UnitTrade{{At: 1, Qty: 1900, Corp: true}}}, 20, false},
		{"a leg that states the new total", UnitStep{Before: 100, After: 2000,
			Trades: []UnitTrade{{At: 1, Qty: 2000, Corp: true}}}, 20, false},
		{"a 2-for-1 leg stated twice", UnitStep{Before: 100, After: 200,
			Trades: []UnitTrade{{At: 1, Qty: 100, Corp: true}, {At: 1, Qty: 100, Corp: true}}}, 2, false},
		{"a buy at the leg's second, sorted first", UnitStep{Before: 100, After: 250,
			Trades: []UnitTrade{{At: 1, Qty: 50}, {At: 1, Qty: 100, Corp: true}}}, 2, false},
		{"a 2-for-1 leg, then a buy", UnitStep{Before: 100, After: 250,
			Trades: []UnitTrade{{At: 1, Qty: 100, Corp: true}, {At: 2, Qty: 50}}}, 2, false},
		{"a 1-for-10 change as one net leg", UnitStep{Before: 100, After: 10,
			Trades: []UnitTrade{{At: 1, Qty: -90, Corp: true}}}, 0.1, false},
		{"a leg that states another change", UnitStep{Before: 100, After: 200,
			Trades: []UnitTrade{{At: 1, Qty: 300, Corp: true}}}, 1, true},
		{"a scrip dividend", UnitStep{Before: 100, After: 102, Trades: []UnitTrade{{At: 1, Qty: 2, Corp: true}}}, 1,
			false},
		{"a reverse split of a small position", UnitStep{Before: 7, After: 1,
			Trades: []UnitTrade{{At: 1, Qty: -7, Corp: true}, {At: 1, Qty: 1, Corp: true}}}, 1, true},
		{"a reverse split as two legs", UnitStep{Before: 105, After: 10,
			Trades: []UnitTrade{{At: 1, Qty: -105, Corp: true}, {At: 1, Qty: 10, Corp: true}}}, 0.1, false},
		{"a buy before a 2-for-1 leg", UnitStep{Before: 100, After: 240,
			Trades: []UnitTrade{{At: 1, Qty: 20}, {At: 2, Qty: 120, Corp: true}}}, 2, false},
		{"a 3-for-2 split with its leg", UnitStep{Before: 100, After: 150,
			Trades: []UnitTrade{{At: 1, Qty: 50, Corp: true}}}, 1, true},
		{"a 10-for-1 change the price shows", UnitStep{Before: 200, After: 2000, ValueBefore: v(10000),
			ValueAfter: v(10460)}, 10, false},
		{"a buy, then a 10-for-1 change", UnitStep{Before: 200, After: 2200, ValueBefore: v(10000),
			ValueAfter: v(10460), Trades: []UnitTrade{{At: 1, Qty: 20}}}, 10, false},
		{"a 10-for-1 change, then a buy", UnitStep{Before: 200, After: 2020, ValueBefore: v(10000),
			ValueAfter: v(10460), Trades: []UnitTrade{{At: 1, Qty: 20}}}, 10, false},
		{"a 1-for-10 change the price shows", UnitStep{Before: 2000, After: 200, ValueBefore: v(10000),
			ValueAfter: v(9800)}, 0.1, false},
		{"a buy no transaction records", UnitStep{Before: 100, After: 200, ValueBefore: v(5000),
			ValueAfter: v(10100)}, 1, false},
		{"a doubling with no price", UnitStep{Before: 100, After: 200}, 1, false},
		{"a 3-for-2 change the price shows", UnitStep{Before: 100, After: 150, ValueBefore: v(3000),
			ValueAfter: v(3100)}, 1, true},
		{"a sale no transaction records", UnitStep{Before: 100, After: 90, ValueBefore: v(3000),
			ValueAfter: v(2600)}, 1, false},
		{"a position opened", UnitStep{Before: 0, After: 100}, 1, false},
	} {
		k, ok := c.s.Change()
		if math.Abs(k-c.k) > 1e-12 || ok == c.units {
			t.Errorf("%s: %v %v, want %v units %v", c.what, k, ok, c.k, c.units)
		}
	}
}
