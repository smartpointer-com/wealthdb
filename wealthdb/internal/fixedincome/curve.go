// Package fixedincome holds the bond and rate arithmetic the yield report
// and the income projection rest on:
//
//   - day counts, and the coupon schedule, price, yield and risk of a
//     fixed-coupon bond;
//   - the price, yield and risk of a bill, and the coupon and risk of a
//     floating-rate note;
//   - rate sensitivity split over the five risk tenors;
//   - a currency's zero curve and its forward rates, built from the
//     published tenors of its government curve, and the rate paths a
//     holding earns.
//
// Every function here is pure: it reads its arguments and nothing else.
package fixedincome

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A Point is one tenor of a published yield curve: its length in months
// and its yield in percent.
type Point struct {
	Months int
	Rate   float64
}

// parZeroMonths is the longest tenor read as a zero rate as published. A
// published yield of a year or less stands for one payment at maturity
// (on the Treasury curve, a bill's bond-equivalent yield). A longer one
// is the coupon of a bond priced at par, and is bootstrapped to a zero
// rate.
const parZeroMonths = 12

// couponMonths is the months between two coupons of the par bonds a
// curve's longer tenors stand for: semiannual, as a Treasury pays.
const couponMonths = 6

// maxRate bounds a tenor's rate, in percent either way: a series in
// basis points lies outside it.
const maxRate = 100

// Curve is a currency's zero curve on a monthly grid: the zero rate of
// every month from the first to the longest tenor, in percent with
// semiannual compounding, the convention of the points it is built from.
// Before the shortest tenor and past the longest one the rate is flat.
type Curve struct {
	zero []float64 // zero[m] is the zero rate of month m, m >= 1; zero[0] repeats zero[1]
}

// ErrNoPoints is returned for a curve built from no points.
var ErrNoPoints = errors.New("fixedincome: a curve needs at least one tenor")

// Bootstrap builds the zero curve of a published par curve:
//
//   - A tenor of a year or less is a zero rate as published
//     (parZeroMonths).
//   - Every six months past a year, and at the longest tenor, the par
//     yield (linear between the two tenors around it) is the coupon of a
//     semiannual bond priced at par, and the zero rate is the one that
//     prices it: 1 = Σ c/2·DF(tᵢ) + (1 + c/2)·DF(tₙ). A longest tenor
//     that is no multiple of six months ends on a short period, which
//     pays its part of the coupon.
//   - Between those points, the zero rate is linear in months.
//
// Rates may be negative. Points need not be sorted. Two points of the
// same length, a length outside 1 to canonical.MaxTenorMonths months, a
// rate that is not finite or reaches ±maxRate percent, and a curve no
// positive discount factor prices are refused.
func Bootstrap(points []Point) (*Curve, error) {
	if len(points) == 0 {
		return nil, ErrNoPoints
	}
	pts := slices.Clone(points)
	slices.SortFunc(pts, func(a, b Point) int { return a.Months - b.Months })
	for i, p := range pts {
		if p.Months < 1 || p.Months > canonical.MaxTenorMonths {
			return nil, fmt.Errorf("fixedincome: tenor of %d months: want 1 to %d", p.Months, canonical.MaxTenorMonths)
		}
		if math.IsNaN(p.Rate) || math.IsInf(p.Rate, 0) || math.Abs(p.Rate) >= maxRate {
			return nil, fmt.Errorf("fixedincome: tenor of %d months: rate %v: want a percentage within ±%d",
				p.Months, p.Rate, maxRate)
		}
		if i > 0 && pts[i-1].Months == p.Months {
			return nil, fmt.Errorf("fixedincome: two tenors of %d months", p.Months)
		}
	}
	last := pts[len(pts)-1].Months

	// The nodes: every published tenor of a year or less, read as a zero
	// rate, and every coupon date past a year up to the longest tenor,
	// bootstrapped in order.
	var nodes []Point
	for _, p := range pts {
		if p.Months <= parZeroMonths {
			nodes = append(nodes, p)
		}
	}
	if last > parZeroMonths {
		// The coupon dates of the shorter par bonds need a discount
		// factor: a node every six months up to a year where no tenor is
		// published, from the par curve, read as a zero rate as the short
		// tenors are.
		for m := couponMonths; m <= parZeroMonths; m += couponMonths {
			if !slices.ContainsFunc(nodes, func(n Point) bool { return n.Months == m }) {
				nodes = append(nodes, Point{m, interpolate(pts, m)})
			}
		}
		slices.SortFunc(nodes, func(a, b Point) int { return a.Months - b.Months })
		// sum is Σ DF(tᵢ) over the coupon dates before the next par bond's
		// maturity: each is a node by the time a bond pays on it.
		sum := 0.0
		for t := couponMonths; t <= parZeroMonths; t += couponMonths {
			sum += discount(interpolate(nodes, t), t)
		}
		for m := parZeroMonths + couponMonths; m < last+couponMonths; m += couponMonths {
			n := min(m, last)
			c := interpolate(pts, n) / 100
			// A final period shorter than six months (a tenor that is no
			// multiple of six) pays its accrued part of the coupon.
			stub := float64(n-(n-1)/couponMonths*couponMonths) / couponMonths
			df := (1 - c/2*sum) / (1 + c/2*stub)
			if !(df > 0) {
				return nil, fmt.Errorf("fixedincome: no positive discount factor prices the par bond of %d months", n)
			}
			nodes = append(nodes, Point{n, zeroFromDiscount(df, n)})
			sum += df
		}
	}

	c := &Curve{zero: make([]float64, last+1)}
	for m := 1; m <= last; m++ {
		c.zero[m] = interpolate(nodes, m)
	}
	c.zero[0] = c.zero[1]
	return c, nil
}

// Months is the curve's longest tenor in months.
func (c *Curve) Months() int { return len(c.zero) - 1 }

// Zero is the zero rate of month m in percent, semiannually compounded:
// flat before the first month and past the longest tenor.
func (c *Curve) Zero(m int) float64 {
	return c.zero[max(0, min(m, len(c.zero)-1))]
}

// Discount is the discount factor of month m: what one paid then is worth
// today. Discount(0) is 1.
func (c *Curve) Discount(m int) float64 {
	if m <= 0 {
		return 1
	}
	return discount(c.Zero(m), m)
}

// Forward is the rate the curve implies today for money lent from month
// from for term months, in percent, semiannually compounded like the
// zero rates: (1 + f/200)^(term/6) = DF(from)/DF(from+term). Forward(0,
// m) is Zero(m). A term below one month is one month.
func (c *Curve) Forward(from, term int) float64 {
	from, term = max(0, from), max(1, term)
	ratio := c.Discount(from) / c.Discount(from+term)
	return 200 * (math.Pow(ratio, couponMonths/float64(term)) - 1)
}

// Path is the forward rate of a term-month loan starting at each month
// 0 … months-1: the rate path a holding that resets to that tenor earns
// under the forward curve. Its first month is today's rate for the term.
func (c *Curve) Path(months, term int) []float64 {
	out := make([]float64, max(0, months))
	for m := range out {
		out[m] = c.Forward(m, term)
	}
	return out
}

// interpolate is the rate at m months over points sorted by length:
// linear between the two around it, flat before the first and past the
// last.
func interpolate(pts []Point, m int) float64 {
	if m <= pts[0].Months {
		return pts[0].Rate
	}
	for i := 1; i < len(pts); i++ {
		if a, b := pts[i-1], pts[i]; m <= b.Months {
			return a.Rate + (b.Rate-a.Rate)*float64(m-a.Months)/float64(b.Months-a.Months)
		}
	}
	return pts[len(pts)-1].Rate
}

// discount is the discount factor of a zero rate z (percent, semiannual)
// at m months.
func discount(z float64, m int) float64 {
	return math.Pow(1+z/200, -float64(m)/couponMonths)
}

// zeroFromDiscount is the zero rate (percent, semiannual) a discount
// factor at m months implies.
func zeroFromDiscount(df float64, m int) float64 {
	return 200 * (math.Pow(df, -couponMonths/float64(m)) - 1)
}
