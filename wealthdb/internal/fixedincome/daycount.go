package fixedincome

import (
	"math"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// YearFraction is the fraction of a year from from to to under the day
// count dc:
//
//   - 30/360 counts Days30360 over 360, the US bond basis.
//   - 30E/360 counts Days30E360 over 360, the European rule.
//   - ACT/360 and ACT/365 count actual days over 360 or 365.
//   - ACT/ACT is the ICMA rule: actual days over the actual days of the
//     coupon period periodStart to periodEnd, times frequency. A
//     frequency of 0 or an empty period reads actual/365.
//
// The period and the frequency matter to ACT/ACT only, whose from and to
// lie within the period. An unknown day count is NaN. 30/360 applies its
// February rules, as for a bond on the end-of-month rule.
func YearFraction(dc canonical.DayCount, from, to, periodStart, periodEnd time.Time, frequency int) float64 {
	return yearFraction(dc, from, to, periodStart, periodEnd, frequency, true)
}

// yearFraction is YearFraction, 30/360's February rules applied only
// where eom is set: they belong to a bond whose coupons fall on month
// ends (SIA). A bond paying on the 28th counts February as any month.
func yearFraction(dc canonical.DayCount, from, to, periodStart, periodEnd time.Time, frequency int, eom bool) float64 {
	switch dc {
	case canonical.DayCount30360:
		return float64(days30360(from, to, eom)) / 360
	case canonical.DayCount30E360:
		return float64(Days30E360(from, to)) / 360
	case canonical.DayCountAct360:
		return float64(days(from, to)) / 360
	case canonical.DayCountAct365:
		return float64(days(from, to)) / 365
	case canonical.DayCountActAct:
		period := days(periodStart, periodEnd)
		if frequency <= 0 || period <= 0 {
			return float64(days(from, to)) / 365
		}
		return float64(days(from, to)) / float64(period*frequency)
	}
	return math.NaN()
}

// Days30360 is the days from a to b under the US 30/360 rule (the bond
// basis) of an end-of-month bond, applied in order:
//
//   - Both dates the last day of February: b counts as the 30th.
//   - a the last day of February: a counts as the 30th.
//   - b the 31st and a the 30th or 31st: b counts as the 30th.
//   - a the 31st: a counts as the 30th.
func Days30360(a, b time.Time) int {
	return days30360(a, b, true)
}

// days30360 is Days30360, its two February rules applied only where eom
// is set.
func days30360(a, b time.Time, eom bool) int {
	d1, d2 := a.Day(), b.Day()
	if eom && lastOfFebruary(a) && lastOfFebruary(b) {
		d2 = 30
	}
	if eom && lastOfFebruary(a) {
		d1 = 30
	}
	if d2 == 31 && d1 >= 30 {
		d2 = 30
	}
	if d1 == 31 {
		d1 = 30
	}
	return days360(a, b, d1, d2)
}

// Days30E360 is the days from a to b under the European 30E/360 rule: a
// 31st of either date counts as the 30th.
func Days30E360(a, b time.Time) int {
	return days360(a, b, min(a.Day(), 30), min(b.Day(), 30))
}

// days360 is the days from a to b on a calendar of twelve 30-day months,
// with the days of the month d1 and d2 the rule gives them.
func days360(a, b time.Time, d1, d2 int) int {
	return 360*(b.Year()-a.Year()) + 30*(int(b.Month())-int(a.Month())) + d2 - d1
}

// days is the actual days from a to b, negative when b is before a. Each
// date counts by its calendar day: a time of day or a zone moves nothing.
func days(a, b time.Time) int {
	return int((civil(b) - civil(a)) / 86400)
}

// civil is the Unix time of t's calendar date at UTC midnight.
func civil(t time.Time) int64 {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

// lastOfMonth reports whether t is the last day of its month.
func lastOfMonth(t time.Time) bool {
	return t.AddDate(0, 0, 1).Day() == 1
}

// lastOfFebruary reports whether t is February 28 of a common year or
// February 29 of a leap year.
func lastOfFebruary(t time.Time) bool {
	return t.Month() == time.February && lastOfMonth(t)
}

// addMonths is t moved by n months. The day is t's, or the month's last
// day when the month is shorter or eom is set.
func addMonths(t time.Time, n int, eom bool) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	d := t.Day()
	if eom || d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}
