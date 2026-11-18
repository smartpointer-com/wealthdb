package yield

import (
	"math"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A fund's lines: the yield and duration of the lines that imply them,
// weighted; the shares of floating, defaulted and paid-in-kind lines. A
// line's price is its value in its own currency per 100 of face, and its
// yield to worst wins over its yield to maturity.
func TestFundLines(t *testing.T) {
	t.Parallel()
	m1 := date(2031, 6, 15)
	lines := []FundLine{{Weight: 40, Maturity: m1}, {Weight: 20, Kind: "Floating", Floats: true}, {Weight: 10, Defaulted: true},
		{Weight: 10, InKind: true}, {Weight: 20, Maturity: date(2028, 6, 15)}}
	infos := []LineInfo{{Yield: num(5), Duration: num(4)}, {}, {}, {}, {Yield: num(3), Duration: num(2)}}
	s := summarizeLines(lines, infos, day, false)
	if s.CoveredPct != 60 || s.FloatingPct != 20 || s.DefaultPct != 10 || s.PaidInKindPct != 10 {
		t.Errorf("shares %+v", s)
	}
	if math.Abs(*s.Yield-(40*5+20*3)/60.0) > 1e-12 || math.Abs(*s.Duration-(40*4+20*2)/60.0) > 1e-12 {
		t.Errorf("yield %v duration %v", *s.Yield, *s.Duration)
	}
	in := &Inputs{Conventions: map[string]canonical.YieldConvention{}}
	l := FundLine{Value: 1000, Quantity: 1000, Currency: "EUR", Maturity: m1, Coupon: num(4),
		Rate: num(0.9)} // euros per dollar
	info := readLine(in, l, day)
	if math.Abs(*info.Price-90) > 1e-9 || *info.Yield <= 4 {
		t.Errorf("price %v yield %v", *info.Price, *info.Yield)
	}
	l.YTM, l.YTW = num(5), num(4.5)
	if info := readLine(in, l, day); *info.Yield != 4.5 {
		t.Errorf("to worst %v", *info.Yield)
	}
	// A line in default, or paid in kind, implies nothing.
	l.YTM, l.YTW, l.Defaulted = nil, nil, true
	if info := readLine(in, l, day); info.Yield != nil || !info.Default {
		t.Errorf("in default %+v", info)
	}
	l.Defaulted, l.InKind = false, true
	if info := readLine(in, l, day); info.Yield != nil {
		t.Errorf("in kind %+v", info)
	}
}

// A fund's debt and equity shares are of the whole fund: a composition
// that lists 60% of it leaves the rest as neither. A line its asset type
// maps nowhere is debt where it states a coupon, not where it only
// matures, as a future does. Debt of under a year is counted apart, and a
// line of type other is a derivative.
func TestALinesShareOfTheWholeFund(t *testing.T) {
	t.Parallel()
	lines := []FundLine{{Weight: 30, Maturity: date(2026, 12, 15), Coupon: num(0)},
		{Weight: 10, Maturity: date(2031, 6, 15), Coupon: num(4)},
		{Weight: 15, Class: "public_equity"}, {Weight: 5, Maturity: date(2026, 9, 15)}, {Weight: 0, Class: "other"}}
	s := summarizeLines(lines, make([]LineInfo, len(lines)), day, false)
	if s.DebtPct != 40 || s.ShortDebtPct != 30 || s.EquityPct != 15 || !s.Derivatives {
		t.Errorf("shares %+v", s)
	}
}
