package returns

// onboardingDedupTol is the absolute (output-currency) tolerance below which a
// real funding flow in the debut bucket is treated as "explaining" the opening
// value, suppressing the synthetic onboarding inflow.
const onboardingDedupTol = 1e-6

// OnboardingFlow returns the synthetic onboarding inflow for an aggregate
// constituent's debut, so the step-up in the aggregate value series on the day a
// constituent first appears is booked as capital-in rather than performance.
//
// realDebutFunding is the sum of real external capital-in flows already recorded
// in the debut bucket. The synthetic inflow covers only the *unexplained*
// opening value (firstValue - realDebutFunding); when a real funding flow
// already accounts for the opening value, nothing is injected (dedup). The
// returned Flow is dated on the debut day with capital-in (positive) sign.
func OnboardingFlow(debutDay int64, firstValue, realDebutFunding float64) (Flow, bool) {
	synthetic := firstValue - realDebutFunding
	if synthetic <= onboardingDedupTol {
		return Flow{}, false
	}
	return Flow{Day: debutDay, Amount: synthetic}, true
}

// ClosureFlow returns the synthetic closure outflow for an explicitly-closed
// aggregate constituent, plus the day from which the constituent's spine
// contribution is zeroed (zeroFrom accompanies every explicit closure,
// injected flow or not, so value and flow stay atomic).
//
// The synthetic books lastValue — the caller passes the value carried on the
// day before the closure day, which is non-zero only when the account zeroes
// on the spine's final day (inside a longer zero tail it is ~0 and ok is
// false; the zeroing value drop is then the exit signal). Under
// ClosureSubsumeDrains the caller subsumes the real closure-window flows, so
// no near-day dedup is needed; under ClosureLedgerExact the caller keeps the
// real flows and skips this synthesis entirely. ok is false when there is no
// explicit closure (closureDay==0) or no value to book. Staleness/dormancy
// must never reach here.
func ClosureFlow(closureDay int64, lastValue float64) (flow Flow, zeroFrom int64, ok bool) {
	if closureDay == 0 {
		return Flow{}, 0, false
	}
	if lastValue <= onboardingDedupTol {
		return Flow{}, closureDay, false // nothing left to book at the boundary
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
