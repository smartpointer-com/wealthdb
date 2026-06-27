package returns

import "math"

// onboardingDedupTol is the absolute (output-currency) tolerance below which a
// real funding flow in the debut bucket is treated as "explaining" the opening
// value, suppressing the synthetic onboarding inflow.
const onboardingDedupTol = 1e-6

// OnboardingFlow returns the synthetic onboarding inflow for an aggregate
// constituent's debut, so the step-up in the aggregate value series on the day a
// constituent first appears is booked as capital-in rather than performance
// (proposal §2.7).
//
// realDebutFunding is the sum of real external capital-in flows already recorded
// in the debut bucket. The synthetic inflow covers only the *unexplained*
// opening value (firstValue - realDebutFunding); when a real funding flow
// already accounts for the opening value, nothing is injected (dedup, GAP2). The
// returned Flow is dated on the debut day with capital-in (positive) sign.
func OnboardingFlow(debutDay int64, firstValue, realDebutFunding float64) (Flow, bool) {
	synthetic := firstValue - realDebutFunding
	if synthetic <= onboardingDedupTol {
		return Flow{}, false
	}
	return Flow{Day: debutDay, Amount: synthetic}, true
}

// ClosureFlow returns the synthetic closure outflow for an explicitly-closed
// aggregate constituent, plus the day from which the constituent's carried-
// forward spine contribution MUST be zeroed.
//
// The two are an atomic pair: the carry-forward spine keeps lastValue in V_end
// past the closure day, so booking the -lastValue outflow without also zeroing
// the spine contribution double-counts and prints a spurious return on the
// closure link (proposal §2.7 / §3, verified). ok is false when there is no
// explicit closure (closureDay==0); staleness/dormancy must route to the
// carried_forward flag, never here — firing on mere silence mis-books a slow
// source as a divestment.
func ClosureFlow(closureDay int64, lastValue float64) (flow Flow, zeroFrom int64, ok bool) {
	if closureDay == 0 {
		return Flow{}, 0, false
	}
	return Flow{Day: closureDay, Amount: -lastValue}, closureDay, true
}

// ZeroedValue is the atomic counterpart to ClosureFlow: a constituent's
// contribution to an aggregate boundary value is its carried-forward value
// before the closure day and exactly 0 from the closure day onward. closureDay==0
// means "not closed" (value passes through unchanged).
func ZeroedValue(value float64, day, closureDay int64) float64 {
	if closureDay != 0 && day >= closureDay {
		return 0
	}
	return value
}

// approxEqual is a small helper used by tests and dedup reasoning.
func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
