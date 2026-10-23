package yield

import (
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// The books here are invented. The day is 2026-06-15.

// slopedCurve is a rising Treasury curve: 4% at three months, 4.5% at a
// year, 5% at ten.
func slopedCurve() *Curve {
	z, _ := fixedincome.NewCurve([]fixedincome.Point{{Months: 3, Rate: 4}, {Months: 12, Rate: 4.5},
		{Months: 120, Rate: 5}}, fixedincome.Treasury)
	return &Curve{Zero: z, Benchmark: 4, BenchmarkMonths: 3}
}

// A fund whose income resets with the rates is floating: its lines float
// for half of it or more, so do its filing's, or its name, or its
// filing's, says it is a floating rate fund. Its value earns a floating
// rate: its implied yield (declared where a ledger record states it),
// else what its indicated distributions pay over its price, else its
// distribution yield. So its income moves one percent
// of its value for rates one percent higher, and along the forward
// curve. An index fund whose name says FLOAT ADJUSTED, its lines fixed,
// pays its distributions as they come, whatever the rates.
func TestAFloatingRateFund(t *testing.T) {
	t.Parallel()
	floatingLines := &LineSummary{AsOf: day, FloatingPct: 96, DebtPct: 96}
	fixedLines := &LineSummary{AsOf: day, DebtPct: 96}
	for _, c := range []struct {
		what     string
		name     string
		fund     Fund
		ledger   *float64
		paid     bool
		floating bool
		income   float64
		basis    Basis
	}{
		{"its lines float", "", Fund{Lines: floatingLines}, nil, true, true, 2400, BasisIndicated},
		{"its filing's lines float", "", Fund{Lines: &LineSummary{AsOf: day, FloatingPct: 30, DebtPct: 96},
			FilingFloatingPct: num(80)}, nil, true, true, 2400, BasisIndicated},
		{"its name says so", "EXAMPLE FLOATING RATE BOND ETF", Fund{Lines: fixedLines}, nil, true, true, 2400,
			BasisIndicated},
		{"its filing's name says so", "", Fund{Lines: fixedLines, FilingName: "Example Floating Rate Bond Fund"},
			nil, true, true, 2400, BasisIndicated},
		{"an implied yield", "", Fund{Lines: floatingLines, YieldToMaturity: &Dated{Value: 5, Day: day}}, nil, true,
			true, 2500, BasisStated},
		{"a ledger record's yield", "", Fund{Lines: floatingLines}, num(5), true, true, 2500, BasisDeclared},
		{"no payment", "", Fund{Lines: floatingLines, DistributionYield: &Dated{Value: 4, Day: day}}, nil, false, true,
			2000, BasisStated},
		{"a float-adjusted index", "EXAMPLE FLOAT ADJUSTED BOND INDEX ETF", Fund{Lines: fixedLines,
			FilingFloatingPct: num(10)}, nil, true, false, 2400, BasisIndicated},
	} {
		in := inputs()
		in.Curves["USD"] = slopedCurve()
		h := etf("A", "EXFR", 1000)
		h.Name = c.name
		in.Holdings = []Holding{h}
		f := c.fund
		in.Funds[h.SecurityID] = &f
		if c.ledger != nil {
			in.Terms[InstrumentKey{"broker", "EXFR"}] = Terms{Yield: c.ledger}
		}
		if c.paid {
			for m := 0; m < 12; m++ {
				pay(in, h.HoldingKey, "A", date(2025, 7+m, 10), 200, num(1000))
			}
		}
		b := Compute(in)
		p := position(b, h.HoldingKey)
		if p.Kind != KindBondFund || p.Tags[TagFloating] != c.floating {
			t.Errorf("%s: kind %s, tags %v", c.what, p.Kind, p.Tags)
		}
		near(t, c.what+" income 12m", p.Income12, c.income, 1e-6)
		moves := 0.0
		if c.floating {
			moves = h.Value / 100
		}
		near(t, c.what+" income 100bp", p.Income100bp, moves, 1e-6)
		var constant, forward float64
		bases := map[Basis]bool{}
		for _, a := range b.Amounts(nil, b.NextMonths(12), DefaultAssumptions) {
			constant += a.Value
			bases[a.Basis] = true
		}
		for _, a := range b.Amounts(nil, b.NextMonths(12), Assumptions{Path: PathForward, Rule: RuleCash}) {
			forward += a.Value
			if c.floating && a.Basis != BasisForward {
				t.Errorf("%s: forward basis %s", c.what, a.Basis)
			}
		}
		if len(bases) != 1 || !bases[c.basis] || (forward > constant+1) != c.floating ||
			(!c.floating && math.Abs(forward-constant) > 1e-9) {
			t.Errorf("%s: bases %v; constant %v, forward %v", c.what, bases, constant, forward)
		}
	}
}

// A floating fund whose ledger records its dividend, with no implied
// yield, floats at what that dividend pays over its price, declared: 0.25
// a share a month over a price of 50 is 6%.
func TestAFloatingFundsDeclaredDividend(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = slopedCurve()
	h := etf("A", "EXFR", 1000)
	in.Holdings = []Holding{h}
	in.Funds[h.SecurityID] = &Fund{Lines: &LineSummary{AsOf: day, FloatingPct: 96, DebtPct: 96}}
	in.Dividends[InstrumentKey{"broker", "EXFR"}] = canonical.DeclaredDividend{Amount: 0.25, Frequency: 12}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if !p.Tags[TagFloating] {
		t.Errorf("tags %v", p.Tags)
	}
	near(t, "income 12m", p.Income12, 3000, 1e-6)
	near(t, "income 100bp", p.Income100bp, h.Value/100, 1e-6)
	for _, a := range b.Amounts(nil, b.NextMonths(12), DefaultAssumptions) {
		if a.Basis != BasisDeclared {
			t.Errorf("an amount %+v", a)
		}
	}
}

// A fund's name says it floats by a word of its own, in any case:
// FLOATING, FLOATER, FLOATERS, FLTG, FRN, FRNS, FLOAT RATE or FLOAT-RATE,
// a broker's FLT RT or FLOAT RT, VARIABLE RATE or VAR RATE. So does a
// fund of loans or of CLOs: SENIOR LOAN, BANK LOAN, LEVERAGED LOAN,
// FLOATING-RATE LOAN, CLO, CLOS. A bare FLOAT does not, as in FLOAT
// ADJUSTED, nor does a longer word that begins with one of them, nor a
// coupon fixed until it floats, in full or short: FIXED-TO-FLOATING, FIXED
// TO FLOAT, FIXED TO FLOATER, FXD TO FLTG. Nor does a fund of variable-rate
// preferreds, which are mostly fixed until they float.
func TestAFloatingName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		want bool
	}{
		{"EXAMPLE FLOATING RATE BOND ETF", true}, {"Example Floating Rate Fund", true}, {"EXAMPLE FLOATER ETF", true},
		{"example floaters fund", true}, {"EXAMPLE FLTG RT NT FD", true}, {"EXAMPLE FRN FUND", true},
		{"example frn fund", true}, {"EXAMPLE FRNs ETF", true}, {"EXAMPLE FLOAT RATE ETF", true},
		{"Example Float-Rate Fund", true}, {"EXAMPLE FLT RT NT ETF", true}, {"EXAMPLE FLOAT RT BD FD", true},
		{"EXAMPLE VARIABLE RATE FUND", true}, {"Example Variable-Rate Income Fund", true},
		{"EXAMPLE VAR RATE DEMAND FD", true}, {"EXAMPLE SENIOR LOAN ETF", true}, {"Example Senior Loans Fund", true},
		{"EXAMPLE BANK LOAN ETF", true}, {"EXAMPLE LEVERAGED LOAN INDEX ETF", true},
		{"EXAMPLE FLOATING-RATE LOAN FUND", true}, {"EXAMPLE AAA CLO ETF", true}, {"EXAMPLE AAA CLOS ETF", true},
		{"EXAMPLE FLOAT ADJUSTED INDEX ETF", false}, {"EXAMPLE FRNT FUND", false}, {"EXAMPLE FLOATINGS FUND", false},
		{"EXAMPLE FLOAT FUND", false}, {"EXAMPLE CLOUD COMPUTING ETF", false}, {"EXAMPLE CLOSED END FUND", false},
		{"EXAMPLE LOAN FUND", false}, {"EXAMPLE FIXED-TO-FLOATING RATE PREFERRED ETF", false},
		{"EXAMPLE FIXED-TO-FLOAT RATE PREFERRED ETF", false}, {"Example Fixed-to-Floating Rate Income Fund", false},
		{"EXAMPLE FIXED TO FLOATER FUND", false}, {"EXAMPLE FIXED TO FLOATERS ETF", false},
		{"EXAMPLE FXD TO FLTG RT PFD ETF", false},
		{"EXAMPLE FIXED TO FLT RT PFD FD", false}, {"EXAMPLE VARIABLE RATE PREFERRED ETF", false},
		{"Example Variable-Rate Preferred Securities Fund", false}, {"EXAMPLE VAR RATE PFD ETF", false},
		{"", false},
	} {
		if got := floatingName(c.name); got != c.want {
			t.Errorf("%q: %v, want %v", c.name, got, c.want)
		}
	}
}

// A floating fund's indicated distributions that rest on one calendar
// month, or on one half of a payer twice a year, read a fraction of the
// year's. It floats at its distribution yield instead, else at its
// trailing yield, and keeps its spread to the benchmark: rates half a
// percent lower take half a percent of its value. Two pay months a year
// apart, of a fund sold and bought again, are one calendar month. With no
// yield stated, its distributions pay as indicated, as they come: rates do
// not move them, and they never fall below zero. A fund held the whole
// year that paid in one month is a yearly payer: it floats at what that
// payment pays over its price, once a year.
func TestAnUnderReadFloatingFund(t *testing.T) {
	t.Parallel()
	oneMonth := func(in *Inputs, h Holding) {
		pay(in, h.HoldingKey, "A", date(2026, 5, 10), 200, num(1000))
		in.Daily[h.HoldingKey] = held(Span{From: dayOf(date(2026, 5, 1)), To: year, Amount: 50000})
	}
	// The interim's quantity is unknown, so it gives no rate: the final's
	// alone pays once a year.
	oneHalf := func(in *Inputs, h Holding) {
		pay(in, h.HoldingKey, "A", date(2025, 12, 10), 200, nil)
		pay(in, h.HoldingKey, "A", date(2026, 6, 10), 200, num(1000))
	}
	// Sold in July 2025 and bought again in June 2026.
	aYearApart := func(in *Inputs, h Holding) {
		pay(in, h.HoldingKey, "A", date(2025, 6, 20), 200, num(1000))
		pay(in, h.HoldingKey, "A", date(2026, 6, 10), 200, num(1000))
		in.Daily[h.HoldingKey] = held(Span{From: 0, To: dayOf(date(2025, 7, 1)), Amount: 50000},
			Span{From: dayOf(date(2026, 6, 1)), To: year, Amount: 50000})
	}
	yearly := func(in *Inputs, h Holding) {
		pay(in, h.HoldingKey, "A", date(2026, 6, 10), 2300, num(1000))
		in.Daily[h.HoldingKey] = held(Span{From: 0, To: year, Amount: 50000})
	}
	for _, c := range []struct {
		what              string
		paid              func(*Inputs, Holding)
		stated, trailing  *float64
		income            float64
		basis             Basis
		lowered, moves100 float64
	}{
		{"one pay month, a distribution yield", oneMonth, num(4.6), nil, 2300, BasisStated, 2300 - 250, 500},
		{"one half, a distribution yield", oneHalf, num(4.6), nil, 2300, BasisStated, 2300 - 250, 500},
		{"a year apart, a distribution yield", aYearApart, num(4.6), nil, 2300, BasisStated, 2300 - 250, 500},
		{"one pay month, a trailing yield", oneMonth, nil, num(4.4), 2200, BasisStated, 2200 - 250, 500},
		{"one pay month alone", oneMonth, nil, nil, 200, BasisIndicated, 200, 0},
		{"once a year, held all year", yearly, num(4), nil, 2300, BasisIndicated, 2300 - 250, 500},
	} {
		in := inputs()
		in.Curves["USD"] = flatCurve(4)
		h := etf("A", "EXFR", 1000)
		in.Holdings = []Holding{h}
		f := Fund{Lines: &LineSummary{AsOf: day, FloatingPct: 96, DebtPct: 96}}
		if c.stated != nil {
			f.DistributionYield = &Dated{Value: *c.stated, Day: day}
		}
		if c.trailing != nil {
			f.TrailingYield = &Dated{Value: *c.trailing, Day: day}
		}
		in.Funds[h.SecurityID] = &f
		c.paid(in, h)
		b := Compute(in)
		p := position(b, h.HoldingKey)
		near(t, c.what+" income 12m", p.Income12, c.income, 1e-6)
		near(t, c.what+" income 100bp", p.Income100bp, c.moves100, 1e-6)
		lowered := 0.0
		for _, a := range b.Amounts(nil, b.NextMonths(12), Assumptions{Path: PathConstant, Rule: RuleCash,
			ShiftBP: -50}) {
			if a.Basis != c.basis || a.Value < 0 {
				t.Errorf("%s: an amount %+v", c.what, a)
			}
			lowered += a.Value
		}
		near(t, c.what+" 50bp lower", &lowered, c.lowered, 1e-6)
	}
}

// A floating fund that pays twice a year pays its floating income on its
// halves' own days a year on, each payment the accrual of the six months
// since the pay day before: 2% of its value, on a flat curve of 4%, pays
// about 500 in November and in May. So it does where its November 2025
// payment is a special no half takes, its halves' newest pay days
// November 2024 and May 2026: a pay day before the newest is no period's
// end. So it does too where its newest pay day, in May 2026, is of no
// rate and of the other half than its last rated one.
func TestAFloatingFundPaysTwiceAYear(t *testing.T) {
	t.Parallel()
	type paid struct {
		d          *time.Time
		perShare   float64
		noQuantity bool
	}
	for _, c := range []struct {
		what string
		paid []paid
	}{
		{"a refused special", []paid{{date(2024, 11, 10), 0.5, false}, {date(2025, 5, 10), 0.5, false},
			{date(2025, 11, 10), 4, false}, {date(2026, 5, 10), 0.5, false}}},
		{"its newest pay day of the other half", []paid{{date(2024, 11, 10), 0.5, false},
			{date(2025, 5, 20), 0.5, false}, {date(2025, 11, 10), 0.5, false}, {date(2026, 5, 10), 0.5, true}}},
	} {
		in := inputs()
		in.Curves["USD"] = flatCurve(4)
		h := etf("A", "EXFR", 1000)
		in.Holdings = []Holding{h}
		in.Funds[h.SecurityID] = &Fund{Lines: &LineSummary{AsOf: day, FloatingPct: 96, DebtPct: 96}}
		for _, p := range c.paid {
			qty := num(1000)
			if p.noQuantity {
				qty = nil
			}
			pay(in, h.HoldingKey, "A", p.d, p.perShare*1000, qty)
		}
		b := Compute(in)
		near(t, c.what+" income 12m", position(b, h.HoldingKey).Income12, 1000, 1e-6)
		for _, d := range []*time.Time{date(2026, 11, 1), date(2027, 5, 1)} {
			got := incomeOf(b, h.HoldingKey, monthIndex(b, *d), RuleCash, PathConstant, 0)
			if math.Abs(got-500) > 5 {
				t.Errorf("%s: %s pays %v", c.what, d.Format("January 2006"), got)
			}
		}
	}
}

// A floating fund that pays quarterly pays its floating income on its own
// pay days, three months apart from its last on 10 June: each payment is
// what its value accrues over the quarter since the one before, and the
// current month, which holds none of them, books nothing. Its twelve
// months still earn its distributions' 4.8% on its value, and move with
// the curve and the shift: half a percent lower takes half a percent of
// its value, the forward curve adds, and a shift far below zero earns
// nothing, never a charge. The calendar lists its payments in their
// months, and its positions sum to its projection. A monthly payer
// accrues month by month, the current month's part with it.
func TestAQuarterlyFloatingFundPaysOnItsPayDays(t *testing.T) {
	t.Parallel()
	read := func(step int) *Book {
		in := inputs()
		in.Curves["USD"] = slopedCurve()
		h := etf("A", "EXFR", 1000)
		in.Holdings = []Holding{h}
		in.Funds[h.SecurityID] = &Fund{Lines: &LineSummary{AsOf: day, FloatingPct: 96, DebtPct: 96}}
		for m := 0; m < 12; m += step {
			pay(in, h.HoldingKey, "A", date(2026, 6-m, 10), 200*float64(step), num(1000))
		}
		return Compute(in)
	}
	b := read(3)
	h := b.Positions[0]
	near(t, "income 12m", h.Income12, 2400, 1e-6)
	near(t, "income 100bp", h.Income100bp, h.Value/100, 1e-6)
	for _, p := range partsOf(b, h.HoldingKey, RuleCash) {
		if p.Day == nil || !p.MonthOnly || p.Day.Day() != 10 || p.Month < 3 || p.Month%3 != 0 || p.Principal == 0 {
			t.Errorf("a part %+v", p)
		}
	}
	total := func(a Assumptions) float64 {
		sum := 0.0
		for _, x := range b.Amounts(nil, b.NextMonths(12), a) {
			if x.Value < 0 {
				t.Errorf("%+v: an amount %+v", a, x)
			}
			sum += x.Value
		}
		return sum
	}
	near(t, "50bp lower", num(total(Assumptions{Path: PathConstant, Rule: RuleCash, ShiftBP: -50})), 2400-250, 1e-6)
	near(t, "far below zero", num(total(Assumptions{Path: PathConstant, Rule: RuleCash, ShiftBP: -2000})), 0, 1e-9)
	if forward := total(Assumptions{Path: PathForward, Rule: RuleCash}); forward < 2400+1 {
		t.Errorf("forward %v", forward)
	}
	w := b.NextMonths(12)
	amounts := b.Amounts(nil, w, DefaultAssumptions)
	cal := b.Calendar(amounts)
	if len(cal) != 4 {
		t.Fatalf("calendar %+v", cal)
	}
	for i, r := range cal {
		if r.Day != nil || !r.Month.Equal(*date(2026, 9+3*i, 1)) || math.Abs(r.Value-600) > 6 {
			t.Errorf("calendar row %d: %s %v %v", i, r.Month.Format(time.DateOnly), r.Day, r.Value)
		}
	}
	rows := b.ProjectionRows(amounts, w, 1)
	positions := b.IncomeRows(GrainPositions, amounts, nil, nil)
	if len(positions) != 1 || math.Abs(positions[0].Expected-rows[len(rows)-2].Expected) > 1e-9 {
		t.Errorf("positions %+v, projection %+v", positions, rows[len(rows)-2])
	}
	monthly := read(1)
	parts := partsOf(monthly, monthly.Positions[0].HoldingKey, RuleCash)
	if len(parts) == 0 || parts[0].Month != 0 || parts[0].Day != nil {
		t.Errorf("monthly: parts %+v", parts)
	}
	for _, p := range parts {
		if p.Day != nil {
			t.Errorf("monthly: a part %+v", p)
		}
	}
	near(t, "monthly income 12m", monthly.Positions[0].Income12, 2400, 1e-6)
}
