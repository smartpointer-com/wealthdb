package fixedincome

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Bond is the terms of a fixed-coupon bond.
//
// Its coupon dates roll back from maturity by 12/Frequency months. A
// maturity on the last day of a month keeps every coupon on a month end
// (the end-of-month rule). The schedule knows no issue date: every
// period is a regular one, the first too.
//
// Yield refuses terms outside the ones below. The other methods give NaN
// for them, and Period and Flows give nothing.
type Bond struct {
	Coupon     float64 // percent a year; 0 for a zero-coupon bond
	Frequency  int     // coupons a year: 1, 2, 4 or 12
	Maturity   time.Time
	DayCount   canonical.DayCount
	Redemption float64 // per 100 face; 0 reads 100
}

// Flow is one payment of a bond, per 100 face.
type Flow struct {
	Date      time.Time
	Coupon    float64
	Principal float64
}

// Risk is how the price of a holding moves with its yield.
type Risk struct {
	// Macaulay is the payments' times in years, weighted by their present
	// values.
	Macaulay float64
	// Modified is the price's relative fall per unit rise of the yield
	// as a decimal: Macaulay over (1 + y/f).
	Modified float64
	// Convexity is the price's relative second derivative in the yield,
	// in years²: positive for a plain bond.
	Convexity float64
	// DV01 is the price's fall per 100 face for a rise of the yield by
	// one basis point.
	DV01 float64
}

var (
	// ErrNonPositivePrice is returned for a price that is zero, negative
	// or NaN.
	ErrNonPositivePrice = errors.New("fixedincome: a price must be positive")
	// ErrMatured is returned for a settlement on or after maturity.
	ErrMatured = errors.New("fixedincome: settlement is on or after maturity")
	// ErrNoYield is returned when no yield in the bracket Yield searches
	// gives the price.
	ErrNoYield = errors.New("fixedincome: no yield from -50% to +100% gives the price")
)

// The bracket Yield searches, in percent, and the steps it takes at
// most. Bisection alone closes the bracket to 1e-12 in under 50 steps.
const (
	minYield      = -50
	maxYield      = 100
	maxIterations = 100
)

// BasisPoint is one basis point, as a decimal.
const BasisPoint = 0.0001

// Period is the coupon period settle falls in: the coupon date on or
// before it, and the one after. A settlement on a coupon date starts that
// date's period. Past maturity the dates roll on by the same rule.
func (b Bond) Period(settle time.Time) (prev, next time.Time) {
	if b.check() != nil {
		return time.Time{}, time.Time{}
	}
	s, k := b.period(settle)
	return s.date(k), s.date(k - 1)
}

// Flows is every payment after settle: a coupon of Coupon/Frequency on
// each coupon date, and the redemption with the last. A zero pays only
// its redemption. A bond pays nothing on or after maturity.
func (b Bond) Flows(settle time.Time) []Flow {
	if b.check() != nil {
		return nil
	}
	flows, _ := b.legs(settle)
	return flows
}

// Accrued is the coupon accrued from the last coupon date to settle, per
// 100 face: Coupon times the day count's year fraction over that span
// (ACT/ACT reads it against the whole period). It is 0 on a coupon date
// and on or after maturity.
func (b Bond) Accrued(settle time.Time) float64 {
	if b.check() != nil {
		return math.NaN()
	}
	s, k := b.period(settle)
	if k <= 0 {
		return 0
	}
	prev, next := s.date(k), s.date(k-1)
	return b.Coupon * yearFraction(b.DayCount, prev, settle, prev, next, b.Frequency, s.eom)
}

// Price is the bond's price per 100 face at a yield in percent, by the
// street convention. The k-th payment from the next one is discounted at
// (1 + y/f)^(k−1+w). w is the part of the current period left after
// settle under the day count: one less the accrued part, the period's
// fraction from the last coupon to settle. On a coupon date w is 1
// under every day count, so a bond at a yield of its coupon is at par.
// The last period compounds as the others do; the street convention's
// simple interest over it differs by a basis point or two.
// A zero discounts over the same quasi-coupon periods, at its Frequency.
// The dirty price includes the accrued coupon and the clean one does
// not. On or after maturity both are 0.
func (b Bond) Price(settle time.Time, yield float64) (dirty, clean float64) {
	if b.check() != nil {
		return math.NaN(), math.NaN()
	}
	dirty = value(b.payments(settle), yield, b.Frequency).pv
	return dirty, dirty - b.Accrued(settle)
}

// Yield is the yield in percent, compounded Frequency times a year, at
// which the bond's dirty price per 100 face is dirty. It runs Newton's
// method within a bracket of -50% to +100%, and bisects where a step
// leaves the bracket. It refuses invalid terms, a settlement on or after
// maturity (ErrMatured), a price that is not positive
// (ErrNonPositivePrice), and a price no yield in the bracket gives
// (ErrNoYield).
func (b Bond) Yield(settle time.Time, dirty float64) (float64, error) {
	if err := b.check(); err != nil {
		return 0, err
	}
	ps := b.payments(settle)
	if len(ps) == 0 {
		return 0, ErrMatured
	}
	if !(dirty > 0) {
		return 0, ErrNonPositivePrice
	}
	lo, hi := float64(minYield), float64(maxYield)
	// The price falls as the yield rises: the bracket holds the yield
	// when its ends price on either side of dirty.
	if value(ps, lo, b.Frequency).pv < dirty || value(ps, hi, b.Frequency).pv > dirty {
		return 0, ErrNoYield
	}
	y := min(max(b.Coupon, lo), hi)
	for range maxIterations {
		v := value(ps, y, b.Frequency)
		diff := v.pv - dirty
		if diff == 0 {
			return y, nil
		}
		if diff > 0 {
			lo = y
		} else {
			hi = y
		}
		// v.d1 is the price's fall per unit of yield as a decimal: per
		// percent it is a hundredth of that.
		next := y + diff*100/v.d1
		if !(next > lo && next < hi) {
			next = (lo + hi) / 2
		}
		if math.Abs(next-y) < 1e-12 {
			return next, nil
		}
		y = next
	}
	return 0, ErrNoYield
}

// Risk is the bond's duration, convexity and DV01 at a yield in percent,
// on the payments and periods Price discounts. Each payment's time in
// years is (k−1+w)/f. A bond on or after maturity has no risk: every
// field is 0.
func (b Bond) Risk(settle time.Time, yield float64) Risk {
	if b.check() != nil {
		nan := math.NaN()
		return Risk{nan, nan, nan, nan}
	}
	return value(b.payments(settle), yield, b.Frequency).risk()
}

// EffectiveAnnual is the effective annual rate of a yield y compounded f
// times a year, both in percent. A frequency of 0 is simple interest:
// the yield is its own annual rate.
func EffectiveAnnual(y float64, f int) float64 {
	if f <= 0 {
		return y
	}
	n := float64(f)
	return (math.Pow(1+y/100/n, n) - 1) * 100
}

// check refuses terms the arithmetic does not take.
func (b Bond) check() error {
	switch {
	case !slices.Contains(canonical.BondFrequencies(), b.Frequency):
		return fmt.Errorf("fixedincome: coupon frequency %d: want %s", b.Frequency,
			orList(canonical.BondFrequencies()))
	case !b.DayCount.Valid():
		names := make([]string, 0, len(canonical.DayCounts()))
		for _, dc := range canonical.DayCounts() {
			names = append(names, string(dc))
		}
		return fmt.Errorf("fixedincome: day count %q: want one of %s", b.DayCount, strings.Join(names, ", "))
	case !(b.Coupon >= 0) || math.IsInf(b.Coupon, 0):
		return fmt.Errorf("fixedincome: coupon %v: want a finite percentage, zero or more", b.Coupon)
	case !(b.Redemption >= 0) || math.IsInf(b.Redemption, 0):
		return fmt.Errorf("fixedincome: redemption %v: want a finite amount per 100 face, zero or more", b.Redemption)
	}
	return nil
}

// orList lists ns as "1, 2, 4 or 12".
func orList(ns []int) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	if len(out) < 2 {
		return strings.Join(out, "")
	}
	return strings.Join(out[:len(out)-1], ", ") + " or " + out[len(out)-1]
}

// period is the bond's schedule and the index k of the period settle
// falls in: the coupons left to pay. k is 0 or less on or after
// maturity.
func (b Bond) period(settle time.Time) (s schedule, k int) {
	s = newSchedule(b.Maturity, b.Frequency)
	return s, s.index(settle)
}

// legs are the payments after settle and the coupon periods to each,
// k−1+w as Price reads them; none on or after maturity.
func (b Bond) legs(settle time.Time) ([]Flow, []float64) {
	s, k := b.period(settle)
	if k <= 0 {
		return nil, nil
	}
	prev, next := s.date(k), s.date(k-1)
	// What is left of the period is the whole less what has accrued
	// (DSC = E − A, as SIA and Excel's PRICE count it): a 30/360 count
	// from settle to the next date need not add up with the one before.
	w := 1 - yearFraction(b.DayCount, prev, settle, prev, next, b.Frequency, s.eom)/
		yearFraction(b.DayCount, prev, next, prev, next, b.Frequency, s.eom)
	first := 0
	if b.Coupon == 0 {
		first = k - 1
	}
	flows := make([]Flow, 0, k-first)
	periods := make([]float64, 0, k-first)
	for i := first; i < k; i++ {
		flows = append(flows, Flow{Date: s.date(k - 1 - i), Coupon: b.Coupon / float64(b.Frequency)})
		periods = append(periods, float64(i)+w)
	}
	flows[len(flows)-1].Principal = b.redemption()
	return flows, periods
}

// payments are the legs as amounts and their times in years.
func (b Bond) payments(settle time.Time) []payment {
	flows, periods := b.legs(settle)
	ps := make([]payment, len(flows))
	for i, f := range flows {
		ps[i] = payment{f.Coupon + f.Principal, periods[i] / float64(b.Frequency)}
	}
	return ps
}

// redemption is the amount paid at maturity per 100 face.
func (b Bond) redemption() float64 {
	if b.Redemption == 0 {
		return 100
	}
	return b.Redemption
}

// schedule is the coupon dates of a bond that matures on end: end rolled
// back by whole periods of a number of months. An end on the last day of
// a month keeps every date on a month end. Each date rolls from end
// itself, so a day a short month cuts returns in the next long one.
type schedule struct {
	end    time.Time
	months int
	eom    bool
}

// newSchedule is the schedule of a maturity and a coupon frequency of
// 1, 2, 4 or 12.
func newSchedule(end time.Time, frequency int) schedule {
	return schedule{end: end, months: 12 / frequency, eom: lastOfMonth(end)}
}

// date is the coupon date k periods before the end: date(0) is the end,
// and a negative k rolls past it.
func (s schedule) date(k int) time.Time {
	return addMonths(s.end, -k*s.months, s.eom)
}

// index is the k of the period t falls in: date(k) <= t < date(k-1).
func (s schedule) index(t time.Time) int {
	gap := 12*(s.end.Year()-t.Year()) + int(s.end.Month()) - int(t.Month())
	k := gap / s.months
	for days(s.date(k), t) < 0 {
		k++
	}
	for days(s.date(k-1), t) >= 0 {
		k--
	}
	return k
}

// periods is the coupon periods from a to b: the whole periods between
// their two periods, and each date's part of its own in actual days.
func (s schedule) periods(a, b time.Time) float64 {
	ka, kb := s.index(a), s.index(b)
	return float64(ka-kb) + s.part(b, kb) - s.part(a, ka)
}

// part is how far t lies into period k, as a share of its actual days.
func (s schedule) part(t time.Time, k int) float64 {
	prev, next := s.date(k), s.date(k-1)
	return float64(days(prev, t)) / float64(days(prev, next))
}

// payment is an amount paid after settlement and its time in years.
type payment struct {
	amount, years float64
}

// valuation is a set of payments at a yield: their present value, the
// sum of their times weighted by value, and the value's first and second
// derivatives in the yield as a decimal. d1 is the first negated, so it
// is positive: the value falls as the yield rises.
type valuation struct {
	pv, pvTime, d1, d2 float64
}

// value is the valuation of payments at a yield in percent, compounded f
// times a year.
func value(ps []payment, yield float64, f int) valuation {
	var v valuation
	for _, p := range ps {
		pv, d1, d2 := discounted(p.amount, p.years, yield, f)
		v.pv += pv
		v.pvTime += pv * p.years
		v.d1 += d1
		v.d2 += d2
	}
	return v
}

// risk is the Risk of a valuation: zero when nothing is left to pay.
func (v valuation) risk() Risk {
	if v.pv == 0 {
		return Risk{}
	}
	return Risk{
		Macaulay:  v.pvTime / v.pv,
		Modified:  v.d1 / v.pv,
		Convexity: v.d2 / v.pv,
		DV01:      v.d1 * BasisPoint,
	}
}

// OnePayment is the Risk of a single payment in t years, at a yield in
// percent compounded f times a year; a frequency of 0 is simple
// interest. Its DV01 is per 100 of value: the holding is taken at par.
func OnePayment(t, yield float64, f int) Risk {
	r := value([]payment{{1, t}}, yield, f).risk()
	r.DV01 = 100 * r.Modified * BasisPoint
	return r
}

// discounted is the present value of an amount paid in t years at a
// yield in percent compounded f times a year, and its d1 and d2 as
// valuation sums them. A frequency of 0 is simple interest.
func discounted(amount, t, yield float64, f int) (pv, d1, d2 float64) {
	if f <= 0 {
		g := 1 + yield/100*t
		pv = amount / g
		return pv, pv * t / g, 2 * pv * t * t / (g * g)
	}
	n := float64(f)
	g := 1 + yield/100/n
	pv = amount * math.Pow(g, -t*n)
	return pv, pv * t / g, pv * t * (t + 1/n) / (g * g)
}
