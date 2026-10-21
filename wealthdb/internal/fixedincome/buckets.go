package fixedincome

import (
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Buckets splits the rate sensitivity of payments over the tenors of
// canonical.RiskTenors, keyed by tenor. Each payment's DV01 is its
// present value times its time over (1 + y/f), times 0.0001, at the
// yield in percent compounded frequency times a year. BucketAt splits it
// between the two tenors around its time.
//
// A payment's time counts in coupon periods of the schedule that ends on
// the last payment: whole periods, plus parts of a period in actual
// days. Price reads the same time for a bond on a coupon date, and for
// an ACT/ACT, ACT/360 or ACT/365 bond on any date. So the buckets of a
// bond's Flows sum to its Risk DV01. A 30/360 or 30E/360 bond between
// coupon dates counts the part of its period in 30-day months instead,
// and its sum differs by that gap's discounting.
//
// A frequency of 0 is simple interest, with the time in actual days over
// 365. Payments on or before settle count nothing. A frequency outside
// canonical.CouponFrequencies gives nil.
func Buckets(flows []Flow, settle time.Time, yield float64, frequency int) map[string]float64 {
	if !canonical.ValidCouponFrequency(frequency) {
		return nil
	}
	out, tenors := buckets()
	var end time.Time
	for _, f := range flows {
		if days(end, f.Date) > 0 {
			end = f.Date
		}
	}
	var s schedule
	if frequency > 0 {
		s = newSchedule(end, frequency)
	}
	for _, f := range flows {
		n := days(settle, f.Date)
		if n <= 0 {
			continue
		}
		t := float64(n) / 365
		if frequency > 0 {
			t = s.periods(settle, f.Date) / float64(frequency)
		}
		_, d1, _ := discounted(f.Coupon+f.Principal, t, yield, frequency)
		split(out, tenors, t, d1*BasisPoint)
	}
	return out
}

// BucketAt splits one sensitivity at a time in years over the tenors of
// canonical.RiskTenors (3M is a quarter of a year). It goes linearly to
// the two tenors around the time: all to the shortest before it, and all
// to the longest past it. It serves a holding whose sensitivity sits at
// one time: a floating note at its reset, a bill, a deposit with a term.
// Every tenor has a key.
func BucketAt(years, dv01 float64) map[string]float64 {
	out, tenors := buckets()
	split(out, tenors, years, dv01)
	return out
}

// buckets is a zero bucket for every tenor of canonical.RiskTenors, and
// the tenors' lengths in years, shortest first.
func buckets() (map[string]float64, []float64) {
	out := make(map[string]float64, len(canonical.RiskTenors))
	years := make([]float64, len(canonical.RiskTenors))
	for i, t := range canonical.RiskTenors {
		// The risk tenors are valid tenors: TenorMonths takes each.
		m, _ := canonical.TenorMonths(t)
		out[t] = 0
		years[i] = float64(m) / 12
	}
	return out, years
}

// split adds dv01 at a time in years to the buckets, as BucketAt
// splits it.
func split(out map[string]float64, tenors []float64, years, dv01 float64) {
	names := canonical.RiskTenors
	if years <= tenors[0] {
		out[names[0]] += dv01
		return
	}
	for i := 1; i < len(tenors); i++ {
		if a, b := tenors[i-1], tenors[i]; years <= b {
			w := (years - a) / (b - a)
			out[names[i-1]] += dv01 * (1 - w)
			out[names[i]] += dv01 * w
			return
		}
	}
	out[names[len(names)-1]] += dv01
}
