package yield

import (
	"math"
	"testing"

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

// A fund's name says it floats by a word of its own: FLOATING, FLOATER,
// FLOATERS, FLTG, FRN, FLOAT RATE or FLOAT-RATE, in any case. A bare
// FLOAT does not, as in FLOAT ADJUSTED, nor does a longer word that begins
// with one of them.
func TestAFloatingName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"EXAMPLE FLOATING RATE BOND ETF": true, "Example Floating Rate Fund": true, "EXAMPLE FLOATER ETF": true,
		"example floaters fund": true, "EXAMPLE FLTG RT NT FD": true, "EXAMPLE FRN FUND": true, "example frn fund": true,
		"EXAMPLE FLOAT RATE ETF": true, "Example Float-Rate Fund": true, "EXAMPLE FLOAT ADJUSTED INDEX ETF": false,
		"EXAMPLE FRNT FUND": false, "EXAMPLE FLOATINGS FUND": false, "EXAMPLE FLOAT FUND": false, "": false,
	} {
		if got := floatingName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

// A floating fund's indicated distributions that rest on one calendar
// month, or on one half of a payer twice a year, read a fraction of the
// year's. It floats at its distribution yield instead, else at its
// trailing yield, and keeps its spread to the benchmark: rates half a
// percent lower take half a percent of its value. Two pay months a year
// apart are one calendar month. With no yield stated, its distributions
// pay as indicated, as they come: rates do not move them, and they never
// fall below zero.
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
