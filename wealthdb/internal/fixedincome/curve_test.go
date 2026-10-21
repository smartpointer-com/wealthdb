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
	c, err := NewCurve(pts, Treasury)
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
		for n := 18; n <= len(c.zero)-1; n += 6 {
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
	if n := len(c.zero) - 1; n != 6 {
		t.Errorf("months = %d, want 6", n)
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
	if _, err := NewCurve(nil, Treasury); !errors.Is(err, ErrNoPoints) {
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
		if _, err := NewCurve(c.pts, Treasury); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}

func mustBasis(t *testing.T, kind, compounding string) Basis {
	t.Helper()
	b, err := ParseBasis(kind, compounding)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustCurveOn(t *testing.T, b Basis, pts ...Point) *Curve {
	t.Helper()
	c, err := NewCurve(pts, b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A spot curve's points are zero rates. Continuously compounded, a rate
// r at t years discounts by exp(-r·t), and the semiannual rate with that
// discount factor is 200·(exp(r/200) - 1). Between tenors the published
// rate is linear in months.
func TestSpotContinuousCurve(t *testing.T) {
	t.Parallel()
	c := mustCurveOn(t, mustBasis(t, "spot", "continuous"), Point{12, 2}, Point{36, 4})
	near(t, "zero 1Y", c.Zero(12), 200*(math.Exp(0.01)-1), 1e-12)
	near(t, "zero 2Y", c.Zero(24), 200*(math.Exp(0.015)-1), 1e-12)
	near(t, "zero 6M, flat before the first tenor", c.Zero(6), c.Zero(12), 0)
	near(t, "discount 3Y", c.Discount(36), math.Exp(-0.12), 1e-12)
	// Lent for two years in a year: continuously (0.12 - 0.02)/2 = 5%.
	near(t, "forward 2Y in 1Y", c.Forward(12, 24), 200*(math.Exp(0.025)-1), 1e-12)
	near(t, "forward from today", c.Forward(0, 12), c.Zero(12), 1e-9)
}

// An annually compounded spot rate r at t years discounts by
// (1 + r/100)^-t.
func TestSpotAnnualCurve(t *testing.T) {
	t.Parallel()
	c := mustCurveOn(t, mustBasis(t, "spot", "annual"), Point{6, 1}, Point{24, 3})
	near(t, "discount 6M", c.Discount(6), math.Pow(1.01, -0.5), 1e-12)
	near(t, "discount 2Y", c.Discount(24), math.Pow(1.03, -2), 1e-12)
	near(t, "zero 2Y", c.Zero(24), 200*(math.Sqrt(1.03)-1), 1e-12)
	growth := math.Pow(1.03, 2) / math.Pow(1.01, 0.5)
	near(t, "forward 18M in 6M", c.Forward(6, 18), 200*(math.Pow(growth, 6.0/18)-1), 1e-12)
}

// A par curve with annual coupons, worked by hand. The 1Y par yield is
// the 1Y zero rate, annually compounded. The 2Y par bond pays 3% at one
// and two years: 0.03·DF(1) + 1.03·DF(2) = 1. The 3Y bond pays 3.5%:
// 0.035·(DF(1) + DF(2)) + 1.035·DF(3) = 1.
func TestParAnnualByHand(t *testing.T) {
	t.Parallel()
	c := mustCurveOn(t, mustBasis(t, "par", "annual"), Point{12, 2}, Point{24, 3}, Point{36, 3.5})
	d1 := 1 / 1.02
	d2 := (1 - 0.03*d1) / 1.03
	d3 := (1 - 0.035*(d1+d2)) / 1.035
	near(t, "discount 1Y", c.Discount(12), d1, 1e-12)
	near(t, "discount 2Y", c.Discount(24), d2, 1e-12)
	near(t, "discount 3Y", c.Discount(36), d3, 1e-12)
	near(t, "zero 1Y", c.Zero(12), 200*(math.Sqrt(1.02)-1), 1e-12)
	// Between nodes the semiannual zero rate is linear in months.
	near(t, "zero 18M", c.Zero(18), (c.Zero(12)+c.Zero(24))/2, 1e-12)
	near(t, "forward 1Y in 1Y", c.Forward(12, 12), 200*(math.Sqrt(d1/d2)-1), 1e-12)
	near(t, "forward 1Y in 2Y", c.Forward(24, 12), 200*(math.Sqrt(d2/d3)-1), 1e-12)

	// The same yields as semiannual par bonds are another curve.
	if s := mustCurve(t, Point{12, 2}, Point{24, 3}, Point{36, 3.5}); math.Abs(s.Zero(36)-c.Zero(36)) < 1e-6 {
		t.Errorf("zero 3Y = %v on both bases", c.Zero(36))
	}
}

// Each bootstrapped zero rate of an annual par curve prices its par bond
// at par, one coupon a year: Σ c·DF(tᵢ) + DF(tₙ) = 1. A tenor that is no
// whole number of years ends on a short period, which pays its part of
// the coupon.
func TestParAnnualPricesParBondsAtPar(t *testing.T) {
	t.Parallel()
	annual := mustBasis(t, "par", "annual")
	pts := []Point{{3, 3.2}, {12, 3}, {24, 2.8}, {60, 3.1}, {120, 3.4}, {360, 3.6}}
	c := mustCurveOn(t, annual, pts...)
	for n := 24; n <= len(c.zero)-1; n += 12 {
		cpn := interpolate(pts, n) / 100
		pv := 0.0
		for i := 12; i <= n; i += 12 {
			pv += cpn * c.Discount(i)
		}
		near(t, "par bond price", pv+c.Discount(n), 1, 1e-12)
	}
	// A 3M par yield of an annual curve is a zero rate, annually
	// compounded.
	near(t, "discount 3M", c.Discount(3), math.Pow(1.032, -0.25), 1e-12)

	odd := mustCurveOn(t, annual, Point{12, 2}, Point{30, 3})
	pv := 0.03*(odd.Discount(12)+odd.Discount(24)) + (1+0.03/2)*odd.Discount(30)
	near(t, "par bond of 30 months", pv, 1, 1e-12)
}

// A series that states no basis, or a Treasury one, maps to the Treasury
// basis. The hand-worked tests above pin what that basis gives.
func TestTreasuryBasis(t *testing.T) {
	t.Parallel()
	pts := []Point{{1, 4.10}, {3, 4.05}, {6, 3.95}, {12, 3.70}, {24, 3.45}, {60, 3.55}, {120, 3.95}, {360, 4.55}}
	want := mustCurve(t, pts...)
	for _, kind := range []string{"", "constant_maturity", "discount"} {
		b := mustBasis(t, kind, "")
		if b != Treasury {
			t.Errorf("basis %q = %+v, want Treasury", kind, b)
		}
		got := mustCurveOn(t, b, pts...)
		for m := 0; m <= len(want.zero)-1; m++ {
			if got.Zero(m) != want.Zero(m) {
				t.Fatalf("basis %q: zero %dM = %v, want %v", kind, m, got.Zero(m), want.Zero(m))
			}
		}
	}
	if b := mustBasis(t, "par", "semiannual"); b != Treasury {
		t.Errorf("semiannual par = %+v, want Treasury", b)
	}
}

func TestParseBasis(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind, compounding string
		want              Basis
	}{
		{"spot", "continuous", Basis{Spot: true}},
		{"spot", "annual", Basis{Spot: true, Frequency: 1}},
		{"spot", "monthly", Basis{Spot: true, Frequency: 12}},
		{"par", "annual", Basis{Frequency: 1}},
	} {
		if got := mustBasis(t, c.kind, c.compounding); got != c.want {
			t.Errorf("%s %s = %+v, want %+v", c.kind, c.compounding, got, c.want)
		}
	}
	for _, c := range [][3]string{
		{"par", "continuous", "want annual or semiannual"},
		{"par", "quarterly", "want annual or semiannual"},
		{"spot", "", "want continuous, annual"},
		{"spot", "daily", "want continuous, annual"},
		{"forward", "annual", "want spot or par"},
		{"constant_maturity", "annual", "states no compounding"},
	} {
		if b, err := ParseBasis(c[0], c[1]); err == nil || !strings.Contains(err.Error(), c[2]) {
			t.Errorf("%s %s = %+v, %v; want refused: %s", c[0], c[1], b, err, c[2])
		}
	}
	for b, want := range map[Basis]string{Treasury: "Treasury", {Spot: true}: "spot continuous",
		{Frequency: 1}: "par annual"} {
		if got := b.String(); got != want {
			t.Errorf("%+v names %q, want %q", b, got, want)
		}
	}
}

func TestNewCurveRefusesABasis(t *testing.T) {
	t.Parallel()
	for _, b := range []Basis{{Frequency: 0}, {Frequency: 4}, {Spot: true, Frequency: 3}, {Spot: true, Frequency: -1}} {
		if _, err := NewCurve([]Point{{12, 2}}, b); err == nil {
			t.Errorf("basis %+v built a curve", b)
		}
	}
}
