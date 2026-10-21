package fixedincome

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

var frequencies = []int{1, 2, 4, 12}

// A bond priced at its coupon on a coupon date is at par, for every
// frequency and day count, and par is priced at the coupon. The
// maturities test a month end and a mid-month date.
func TestParBondOnCouponDate(t *testing.T) {
	t.Parallel()
	for _, mat := range []time.Time{ymd(2034, 8, 31), ymd(2031, 5, 15)} {
		settle := mat.AddDate(-7, 0, 0)
		for _, f := range frequencies {
			for _, dc := range canonical.DayCounts() {
				b := Bond{Coupon: 4.5, Frequency: f, Maturity: mat, DayCount: dc}
				what := fmt.Sprintf("%s %s f%d", dc, mat.Format(time.DateOnly), f)
				dirty, clean := b.Price(settle, 4.5)
				near(t, what+" dirty", dirty, 100, 1e-9)
				near(t, what+" clean", clean, 100, 1e-9)
				y, err := b.Yield(settle, 100)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				near(t, what+" yield", y, 4.5, 1e-9)
			}
		}
	}
}

// Ten years of a 6% semiannual bond at 8%: 3·a(20, 4%) + 100/1.04²⁰.
func TestTextbookPrice(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 6, Frequency: 2, Maturity: ymd(2034, 3, 15), DayCount: canonical.DayCount30360}
	dirty, clean := b.Price(ymd(2024, 3, 15), 8)
	near(t, "dirty", dirty, 86.4097, 5e-5)
	near(t, "clean", clean, dirty, 0)
}

// Five years of a 10% annual bond at 10%: Macaulay 4.1699 years.
func TestTextbookDuration(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 10, Frequency: 1, Maturity: ymd(2030, 6, 1), DayCount: canonical.DayCountActAct}
	r := b.Risk(ymd(2025, 6, 1), 10)
	near(t, "Macaulay", r.Macaulay, 4.1699, 5e-5)
	near(t, "modified", r.Modified, r.Macaulay/1.1, 1e-12)
}

// A zero pays only its redemption. Its Macaulay duration is its term in
// quasi-coupon periods, and its convexity a single payment's.
func TestZero(t *testing.T) {
	t.Parallel()
	b := Bond{Frequency: 2, Maturity: ymd(2034, 6, 15), DayCount: canonical.DayCountActAct}
	settle := ymd(2024, 6, 15)
	flows := b.Flows(settle)
	if len(flows) != 1 || !flows[0].Date.Equal(b.Maturity) || flows[0].Coupon != 0 || flows[0].Principal != 100 {
		t.Fatalf("flows = %+v, want 100 at maturity", flows)
	}
	dirty, clean := b.Price(settle, 4)
	near(t, "price", dirty, 100/math.Pow(1.02, 20), 1e-9)
	near(t, "clean", clean, dirty, 0)
	r := b.Risk(settle, 4)
	near(t, "Macaulay", r.Macaulay, 10, 1e-12)
	near(t, "convexity", r.Convexity, 10*10.5/(1.02*1.02), 1e-9)
	if r.Convexity <= 0 {
		t.Errorf("convexity = %v, want positive", r.Convexity)
	}
	// Between quasi-coupon dates the term counts the part of a period
	// left: 2.5 months of a 6-month period, in actual days.
	settle = ymd(2033, 9, 30)
	r = b.Risk(settle, 4)
	near(t, "Macaulay mid-period", r.Macaulay, (1+float64(days(settle, ymd(2033, 12, 15)))/183)/2, 1e-12)
	y, err := b.Yield(settle, 97)
	if err != nil {
		t.Fatal(err)
	}
	dirty, _ = b.Price(settle, y)
	near(t, "round trip", dirty, 97, 1e-9)
	// A redemption above par is the zero's one payment.
	b.Redemption = 105
	if flows := b.Flows(settle); len(flows) != 1 || flows[0].Principal != 105 {
		t.Errorf("flows = %+v, want 105 at maturity", flows)
	}
}

// The yield that prices a bond between coupon dates is the yield it was
// priced at, for every day count and frequency, at rates from negative
// to high.
func TestYieldRoundTrip(t *testing.T) {
	t.Parallel()
	for _, dc := range canonical.DayCounts() {
		for _, f := range frequencies {
			for _, coupon := range []float64{0, 4.5} {
				b := Bond{Coupon: coupon, Frequency: f, Maturity: ymd(2033, 11, 30), DayCount: dc}
				for _, settle := range []time.Time{ymd(2026, 3, 10), ymd(2033, 11, 1)} {
					for _, y := range []float64{-1, 0, 0.5, 3, 7, 15, 60} {
						dirty, _ := b.Price(settle, y)
						got, err := b.Yield(settle, dirty)
						if err != nil {
							t.Fatalf("%s f%d coupon %v yield %v: %v", dc, f, coupon, y, err)
						}
						near(t, string(dc)+" round trip", got, y, 1e-8)
					}
				}
			}
		}
	}
}

// The accrued coupon follows the day count. Sixty 30/360 days of a 6%
// semiannual bond accrue 1.0; ACT/ACT reads sixty days against a
// 181-day period.
func TestAccrued(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 6, Frequency: 2, Maturity: ymd(2030, 7, 15), DayCount: canonical.DayCount30360}
	settle := ymd(2025, 3, 15)
	near(t, "30/360", b.Accrued(settle), 1, 1e-12)
	b.DayCount = canonical.DayCountActAct
	// Jan 15 to Jul 15 2025 is 181 days, and Jan 15 to Mar 16 is 60.
	settle = ymd(2025, 3, 16)
	if prev, next := b.Period(settle); days(prev, next) != 181 || days(prev, settle) != 60 {
		t.Fatalf("period %s to %s", prev.Format(time.DateOnly), next.Format(time.DateOnly))
	}
	near(t, "ACT/ACT", b.Accrued(settle), 6*60.0/362, 1e-12)
	b.DayCount = canonical.DayCountAct360
	near(t, "ACT/360", b.Accrued(settle), 6*60.0/360, 1e-12)
	// The clean price is the dirty one less the accrued coupon.
	dirty, clean := b.Price(settle, 5)
	near(t, "clean", clean, dirty-b.Accrued(settle), 1e-12)
	// Nothing accrues on a coupon date.
	near(t, "on a coupon date", b.Accrued(ymd(2025, 7, 15)), 0, 0)
}

// A maturity on a month end keeps its coupons on month ends: on the last
// of February too, in leap and common years. Any other day rolls back
// from maturity, so a day February cuts short returns in August.
func TestSchedule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		maturity   time.Time
		freq       int
		settle     time.Time
		prev, next time.Time
	}{
		{ymd(2030, 8, 31), 2, ymd(2025, 3, 1), ymd(2025, 2, 28), ymd(2025, 8, 31)},
		{ymd(2030, 8, 31), 2, ymd(2024, 3, 1), ymd(2024, 2, 29), ymd(2024, 8, 31)},
		// A settlement on a coupon date falls in the period it starts.
		{ymd(2030, 8, 31), 2, ymd(2025, 2, 28), ymd(2025, 2, 28), ymd(2025, 8, 31)},
		{ymd(2030, 8, 31), 4, ymd(2025, 12, 15), ymd(2025, 11, 30), ymd(2026, 2, 28)},
		{ymd(2030, 8, 31), 12, ymd(2025, 4, 30), ymd(2025, 4, 30), ymd(2025, 5, 31)},
		{ymd(2030, 8, 31), 1, ymd(2025, 4, 30), ymd(2024, 8, 31), ymd(2025, 8, 31)},
		// February 28 of a common year is a month end.
		{ymd(2027, 2, 28), 2, ymd(2026, 3, 1), ymd(2026, 2, 28), ymd(2026, 8, 31)},
		{ymd(2027, 2, 28), 2, ymd(2024, 1, 10), ymd(2023, 8, 31), ymd(2024, 2, 29)},
		{ymd(2028, 2, 29), 2, ymd(2027, 3, 1), ymd(2027, 2, 28), ymd(2027, 8, 31)},
		// February 28 of a leap year is not.
		{ymd(2028, 2, 28), 2, ymd(2027, 3, 1), ymd(2027, 2, 28), ymd(2027, 8, 28)},
		{ymd(2030, 8, 30), 2, ymd(2025, 3, 1), ymd(2025, 2, 28), ymd(2025, 8, 30)},
		{ymd(2030, 8, 30), 2, ymd(2024, 3, 1), ymd(2024, 2, 29), ymd(2024, 8, 30)},
		// On and past maturity the dates roll on.
		{ymd(2030, 8, 31), 2, ymd(2030, 8, 31), ymd(2030, 8, 31), ymd(2031, 2, 28)},
		{ymd(2030, 8, 31), 2, ymd(2031, 6, 1), ymd(2031, 2, 28), ymd(2031, 8, 31)},
	} {
		b := Bond{Coupon: 5, Frequency: c.freq, Maturity: c.maturity, DayCount: canonical.DayCount30360}
		prev, next := b.Period(c.settle)
		if !prev.Equal(c.prev) || !next.Equal(c.next) {
			t.Errorf("maturity %s f%d settle %s: period %s to %s, want %s to %s",
				c.maturity.Format(time.DateOnly), c.freq, c.settle.Format(time.DateOnly),
				prev.Format(time.DateOnly), next.Format(time.DateOnly),
				c.prev.Format(time.DateOnly), c.next.Format(time.DateOnly))
		}
	}
}

// Flows are the coupon dates after settlement, a regular coupon on each
// and the redemption with the last.
func TestFlows(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 5, Frequency: 2, Maturity: ymd(2030, 8, 31), DayCount: canonical.DayCount30360}
	want := []Flow{
		{ymd(2029, 2, 28), 2.5, 0},
		{ymd(2029, 8, 31), 2.5, 0},
		{ymd(2030, 2, 28), 2.5, 0},
		{ymd(2030, 8, 31), 2.5, 100},
	}
	// A payment on the settlement date is not after it.
	for _, settle := range []time.Time{ymd(2029, 1, 10), ymd(2028, 8, 31)} {
		got := b.Flows(settle)
		if len(got) != len(want) {
			t.Fatalf("%d flows, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if !got[i].Date.Equal(want[i].Date) || got[i].Coupon != want[i].Coupon || got[i].Principal != want[i].Principal {
				t.Errorf("flow %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	}
	// The price is the flows discounted.
	settle := ymd(2029, 1, 10)
	dirty, _ := b.Price(settle, 6)
	prev, next := b.Period(settle)
	w := float64(Days30360(settle, next)) / float64(Days30360(prev, next))
	pv := 0.0
	for k, fl := range b.Flows(settle) {
		pv += (fl.Coupon + fl.Principal) / math.Pow(1.03, float64(k)+w)
	}
	near(t, "price of the flows", dirty, pv, 1e-10)
}

// DV01 is the dirty price times the modified duration times a basis
// point, and the central difference of Price. Convexity is Price's
// second difference.
func TestRisk(t *testing.T) {
	t.Parallel()
	for _, dc := range canonical.DayCounts() {
		for _, f := range frequencies {
			b := Bond{Coupon: 3.25, Frequency: f, Maturity: ymd(2045, 2, 15), DayCount: dc}
			settle, y := ymd(2026, 10, 21), 4.1
			r := b.Risk(settle, y)
			dirty, _ := b.Price(settle, y)
			up, _ := b.Price(settle, y+0.01)
			down, _ := b.Price(settle, y-0.01)
			near(t, "DV01 = dirty × modified × 1bp", r.DV01, dirty*r.Modified*0.0001, 1e-12)
			// The difference is off by Price's third derivative × h³/6.
			near(t, "DV01 = central difference", r.DV01, (down-up)/2, 1e-6*r.DV01)
			near(t, "modified", r.Modified, r.Macaulay/(1+y/100/float64(f)), 1e-12)
			const h = 0.1
			up, _ = b.Price(settle, y+h)
			down, _ = b.Price(settle, y-h)
			second := (up + down - 2*dirty) / (dirty * (h / 100) * (h / 100))
			near(t, "convexity", r.Convexity, second, 1e-4*r.Convexity)
			if r.DV01 <= 0 || r.Convexity <= 0 {
				t.Errorf("%s f%d: DV01 %v convexity %v, want positive", dc, f, r.DV01, r.Convexity)
			}
		}
	}
}

// On and after maturity nothing is left: no flows, no accrued coupon, no
// price and no risk.
func TestMatured(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 5, Frequency: 2, Maturity: ymd(2030, 8, 31), DayCount: canonical.DayCountActAct}
	for _, settle := range []time.Time{b.Maturity, ymd(2031, 1, 10)} {
		if fl := b.Flows(settle); len(fl) != 0 {
			t.Errorf("flows = %+v, want none", fl)
		}
		near(t, "accrued", b.Accrued(settle), 0, 0)
		dirty, clean := b.Price(settle, 5)
		near(t, "dirty", dirty, 0, 0)
		near(t, "clean", clean, 0, 0)
		if r := b.Risk(settle, 5); r != (Risk{}) {
			t.Errorf("risk = %+v, want none", r)
		}
	}
}

func TestEffectiveAnnual(t *testing.T) {
	t.Parallel()
	near(t, "semiannual 4%", EffectiveAnnual(4, 2), 4.04, 1e-12)
	near(t, "monthly 6%", EffectiveAnnual(6, 12), (math.Pow(1.005, 12)-1)*100, 1e-12)
	near(t, "annual", EffectiveAnnual(3.3, 1), 3.3, 1e-12)
	near(t, "simple", EffectiveAnnual(3.3, 0), 3.3, 0)
	near(t, "back from 4.04", FromEffectiveAnnual(4.04, 2), 4, 1e-12)
	near(t, "back simple", FromEffectiveAnnual(3.3, 0), 3.3, 0)
	for _, f := range frequencies {
		for _, y := range []float64{-2, 0, 2.5, 12} {
			near(t, "round trip", FromEffectiveAnnual(EffectiveAnnual(y, f), f), y, 1e-12)
		}
	}
}

func TestYieldRefuses(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 5, Frequency: 2, Maturity: ymd(2030, 8, 31), DayCount: canonical.DayCount30360}
	settle := ymd(2026, 1, 15)
	for name, c := range map[string]struct {
		settle time.Time
		price  float64
		want   error
	}{
		"at maturity":    {b.Maturity, 100, ErrMatured},
		"after maturity": {ymd(2031, 1, 1), 100, ErrMatured},
		"price of 0":     {settle, 0, ErrNonPositivePrice},
		"negative price": {settle, -5, ErrNonPositivePrice},
		"NaN price":      {settle, math.NaN(), ErrNonPositivePrice},
		// Above the price at -50%, and below the price at +100%.
		"price too high":   {settle, 1e6, ErrNoYield},
		"price too low":    {settle, 1, ErrNoYield},
		"price not finite": {settle, math.Inf(1), ErrNoYield},
	} {
		if _, err := b.Yield(c.settle, c.price); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
	for name, c := range map[string]struct {
		b    Bond
		want string
	}{
		"no frequency":    {Bond{Coupon: 5, Maturity: b.Maturity, DayCount: b.DayCount}, "coupon frequency 0"},
		"thrice a year":   {Bond{Coupon: 5, Frequency: 3, Maturity: b.Maturity, DayCount: b.DayCount}, "want 1, 2, 4 or 12"},
		"unknown count":   {Bond{Coupon: 5, Frequency: 2, Maturity: b.Maturity, DayCount: "ACT/999"}, "day count \"ACT/999\""},
		"no count":        {Bond{Coupon: 5, Frequency: 2, Maturity: b.Maturity}, "want one of 30/360"},
		"NaN coupon":      {Bond{Coupon: math.NaN(), Frequency: 2, Maturity: b.Maturity, DayCount: b.DayCount}, "coupon NaN"},
		"negative coupon": {Bond{Coupon: -1, Frequency: 2, Maturity: b.Maturity, DayCount: b.DayCount}, "coupon -1"},
		"infinite coupon": {Bond{Coupon: math.Inf(1), Frequency: 2, Maturity: b.Maturity, DayCount: b.DayCount}, "coupon +Inf"},
		"NaN redemption":  {Bond{Coupon: 5, Frequency: 2, Maturity: b.Maturity, DayCount: b.DayCount, Redemption: math.NaN()}, "redemption NaN"},
		"below zero":      {Bond{Coupon: 5, Frequency: 2, Maturity: b.Maturity, DayCount: b.DayCount, Redemption: -100}, "redemption -100"},
	} {
		_, err := c.b.Yield(settle, 100)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
		// The other methods give NaN, or nothing, for the same terms.
		if d, _ := c.b.Price(settle, 5); !math.IsNaN(d) {
			t.Errorf("%s: price %v, want NaN", name, d)
		}
		if a := c.b.Accrued(settle); !math.IsNaN(a) {
			t.Errorf("%s: accrued %v, want NaN", name, a)
		}
		if r := c.b.Risk(settle, 5); !math.IsNaN(r.DV01) {
			t.Errorf("%s: DV01 %v, want NaN", name, r.DV01)
		}
		if fl := c.b.Flows(settle); fl != nil {
			t.Errorf("%s: flows %+v, want none", name, fl)
		}
		if prev, next := c.b.Period(settle); !prev.IsZero() || !next.IsZero() {
			t.Errorf("%s: period %v to %v, want none", name, prev, next)
		}
	}
	// A price at a NaN yield is NaN.
	if d, c := b.Price(settle, math.NaN()); !math.IsNaN(d) || !math.IsNaN(c) {
		t.Errorf("price at a NaN yield = %v, %v", d, c)
	}
}

// Excel's PRICE example: a 5.75% bond maturing 2017-11-15, settled
// 2008-02-15 at 6.5%, 30/360 semiannual, prices at 94.6343616 clean.
func TestAMidPeriodPriceAsExcelGivesIt(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 5.75, Frequency: 2, Maturity: ymd(2017, 11, 15), DayCount: canonical.DayCount30360}
	_, clean := b.Price(ymd(2008, 2, 15), 6.5)
	near(t, "clean", clean, 94.6343616, 1e-7)
}

// The February rules of 30/360 belong to an end-of-month bond. A bond
// paying on the 28th counts February like any month: the day before its
// August coupon it has accrued 179 of 180 days.
func TestA30360BondOffTheMonthEnd(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 6, Frequency: 2, Maturity: ymd(2031, 8, 28), DayCount: canonical.DayCount30360}
	near(t, "accrued", b.Accrued(ymd(2027, 8, 27)), 6*179.0/360, 1e-12)
	eom := Bond{Coupon: 6, Frequency: 2, Maturity: ymd(2031, 8, 31), DayCount: canonical.DayCount30360}
	// An end-of-month bond's February 28 is the 30th: 1 day of the
	// period before March 1.
	near(t, "end of month", eom.Accrued(ymd(2027, 3, 1)), 6*1.0/360, 1e-12)
}

// Settled on a 31st, a 30/360 bond's accrued and remaining parts still
// make one period: its price at its coupon yield is par plus accrued
// less the discounting of a part period, within a cent.
func TestA30360SettleOnThe31st(t *testing.T) {
	t.Parallel()
	b := Bond{Coupon: 4, Frequency: 2, Maturity: ymd(2030, 1, 15), DayCount: canonical.DayCount30360}
	settle := ymd(2027, 7, 31)
	dirty, clean := b.Price(settle, 4)
	near(t, "clean at the coupon yield", clean, 100, 0.01)
	y, err := b.Yield(settle, dirty)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "yield", y, 4, 1e-9)
}
