package fixedincome

import (
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func sum(m map[string]float64) float64 {
	s := 0.0
	for _, v := range m {
		s += v
	}
	return s
}

// One sensitivity splits linearly between the two tenors around its
// time: 7.5 years is half 5Y and half 10Y. Before 3M all of it is 3M,
// and past 30Y all of it is 30Y.
func TestBucketAt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		years float64
		want  map[string]float64
	}{
		{7.5, map[string]float64{"5Y": 0.5, "10Y": 0.5}},
		{0.625, map[string]float64{"3M": 0.5, "1Y": 0.5}},
		{2, map[string]float64{"1Y": 0.75, "5Y": 0.25}},
		{20, map[string]float64{"10Y": 0.5, "30Y": 0.5}},
		{1, map[string]float64{"1Y": 1}},
		{0.1, map[string]float64{"3M": 1}},
		{0, map[string]float64{"3M": 1}},
		{-1, map[string]float64{"3M": 1}},
		{30, map[string]float64{"30Y": 1}},
		{45, map[string]float64{"30Y": 1}},
	} {
		got := BucketAt(c.years, 2)
		if len(got) != len(canonical.RiskTenors) {
			t.Errorf("%v years: %d buckets, want one per tenor", c.years, len(got))
		}
		for _, tenor := range canonical.RiskTenors {
			near(t, tenor, got[tenor], 2*c.want[tenor], 1e-12)
		}
	}
}

// The buckets of a bond's flows sum to its DV01. A bond on a coupon date,
// and an ACT/ACT bond on any date, read the same payment times as Price.
// A 30/360 bond between coupon dates counts the part of its period in
// 30-day months instead, and its sum differs by a few parts in 10⁵.
func TestBucketsSumToDV01(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		dc     canonical.DayCount
		settle time.Time
		tol    float64
	}{
		{canonical.DayCountActAct, ymd(2026, 10, 21), 1e-12},
		{canonical.DayCountAct360, ymd(2026, 10, 21), 1e-12},
		{canonical.DayCount30360, ymd(2026, 8, 15), 1e-12},
		{canonical.DayCount30360, ymd(2026, 10, 21), 1e-4},
	} {
		for _, f := range frequencies {
			b := Bond{Coupon: 4.25, Frequency: f, Maturity: ymd(2056, 8, 15), DayCount: c.dc}
			r := b.Risk(c.settle, 4.6)
			got := Buckets(b.Flows(c.settle), c.settle, 4.6, f)
			near(t, string(c.dc)+" sum", sum(got), r.DV01, c.tol*r.DV01)
			for _, tenor := range canonical.RiskTenors {
				if got[tenor] < 0 {
					t.Errorf("%s f%d: bucket %s = %v, want none below 0", c.dc, f, tenor, got[tenor])
				}
			}
		}
	}
}

// A zero 7.5 years out on a quasi-coupon date puts half its DV01 on 5Y
// and half on 10Y.
func TestBucketsOfAZero(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 5, 15)
	b := Bond{Frequency: 2, Maturity: ymd(2033, 11, 15), DayCount: canonical.DayCountActAct}
	r := b.Risk(settle, 3)
	got := Buckets(b.Flows(settle), settle, 3, 2)
	near(t, "5Y", got["5Y"], r.DV01/2, 1e-12)
	near(t, "10Y", got["10Y"], r.DV01/2, 1e-12)
	for _, tenor := range []string{"3M", "1Y", "30Y"} {
		near(t, tenor, got[tenor], 0, 0)
	}
}

// Payments need not come sorted, payments on or before settlement count
// nothing, and a frequency of 0 discounts at simple interest.
func TestBucketsInputs(t *testing.T) {
	t.Parallel()
	settle := ymd(2026, 5, 15)
	b := Bond{Coupon: 5, Frequency: 2, Maturity: ymd(2031, 5, 15), DayCount: canonical.DayCountActAct}
	flows := b.Flows(settle)
	want := Buckets(flows, settle, 4, 2)
	mixed := slices.Clone(flows)
	slices.Reverse(mixed)
	mixed = append(mixed, Flow{Date: settle, Coupon: 2.5}, Flow{Date: ymd(2025, 11, 15), Coupon: 2.5})
	got := Buckets(mixed, settle, 4, 2)
	for _, tenor := range canonical.RiskTenors {
		near(t, tenor, got[tenor], want[tenor], 1e-12)
	}
	// One payment of 100 in 73 days at 5% simple: worth 100/1.01, and its
	// DV01 is that value times 0.2 years over 1.01, times a basis point.
	one := []Flow{{Date: settle.AddDate(0, 0, 73), Principal: 100}}
	simple := Buckets(one, settle, 5, 0)
	near(t, "simple", simple["3M"], 100/1.01*0.2/1.01*0.0001, 1e-15)
	if got := Buckets(flows, settle, 4, 3); got != nil {
		t.Errorf("frequency 3: %v, want nil", got)
	}
	if got := Buckets(nil, settle, 4, 2); len(got) != len(canonical.RiskTenors) || sum(got) != 0 {
		t.Errorf("no flows: %v, want every tenor at 0", got)
	}
}
