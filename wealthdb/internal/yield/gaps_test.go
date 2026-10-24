package yield

import (
	"testing"
	"time"
)

// An issuer's gap is the median of its distributions' business days from
// record day to pay day, between the shortest and the longest. The
// events are invented: of a share of the United States, whose business
// days step over TARGET2's closings (Good Friday and Easter Monday 2026
// fall on 3 and 6 April), else of Hong Kong, where no settlement cycle
// derives a record day.
func TestLearnGap(t *testing.T) {
	t.Parallel()
	cycles := builtinCycles()
	for _, c := range []struct {
		name   string
		events []CalendarEvent
		market string
		want   Lag
		n      int
	}{
		{"three record days, the median of three", []CalendarEvent{
			{Record: date(2026, 2, 2), Pay: date(2026, 3, 2)}, {Record: date(2025, 11, 3), Pay: date(2025, 12, 2)},
			{Record: date(2025, 8, 1), Pay: date(2025, 8, 27)}}, "US",
			Lag{Days: 20, Business: true, Near: 18, Far: 21, Market: "US"}, 3},
		{"two: the lower of the middle two", []CalendarEvent{
			{Record: date(2026, 3, 2), Pay: date(2026, 3, 5)}, {Record: date(2026, 3, 9), Pay: date(2026, 3, 13)}},
			"US", Lag{Days: 3, Business: true, Near: 3, Far: 4, Market: "US"}, 2},
		{"over Easter's closing days", []CalendarEvent{
			{Record: date(2026, 4, 1), Pay: date(2026, 4, 8)}, {Record: date(2026, 3, 2), Pay: date(2026, 3, 5)}},
			"US", Lag{Days: 3, Business: true, Near: 3, Far: 3, Market: "US"}, 2},
		{"an ex day alone derives its record day by the cycle", []CalendarEvent{
			{Ex: date(2024, 5, 23), Pay: date(2024, 5, 29)}, {Ex: date(2024, 5, 28), Pay: date(2024, 5, 31)}},
			"US", Lag{Days: 3, Business: true, Near: 3, Far: 3, Market: "US"}, 2},
		{"an ex day where no cycle derives one counts itself", []CalendarEvent{
			{Ex: date(2026, 5, 20), Pay: date(2026, 6, 10)}, {Ex: date(2025, 11, 19), Pay: date(2025, 12, 10)}},
			"HK", Lag{Days: 15, Business: true, Near: 15, Far: 15, Market: "HK"}, 2},
		{"a pay day the market's calendar closes counts as the reader steps back", []CalendarEvent{
			{Record: date(2026, 4, 30), Pay: date(2026, 5, 1)}, {Record: date(2026, 4, 29), Pay: date(2026, 4, 30)}},
			"US", Lag{Days: 1, Business: true, Near: 1, Far: 1, Market: "US"}, 2},
		{"with no cycle, a stated ex day counts before a record day", []CalendarEvent{
			{Ex: date(2026, 5, 19), Record: date(2026, 5, 20), Pay: date(2026, 6, 10)},
			{Ex: date(2025, 11, 19), Pay: date(2025, 12, 10)}},
			"HK", Lag{Days: 15, Business: true, Near: 15, Far: 16, Market: "HK"}, 2},
		{"one event alone teaches nothing", []CalendarEvent{{Record: date(2026, 2, 2), Pay: date(2026, 3, 2)}},
			"US", Lag{}, 1},
		{"a gap outside 0 to 90 days teaches nothing", []CalendarEvent{
			{Record: date(2025, 8, 1), Pay: date(2025, 11, 29)}, {Record: date(2026, 3, 2), Pay: date(2026, 3, 1)},
			{Record: date(2026, 2, 2), Pay: date(2026, 3, 2)}}, "US", Lag{}, 1},
		{"an estimate, no pay day, no day, or the same days again teach nothing", []CalendarEvent{
			{Record: date(2026, 2, 2), Pay: date(2026, 3, 2), Estimated: true}, {Record: date(2026, 2, 2)},
			{Pay: date(2026, 3, 2)}, {Record: date(2025, 11, 3), Pay: date(2025, 12, 1)},
			{Ex: date(2025, 11, 3), Record: date(2025, 11, 3), Pay: date(2025, 12, 1)}}, "US", Lag{}, 1},
	} {
		got, n, ok := LearnGap(c.events, c.market, cycles)
		if got != c.want || n != c.n || ok != (c.want.Business) {
			t.Errorf("%s: %+v of %d (%v), want %+v of %d", c.name, got, n, ok, c.want, c.n)
		}
	}
}

// A pool's gap is the median of its funds' gaps, between the shortest and
// the longest; one fund alone gives none. The United States's funds pool
// together, Europe's together, the funds of any other market apart.
func TestPoolGapsAndFundPool(t *testing.T) {
	t.Parallel()
	three, five := Lag{Days: 3, Business: true}, Lag{Days: 5, Business: true}
	if got, ok := PoolGaps([]Lag{five, three, three}, "US"); !ok ||
		got != (Lag{Days: 3, Business: true, Near: 3, Far: 5, Market: "US"}) {
		t.Errorf("PoolGaps = %+v (%v)", got, ok)
	}
	if _, ok := PoolGaps([]Lag{three}, "US"); ok {
		t.Error("one fund's gap pools")
	}
	for market, want := range map[string]string{"US": "US", "IE": "EU", "LU": "EU", "CH": "EU", "GB": "EU",
		"SE": "EU", "JP": "JP", "CA": "CA", "": ""} {
		if got := FundPool(market); got != want {
			t.Errorf("FundPool(%q) = %q, want %q", market, got, want)
		}
	}
}

// A learned gap dates a payment no event of the calendar dates, after a
// stated or derived day and before the market's lag; its quantity reads
// as the record day's ex day began. A fund with no lag takes it as a
// share does, and its window runs from the furthest gap to the nearest.
func TestRecordOfLearned(t *testing.T) {
	t.Parallel()
	cycles := builtinCycles()
	us := RecordLag("US", "USD", false, true)
	gap := Lag{Days: 20, Business: true, Near: 18, Far: 21, Market: "US"}
	for _, c := range []struct {
		name   string
		events []CalendarEvent
		pay    *time.Time
		lag    Lag
		share  bool
		market string
		read   *time.Time
		source RecordSource
	}{
		{"a stated record day wins", []CalendarEvent{{Record: date(2026, 5, 28), Pay: date(2026, 6, 11)}},
			date(2026, 6, 11), us, true, "US", date(2026, 5, 28), RecordStated},
		{"a derived one wins", []CalendarEvent{{Ex: date(2026, 5, 28), Pay: date(2026, 6, 11)}},
			date(2026, 6, 11), us, true, "US", date(2026, 5, 28), RecordDerived},
		{"no event: the gap before the lag", nil, date(2026, 6, 11), us, true, "US", date(2026, 5, 14),
			RecordLearned},
		{"an event of no record day: the gap", []CalendarEvent{{Pay: date(2026, 6, 11)}}, date(2026, 6, 11), us,
			true, "US", date(2026, 5, 14), RecordLearned},
		{"a fund with no lag", nil, date(2026, 6, 11), Lag{}, false, "US", date(2026, 5, 14), RecordLearned},
		{"under T+2 the ex day a business day before, 1 May closed", nil, date(2024, 5, 10), us, true, "US",
			date(2024, 4, 10),
			RecordLearned},
	} {
		read, source := RecordOf(c.events, *c.pay, c.lag, gap, c.share, c.market, cycles)
		if source != c.source || !read.Equal(*c.read) {
			t.Errorf("%s: reads %s %q; want %s %q", c.name, read.Format(time.DateOnly), source,
				c.read.Format(time.DateOnly), c.source)
		}
	}
	// The window reaches two business days past the longest gap.
	from, to := gap.ExWindow(*date(2026, 6, 11), "US", cycles)
	if !from.Equal(*date(2026, 5, 11)) || !to.Equal(*date(2026, 5, 18)) {
		t.Errorf("ExWindow = %s to %s", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	// A record day comes back from its gap, whether or not the pay day is
	// a business day.
	for _, c := range []struct{ record, pay *time.Time }{
		{date(2026, 4, 30), date(2026, 5, 1)}, {date(2026, 5, 29), date(2026, 5, 31)},
		{date(2026, 3, 27), date(2026, 4, 8)},
	} {
		g := Lag{Days: businessGap("US", *c.record, *c.pay), Business: true, Market: "US"}
		if got := g.RecordDay(*c.pay); !got.Equal(*c.record) {
			t.Errorf("%s to %s: gap %d gives back %s", c.record.Format(time.DateOnly), c.pay.Format(time.DateOnly),
				g.Days, got.Format(time.DateOnly))
		}
	}
}
