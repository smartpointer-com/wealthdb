package silver

import (
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestInKindReducesThePaidInBookValueFromThePeriodEnd(t *testing.T) {
	dec := func(n int64) canonical.Decimal { return canonical.NewDecimalFromInt(n) }
	paidIn := canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
	reduced := paidIn
	reduced.Origin = canonical.BasisDerived

	var k InKind
	k.Add("a", 200, dec(400))
	k.Add("a", 100, dec(100))
	k.Add("a", 100, dec(0)) // states none
	k.Add("b", 100, dec(3000))
	if got, want := k.PeriodEnds(), []int64{100, 200}; !reflect.DeepEqual(got, want) {
		t.Errorf("PeriodEnds = %v, want %v", got, want)
	}

	pi := dec(1000)
	for _, c := range []struct {
		key   string
		t     int64
		book  string
		stamp canonical.Basis
		extra map[string]any
	}{
		{"a", 99, "1000", paidIn, nil},
		{"a", 100, "900", reduced, map[string]any{"paid_in": "1000", "property_distributed": "100"}},
		{"a", 200, "500", reduced, map[string]any{"paid_in": "1000", "property_distributed": "500"}},
		{"b", 100, "0", reduced, map[string]any{"paid_in": "1000", "property_distributed": "3000"}},
		{"c", 300, "1000", paidIn, nil},
	} {
		v, b, extra := k.BookValue(c.key, c.t, &pi, paidIn)
		if v == nil || v.String() != c.book || b != c.stamp || !reflect.DeepEqual(extra, c.extra) {
			t.Errorf("%s at %d: %v %+v %v, want %s %+v %v", c.key, c.t, v, b, extra, c.book, c.stamp, c.extra)
		}
	}
	if len(k.clamped) != 1 || !k.clamped["b"] {
		t.Errorf("clamped = %v, want b alone", k.clamped)
	}
	if v, b, extra := k.BookValue("a", 300, nil, paidIn); v != nil || !b.IsZero() || extra != nil {
		t.Errorf("no capital paid in: %v %+v %v, want no book value", v, b, extra)
	}

	var empty InKind
	if v, b, _ := empty.BookValue("a", 300, &pi, paidIn); v == nil || !v.Equal(pi) || b != paidIn {
		t.Errorf("zero value: %v %+v, want the paid-in figure", v, b)
	}
	if got := empty.PeriodEnds(); len(got) != 0 {
		t.Errorf("zero value PeriodEnds = %v, want none", got)
	}
}
