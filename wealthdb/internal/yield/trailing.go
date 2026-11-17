package yield

import (
	"math"
	"slices"
	"time"
)

// Period is one reported figure for a period.
type Period struct {
	Start, End time.Time
	Value      float64
}

// staleDays bounds the age of the newest quarter a trailing figure may
// end on: a quarter and the 45 days a quarterly report may take.
const staleDays = 135

// Trailing is the trailing twelve months of a reported figure as of day,
// years back: the sum of the four quarters ending with the newest one
// (no older than staleDays), each quarter a three-month period, the
// difference of two year-to-date periods that start on one day, or a
// year less the nine months to its third quarter; else the newest
// fiscal year ending within a quarter of the newest quarter. ok is false
// where neither is known.
func Trailing(periods []Period, day time.Time, years int) (float64, bool) {
	at := day.AddDate(-years, 0, 0)
	monthsBetween := func(a, b time.Time) int { return int(math.Round(b.Sub(a).Hours() / 24 / 30.44)) }
	months := func(p Period) int { return monthsBetween(p.Start, p.End) }
	quarters := map[time.Time]float64{} // by end
	byStart := map[time.Time][]Period{}
	for _, p := range periods {
		if p.End.After(at) {
			continue
		}
		switch months(p) {
		case 3:
			quarters[p.End] = p.Value
		case 6, 9, 12:
			byStart[p.Start] = append(byStart[p.Start], p)
		}
	}
	// Year-to-date periods that start on one day differ by a quarter.
	for _, ps := range byStart {
		slices.SortFunc(ps, func(a, b Period) int { return a.End.Compare(b.End) })
		for i := range ps {
			var prevEnd time.Time
			var prevValue float64
			q, hasQ := quarters[ps[i].Start.AddDate(0, 3, -1)]
			switch {
			case i > 0:
				prevEnd, prevValue = ps[i-1].End, ps[i-1].Value
			case hasQ && months(ps[i]) == 6:
				prevEnd, prevValue = ps[i].Start.AddDate(0, 3, -1), q
			default:
				continue
			}
			if monthsBetween(prevEnd, ps[i].End) == 3 {
				if _, ok := quarters[ps[i].End]; !ok {
					quarters[ps[i].End] = ps[i].Value - prevValue
				}
			}
		}
	}
	ends := make([]time.Time, 0, len(quarters))
	for e := range quarters {
		ends = append(ends, e)
	}
	slices.SortFunc(ends, func(a, b time.Time) int { return b.Compare(a) })
	if len(ends) >= 4 && at.Sub(ends[0]) <= staleDays*24*time.Hour {
		sum := 0.0
		ok := true
		for i := 0; i < 4; i++ {
			if i > 0 && monthsBetween(ends[i], ends[i-1]) != 3 {
				ok = false
				break
			}
			sum += quarters[ends[i]]
		}
		if ok {
			return sum, true
		}
	}
	var year *Period
	for _, ps := range byStart {
		for i := range ps {
			p := ps[i]
			if months(p) == 12 && (year == nil || p.End.After(year.End)) {
				year = &p
			}
		}
	}
	if year != nil && at.Sub(year.End) <= (staleDays+92)*24*time.Hour {
		return year.Value, true
	}
	return 0, false
}
