package canonical

import "testing"

// TestLotsAddUp pins the lot add-up test: the quantities within a
// millionth (signed, or as magnitudes), the costs within a cent per lot
// where all are stated, and an unstated quantity never adding up.
func TestLotsAddUp(t *testing.T) {
	d := func(f float64) *Decimal { v := NewDecimalFromFloat(f); return &v }
	type lot struct{ q, c *Decimal }
	figures := func(l lot) (*Decimal, *Decimal) { return l.q, l.c }
	short := []lot{{d(-4), d(400)}, {d(-6), d(600.01)}}
	for _, c := range []struct {
		name       string
		qty, cost  *Decimal
		lots       []lot
		magnitudes bool
		want       bool
	}{
		{"signed", d(-10), d(1000), short, false, true},
		{"signed against a magnitude", d(10), d(1000), short, false, false},
		{"magnitudes", d(10), d(1000), short, true, true},
		{"magnitudes of a short position", d(-10), d(1000), short, true, true},
		{"within a millionth", d(-10.0000001), nil, short, false, true},
		{"quantity off", d(-10.001), nil, short, false, false},
		{"cost off by more than a cent a lot", d(-10), d(1000.04), short, false, false},
		{"a lot's cost unstated", d(-10), d(5), []lot{{d(-10), nil}}, false, true},
		{"a lot's quantity unstated", d(-10), nil, []lot{{nil, nil}}, false, false},
		{"no quantity", nil, nil, short, false, false},
		{"no lots", d(1), nil, nil, false, false},
	} {
		if got := LotsAddUp(c.qty, c.cost, c.lots, figures, c.magnitudes); got != c.want {
			t.Errorf("%s: LotsAddUp = %v, want %v", c.name, got, c.want)
		}
	}
}
