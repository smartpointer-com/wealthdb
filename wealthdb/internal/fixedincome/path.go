package fixedincome

// A rate path is the rate in percent a holding earns in each month from
// today: one rate a month, month 0 first.

// ConstantPath is the path that holds today's rate for months months.
func ConstantPath(rate float64, months int) []float64 {
	out := make([]float64, max(0, months))
	for m := range out {
		out[m] = rate
	}
	return out
}

// ForwardPath is the path a holding that rolls into termMonths loans
// earns under the curve's forward rates, c.Path, plus a spread in
// percentage points. c must not be nil: a currency with no curve has
// ConstantPath instead.
func ForwardPath(c *Curve, months, termMonths int, spread float64) []float64 {
	out := c.Path(months, termMonths)
	for m := range out {
		out[m] += spread
	}
	return out
}

// Shift is a copy of path moved in parallel by basisPoints basis points.
func Shift(path []float64, basisPoints float64) []float64 {
	out := make([]float64, len(path))
	for m, r := range path {
		out[m] = r + basisPoints/100
	}
	return out
}
