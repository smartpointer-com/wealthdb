package gold

import "testing"

// TestDailyRatesReadAtOrBeforeTheDay pins the lookup: the rate at or
// before the day, the exact day included, with the day it was quoted;
// none before the pair's first day or for a pair with no rates.
func TestDailyRatesReadAtOrBeforeTheDay(t *testing.T) {
	r := &DailyRates{pairs: map[[2]string][]dayRate{{"USD", "CHF"}: {{10, 0.9}, {20, 0.8}}}}
	for _, c := range []struct {
		from, to string
		day      int64
		want     float64
		at       int64
		ok       bool
	}{
		{"USD", "CHF", 10, 0.9, 10, true},
		{"USD", "CHF", 15, 0.9, 10, true},
		{"USD", "CHF", 20, 0.8, 20, true},
		{"USD", "CHF", 99, 0.8, 20, true},
		{"USD", "CHF", 9, 0, 0, false},
		{"EUR", "CHF", 15, 0, 0, false},
	} {
		got, at, ok := r.AtOrBefore(c.from, c.to, c.day)
		if got != c.want || at != c.at || ok != c.ok {
			t.Errorf("AtOrBefore(%s, %s, %d) = %v, %d, %v; want %v, %d, %v",
				c.from, c.to, c.day, got, at, ok, c.want, c.at, c.ok)
		}
		if x, ok := r.Direct(c.from, c.to, c.day); x != c.want || ok != c.ok {
			t.Errorf("Direct(%s, %s, %d) = %v, %v; want %v, %v", c.from, c.to, c.day, x, ok, c.want, c.ok)
		}
	}
}
