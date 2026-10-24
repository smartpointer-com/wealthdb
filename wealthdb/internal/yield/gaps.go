package yield

import (
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The gap an issuer keeps between a distribution's record day and its pay
// day, learned from the distributions the dividend calendar states of it
// (docs/YIELD.md §4): a payment the calendar dates no day of takes its
// record day from its pay day less that gap (RecordLearned).

// The bounds of a gap one distribution teaches, in calendar days from its
// record day to its pay day, and the fewest distributions a gap is learned
// from. A gap outside the bounds says nothing of the issuer's habit: a day
// misread, or a payment held back for months.
const (
	gapMaxDays   = 90
	gapMinEvents = 2
)

// learnedSlack is how many business days a learned gap's record window
// reaches past its longest gap, as the lags' windows do (RecordLag): a
// payment booked a day or two after its pay day then still finds its
// record day inside the window.
const learnedSlack = 2

// LearnGap is the gap the distributions an issuer's calendar states show,
// in business days of market from each one's record day to its pay day:
// a business-day Lag whose Days is their median (the lower of the middle
// two), Near the shortest and Far the longest, and how many distributions
// it rests on. A distribution's record day is the one it states, else the
// one its ex day derives by the market's cycle on it
// (SettlementCycles.RecordDay). Where the market has no cycle then, it
// counts from its ex day where it states one, the day its quantity reads
// by (RecordOf), else from its record day. One the calendar estimates,
// one with no pay day or neither day, one outside 0 to gapMaxDays days,
// and one whose two days another one states again teach nothing. ok is
// false where fewer than gapMinEvents teach a gap.
func LearnGap(events []CalendarEvent, market string, cycles SettlementCycles) (gap Lag, n int, ok bool) {
	type days struct{ record, pay time.Time }
	seen := map[days]bool{}
	var gaps []int
	for _, e := range events {
		if e.Estimated || e.Pay == nil {
			continue
		}
		var record time.Time
		switch {
		case e.Ex != nil && cycles.On(market, *e.Ex) == 0:
			record = *e.Ex
		case e.Record != nil:
			record = *e.Record
		case e.Ex != nil:
			record, _ = cycles.RecordDay(market, *e.Ex)
		default:
			continue
		}
		if span := e.Pay.Sub(record); span < 0 || span > gapMaxDays*24*time.Hour || seen[days{record, *e.Pay}] {
			continue
		}
		seen[days{record, *e.Pay}] = true
		gaps = append(gaps, businessGap(market, record, *e.Pay))
	}
	if len(gaps) < gapMinEvents {
		return Lag{}, len(gaps), false
	}
	return medianLag(gaps, market), len(gaps), true
}

// PoolGaps is the gap of a pool of funds (FundPool): the median of the
// gaps learned of each fund that has one of its own (the lower of the
// middle two), Near the shortest of them and Far the longest, in the
// business days of market. ok is false where fewer than two funds have a
// gap: one fund alone does not speak for a market.
func PoolGaps(gaps []Lag, market string) (Lag, bool) {
	if len(gaps) < 2 {
		return Lag{}, false
	}
	days := make([]int, len(gaps))
	for i, g := range gaps {
		days[i] = g.Days
	}
	return medianLag(days, market), true
}

// medianLag is the business-day Lag of gaps (at least one): their median,
// the lower of the middle two, between their shortest and their longest.
func medianLag(gaps []int, market string) Lag {
	slices.Sort(gaps)
	return Lag{Days: gaps[(len(gaps)-1)/2], Business: true, Near: gaps[0], Far: gaps[len(gaps)-1], Market: market}
}

// FundPool is the pool of funds whose gaps a fund of market takes where it
// has learned none of its own: the funds of the United States together
// ("US"), the European funds together ("EU": the euro area's, and those
// of Switzerland, the United Kingdom, the Nordic markets and
// Liechtenstein), else the funds of its own market. "" for no market.
func FundPool(market string) string {
	switch {
	case market == "US" || market == "":
		return market
	case slices.Contains(canonical.EuroAreaMarkets, market) || slices.Contains(europeanMarkets, market):
		return "EU"
	}
	return market
}

// europeanMarkets are the European markets beyond the euro area whose
// funds pool with its (FundPool). The set is wider than the European
// settlement cycle's markets (canonical.BuiltinSettlementCycles): a fund
// of Iceland or Liechtenstein pools with Europe's though no built-in
// cycle covers its market.
var europeanMarkets = []string{"CH", "GB", "SE", "DK", "NO", "IS", "LI"}

// ExWindow is the record window of a payment whose day begins at pay and
// whose record day a learned gap gives: from the day the quantity of its
// furthest record day reads, learnedSlack business days past its longest
// gap, to that of its nearest, each the ex day its record day gives by
// market's cycle, else the record day itself (RecordOf).
func (l Lag) ExWindow(pay time.Time, market string, cycles SettlementCycles) (from, to time.Time) {
	return exOf(l.back(pay, l.Far+learnedSlack), market, cycles), exOf(l.back(pay, l.Near), market, cycles)
}

// exOf is the day whose start gives the quantity of a distribution with
// record day record: its ex day by market's cycle on it, else the record
// day itself where the market has no cycle then.
func exOf(record time.Time, market string, cycles SettlementCycles) time.Time {
	if d, ok := cycles.ExDay(market, record); ok {
		return d
	}
	return record
}

// businessGap is the business days of market from d to pay, as the
// reader steps back: the fewest steps of businessDays(market, pay, -n)
// that reach d or before it. So the reader gives d back where it is a
// business day, whether or not pay is one.
func businessGap(market string, d, pay time.Time) int {
	n := 0
	for x := pay; x.After(d); n++ {
		x = businessDays(market, x, -1)
	}
	return n
}
