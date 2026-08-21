package returns

import (
	"math"
	"time"
)

// minAnnualizeDays is the GIPS convention boundary: never annualize a span
// shorter than one year (it would extrapolate noise).
const minAnnualizeDays = 365.0

// BucketKind enumerates the period granularities.
type BucketKind int

const (
	BucketMonthly BucketKind = iota
	BucketQuarterly
	BucketAnnual
	BucketTotal // a single bucket spanning the whole window
)

// Bucket is one sub-period's Modified-Dietz result. OK=false marks a degenerate
// bucket (see ModifiedDietz) that breaks the geometric chain.
type Bucket struct {
	R  float64
	OK bool
}

// Chain links per-bucket returns geometrically into a cumulative return:
// Π(1+R)-1. ok is false (cumulative undefined) if any bucket is degenerate — a
// poisoned factor must never be multiplied into the headline — or if there are
// no buckets.
func Chain(buckets []Bucket) (cum float64, ok bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	prod := 1.0
	for _, b := range buckets {
		if !b.OK {
			return 0, false
		}
		prod *= 1 + b.R
	}
	return prod - 1, true
}

// Annualize converts a cumulative return over `days` to an annual rate:
// (1+cum)^(365.25/days)-1.
func Annualize(cum, days float64) float64 {
	if days <= 0 {
		return cum
	}
	return math.Pow(1+cum, xirrDayBasis/days) - 1
}

// DeAnnualize is the inverse of Annualize: it converts an annual rate to the
// equivalent cumulative return over `days`. Used to render a money-weighted
// (XIRR, natively annual) figure as a period figure consistent with the
// cumulative TWR column.
func DeAnnualize(annual, days float64) float64 {
	if days <= 0 {
		return annual
	}
	return math.Pow(1+annual, days/xirrDayBasis) - 1
}

// ShouldAnnualize implements the --annualize policy: auto annualizes only spans
// of at least a year; always/never override.
func ShouldAnnualize(mode string, days float64) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	default: // "auto"
		return days >= minAnnualizeDays
	}
}

// BucketBoundaries splits [fromDay, toDay] into consecutive [start, end] pairs
// aligned to the requested calendar granularity. Consecutive buckets share a
// boundary day (bucket i's end == bucket i+1's start), so boundary values are
// looked up once per boundary. fromDay and toDay are always exact endpoints.
func BucketBoundaries(fromDay, toDay int64, kind BucketKind) [][2]int64 {
	if toDay <= fromDay {
		return [][2]int64{{fromDay, toDay}}
	}
	bounds := boundaryDays(fromDay, toDay, kind)
	out := make([][2]int64, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		out = append(out, [2]int64{bounds[i], bounds[i+1]})
	}
	return out
}

func boundaryDays(fromDay, toDay int64, kind BucketKind) []int64 {
	if kind == BucketTotal {
		return []int64{fromDay, toDay}
	}
	out := []int64{fromDay}
	for d := fromDay; d < toDay; d++ {
		if isPeriodEnd(d, kind) && d != fromDay {
			out = append(out, d)
		}
	}
	if out[len(out)-1] != toDay {
		out = append(out, toDay)
	}
	return out
}

// isPeriodEnd reports whether `day` is the last UTC day of its month / quarter /
// year for the given calendar granularity (i.e. the next day rolls into a new
// period).
func isPeriodEnd(day int64, kind BucketKind) bool {
	t := dayToTime(day)
	n := dayToTime(day + 1)
	switch kind {
	case BucketMonthly:
		return t.Month() != n.Month()
	case BucketQuarterly:
		return quarterOf(t) != quarterOf(n)
	case BucketAnnual:
		return t.Year() != n.Year()
	}
	return false
}

func quarterOf(t time.Time) int {
	return t.Year()*4 + (int(t.Month())-1)/3
}

func dayToTime(day int64) time.Time {
	return time.Unix(day*86400, 0).UTC()
}

// DayToTimeUTC converts an epoch day to its UTC midnight time. Exported for
// callers that label reporting periods.
func DayToTimeUTC(day int64) time.Time { return dayToTime(day) }
