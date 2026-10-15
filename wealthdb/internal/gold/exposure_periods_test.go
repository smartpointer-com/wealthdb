package gold

import (
	"slices"
	"testing"
	"time"
)

func TestPeriodEnds(t *testing.T) {
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() }
	end := func(y int, m time.Month, d int) int64 { return day(y, m, d) + 86399 }
	for _, c := range []struct {
		name     string
		from, to int64
		months   int
		want     []int64
	}{
		{"monthly, the last clipped", day(2026, 1, 15), end(2026, 3, 10), 1,
			[]int64{end(2026, 1, 31), end(2026, 2, 28), end(2026, 3, 10)}},
		{"monthly, ending on a month end", day(2026, 6, 1), end(2026, 6, 30), 1, []int64{end(2026, 6, 30)}},
		{"quarterly from inside a quarter", day(2026, 2, 10), end(2026, 8, 15), 3,
			[]int64{end(2026, 3, 31), end(2026, 6, 30), end(2026, 8, 15)}},
		{"quarterly from a quarter's last month", day(2025, 12, 31), end(2026, 1, 5), 3,
			[]int64{end(2025, 12, 31), end(2026, 1, 5)}},
		{"annual", day(2025, 3, 1), end(2026, 8, 15), 12, []int64{end(2025, 12, 31), end(2026, 8, 15)}},
		{"one instant", day(2026, 4, 1), day(2026, 4, 1), 3, []int64{day(2026, 4, 1)}},
		{"from after to", day(2026, 8, 20), end(2026, 8, 15), 1, nil},
	} {
		if got := PeriodEnds(c.from, c.to, c.months); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
