package yield

import (
	"math"
	"testing"
	"time"
)

func period(start, end time.Time, v float64) Period {
	return Period{Start: start, End: end, Value: v}
}

// The trailing twelve months of a reported figure: the four newest
// quarters; a quarter a year-to-date figure implies, the difference from
// the one before it (six months less the first quarter, nine less six, a
// year less nine); a fiscal year where the quarters are stale; a year
// earlier for growth. The figures are invented.
func TestTrailingTwelveMonths(t *testing.T) {
	t.Parallel()
	var quarters []Period
	for i := 0; i < 8; i++ {
		start := date(2024, 4, 1).AddDate(0, 3*i, 0)
		quarters = append(quarters, period(start, start.AddDate(0, 3, -1), float64(i+1)))
	}
	check := func(what string, ps []Period, years int, want float64, ok bool) {
		t.Helper()
		got, gotOK := Trailing(ps, day, years)
		if gotOK != ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: %v %v, want %v %v", what, got, gotOK, want, ok)
		}
	}
	check("quarters", quarters, 0, 5+6+7+8, true)
	check("a year earlier", quarters, 1, 1+2+3+4, true)
	ytd := []Period{
		period(*date(2024, 7, 1), *date(2025, 3, 31), 5), // nine months
		period(*date(2024, 7, 1), *date(2025, 6, 30), 8), // the fiscal year
		period(*date(2025, 7, 1), *date(2025, 9, 30), 1), // the first quarter
		period(*date(2025, 7, 1), *date(2025, 12, 31), 3),
		period(*date(2025, 7, 1), *date(2026, 3, 31), 6),
	}
	check("year to date", ytd, 0, 3+2+1+3, true)
	stale := []Period{period(*date(2025, 1, 1), *date(2025, 12, 31), 7), period(*date(2025, 10, 1), *date(2025, 12, 31), 2)}
	check("a fiscal year", stale, 0, 7, true)
	check("nothing", nil, 0, 0, false)
}
