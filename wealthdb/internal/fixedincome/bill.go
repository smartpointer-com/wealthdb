package fixedincome

import (
	"math"
	"time"
)

// A discount instrument, such as a bill or commercial paper, pays its
// face at maturity and no coupon. Its price is quoted as a discount
// yield on ACT/360: the discount from face as a yearly share of the
// face. Each function counts the actual days from settle to maturity.

// BillDiscount is the discount yield in percent of paper priced at price
// per 100 face: (100 − price) × 360 / days. It is NaN for a price that
// is not positive and for a settlement on or after maturity.
func BillDiscount(settle, maturity time.Time, price float64) float64 {
	n := days(settle, maturity)
	if n <= 0 || !(price > 0) {
		return math.NaN()
	}
	return (100 - price) * 360 / float64(n)
}

// BillEffectiveAnnual is the effective annual rate in percent that paper
// priced at price earns to maturity: ((100/price)^(365/days) − 1) × 100.
// It is NaN where BillDiscount is.
func BillEffectiveAnnual(settle, maturity time.Time, price float64) float64 {
	n := days(settle, maturity)
	if n <= 0 || !(price > 0) {
		return math.NaN()
	}
	return (math.Pow(100/price, 365/float64(n)) - 1) * 100
}

// BillBondEquivalent is the bond-equivalent yield in percent of paper
// priced at price: the coupon-equivalent rate a government bill's
// constant-maturity yield is quoted in, so a bill compares with a
// curve. To a half year it is simple interest on ACT/365, (100 − price)
// / price × 365/days; past it, the rate that grows the price to 100 with
// one half year's compounding and simple interest on the rest. It is NaN
// where BillDiscount is.
func BillBondEquivalent(settle, maturity time.Time, price float64) float64 {
	n := days(settle, maturity)
	if n <= 0 || !(price > 0) {
		return math.NaN()
	}
	t := float64(n) / 365
	if n <= 182 {
		return (100 - price) / price / t * 100
	}
	a := 2*t - 1
	return (-2*t + 2*math.Sqrt(t*t-a*(1-100/price))) / a * 100
}

// BillRisk is the Risk of paper priced at price:
//
//   - Macaulay is the time of its one payment: days/365.
//   - Modified and Convexity are a single payment's, against the
//     effective annual rate.
//   - DV01 is the price's fall for a basis point of that rate: price ×
//     Modified × 0.0001, as a bond's is against its yield. (Against the
//     discount yield, on which the price is linear, it is 100 ×
//     days/360 × 0.0001.)
//
// Paper on or after maturity has no risk. A price that is not positive
// gives NaN.
func BillRisk(settle, maturity time.Time, price float64) Risk {
	n := days(settle, maturity)
	if n <= 0 {
		return Risk{}
	}
	r := OnePayment(float64(n)/365, BillEffectiveAnnual(settle, maturity, price), 1)
	r.DV01 = price * r.Modified * BasisPoint
	return r
}
