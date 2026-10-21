package fixedincome

import (
	"math"
	"time"
)

// A discount instrument, such as a bill or commercial paper, pays its
// face at maturity and no coupon. Its price is quoted as a discount
// yield on ACT/360: the discount from face as a yearly share of the
// face. Each function counts the actual days from settle to maturity.

// BillPrice is the price per 100 face of discount paper at a discount
// yield in percent: 100 × (1 − d/100 × days/360). Paper on or after
// maturity pays nothing more: its price is 0.
func BillPrice(settle, maturity time.Time, discount float64) float64 {
	n := days(settle, maturity)
	if n <= 0 {
		return 0
	}
	return 100 * (1 - discount/100*float64(n)/360)
}

// BillDiscount is the discount yield in percent of paper priced at price
// per 100 face. It undoes BillPrice. It is NaN for a price that is not
// positive and for a settlement on or after maturity.
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
	r := onePayment(float64(n)/365, BillEffectiveAnnual(settle, maturity, price), 1)
	r.DV01 = price * r.Modified * bp
	return r
}
