package fixedincome

import (
	"math"
	"testing"
	"time"
)

// billPrice is the price per 100 face of discount paper at a discount
// yield in percent: 100 × (1 − d/100 × days/360). BillDiscount undoes
// it.
func billPrice(settle, maturity time.Time, discount float64) float64 {
	return 100 * (1 - discount/100*float64(days(settle, maturity))/360)
}

// A 182-day bill at 97.50 has a discount yield of 4.945% and an
// effective annual rate of 5.2086%.
func TestBillTextbook(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 1, 5)
	maturity := settle.AddDate(0, 0, 182)
	d := BillDiscount(settle, maturity, 97.5)
	near(t, "discount", d, 4.945, 5e-4)
	near(t, "discount exact", d, 2.5*360/182, 1e-12)
	near(t, "price", billPrice(settle, maturity, d), 97.5, 1e-12)
	ear := BillEffectiveAnnual(settle, maturity, 97.5)
	near(t, "effective annual", ear, 5.2086, 5e-5)
	near(t, "effective annual exact", ear, (math.Pow(100/97.5, 365.0/182)-1)*100, 1e-12)
}

// A bill's bond-equivalent yield is simple interest on ACT/365 to a
// half year, and past it grows the price to 100 with one half year
// compounded: a 182-day bill at 97.50 yields 5.1423%, a 364-day bill at
// 95 5.2099%.
func TestBillBondEquivalent(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 1, 5)
	near(t, "half year", BillBondEquivalent(settle, settle.AddDate(0, 0, 182), 97.5), 2.5/97.5*365/182*100, 1e-12)
	y := BillBondEquivalent(settle, settle.AddDate(0, 0, 364), 95)
	near(t, "a year", y, 5.2099, 5e-5)
	near(t, "grows to 100", 95*(1+y/200)*(1+y/100*(364.0/365-0.5)), 100, 1e-9)
	if y := BillBondEquivalent(settle, settle, 99); !math.IsNaN(y) {
		t.Errorf("bond-equivalent at maturity = %v, want NaN", y)
	}
}

// A price round-trips through its discount yield, at a premium too: a
// price above par has a negative discount yield.
func TestBillRoundTrip(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 3, 2)
	for _, n := range []int{1, 28, 91, 364} {
		maturity := settle.AddDate(0, 0, n)
		for _, d := range []float64{-0.5, 0, 1.2, 5.3} {
			p := billPrice(settle, maturity, d)
			near(t, "discount", BillDiscount(settle, maturity, p), d, 1e-10)
		}
	}
	if d := BillDiscount(settle, settle.AddDate(0, 0, 91), 100.1); d >= 0 {
		t.Errorf("discount at a premium = %v, want below zero", d)
	}
}

// The risk of a bill is that of its one payment. Its DV01 is the price's
// fall for a basis point of the effective annual rate its duration is
// against: price × modified × 1bp, which a central difference of the
// price in that rate confirms.
func TestBillRisk(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 1, 5)
	maturity := settle.AddDate(0, 0, 182)
	r := BillRisk(settle, maturity, 97.5)
	ear := BillEffectiveAnnual(settle, maturity, 97.5) / 100
	ty := 182.0 / 365
	near(t, "Macaulay", r.Macaulay, ty, 1e-12)
	near(t, "modified", r.Modified, ty/(1+ear), 1e-12)
	near(t, "convexity", r.Convexity, ty*(ty+1)/((1+ear)*(1+ear)), 1e-12)
	near(t, "DV01", r.DV01, 97.5*r.Modified*0.0001, 1e-15)
	at := func(rate float64) float64 { return 100 / math.Pow(1+rate, ty) }
	near(t, "DV01 from the price", r.DV01, (at(ear-0.0001)-at(ear+0.0001))/2, 1e-9)
	if bad := BillRisk(settle, maturity, 0); !math.IsNaN(bad.DV01) {
		t.Errorf("DV01 at a price of 0 = %v, want NaN", bad.DV01)
	}
}

// A bill on or after maturity pays nothing more and has no yield. A price
// that is not positive has no yield either.
func TestBillRefuses(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 1, 5)
	maturity := settle.AddDate(0, 0, 182)
	for _, s := range []float64{100, 97.5} {
		if d := BillDiscount(maturity, maturity, s); !math.IsNaN(d) {
			t.Errorf("discount at maturity = %v, want NaN", d)
		}
		if r := BillEffectiveAnnual(maturity.AddDate(0, 0, 3), maturity, s); !math.IsNaN(r) {
			t.Errorf("effective annual after maturity = %v, want NaN", r)
		}
	}
	for _, p := range []float64{0, -1, math.NaN()} {
		if d := BillDiscount(settle, maturity, p); !math.IsNaN(d) {
			t.Errorf("discount at a price of %v = %v, want NaN", p, d)
		}
		if r := BillEffectiveAnnual(settle, maturity, p); !math.IsNaN(r) {
			t.Errorf("effective annual at a price of %v = %v, want NaN", p, r)
		}
		if r := BillRisk(settle, maturity, p); !math.IsNaN(r.Modified) {
			t.Errorf("modified at a price of %v = %v, want NaN", p, r.Modified)
		}
	}
	if r := BillRisk(maturity, maturity, 100); r != (Risk{}) {
		t.Errorf("risk at maturity = %+v, want none", r)
	}
}
