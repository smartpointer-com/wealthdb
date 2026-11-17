package returns

import "math"

// Sum is an exact float sum, rounded once at the end (Shewchuk's
// algorithm, as Python's math.fsum computes it). A plain float sum rounds
// at every term, so its last digits follow the order of its terms; Sum
// keeps the exact total as non-overlapping partials, so its result is the
// same in any order. The zero value is an empty sum.
type Sum struct {
	partials []float64 // ascending magnitude, non-overlapping
}

// Add adds x to the sum.
func (k *Sum) Add(x float64) {
	i := 0
	for _, y := range k.partials {
		if math.Abs(x) < math.Abs(y) {
			x, y = y, x
		}
		hi := x + y
		lo := y - (hi - x)
		if lo != 0 {
			k.partials[i] = lo
			i++
		}
		x = hi
	}
	k.partials = append(k.partials[:i], x)
}

// Total is the sum of every term added, correctly rounded.
func (k *Sum) Total() float64 {
	p := k.partials
	n := len(p)
	if n == 0 {
		return 0
	}
	n--
	hi, lo := p[n], 0.0
	for n > 0 {
		x, y := hi, p[n-1]
		n--
		hi = x + y
		lo = y - (hi - x)
		if lo != 0 {
			break
		}
	}
	// Round half to even across the partials left: where the next one
	// below has lo's sign, the exact total lies past the halfway point.
	if n > 0 && (lo < 0 && p[n-1] < 0 || lo > 0 && p[n-1] > 0) {
		y := lo * 2
		x := hi + y
		if x-hi == y {
			hi = x
		}
	}
	return hi
}
