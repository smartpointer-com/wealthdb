package fixedincome

import "time"

// A floating-rate note pays a reference rate plus a quoted spread, and
// resets its rate every few months. At each reset its coupon meets the
// market again, so the note sits near par. Its price risk runs only to
// its next reset: the risk of one payment of par on that date.

// FloaterCoupon is a floating note's coupon in percent: the reference
// rate plus the quoted spread. Either may be negative, and so may the
// sum: a floor is not modelled.
func FloaterCoupon(reference, spread float64) float64 {
	return reference + spread
}

// FloaterRisk is the Risk of a floating note whose rate resets on
// nextReset, at its current coupon rate in percent, paid frequency times
// a year:
//
//   - Macaulay is the years to the reset, actual days over 365.
//   - Modified is Macaulay / (1 + rate/100/frequency).
//   - DV01 is per 100 face: the note is taken at par.
//   - Convexity is that of a single payment at the reset.
//
// A frequency of 0 is simple interest to the reset. A reset on or before
// settle leaves no risk.
func FloaterRisk(settle, nextReset time.Time, rate float64, frequency int) Risk {
	n := days(settle, nextReset)
	if n <= 0 {
		return Risk{}
	}
	return onePayment(float64(n)/365, rate, frequency)
}
