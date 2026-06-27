package returns

import "math"

// dietzDegenTol is the smallest average-capital denominator we treat as
// meaningful, in output-currency units. A denominator at or below this (which
// includes the zero and negative cases) makes the Modified-Dietz return
// undefined or sign-flipped, so the bucket is reported degenerate.
const dietzDegenTol = 1e-6

// ModifiedDietz returns the time-weighted approximation of a single sub-period's
// return, given the boundary values v0 (period start) and v1 (period end) and
// the external flows that occurred within (startDay, endDay].
//
//	R = (v1 - v0 - ΣF_i) / (v0 + Σ w_i·F_i),   w_i = (D - d_i)/D
//
// with D = endDay-startDay (days), d_i = flowDay-startDay, and F_i = Flow.Amount
// (capital-in positive). ok is false when the average-capital denominator is not
// meaningfully positive (|den| ≤ tol, or den < 0 from a net-negative / liability
// base) — the caller then reports n/a + dietz_degenerate and breaks the chain
// rather than multiplying a ±∞ or sign-flipped factor into the cumulative TWR.
func ModifiedDietz(v0, v1 float64, startDay, endDay int64, flows []Flow) (r float64, ok bool) {
	d := float64(endDay - startDay)
	if d <= 0 {
		// Degenerate window (zero/negative length): no time to weight over.
		if len(flows) == 0 && math.Abs(v0) > dietzDegenTol {
			return v1/v0 - 1, v0 > dietzDegenTol
		}
		return 0, false
	}

	var sumF, weighted float64
	for _, f := range flows {
		di := float64(f.Day - startDay)
		w := (d - di) / d
		sumF += f.Amount
		weighted += w * f.Amount
	}

	den := v0 + weighted
	if den <= dietzDegenTol { // covers zero, sub-tolerance, and negative bases
		return 0, false
	}
	return (v1 - v0 - sumF) / den, true
}
