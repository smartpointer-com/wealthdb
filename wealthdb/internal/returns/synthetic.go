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
// aggregate constituent, plus the day from which the constituent's carried-
// forward spine contribution MUST be zeroed.
//
// The spine zeroing (zeroFrom) is returned whenever there is an explicit closure,
// independent of whether a synthetic flow is injected — the carry-forward spine
// keeps lastValue in V_end past the closure day, so the zeroing is mandatory.
//
// The synthetic outflow is deduped against any REAL closing capital-out near the
// closure day (realClosing = magnitude of real withdrawal/transfer_out flows),
// mirroring the onboarding side: the textbook "withdraw everything" closure books
// a real −lastValue AND drives the snapshot to ~0, so injecting another
// −lastValue would double-count and depress the closure-link return.
// Only the unexplained remainder (lastValue − realClosing) is synthesized; ok is
// false (no flow) when a real closing flow already covers it, or when there is no
// explicit closure (closureDay==0). Staleness/dormancy must never reach here.
func ClosureFlow(closureDay int64, lastValue, realClosing float64) (flow Flow, zeroFrom int64, ok bool) {
	if closureDay == 0 {
		return Flow{}, 0, false
	}
	synthetic := lastValue - realClosing
	if synthetic <= onboardingDedupTol {
		return Flow{}, closureDay, false // real closing flow already explains the exit
	}
	return Flow{Day: closureDay, Amount: -synthetic}, closureDay, true
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
