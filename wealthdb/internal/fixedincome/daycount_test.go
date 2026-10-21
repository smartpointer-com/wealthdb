package fixedincome

import (
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// ymd is a date at UTC midnight, as gold's DATE columns read.
func ymd(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// The textbook cases of the two 30/360 rules, the US one for a bond on
// the end-of-month rule. They part on a 31st after a day below the 30th,
// and on the last day of February.
func TestDays360(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		a, b   time.Time
		us, eu int
	}{
		{"Jan 31 to Mar 1, common year", ymd(2023, 1, 31), ymd(2023, 3, 1), 31, 31},
		{"Jan 31 to Mar 1, leap year", ymd(2024, 1, 31), ymd(2024, 3, 1), 31, 31},
		{"Feb 28 to Mar 31, common year", ymd(2023, 2, 28), ymd(2023, 3, 31), 30, 32},
		{"Feb 28 to Mar 31, leap year", ymd(2024, 2, 28), ymd(2024, 3, 31), 33, 32},
		{"Feb 29 to Mar 31", ymd(2024, 2, 29), ymd(2024, 3, 31), 30, 31},
		{"Aug 31 to Feb 28", ymd(2022, 8, 31), ymd(2023, 2, 28), 178, 178},
		{"Aug 31 to Feb 29", ymd(2023, 8, 31), ymd(2024, 2, 29), 179, 179},
		{"Feb 28 to Aug 31", ymd(2023, 2, 28), ymd(2023, 8, 31), 180, 182},
		{"last of Feb to last of Feb", ymd(2023, 2, 28), ymd(2024, 2, 29), 360, 361},
		{"Mar 31 to Sep 30", ymd(2024, 3, 31), ymd(2024, 9, 30), 180, 180},
		{"Mar 15 to Jul 31", ymd(2024, 3, 15), ymd(2024, 7, 31), 136, 135},
		{"Jan 30 to Mar 31", ymd(2024, 1, 30), ymd(2024, 3, 31), 60, 60},
		{"a half year", ymd(2024, 3, 15), ymd(2024, 9, 15), 180, 180},
		{"backwards", ymd(2024, 9, 15), ymd(2024, 3, 15), -180, -180},
	} {
		if got := days30360(c.a, c.b, true); got != c.us {
			t.Errorf("%s: 30/360 = %d, want %d", c.name, got, c.us)
		}
		if got := days30E360(c.a, c.b); got != c.eu {
			t.Errorf("%s: 30E/360 = %d, want %d", c.name, got, c.eu)
		}
	}
}

// A whole coupon period of each count, and part of one.
func TestYearFraction(t *testing.T) {
	t.Parallel()
	// A half year of 184 actual days, and a year of 365.
	from, half, year := ymd(2024, 3, 15), ymd(2024, 9, 15), ymd(2025, 3, 15)
	for _, c := range []struct {
		dc       canonical.DayCount
		to, end  time.Time
		freq     int
		want     float64
		describe string
	}{
		{canonical.DayCount30360, half, half, 2, 0.5, "30/360 half year"},
		{canonical.DayCount30E360, half, half, 2, 0.5, "30E/360 half year"},
		{canonical.DayCountActAct, half, half, 2, 0.5, "ACT/ACT half year"},
		{canonical.DayCountAct360, half, half, 2, 184.0 / 360, "ACT/360 half year"},
		{canonical.DayCountAct365, half, half, 2, 184.0 / 365, "ACT/365 half year"},
		{canonical.DayCountActAct, year, year, 1, 1, "ACT/ACT year"},
		{canonical.DayCount30360, year, year, 1, 1, "30/360 year"},
		{canonical.DayCountAct365, year, year, 1, 1, "ACT/365 year"},
		// ACT/ACT reads part of a period against the whole: 60 days of
		// a 184-day half year.
		{canonical.DayCountActAct, ymd(2024, 5, 14), half, 2, 60.0 / 368, "ACT/ACT part"},
		// Without a frequency or a period, ACT/ACT reads actual/365.
		{canonical.DayCountActAct, half, half, 0, 184.0 / 365, "ACT/ACT no frequency"},
		{canonical.DayCountActAct, half, from, 2, 184.0 / 365, "ACT/ACT no period"},
	} {
		got := yearFraction(c.dc, from, c.to, from, c.end, c.freq, true)
		near(t, c.describe, got, c.want, 1e-15)
	}
	if got := yearFraction("ACT/999", from, half, from, half, 2, true); !math.IsNaN(got) {
		t.Errorf("an unknown day count = %v, want NaN", got)
	}
}

// A date counts by its calendar day: a time of day moves nothing.
func TestDaysByCalendarDate(t *testing.T) {
	t.Parallel()
	a := time.Date(2024, 3, 15, 23, 30, 0, 0, time.UTC)
	b := time.Date(2024, 3, 16, 0, 15, 0, 0, time.UTC)
	if got := days(a, b); got != 1 {
		t.Errorf("days = %d, want 1", got)
	}
	if got := days(ymd(1960, 1, 1), ymd(1961, 1, 1)); got != 366 {
		t.Errorf("days in 1960 = %d, want 366", got)
	}
}

// A month added keeps the day, or takes the month's last when the month
// is shorter or the end-of-month rule holds.
func TestAddMonths(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		from time.Time
		n    int
		eom  bool
		want time.Time
	}{
		{ymd(2024, 8, 31), -6, false, ymd(2024, 2, 29)},
		{ymd(2025, 8, 31), -6, false, ymd(2025, 2, 28)},
		{ymd(2025, 2, 28), 6, true, ymd(2025, 8, 31)},
		{ymd(2025, 2, 28), 6, false, ymd(2025, 8, 28)},
		{ymd(2025, 1, 15), -13, false, ymd(2023, 12, 15)},
		{ymd(2025, 11, 30), 3, true, ymd(2026, 2, 28)},
	} {
		if got := addMonths(c.from, c.n, c.eom); !got.Equal(c.want) {
			t.Errorf("%s %+d months (eom %v) = %s, want %s", c.from.Format(time.DateOnly), c.n, c.eom,
				got.Format(time.DateOnly), c.want.Format(time.DateOnly))
		}
	}
}
