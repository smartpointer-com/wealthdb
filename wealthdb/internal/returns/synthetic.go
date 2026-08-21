package returns

// syntheticTol is the absolute (output-currency) value below which no
// synthetic onboarding/closure flow is booked — a ~0 or negative amount
// (a per-entity-once step-up fully netted by sibling funding drops, a
// closure with nothing left at the boundary) is suppressed, not injected.
const syntheticTol = 1e-6

// OnboardingFlow returns the synthetic onboarding inflow for an aggregate
// constituent's debut, so the step-up in the aggregate value series on the day
// a constituent first appears is booked as capital-in rather than performance.
// The caller subsumes the constituent's real pre-debut flows first
// (entityFlows), so the synthetic books the full debut amount; ~0/negative
// amounts are suppressed. The returned Flow is dated on the debut day with
// capital-in (positive) sign.
func OnboardingFlow(debutDay int64, firstValue float64) (Flow, bool) {
	if firstValue <= syntheticTol {
		return Flow{}, false
	}
	return Flow{Day: debutDay, Amount: firstValue}, true
}

// ClosureFlow returns the synthetic closure outflow for an explicitly-closed
// aggregate constituent.
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
func ClosureFlow(closureDay int64, lastValue float64) (Flow, bool) {
	if closureDay == 0 {
		return Flow{}, false
	}
	if lastValue <= syntheticTol {
		return Flow{}, false // nothing left to book at the boundary
	}
	return Flow{Day: closureDay, Amount: -lastValue}, true
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
