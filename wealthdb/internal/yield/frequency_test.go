package yield

import (
	"testing"
	"time"
)

// The books here are invented. The day is 2026-06-15.

// etf is a holding in account acct of a fund of bonds its broker names
// by iid, priced at 50 a share.
func etf(acct, iid string, qty float64) Holding {
	return Holding{HoldingKey: HoldingKey{"broker", acct, iid}, Instrument: iid, AssetClass: "fixed_income",
		Vehicle: "etf", Currency: "USD", Value: qty * 50, Amount: qty * 50, Quantity: num(qty),
		SecurityID: "isin:US0000000001", Key: "isin:US0000000001"}
}

// The payments a year are the months of the calendar paid in, a pay
// month and one 11 or 12 months after it counted once (payMonths), per
// month held, as a year's worth, no more than the closest two pay months
// allow, round the year or in time; halfway between two frequencies, the
// higher where the closest two allow it; one calendar month reads once a
// year.
// A month is numbered as monthOf numbers it: 24306 is July 2025.
func TestAPayFrequency(t *testing.T) {
	t.Parallel()
	seq := func(n, step int) []int {
		var out []int
		for i := 0; i < n; i++ {
			out = append(out, 24300+i*step)
		}
		return out
	}
	for _, c := range []struct {
		what   string
		months []int
		share  float64
		want   int
	}{
		{"monthly, held five months", seq(5, 1), 5.0 / 12, 12},
		{"monthly, held the year", seq(12, 1), 1, 12},
		{"monthly, thirteen pay months in the year", seq(13, 1), 1, 12},
		{"monthly, held one month", seq(1, 1), 1.0 / 12, 1},
		// July and August 2025, then June 2026: the June drops July from
		// the count, but August keeps its month's gap in time.
		{"monthly, held in two stretches a year apart", []int{24306, 24307, 24317}, 81.0 / 365, 12},
		{"quarterly, held the year", seq(4, 3), 1, 4},
		{"quarterly, twice in four months", seq(2, 3), 4.0 / 12, 4},
		{"quarterly, its pay day drifting across a month end", []int{24306, 24308, 24311, 24314, 24317}, 1, 4},
		{"twice a year, held seven months", seq(2, 6), 7.0 / 12, 2},
		{"twice a year, held the year", seq(2, 6), 1, 2},
		{"quarterly, one payment missed, held the year", seq(3, 3), 1, 4},
		{"twice a year, its pay day in its month a year on", seq(3, 6), 1, 2},
		{"twice a year, its pay day in the month before a year on", []int{24306, 24311, 24317}, 1, 2},
		{"once a year, its pay day in its month a year on", seq(2, 12), 1, 1},
		{"once a year, its pay day in the month before a year on", []int{24306, 24317}, 1, 1},
		{"twice a year, in October and July", []int{24300 + 9, 24300 + 18}, 1, 2},
		// Two payments two months apart in seven months held read
		// quarterly, as a quarterly payer's would.
		{"twice a year, July and September, held seven months", []int{24306, 24308}, 7.0 / 12, 4},
		{"once, held three months", seq(1, 1), 0.25, 1},
		{"never", nil, 1, 1},
	} {
		months := map[int]bool{}
		for _, m := range c.months {
			months[m] = true
		}
		if got, _ := payFrequency(months, c.share); got != c.want {
			t.Errorf("%s: %d, want %d", c.what, got, c.want)
		}
	}
}

// A pay day a year on that drifted into the month before is the same
// payment. A payer twice a year that paid on 1 July 2025, 10 December
// 2025 and 12 June 2026 pays its interim and its final; a yearly payer
// that paid on 1 July 2025 and 12 June 2026 pays once a year. Where the
// interim gives no rate, its quantity unknown, the final of July 2025 is
// no other half: it is June's own payment a year earlier. The final then
// pays once a year, partial, as it does where July gives no rate either.
func TestAPayDayDriftingIntoTheMonthBefore(t *testing.T) {
	t.Parallel()
	type paid struct {
		day  *time.Time
		rate float64
		qty  *float64
	}
	for _, c := range []struct {
		what    string
		paid    []paid
		yield   float64
		partial bool
	}{
		{"twice a year", []paid{{date(2025, 7, 1), 0.6, num(100)}, {date(2025, 12, 10), 0.4, num(100)},
			{date(2026, 6, 12), 0.6, num(100)}}, 0.4 + 0.6, false},
		{"once a year", []paid{{date(2025, 7, 1), 1, num(100)}, {date(2026, 6, 12), 1, num(100)}}, 1, false},
		{"twice a year, the interim of no rate", []paid{{date(2025, 7, 1), 0.6, num(100)},
			{date(2025, 12, 10), 0.4, nil}, {date(2026, 6, 12), 0.6, num(100)}}, 0.6, true},
		{"twice a year, July and the interim of no rate", []paid{{date(2025, 7, 1), 0.6, nil},
			{date(2025, 12, 10), 0.4, nil}, {date(2026, 6, 12), 0.6, num(100)}}, 0.6, true},
	} {
		in := inputs()
		h := stock("A", "QSTK", 100)
		in.Holdings = []Holding{h}
		for _, p := range c.paid {
			pay(in, h.HoldingKey, "A", p.day, p.rate*100, p.qty)
		}
		p := position(Compute(in), h.HoldingKey)
		near(t, c.what+" dividend yield", p.DividendYield, c.yield, 1e-9)
		near(t, c.what+" income 12m", p.Income12, c.yield*100, 1e-9)
		if p.Tags[TagPartial] != c.partial {
			t.Errorf("%s: tags %v", c.what, p.Tags)
		}
	}
}

// A fund bought five months ago that paid in each of them pays monthly:
// its next twelve months are twelve payments. Where nothing says how long
// it was held, its five pay months read as a year's: quarterly.
func TestAFundHeldFiveMonthsPaysMonthly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		what   string
		daily  bool
		income float64
	}{{"held five months", true, 12 * 50}, {"held for all the inputs say", false, 4 * 50}} {
		in := inputs()
		h := etf("A", "EXFR", 1000)
		in.Holdings = []Holding{h}
		for m := 0; m < 5; m++ {
			pay(in, h.HoldingKey, "A", date(2026, 2+m, 10), 50, num(1000))
		}
		if c.daily {
			in.Daily[h.HoldingKey] = held(Span{From: dayOf(date(2026, 1, 20)), To: year, Amount: 50000})
		}
		near(t, c.what, position(Compute(in), h.HoldingKey).Income12, c.income, 1e-9)
	}
}

// The months held count the days of a position of the instrument that is
// no holding (FormerDaily), such as the one an account held before its
// source changed the id it keys the instrument by: gold keys that
// position by the id the matching folds it into (Fold). Two payments in a
// year held whole read twice a year. Counted over the holding's own two
// weeks alone, the same two read as often as their three months apart
// allow: quarterly.
func TestTheMonthsHeldCountAFormerPosition(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		what   string
		former bool
		income float64
	}{{"with the former position", true, 2 * 50}, {"the holding alone", false, 4 * 50}} {
		in := inputs()
		h := etf("B", "EXFR", 1000)
		in.Holdings = []Holding{h}
		pay(in, h.HoldingKey, "B", date(2026, 1, 10), 50, num(1000))
		pay(in, h.HoldingKey, "B", date(2026, 4, 10), 50, num(1000))
		in.Daily[h.HoldingKey] = held(Span{From: dayOf(date(2026, 6, 1)), To: year, Amount: 50000})
		if c.former {
			in.FormerDaily[InstrumentKey{"broker", "EXFR"}] = []Daily{held(Span{From: 0, To: dayOf(date(2026, 6, 1)),
				Amount: 50000})}
		}
		near(t, c.what, position(Compute(in), h.HoldingKey).Income12, c.income, 1e-9)
	}
}
