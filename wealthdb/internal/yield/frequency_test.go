package yield

import (
	"slices"
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
		// Two payments two months apart in seven months held, no rates to
		// tell them apart, read quarterly, as a quarterly payer's would.
		{"twice a year, July and September, held seven months", []int{24306, 24308}, 7.0 / 12, 4},
		{"once, held three months", seq(1, 1), 0.25, 1},
		{"never", nil, 1, 1},
	} {
		months := map[int]bool{}
		for _, m := range c.months {
			months[m] = true
		}
		if got, _ := payFrequency(months, nil, c.share); got != c.want {
			t.Errorf("%s: %d, want %d", c.what, got, c.want)
		}
	}
}

// Two months of the calendar more than a month apart that the halves
// rule (halves) calls a payer's two halves read twice a year, however
// short the holding: July and September held seven months. Without it
// they read as often as the months held allow: quarterly. Two months next
// to each other, a monthly payer's, read monthly, and three months are no
// two halves. The rule reads the two months in time. A month is numbered
// as monthOf numbers it: 24306 is July 2025.
func TestTwoHalvesOfTwoMonths(t *testing.T) {
	t.Parallel()
	yes := func(int, int) bool { return true }
	no := func(int, int) bool { return false }
	for _, c := range []struct {
		what   string
		months []int
		halves func(int, int) bool
		share  float64
		want   int
	}{
		{"July and September, two halves", []int{24306, 24308}, yes, 7.0 / 12, 2},
		{"July and September, no halves", []int{24306, 24308}, no, 7.0 / 12, 4},
		{"July and September, no rule", []int{24306, 24308}, nil, 7.0 / 12, 4},
		{"July and August, two halves", []int{24306, 24307}, yes, 2.0 / 12, 12},
		{"July, September and November", []int{24306, 24308, 24310}, yes, 7.0 / 12, 4},
	} {
		months := map[int]bool{}
		for _, m := range c.months {
			months[m] = true
		}
		if got, _ := payFrequency(months, c.halves, c.share); got != c.want {
			t.Errorf("%s: %d, want %d", c.what, got, c.want)
		}
	}
	var read []int
	record := func(a, b int) bool { read = append(read, a, b); return false }
	payFrequency(map[int]bool{24308: true, 24306: true}, record, 1)
	if !slices.Equal(read, []int{24306, 24308}) {
		t.Errorf("the rule read %v", read)
	}
}

// Two rates hold one level where the higher exceeds the lower by a third
// of it at most: 0.75 and 1 do, 0.75 and 1.01 do not.
func TestOneLevel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b float64
		want bool
	}{{0.75, 1, true}, {1, 0.75, true}, {0.25, 0.25, true}, {0.75, 1.01, false}, {0.1, 0.3, false}} {
		if got := oneLevel(c.a, c.b); got != c.want {
			t.Errorf("%v and %v: %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Two pay months two or three months apart, of unlike rates, are the two
// halves of a payer twice a year where the holding covered a quarter
// before the older, or after the newer, that paid nothing (quietQuarter):
// a quarterly payer would have paid in it. It covers the quarter from as
// far before it as a record window may reach (recordReach): 35 days,
// where the payments state no lag; 63 for a lag of 30 days.
//
//   - A payer of 0.1 in April and 0.3 in June held from 20 October, over
//     a quiet December and January, pays both a year, each in its own
//     month a year on, nothing in December.
//   - So does one paid on 30 April and 30 June, a share of 30 days' lag
//     held from 20 March, over a quiet September and October, on 15
//     November.
//   - Where one of the first payer's payments states a lag of 30 days,
//     the holding covered no quarter from 63 days before: it pays 0.3
//     four times a year.
//   - So does a quarterly payer that raised its dividend from 0.2 to 0.3,
//     bought on 20 October or on 15 November, and one of one level.
//   - So does a fund that pays 0.15 in March and 0.50 in June,
//     seasonally, bought on 1 December or on 20 December: it held no
//     December from 35 days before.
//   - A May whose only payment is a special shows no rate: its months
//     read as often as the months held allow.
func TestTwoUnlikePayMonths(t *testing.T) {
	t.Parallel()
	type paid struct {
		d    *time.Time
		rate float64
		lag  int
	}
	quarters := []*time.Time{date(2026, 9, 1), date(2026, 12, 1), date(2027, 3, 1), date(2027, 6, 1)}
	for _, c := range []struct {
		what      string
		fund      bool
		now, from *time.Time
		paid      []paid
		yield     float64
		months    []*time.Time
		incomes   []float64
	}{
		{"uneven halves over a quiet quarter", false, &day, date(2025, 10, 20),
			[]paid{{date(2026, 4, 10), 0.1, 0}, {date(2026, 6, 10), 0.3, 0}}, 0.4,
			[]*time.Time{date(2027, 4, 1), date(2027, 6, 1), date(2026, 12, 1)}, []float64{10, 30, 0}},
		{"uneven halves over a quiet quarter after them", false, date(2026, 11, 15), date(2026, 3, 20),
			[]paid{{date(2026, 4, 30), 0.1, 30}, {date(2026, 6, 30), 0.3, 30}}, 0.4,
			[]*time.Time{date(2027, 4, 1), date(2027, 6, 1), date(2026, 12, 1)}, []float64{10, 30, 0}},
		{"uneven halves, one of a lag of 30 days", false, &day, date(2025, 10, 20),
			[]paid{{date(2026, 4, 10), 0.1, 30}, {date(2026, 6, 10), 0.3, 0}}, 1.2, quarters,
			[]float64{30, 30, 30, 30}},
		{"a raised quarterly payer bought on 20 October", false, &day, date(2025, 10, 20),
			[]paid{{date(2026, 3, 10), 0.2, 30}, {date(2026, 6, 10), 0.3, 30}}, 1.2, quarters,
			[]float64{30, 30, 30, 30}},
		{"a raised quarterly payer bought on 15 November", false, &day, date(2025, 11, 15),
			[]paid{{date(2026, 3, 10), 0.2, 30}, {date(2026, 6, 10), 0.3, 30}}, 1.2, quarters,
			[]float64{30, 30, 30, 30}},
		{"a quarterly payer of one level", false, &day, date(2025, 11, 15),
			[]paid{{date(2026, 3, 10), 0.25, 0}, {date(2026, 6, 10), 0.25, 0}}, 1, quarters,
			[]float64{25, 25, 25, 25}},
		{"a seasonal quarterly fund bought on 1 December", true, &day, date(2025, 12, 1),
			[]paid{{date(2026, 3, 10), 0.15, 0}, {date(2026, 6, 10), 0.5, 0}}, 0.5 * 4 / 50 * 100, quarters,
			[]float64{50, 50, 50, 50}},
		{"a seasonal quarterly fund bought on 20 December", true, &day, date(2025, 12, 20),
			[]paid{{date(2026, 3, 10), 0.15, 0}, {date(2026, 6, 10), 0.5, 0}}, 0.5 * 4 / 50 * 100, quarters,
			[]float64{50, 50, 50, 50}},
		{"a May of only a special", false, &day, date(2025, 11, 1),
			[]paid{{date(2025, 5, 12), 0.1, 0}, {date(2026, 3, 10), 0.1, 0}, {date(2026, 5, 10), 1, 0}}, 0.4, quarters,
			[]float64{10, 10, 10, 10}},
	} {
		in := inputs()
		in.Day, in.AsOf = *c.now, c.now.Unix()+86399
		h, price := stock("A", "QSTK", 100), 100.0
		if c.fund {
			h, price = etf("A", "EXEQ", 100), 50
			h.AssetClass = "public_equity"
		}
		in.Holdings = []Holding{h}
		from := int(c.from.Sub(in.Day.AddDate(-1, 0, 1)).Hours() / 24)
		in.Daily[h.HoldingKey] = held(Span{From: from, To: year, Amount: 100 * price})
		for _, p := range c.paid {
			pay(in, h.HoldingKey, "A", p.d, p.rate*100, num(100))
			in.Payments[h.HoldingKey][len(in.Payments[h.HoldingKey])-1].LagDays = p.lag
		}
		b := Compute(in)
		p := position(b, h.HoldingKey)
		near(t, c.what+" dividend yield", p.DividendYield, c.yield, 1e-9)
		near(t, c.what+" income 12m", p.Income12, c.yield*price, 1e-9)
		for i, d := range c.months {
			near(t, c.what+" "+d.Format("January 2006"), num(incomeOf(b, h.HoldingKey, monthIndex(b, *d), RuleCash,
				PathConstant, 0)), c.incomes[i], 1e-9)
		}
	}
}

// A quarter before April and June 2026 is quiet where the holding covered
// January and December, from 35 days before them, and neither paid: a
// pay day that drifted into December is no quiet quarter. A holding
// bought on 15 November covered no December from 35 days before. A
// quarter that lies partly before the twelve months to the instant, or
// is not over by it, tells nothing.
func TestAQuietQuarter(t *testing.T) {
	t.Parallel()
	apr, jun := monthOf(*date(2026, 4, 1)), monthOf(*date(2026, 6, 1))
	for _, c := range []struct {
		what          string
		from          *time.Time
		first, second int
		paid          []*time.Time
		want          bool
	}{
		{"nothing paid", date(2025, 6, 16), apr, jun, nil, true},
		{"a pay day in January", date(2025, 6, 16), apr, jun, []*time.Time{date(2026, 1, 30)}, false},
		{"a pay day drifted into December", date(2025, 6, 16), apr, jun, []*time.Time{date(2025, 12, 31)}, false},
		{"bought on 15 November", date(2025, 11, 15), apr, jun, nil, false},
		{"a quarter partly before the year", date(2025, 6, 16), monthOf(*date(2025, 10, 1)), jun, nil, false},
	} {
		in := inputs()
		h := stock("A", "QSTK", 100)
		in.Holdings = []Holding{h}
		in.Daily[h.HoldingKey] = held(Span{From: dayOf(c.from), To: year, Amount: 10000})
		paid := map[int]bool{}
		for _, d := range c.paid {
			paid[monthOf(*d)] = true
		}
		if got := in.quietQuarter(h, c.first, c.second, paid, recordReach(0)); got != c.want {
			t.Errorf("%s: %v, want %v", c.what, got, c.want)
		}
	}
}

// A fund held two months that paid once reads its one distribution once a
// year: one month of the calendar shows no rhythm, and a quarterly payer
// would read a quarter of what it earns. Its page's yield wins: a bond
// fund's distribution or SEC yield, else its trailing yield, a floating
// one's as its floating rate; an equity fund's trailing yield; a
// multi-asset fund's distribution yield. With none, it pays its one
// distribution once a year, as a stock held two months does, whatever
// yield a page states for it. A fund held eleven months of the twelve or
// more that paid once is a yearly payer: it keeps its reading, whatever
// its page states, also where a transfer left five days of September
// unheld.
func TestAFundOfOnePayMonth(t *testing.T) {
	t.Parallel()
	allYear := []Span{{From: 0, To: year, Amount: 50000}}
	gapped := []Span{{From: 0, To: dayOf(date(2025, 9, 10)), Amount: 50000},
		{From: dayOf(date(2025, 9, 15)), To: year, Amount: 50000}}
	for _, c := range []struct {
		what     string
		class    string
		fund     Fund
		held     []Span
		dividend *float64
		income   float64
		basis    Basis
	}{
		{"a bond fund, its distribution yield", "fixed_income",
			Fund{DistributionYield: &Dated{Value: 4.6, Day: day}, TrailingYield: &Dated{Value: 4.4, Day: day}}, nil,
			nil, 2300, BasisStated},
		{"a bond fund, its trailing yield", "fixed_income", Fund{TrailingYield: &Dated{Value: 4.4, Day: day}}, nil,
			nil, 2200, BasisStated},
		{"a bond fund, its SEC yield", "fixed_income", Fund{SECYield: &Dated{Value: 4.2, Day: day}}, nil, nil, 2100,
			BasisStated},
		{"a floating bond fund, its distribution yield", "fixed_income",
			Fund{Lines: &LineSummary{AsOf: day, FloatingPct: 96, DebtPct: 96},
				DistributionYield: &Dated{Value: 4.6, Day: day}},
			nil, nil, 2300, BasisStated},
		{"a bond fund, no yield stated", "fixed_income", Fund{}, nil, nil, 200, BasisIndicated},
		{"a bond fund held all year", "fixed_income", Fund{DistributionYield: &Dated{Value: 4.6, Day: day}}, allYear,
			nil, 200, BasisIndicated},
		{"an equity fund, its trailing yield", "public_equity", Fund{TrailingYield: &Dated{Value: 3, Day: day},
			DistributionYield: &Dated{Value: 3.5, Day: day}}, nil, num(3), 1500, BasisStated},
		{"an equity fund, no yield stated", "public_equity", Fund{}, nil, num(0.4), 200, BasisIndicated},
		{"an equity fund held all year", "public_equity", Fund{TrailingYield: &Dated{Value: 3, Day: day}}, allYear,
			num(0.4), 200, BasisIndicated},
		{"an equity fund held all year but five days", "public_equity",
			Fund{TrailingYield: &Dated{Value: 3, Day: day}}, gapped, num(0.4), 200, BasisIndicated},
		{"a multi-asset fund, its distribution yield", "multi_asset",
			Fund{DistributionYield: &Dated{Value: 4, Day: day}}, nil, num(4), 2000, BasisStated},
	} {
		in := inputs()
		h := etf("A", "EXFD", 1000)
		h.AssetClass = c.class
		in.Holdings = []Holding{h}
		f := c.fund
		in.Funds[h.SecurityID] = &f
		pay(in, h.HoldingKey, "A", date(2026, 5, 10), 200, num(1000))
		spans := c.held
		if spans == nil {
			spans = []Span{{From: dayOf(date(2026, 4, 15)), To: year, Amount: 50000}}
		}
		in.Daily[h.HoldingKey] = held(spans...)
		b := Compute(in)
		p := position(b, h.HoldingKey)
		near(t, c.what+" income 12m", p.Income12, c.income, 1e-6)
		if c.dividend != nil {
			near(t, c.what+" dividend yield", p.DividendYield, *c.dividend, 1e-9)
		}
		for _, a := range b.Amounts(nil, b.NextMonths(12), DefaultAssumptions) {
			if a.Basis != c.basis {
				t.Errorf("%s: an amount %+v", c.what, a)
			}
		}
	}
	in := inputs()
	h := stock("A", "QSTK", 100)
	h.SecurityID = "isin:US0000000002"
	in.Holdings = []Holding{h}
	in.Funds[h.SecurityID] = &Fund{TrailingYield: &Dated{Value: 3, Day: day}}
	pay(in, h.HoldingKey, "A", date(2026, 5, 10), 200, num(100))
	in.Daily[h.HoldingKey] = held(Span{From: dayOf(date(2026, 4, 15)), To: year, Amount: 10000})
	dividendOf(t, Compute(in), h, 2, 200)
}

// A bond fund that paid nothing, its page stating only its trailing
// yield, distributes that yield month by month: 4% of 50000.
func TestABondFundsTrailingYield(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := etf("A", "EXBF", 1000)
	in.Holdings = []Holding{h}
	in.Funds[h.SecurityID] = &Fund{TrailingYield: &Dated{Value: 4, Day: day}}
	b := Compute(in)
	near(t, "income 12m", position(b, h.HoldingKey).Income12, 2000, 1e-6)
	for _, a := range b.Amounts(nil, b.NextMonths(12), DefaultAssumptions) {
		if a.Basis != BasisStated || a.Day != nil {
			t.Errorf("an amount %+v", a)
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
