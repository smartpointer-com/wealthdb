package fixedincome

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.10f, want %.10f (±%g)", what, got, want, tol)
	}
}

func mustCurve(t *testing.T, pts ...Point) *Curve {
	t.Helper()
	c, err := Bootstrap(pts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A flat par curve is a flat zero curve: a bond that pays the curve's
// rate as its coupon is priced at par by discounting at that same rate.
// Every forward rate is the same rate.
func TestFlatParCurveIsFlat(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{3, 4}, Point{12, 4}, Point{60, 4}, Point{360, 4})
	for _, m := range []int{1, 3, 7, 12, 30, 60, 121, 360, 400} {
		near(t, "zero", c.Zero(m), 4, 1e-9)
	}
	for _, f := range [][2]int{{0, 3}, {12, 12}, {35, 1}, {100, 120}} {
		near(t, "forward", c.Forward(f[0], f[1]), 4, 1e-9)
	}
}

// Each bootstrapped zero rate prices its par bond at par:
// Σ c/2·DF(tᵢ) + DF(tₙ) = 1 on the semiannual dates.
func TestBootstrappedZerosPriceParBondsAtPar(t *testing.T) {
	t.Parallel()
	full := []Point{{1, 4.10}, {3, 4.05}, {6, 3.95}, {12, 3.70}, {24, 3.45}, {36, 3.40}, {60, 3.55},
		{84, 3.75}, {120, 3.95}, {240, 4.40}, {360, 4.55}}
	// A curve whose shortest tenor is past a year has no short tenor for
	// its first coupons: they read the par rate flat before it as a zero
	// rate.
	long := []Point{{24, 3}, {120, 4}}
	for _, pts := range [][]Point{full, long} {
		c := mustCurve(t, pts...)
		for n := 18; n <= c.Months(); n += 6 {
			cpn := interpolate(pts, n) / 100
			pv := 0.0
			for i := 6; i <= n; i += 6 {
				pv += cpn / 2 * c.Discount(i)
			}
			near(t, "par bond price", pv+c.Discount(n), 1, 1e-12)
		}
	}
	// A tenor of a year or less is its published rate.
	c := mustCurve(t, full...)
	near(t, "zero 3M", c.Zero(3), 4.05, 1e-12)
	near(t, "zero 1Y", c.Zero(12), 3.70, 1e-12)
	c = mustCurve(t, long...)
	near(t, "zero 6M of a long curve", c.Zero(6), 3, 1e-12)
	near(t, "zero 1Y of a long curve", c.Zero(12), 3, 1e-12)
}

// An upward-sloping par curve has zero rates above the par yields past
// a year (the coupons of a par bond are discounted at the lower short
// rates), and forward rates above the zero rates.
func TestUpwardSlopingCurve(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{6, 2}, Point{12, 2.5}, Point{24, 3}, Point{60, 4}, Point{120, 5})
	if z := c.Zero(120); z <= 5 {
		t.Errorf("zero 10Y = %v, want above the par yield 5", z)
	}
	if f, z := c.Forward(60, 60), c.Zero(60); f <= z {
		t.Errorf("forward 5Y in 5Y = %v, want above the 5Y zero %v", f, z)
	}
}

// A two-period example worked by hand. Zero 6M = 2%, zero 1Y = 3%; the
// 18-month par yield 3.5% has the zero rate z with
// 0.0175·DF(6) + 0.0175·DF(12) + 1.0175·DF(18) = 1.
func TestBootstrapByHand(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{6, 2}, Point{12, 3}, Point{18, 3.5})
	d6, d12 := 1/1.01, 1/math.Pow(1.015, 2)
	d18 := (1 - 0.0175*(d6+d12)) / 1.0175
	want := 200 * (math.Pow(d18, -1.0/3) - 1)
	near(t, "zero 18M", c.Zero(18), want, 1e-12)
	// Between nodes the zero rate is linear in months.
	near(t, "zero 9M", c.Zero(9), 2.5, 1e-12)
	near(t, "zero 15M", c.Zero(15), (3+want)/2, 1e-12)
}

func TestForwardFromDiscountFactors(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{1, 5.2}, Point{3, 5.0}, Point{12, 4.6}, Point{24, 4.1}, Point{120, 4.3})
	for _, f := range [][2]int{{0, 1}, {0, 24}, {3, 3}, {12, 12}, {30, 6}, {100, 60}} {
		got := c.Forward(f[0], f[1])
		lhs := math.Pow(1+got/200, float64(f[1])/6)
		near(t, "growth", lhs, c.Discount(f[0])/c.Discount(f[0]+f[1]), 1e-12)
	}
	// Today's forward for a term is the zero rate of the term.
	for _, m := range []int{1, 3, 12, 24, 120} {
		near(t, "forward from today", c.Forward(0, m), c.Zero(m), 1e-9)
	}
	// An inverted short end: later three-month money is cheaper.
	if c.Forward(12, 3) >= c.Forward(0, 3) {
		t.Errorf("forward 3M in 1Y = %v, want below today's 3M %v", c.Forward(12, 3), c.Forward(0, 3))
	}
}

// The rate path of a three-month holding starts at today's three-month
// rate and follows the curve.
func TestPath(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{3, 4}, Point{12, 3}, Point{24, 3})
	p := c.Path(36, 3)
	if len(p) != 36 {
		t.Fatalf("path of %d months, want 36", len(p))
	}
	near(t, "month 0", p[0], 4, 1e-9)
	for m, r := range p {
		near(t, "path", r, c.Forward(m, 3), 0)
	}
	if len(c.Path(0, 3)) != 0 {
		t.Error("a path of no months has rates")
	}
}

// Rates may be negative: the discount factor then exceeds one, and the
// arithmetic holds as above.
func TestNegativeRates(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{3, -0.75}, Point{12, -0.6}, Point{60, -0.25}, Point{120, 0.1})
	near(t, "zero 3M", c.Zero(3), -0.75, 1e-12)
	if d := c.Discount(12); d <= 1 {
		t.Errorf("discount 1Y = %v, want above 1", d)
	}
	near(t, "forward from today", c.Forward(0, 60), c.Zero(60), 1e-9)
}

// Before the shortest tenor and past the longest the zero rate is flat;
// a curve of short tenors only is its published points.
func TestFlatOutside(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{3, 1.5}, Point{6, 2})
	if c.Months() != 6 {
		t.Errorf("months = %d, want 6", c.Months())
	}
	near(t, "zero 1M", c.Zero(1), 1.5, 0)
	near(t, "zero 0", c.Zero(0), 1.5, 0)
	near(t, "zero 4M", c.Zero(4), 1.5+0.5/3, 1e-12)
	near(t, "zero 2Y", c.Zero(24), 2, 0)
	near(t, "discount 0", c.Discount(0), 1, 0)
	near(t, "discount before today", c.Discount(-3), 1, 0)
}

// A loan starting before today starts today, and a term below one month
// is one month.
func TestForwardClamps(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{1, 5.2}, Point{3, 5.0}, Point{12, 4.6}, Point{24, 4.1}, Point{120, 4.3})
	near(t, "forward from before today", c.Forward(-5, 3), c.Forward(0, 3), 0)
	near(t, "forward for no term", c.Forward(12, 0), c.Forward(12, 1), 0)
	near(t, "forward for a negative term", c.Forward(12, -2), c.Forward(12, 1), 0)
}

// A tenor that is no multiple of six months ends on a short coupon
// period: its bond pays c/2 at 6, 12 and 18 months, then 1 plus half a
// period's coupon at 21, and is priced at par.
func TestOddTenor(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{6, 3}, Point{12, 3}, Point{21, 3})
	pv := 0.015*(c.Discount(6)+c.Discount(12)+c.Discount(18)) + (1+0.015/2)*c.Discount(21)
	near(t, "par bond price", pv, 1, 1e-12)
	// A flat curve stays flat to within the short period's simple accrual.
	near(t, "zero 21M", c.Zero(21), 3, 0.01)
}

// Points need not come sorted, and one tenor is a flat curve.
func TestUnsortedAndSingleTenor(t *testing.T) {
	t.Parallel()
	a := mustCurve(t, Point{120, 4.3}, Point{3, 5.0}, Point{24, 4.1}, Point{12, 4.6})
	b := mustCurve(t, Point{3, 5.0}, Point{12, 4.6}, Point{24, 4.1}, Point{120, 4.3})
	for _, m := range []int{1, 7, 30, 120} {
		near(t, "zero", a.Zero(m), b.Zero(m), 0)
	}
	one := mustCurve(t, Point{3, 2.5})
	for _, m := range []int{0, 1, 3, 60} {
		near(t, "zero", one.Zero(m), 2.5, 0)
	}
	near(t, "forward", one.Forward(12, 12), 2.5, 1e-9)
}

func TestBootstrapRefuses(t *testing.T) {
	t.Parallel()
	if _, err := Bootstrap(nil); !errors.Is(err, ErrNoPoints) {
		t.Errorf("no points: %v", err)
	}
	for name, c := range map[string]struct {
		pts  []Point
		want string
	}{
		"zero months": {[]Point{{0, 1}}, "want 1 to 600"},
		"too long":    {[]Point{{601, 1}}, "want 1 to 600"},
		"twice":       {[]Point{{12, 1}, {12, 2}}, "two tenors of 12 months"},
		"not finite":  {[]Point{{12, math.NaN()}}, "within ±100"},
		// A series in basis points, read as percent.
		"basis points": {[]Point{{3, 425}, {12, 450}, {24, 470}}, "within ±100"},
		// A rate past -100% lies outside ±maxRate.
		"below -100%": {[]Point{{12, -150}}, "within ±100"},
		// No positive discount factor prices this par bond.
		"no price": {[]Point{{3, 0}, {12, 0}, {360, 60}}, "no positive discount factor"},
	} {
		if _, err := Bootstrap(c.pts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}
