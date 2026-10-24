package yield

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// builtinCycles are the built-in settlement cycles, by market.
func builtinCycles() SettlementCycles {
	out := SettlementCycles{}
	for _, c := range canonical.BuiltinSettlementCycles() {
		out[c.Market] = append(out[c.Market], c)
	}
	return out
}

// A payment's record day comes from its event in the calendar: the one
// whose pay day lies nearest, from ten days before the payment to three
// after; else, of those that state no pay day, the one whose record or ex
// day lies latest in the record window: with no lag, the 105 days to the
// payment for a share, the 63 for a fund. An event whose record day or
// ex day lies after the payment is none of its, and an estimate loses to
// an event that is none. A stated record day wins, an ex day derives one by the
// market's cycle, and an event that gives neither, or no event, leaves it
// to the market's lag. The quantity reads as the event's ex day began:
// the stated one, else the one the record day gives by the cycle. The
// events are invented.
func TestRecordOf(t *testing.T) {
	t.Parallel()
	cycles := builtinCycles()
	us := RecordLag("US", "USD", false, true)
	ch := RecordLag("CH", "CHF", false, true)
	for _, c := range []struct {
		name   string
		events []CalendarEvent
		pay    *time.Time
		lag    Lag
		fund   bool
		market string
		read   *time.Time
		source RecordSource
	}{
		{"a stated record day under T+1", []CalendarEvent{{Ex: date(2026, 5, 28), Record: date(2026, 5, 28),
			Pay: date(2026, 6, 10)}}, date(2026, 6, 11), us, false, "US", date(2026, 5, 28), RecordStated},
		{"a stated record day and no ex day under T+1", []CalendarEvent{{Record: date(2026, 5, 28),
			Pay: date(2026, 6, 10)}}, date(2026, 6, 11), us, false, "US", date(2026, 5, 28), RecordStated},
		{"a stated record day and no ex day under T+2, across a closing day", []CalendarEvent{{
			Record: date(2025, 8, 4), Pay: date(2025, 8, 5)}}, date(2025, 8, 5), ch, false, "CH", date(2025, 7, 31),
			RecordStated},
		{"a stated ex day before its stated record day", []CalendarEvent{{Ex: date(2025, 7, 31),
			Record: date(2025, 8, 4), Pay: date(2025, 8, 5)}}, date(2025, 8, 5), ch, false, "CH", date(2025, 7, 31),
			RecordStated},
		{"a stated record day and no ex day before the first cycle", []CalendarEvent{{Record: date(2013, 5, 21),
			Pay: date(2013, 5, 23)}}, date(2013, 5, 23), RecordLag("DE", "EUR", false, true), false, "DE",
			date(2013, 5, 21), RecordStated},
		{"the nearest pay day", []CalendarEvent{
			{Record: date(2026, 3, 1), Pay: date(2026, 6, 2)}, {Record: date(2026, 5, 20), Pay: date(2026, 6, 12)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 20), RecordStated},
		{"a record day over an ex day as near", []CalendarEvent{
			{Ex: date(2026, 5, 27), Pay: date(2026, 6, 11)}, {Record: date(2026, 5, 28), Pay: date(2026, 6, 11)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 28), RecordStated},
		{"two providers state one event: the one that states more wins", []CalendarEvent{
			{Record: date(2026, 5, 28), Pay: date(2026, 6, 11)},
			{Ex: date(2026, 5, 27), Record: date(2026, 5, 28), Pay: date(2026, 6, 11)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 27), RecordStated},
		{"a final event over a nearer estimate", []CalendarEvent{
			{Record: date(2026, 3, 20), Pay: date(2026, 3, 27), Estimated: true},
			{Record: date(2026, 3, 19), Pay: date(2026, 3, 25)}},
			date(2026, 3, 27), us, false, "US", date(2026, 3, 19), RecordStated},
		{"an estimate alone", []CalendarEvent{{Record: date(2026, 3, 20), Pay: date(2026, 3, 27), Estimated: true}},
			date(2026, 3, 27), us, false, "US", date(2026, 3, 20), RecordStated},
		{"a final event over a later estimate with no pay day", []CalendarEvent{
			{Record: date(2026, 5, 20), Estimated: true}, {Record: date(2026, 5, 10)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 10), RecordStated},
		{"a pay day ten days before", []CalendarEvent{{Record: date(2026, 5, 20), Pay: date(2026, 6, 1)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 20), RecordStated},
		{"a pay day eleven days before is no match", []CalendarEvent{{Record: date(2026, 5, 1), Pay: date(2026, 5, 31)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 12), RecordLagged},
		{"a pay day three days after", []CalendarEvent{{Record: date(2026, 5, 20), Pay: date(2026, 6, 14)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 20), RecordStated},
		{"a pay day four days after is no match", []CalendarEvent{{Record: date(2026, 5, 1), Pay: date(2026, 6, 15)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 12), RecordLagged},
		{"a record day after the payment is no match", []CalendarEvent{{Record: date(2026, 6, 12), Pay: date(2026, 6, 12)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 12), RecordLagged},
		{"an ex day after the payment is no match", []CalendarEvent{{Ex: date(2026, 6, 12), Record: date(2026, 6, 1),
			Pay: date(2026, 6, 11)}}, date(2026, 6, 11), us, false, "US", date(2026, 5, 12), RecordLagged},
		{"no pay day: the latest record day in the window", []CalendarEvent{
			{Record: date(2026, 5, 10)}, {Record: date(2026, 5, 20)}, {Record: date(2026, 6, 9)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 20), RecordStated},
		{"no pay day: a record day after the payment is no match, its ex day on it", []CalendarEvent{
			{Record: date(2026, 5, 12)}, {Ex: date(2026, 6, 11), Record: date(2026, 6, 12)}},
			date(2026, 6, 11), Lag{}, true, "IE", date(2026, 5, 11), RecordStated},
		{"no pay day and no lag: a fund's 63 days before", []CalendarEvent{{Record: date(2026, 4, 9)}, {Record: date(2026, 4, 10)}},
			date(2026, 6, 11), Lag{}, true, "IE", date(2026, 4, 9), RecordStated},
		{"an ex day across a closing day", []CalendarEvent{{Ex: date(2024, 7, 31), Pay: date(2024, 8, 5)}},
			date(2024, 8, 5), ch, false, "CH", date(2024, 7, 31), RecordDerived},
		{"an ex day across Easter", []CalendarEvent{{Ex: date(2024, 3, 28)}},
			date(2024, 4, 3), Lag{}, false, "DE", date(2024, 3, 28), RecordDerived},
		{"an ex day under T+2", []CalendarEvent{{Ex: date(2024, 5, 23), Pay: date(2024, 6, 10)}},
			date(2024, 6, 10), us, false, "US", date(2024, 5, 23), RecordDerived},
		{"an ex day under T+1", []CalendarEvent{{Ex: date(2024, 5, 28), Pay: date(2024, 6, 10)}},
			date(2024, 6, 10), us, false, "US", date(2024, 5, 28), RecordDerived},
		{"an ex day under T+3", []CalendarEvent{{Ex: date(2017, 8, 23), Pay: date(2017, 9, 8)}},
			date(2017, 9, 8), us, false, "US", date(2017, 8, 23), RecordDerived},
		{"an ex day whose record day falls after the payment: the lag", []CalendarEvent{{Ex: date(2024, 5, 23),
			Pay: date(2024, 5, 23)}}, date(2024, 5, 23), us, false, "US", date(2024, 4, 23), RecordLagged},
		{"Hong Kong derives nothing: the lag", []CalendarEvent{{Ex: date(2026, 5, 20), Pay: date(2026, 6, 10)}},
			date(2026, 6, 11), RecordLag("HK", "HKD", false, true), false, "HK", date(2026, 5, 12), RecordLagged},
		{"no cycle before the first: the lag", []CalendarEvent{{Ex: date(2013, 5, 20), Pay: date(2013, 5, 23)}},
			date(2013, 5, 23), RecordLag("DE", "EUR", false, true), false, "DE", date(2013, 5, 21), RecordLagged},
		{"a pay day alone: the lag", []CalendarEvent{{Pay: date(2026, 6, 10)}},
			date(2026, 6, 11), us, false, "US", date(2026, 5, 12), RecordLagged},
		{"a pay day alone and no lag", []CalendarEvent{{Pay: date(2026, 6, 10)}},
			date(2026, 6, 11), Lag{}, true, "IE", nil, RecordNone},
		{"no pay day and no lag: a share's 105 days before", []CalendarEvent{{Record: date(2026, 3, 18)},
			{Ex: date(2026, 2, 25)}}, date(2026, 6, 11), Lag{}, false, "JP", date(2026, 3, 17), RecordStated},
		{"a share's event 120 days before stays on its rule", []CalendarEvent{{Record: date(2026, 2, 11)}},
			date(2026, 6, 11), Lag{}, false, "JP", nil, RecordNone},
		{"a fund's event 64 days before stays on its rule", []CalendarEvent{{Record: date(2026, 4, 8)}},
			date(2026, 6, 11), Lag{}, true, "IE", nil, RecordNone},
		{"no event, the lag", nil, date(2026, 6, 11), us, false, "US", date(2026, 5, 12), RecordLagged},
		{"no event and no lag", nil, date(2026, 6, 11), Lag{}, false, "US", nil, RecordNone},
	} {
		read, source := RecordOf(c.events, *c.pay, c.lag, Lag{}, !c.fund, c.market, cycles)
		if source != c.source || (c.read == nil) != read.IsZero() || c.read != nil && !read.Equal(*c.read) {
			t.Errorf("%s: reads %s %q; want %v %q", c.name, read.Format(time.DateOnly), source, c.read, c.source)
		}
	}
}

// A distribution's record day is its ex day plus one business day less
// than its market's cycle on the ex day: over a closing day and Easter,
// under each cycle the United States has had. A market with no cycle
// then derives none.
func TestSettlementCyclesRecordDay(t *testing.T) {
	t.Parallel()
	cycles := builtinCycles()
	for _, c := range []struct {
		name, market string
		ex, want     *time.Time
	}{
		{"across a closing day", "CH", date(2024, 7, 31), date(2024, 8, 2)},
		{"across Easter", "DE", date(2024, 3, 28), date(2024, 4, 2)},
		{"T+3", "US", date(2017, 8, 23), date(2017, 8, 25)},
		{"T+2", "US", date(2024, 5, 23), date(2024, 5, 24)},
		{"T+1", "US", date(2024, 5, 28), date(2024, 5, 28)},
	} {
		if got, ok := cycles.RecordDay(c.market, *c.ex); !ok || !got.Equal(*c.want) {
			t.Errorf("%s: record day of %s = %s %v, want %s", c.name, c.ex.Format(time.DateOnly),
				got.Format(time.DateOnly), ok, c.want.Format(time.DateOnly))
		}
	}
	for _, c := range []struct {
		market string
		ex     *time.Time
	}{{"HK", date(2026, 5, 20)}, {"DE", date(2013, 5, 20)}} {
		if _, ok := cycles.RecordDay(c.market, *c.ex); ok {
			t.Errorf("%s gives a record day on %s", c.market, c.ex.Format(time.DateOnly))
		}
	}
}

// A record day's ex day is the one whose record day it is: the record
// day less one business day less than the cycle, over the market's
// closing days. Each business day of the years about the cycles' changes
// reads back to itself.
func TestExDayInvertsRecordDay(t *testing.T) {
	t.Parallel()
	cycles := builtinCycles()
	for _, c := range []struct {
		market         string
		record, wantEx *time.Time
	}{
		{"CH", date(2025, 8, 4), date(2025, 7, 31)}, {"DE", date(2024, 4, 2), date(2024, 3, 28)},
		{"US", date(2017, 8, 25), date(2017, 8, 23)}, {"US", date(2024, 5, 29), date(2024, 5, 29)},
		{"SE", date(2026, 6, 22), date(2026, 6, 18)}, {"CH", date(2027, 10, 11), date(2027, 10, 11)},
	} {
		if ex, ok := cycles.ExDay(c.market, *c.record); !ok || !ex.Equal(*c.wantEx) {
			t.Errorf("%s: ex day of %s = %s %v, want %s", c.market, c.record.Format(time.DateOnly),
				ex.Format(time.DateOnly), ok, c.wantEx.Format(time.DateOnly))
		}
	}
	if _, ok := cycles.ExDay("HK", *date(2026, 6, 1)); ok {
		t.Error("Hong Kong gives an ex day")
	}
	for _, market := range []string{"CH", "DE", "DK", "SE", "US", "JP"} {
		for d := *date(2016, 1, 1); d.Before(*date(2028, 12, 31)); d = d.AddDate(0, 0, 1) {
			if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || closed(market, d) {
				continue
			}
			ex, ok := cycles.ExDay(market, d)
			if !ok || cycles.On(market, ex) != cycles.On(market, d) {
				continue
			}
			if back, _ := cycles.RecordDay(market, ex); !back.Equal(d) {
				t.Errorf("%s: record day %s reads ex day %s, whose record day is %s", market, d.Format(time.DateOnly),
					ex.Format(time.DateOnly), back.Format(time.DateOnly))
			}
		}
	}
}

// A market's cycle is that of its newest cycle from on or before the
// day; a market the cycles do not name has none, and so has one before
// its first cycle.
func TestSettlementCyclesOn(t *testing.T) {
	t.Parallel()
	cycles := builtinCycles()
	for _, c := range []struct {
		market string
		day    *time.Time
		want   int
	}{
		{"US", date(2017, 9, 4), 3}, {"US", date(2017, 9, 5), 2}, {"US", date(2024, 5, 27), 2},
		{"US", date(2024, 5, 28), 1}, {"CA", date(2024, 5, 26), 2}, {"CA", date(2024, 5, 27), 1}, {"JP", date(2019, 7, 15), 3},
		{"JP", date(2019, 7, 16), 2}, {"IE", date(2014, 10, 5), 0}, {"IE", date(2014, 10, 6), 2},
		{"CH", date(2027, 10, 11), 1}, {"HK", date(2026, 1, 1), 0}, {"", date(2026, 1, 1), 0},
	} {
		if got := cycles.On(c.market, *c.day); got != c.want {
			t.Errorf("%s on %s = %d, want %d", c.market, c.day.Format(time.DateOnly), got, c.want)
		}
	}
}

// A share and a receipt settle where they are quoted, a share in euros
// in its issuer's euro-area market; a fund settles in its country.
func TestSettlementMarket(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		situs, currency string
		receipt, share  bool
		want            string
	}{
		{"US", "USD", false, true, "US"}, {"DE", "USD", false, true, "US"}, {"DE", "EUR", false, true, "DE"},
		{"NL", "EUR", false, true, "NL"}, {"", "EUR", false, true, ""}, {"JP", "USD", true, false, "US"},
		{"CH", "CHF", false, true, "CH"}, {"JP", "JPY", false, true, "JP"}, {"IE", "USD", false, false, "IE"},
		{"", "GBP", false, false, "GB"}, {"", "", false, false, ""},
	} {
		if got := SettlementMarket(c.situs, c.currency, c.receipt, c.share); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
}

// The coverage view counts, over the payments the readings rate, where
// each record day comes from and how many the record window drops: each
// payment once, though two holdings of one security read it, and the
// holdings whose readings rate them with their value.
func TestCoverageCountsTheRecordDays(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b := stock("A", "QSTK", 100), stock("B", "QSTK", 300)
	a.SecurityID, b.SecurityID = "isin:XX0000000001", "isin:XX0000000001"
	f := stock("A", "QFND", 50)
	f.Vehicle = "etf"
	in.Holdings = []Holding{a, b, f}
	paid := func(k HoldingKey, acct string, d *time.Time, source RecordSource, least, most float64) {
		p := Payment{Day: *d, Amount: 50, Account: acct, Quantity: num(100), RecordSource: source,
			Type: TypeDividends}
		if least > 0 {
			p.Record, p.Least, p.Most = num(least), num(least), num(most)
		}
		in.Payments[k] = append(in.Payments[k], p)
	}
	paid(a.HoldingKey, "A", quarterly[0], RecordStated, 100, 100)
	paid(a.HoldingKey, "A", quarterly[1], RecordDerived, 100, 100)
	paid(b.HoldingKey, "B", quarterly[1], RecordLearned, 100, 100)
	paid(b.HoldingKey, "B", quarterly[2], RecordLagged, 100, 150)
	paid(b.HoldingKey, "B", quarterly[3], RecordLagged, 100, 100)
	paid(f.HoldingKey, "A", quarterly[3], RecordNone, 0, 0)
	in.Payments[f.HoldingKey] = append(in.Payments[f.HoldingKey], Payment{Day: *quarterly[3], Amount: 5,
		Account: "A", Type: TypeInterest})
	var got []string
	for _, r := range Compute(in).Coverage() {
		if r.Section == "record_day" {
			got = append(got, r.Reason+" "+strconv.Itoa(r.Payments)+" "+strconv.Itoa(r.Holdings))
		}
	}
	if want := "stated 1 2|derived 1 2|learned 1 2|lag 2 2|none 1 1|dropped 1 2"; strings.Join(got, "|") != want {
		t.Errorf("record_day lines %q, want %q", strings.Join(got, "|"), want)
	}
}
