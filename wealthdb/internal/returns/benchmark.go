package returns

import "math"

// BenchmarkMonth is a benchmark's return for one calendar month: End is
// the month's last day, R the return as a ratio. OK false is a month
// with no return, a gap.
type BenchmarkMonth struct {
	End int64
	R   float64
	OK  bool
}

// Index reads a run of consecutive benchmark months as growth between any
// two days. A value on a day inside a month has earned the share of the
// month's return its days so far make up, compounded per day. The first
// of a month has earned none of it, so a window from January 1 takes
// January whole, and a window of whole months compounds them exactly.
type Index struct {
	months []BenchmarkMonth
	start  int64     // the last day before the first month
	cum    []float64 // cum[i]: the level before months[i]; a gap adds nothing
}

// NewIndex is the index of months, ascending and consecutive, every month
// from the first to the last present, a gap with OK false. It has no
// months where nil.
func NewIndex(months []BenchmarkMonth) *Index {
	if len(months) == 0 {
		return nil
	}
	first := DayToTimeUTC(months[0].End)
	ix := &Index{months: months, start: months[0].End - int64(first.Day()), cum: make([]float64, len(months)+1)}
	ix.cum[0] = 1
	for i, m := range months {
		ix.cum[i+1] = ix.cum[i]
		if m.OK {
			ix.cum[i+1] *= 1 + m.R
		}
	}
	return ix
}

// Start is the last day before the first month: the earliest day a
// window can start on.
func (ix *Index) Start() int64 { return ix.start }

// End is the last month's last day.
func (ix *Index) End() int64 { return ix.months[len(ix.months)-1].End }

// locate is the month `day` falls in and the share of it done by then.
// The day before the first month is its start. day is in [Start, End].
func (ix *Index) locate(day int64) (i int, share float64) {
	if day == ix.start {
		return 0, 0
	}
	prev := ix.start
	for i, m := range ix.months {
		if day <= m.End {
			if day == m.End {
				return i, 1
			}
			if done := day - prev; done > 1 {
				return i, float64(done) / float64(m.End-prev)
			}
			return i, 0
		}
		prev = m.End
	}
	return len(ix.months) - 1, 1
}

func (ix *Index) level(i int, share float64) float64 {
	if !ix.months[i].OK {
		return ix.cum[i]
	}
	return ix.cum[i] * math.Pow(1+ix.months[i].R, share)
}

// Growth is the factor a value grows by from the end of day `from` to the
// end of day `to`. ok is false where the span leaves the index, runs
// through a gap, or starts from a level of zero.
func (ix *Index) Growth(from, to int64) (factor float64, ok bool) {
	if ix == nil || from > to || from < ix.start || to > ix.End() {
		return 0, false
	}
	i, si := ix.locate(from)
	j, sj := ix.locate(to)
	first, last := i, j
	if si == 1 {
		first++
	}
	if sj == 0 {
		last--
	}
	for k := first; k <= last; k++ {
		if !ix.months[k].OK {
			return 0, false
		}
	}
	base := ix.level(i, si)
	if base <= 0 {
		return 0, false
	}
	return ix.level(j, sj) / base, true
}

// ShadowValues is what v0 on day `from` and each of the flows would be
// worth on each of the days `ends`, ascending and after `from`, had they
// been invested in the index: same money, same timing, another
// investment. Flows are sorted by day, each enters at the end of its
// day, and only those after `from` count. Each value holds where ok; a
// day past a gap has none. short says the flows have taken out all the
// shadow held by that day: past it the shadow is a short position, which
// no household holds, so a return on it means nothing.
func (ix *Index) ShadowValues(v0 float64, from int64, flows []Flow, ends []int64) (values []float64, ok, short []bool) {
	values, ok, short = make([]float64, len(ends)), make([]bool, len(ends)), make([]bool, len(ends))
	value, at, next, spent := v0, from, 0, false
	for next < len(flows) && flows[next].Day <= from {
		next++
	}
	advance := func(day int64) bool {
		g, fine := ix.Growth(at, day)
		value, at = value*g, day
		return fine
	}
	for e, end := range ends {
		for next < len(flows) && flows[next].Day <= end {
			if !advance(flows[next].Day) {
				return values, ok, short
			}
			value += flows[next].Amount
			spent = spent || value <= 0
			next++
		}
		if !advance(end) {
			return values, ok, short
		}
		values[e], ok[e], short[e] = value, true, spent
	}
	return values, ok, short
}
