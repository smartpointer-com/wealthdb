// Package fixedincome holds the bond and rate arithmetic the yield report
// and the income projection rest on:
//
//   - day counts, and the coupon schedule, price, yield and risk of a
//     fixed-coupon bond;
//   - the price, yield and risk of a bill, and the coupon and risk of a
//     floating-rate note;
//   - rate sensitivity split over the five risk tenors;
//   - a currency's zero curve and its forward rates, built from the
//     published tenors of its government curve (par yields or zero
//     rates), and the rate paths a holding earns.
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
// and its rate in percent, on the curve's Basis.
type Point struct {
	Months int
	Rate   float64
}

// A Basis is what the rates of a published curve are and how they
// compound.
type Basis struct {
	// Spot says the rates are zero rates: each the yield of one payment
	// at its tenor. Otherwise they are par yields: each the coupon of a
	// bond of that tenor priced at par.
	Spot bool
	// Frequency is how many times a year a rate compounds: a zero rate's
	// compounding, or a par bond's coupons. Zero is continuous, which
	// only a zero rate takes.
	Frequency int
}

// Treasury is the basis of the US Treasury curve: semiannual par
// yields, where a tenor of a year or less is a bill's bond-equivalent
// yield, read as a zero rate as published. A rate series that states no
// basis is read on it. So is a bill's discount rate (fred's `discount`
// series): read as if it were a bond-equivalent yield, an approximation
// that reads it slightly low.
var Treasury = Basis{Frequency: 2}

// compoundings are the frequencies a zero rate compounds at, by the name
// a rate series states; 0 is continuous.
var compoundings = map[string]int{"continuous": 0, "annual": 1, "semiannual": 2, "quarterly": 4, "monthly": 12}

// ParseBasis reads the basis a rate series states: its kind and its
// compounding, as a collector's payload names them.
//
//   - spot: zero rates, their compounding continuous, annual,
//     semiannual, quarterly or monthly;
//   - par: par yields, their coupons annual or semiannual;
//   - constant_maturity or discount (the Treasury's series), or no kind:
//     Treasury, which states no compounding.
//
// Any other kind or compounding is refused.
func ParseBasis(kind, compounding string) (Basis, error) {
	switch kind {
	case "", "constant_maturity", "discount":
		if compounding != "" {
			return Basis{}, fmt.Errorf("fixedincome: basis %q: states no compounding, not %q", kind, compounding)
		}
		return Treasury, nil
	case "spot", "par":
		f, ok := compoundings[compounding]
		b := Basis{Spot: kind == "spot", Frequency: f}
		if !ok || b.check() != nil {
			want := "continuous, annual, semiannual, quarterly or monthly"
			if !b.Spot {
				want = "annual or semiannual"
			}
			return Basis{}, fmt.Errorf("fixedincome: basis %q: compounding %q: want %s", kind, compounding, want)
		}
		return b, nil
	}
	return Basis{}, fmt.Errorf("fixedincome: basis %q: want spot or par", kind)
}

// String names a basis as a rate series states it: "spot continuous",
// "par annual"; Treasury for the Treasury's.
func (b Basis) String() string {
	if b == Treasury {
		return "Treasury"
	}
	kind := "par"
	if b.Spot {
		kind = "spot"
	}
	for name, f := range compoundings {
		if f == b.Frequency {
			return kind + " " + name
		}
	}
	return fmt.Sprintf("%s %d a year", kind, b.Frequency)
}

// check refuses a basis no curve is built on: a zero rate compounds at
// one of compoundings, a par bond pays its coupon once or twice a year.
func (b Basis) check() error {
	if !b.Spot && b.Frequency != 1 && b.Frequency != 2 {
		return fmt.Errorf("fixedincome: a par curve pays its coupons once or twice a year, not %d times", b.Frequency)
	}
	for _, f := range compoundings {
		if f == b.Frequency {
			return nil
		}
	}
	return fmt.Errorf("fixedincome: a zero rate compounds continuously or 1, 2, 4 or 12 times a year, not %d",
		b.Frequency)
}

// parZeroMonths is the longest tenor of a par curve read as a zero rate
// as published. A published yield of a year or less stands for one
// payment at maturity (on the Treasury curve, a bill's bond-equivalent
// yield). A longer one is the coupon of a bond priced at par, and is
// bootstrapped to a zero rate.
const parZeroMonths = 12

// compoundMonths is the months between two compoundings of the Curve's
// rates: semiannual, the Treasury's convention.
const compoundMonths = 6

// maxRate bounds a tenor's rate, in percent either way: a series in
// basis points lies outside it.
const maxRate = 100

// Curve is a currency's zero curve on a monthly grid: the zero rate of
// every month from the first to the longest tenor, in percent with
// semiannual compounding, whatever the basis of the points it is built
// from. Before the shortest tenor and past the longest one the rate is
// flat.
type Curve struct {
	zero []float64 // zero[m] is the zero rate of month m, m >= 1; zero[0] repeats zero[1]
}

// ErrNoPoints is returned for a curve built from no points.
var ErrNoPoints = errors.New("fixedincome: a curve needs at least one tenor")

// Bootstrap is the Treasury shorthand: NewCurve(points, Treasury).
func Bootstrap(points []Point) (*Curve, error) {
	return NewCurve(points, Treasury)
}

// NewCurve builds the zero curve of the points of a curve published on
// basis b:
//
//   - A spot curve's points are its zero rates. Between two tenors the
//     rate is linear in months, and each month's rate is converted from
//     the basis's compounding to the Curve's semiannual one.
//   - A par curve's tenors of a year or less are zero rates as published,
//     at the basis's compounding (parZeroMonths). At every coupon date
//     past a year, and at the longest tenor, the par yield (linear
//     between the two tenors around it) is the coupon of a bond priced
//     at par paying f coupons a year, and the zero rate is the one that
//     prices it: 1 = Σ c/f·DF(tᵢ) + (1 + c/f)·DF(tₙ). A longest tenor
//     that is no whole number of coupon periods ends on a short period,
//     which pays its part of the coupon. Between those points, the zero
//     rate is linear in months.
//
// Rates may be negative. Points need not be sorted. A basis no curve is
// built on, two points of the same length, a length outside 1 to
// canonical.MaxTenorMonths months, a rate that is not finite or reaches
// ±maxRate percent, and a par curve no positive discount factor prices
// are refused.
func NewCurve(points []Point, b Basis) (*Curve, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
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
	c := &Curve{zero: make([]float64, last+1)}
	if b.Spot {
		for m := 1; m <= last; m++ {
			c.zero[m] = semiannual(interpolate(pts, m), b.Frequency)
		}
	} else {
		nodes, err := bootstrap(pts, b.Frequency)
		if err != nil {
			return nil, err
		}
		for m := 1; m <= last; m++ {
			c.zero[m] = interpolate(nodes, m)
		}
	}
	c.zero[0] = c.zero[1]
	return c, nil
}

// bootstrap is the nodes of a par curve's zero rates, in percent with
// semiannual compounding: every published tenor of a year or less, read
// as a zero rate, and every coupon date past a year up to the longest
// tenor, bootstrapped in order. pts are sorted and checked; freq is the
// coupons a year.
func bootstrap(pts []Point, freq int) ([]Point, error) {
	period := 12 / freq
	last := pts[len(pts)-1].Months
	var nodes []Point
	for _, p := range pts {
		if p.Months <= parZeroMonths {
			nodes = append(nodes, Point{p.Months, semiannual(p.Rate, freq)})
		}
	}
	if last <= parZeroMonths {
		return nodes, nil
	}
	// The coupon dates of the shorter par bonds need a discount factor: a
	// node every coupon period up to a year where no tenor is published,
	// from the par curve, read as a zero rate as the short tenors are.
	for m := period; m <= parZeroMonths; m += period {
		if !slices.ContainsFunc(nodes, func(n Point) bool { return n.Months == m }) {
			nodes = append(nodes, Point{m, semiannual(interpolate(pts, m), freq)})
		}
	}
	slices.SortFunc(nodes, func(a, b Point) int { return a.Months - b.Months })
	// sum is Σ DF(tᵢ) over the coupon dates before the next par bond's
	// maturity: each is a node by the time a bond pays on it.
	sum := 0.0
	for t := period; t <= parZeroMonths; t += period {
		sum += discount(interpolate(nodes, t), t)
	}
	for m := parZeroMonths + period; m < last+period; m += period {
		n := min(m, last)
		// The coupon of one period. A final period shorter than a whole
		// one (a tenor that is no whole number of periods) pays its
		// accrued part of it.
		c := interpolate(pts, n) / 100 / float64(freq)
		stub := float64(n-(n-1)/period*period) / float64(period)
		df := (1 - c*sum) / (1 + c*stub)
		if !(df > 0) {
			return nil, fmt.Errorf("fixedincome: no positive discount factor prices the par bond of %d months", n)
		}
		nodes = append(nodes, Point{n, zeroFromDiscount(df, n)})
		sum += df
	}
	return nodes, nil
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
	return 200 * (math.Pow(ratio, compoundMonths/float64(term)) - 1)
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
	return math.Pow(1+z/200, -float64(m)/compoundMonths)
}

// zeroFromDiscount is the zero rate (percent, semiannual) a discount
// factor at m months implies.
func zeroFromDiscount(df float64, m int) float64 {
	return 200 * (math.Pow(df, -compoundMonths/float64(m)) - 1)
}

// semiannual is a zero rate in percent compounded freq times a year
// (continuously for 0) restated with semiannual compounding: the rate
// with the same discount factor at every tenor.
func semiannual(r float64, freq int) float64 {
	switch freq {
	case 2:
		return r
	case 0:
		return 200 * math.Expm1(r/200)
	}
	f := float64(freq)
	return 200 * (math.Pow(1+r/(100*f), f/2) - 1)
}
