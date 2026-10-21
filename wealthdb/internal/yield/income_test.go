package yield

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// incomeBook is an invented book on day: a bond paying in December and
// June, a bill maturing at the end of June, a deposit at a stated rate, a
// stock that paid nothing, a fund nothing describes, and an account that
// received rent in March.
func incomeBook() (*Book, []ReceivedLine) {
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
	b := bondHolding("broker", "A", "QB", 20000, 20000)
	bill := bondHolding("broker", "A", "QBILL", 5000, 4990)
	d := cash("bank", "S", "USD", 12000)
	s := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 500, Amount: 500, Quantity: num(5)}
	f := fund("QBF", "fixed_income", 1000)
	in.Holdings = []Holding{b, bill, d, s, f}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(6), Maturity: date(2030, 12, 15)}
	zero := 0
	in.Terms[InstrumentKey{"broker", "QBILL"}] = Terms{Frequency: &zero, Maturity: date(2026, 6, 30)}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 3}}
	in.Accounts[AccountKey{"broker", "A"}] = Account{Name: "Brokerage", TaxWrapper: personal}
	in.Situs[b.HoldingKey] = "US"
	in.Withholding["US"] = map[string]float64{canonical.WithholdingDefault: 30, personal: 15}
	in.AccountPayments = []AccountPayment{{AccountCurrency: AccountCurrency{AccountKey{"bank", "S"}, "USD"},
		Day: *date(2026, 3, 1), Amount: 900, Type: TypeRent}}
	received := []ReceivedLine{
		{Holding: b.HoldingKey, Day: *date(2026, 6, 15), Type: TypeCoupons, Value: 600},
		{Holding: HoldingKey{"bank", "S", "income:rent:USD"}, Day: *date(2026, 3, 1), Type: TypeRent, Value: 900},
		{Holding: b.HoldingKey, Day: *date(2025, 12, 15), Type: TypeCoupons, Value: 600},
		// Booked today, at any hour: received.
		{Holding: b.HoldingKey, Day: date(2026, 6, 15).Add(10 * time.Hour), Type: TypeCoupons, Value: 5},
		// After today: not yet received, whatever a source says.
		{Holding: b.HoldingKey, Day: *date(2026, 6, 20), Type: TypeCoupons, Value: 1},
	}
	return Compute(in), received
}

var personal = string(canonical.TaxWrapperTaxablePersonal)

var year2026 = Window{From: *date(2026, 1, 1), To: date(2026, 12, 31).Add(24*time.Hour - time.Second)}

// This year: received through today, expected for the rest; the current
// month holds both halves.
func TestThisYearsProjection(t *testing.T) {
	t.Parallel()
	b, received := incomeBook()
	amounts := b.Amounts(received, year2026, DefaultAssumptions)
	rows := b.ProjectionRows(amounts, year2026, 1)
	if len(rows) != 14 || !rows[12].Total || !rows[13].Average {
		t.Fatalf("%d rows", len(rows))
	}
	jun, dec := rows[5], rows[11]
	if jun.Received != 605 || jun.Status != "mixed" || rows[2].Received != 900 || rows[2].Status != "received" {
		t.Errorf("June %+v March %+v", jun, rows[2])
	}
	// December: the coupon of 600, the deposit's 30 a month, and the
	// rent of March repeated a year later only from 2027.
	if math.Abs(dec.Expected-630) > 1e-9 || dec.Status != "expected" {
		t.Errorf("December %+v", dec)
	}
	// The rest of June: the deposit's 30 for the half month left, and
	// the bill's discount of 10 at its end; its face returns then.
	if math.Abs(jun.Expected-(30*15.0/30+10)) > 1e-9 || jun.Principal != 5000 {
		t.Errorf("rest of June %v, principal %v", jun.Expected, jun.Principal)
	}
	var names []string
	for _, r := range b.IncomeRows(GrainPositions, amounts, nil, nil) {
		names = append(names, r.Holding.PositionKey+":"+r.Basis)
	}
	// Every holding is a row, the ones that pay nothing too.
	if got := strings.Join(names, " "); !strings.Contains(got, "QSTK:(none)") || !strings.Contains(got, "QB:declared") ||
		!strings.Contains(got, "cash:USD:stated") || !strings.Contains(got, "QBF:unknown") {
		t.Errorf("positions %s", got)
	}
	// The coupon is withheld at the wrapper's 15%; the deposit's situs is
	// unknown, so nothing is.
	if dec.Withheld == nil || math.Abs(*dec.Withheld-90) > 1e-9 {
		t.Errorf("December withheld %v", dec.Withheld)
	}
	if rows[12].Basis != "declared=76% stated=24%" {
		t.Errorf("basis %q", rows[12].Basis)
	}
	near(t, "monthly average", &rows[13].Received, rows[12].Received/12, 1e-9)
}

// Under every rule and path, a month's projection equals the sum of its
// calendar rows, which list no payment of zero; and every grain sums to
// the projection, received and expected.
func TestTheViewsReconcile(t *testing.T) {
	t.Parallel()
	b, received := incomeBook()
	for _, rule := range Rules {
		for _, path := range Paths {
			a := Assumptions{Path: path, Rule: rule, ShiftBP: 25}
			amounts := b.Amounts(received, year2026, a)
			rows := b.ProjectionRows(amounts, year2026, 1)
			cal := map[time.Time]float64{}
			for _, c := range b.Calendar(amounts) {
				if c.Value == 0 && !c.Principal {
					t.Errorf("%v: a zero payment %+v", a, c)
				}
				if !c.Principal {
					cal[c.Month] += c.Value
				}
			}
			for _, r := range rows[:12] {
				if math.Abs(cal[r.Period]-r.Expected) > 1e-9 {
					t.Errorf("%v %s: calendar %v, projection %v", a, r.Period.Format("2006-01"), cal[r.Period], r.Expected)
				}
			}
			for _, g := range []Grain{GrainSources, GrainPortfolios, GrainAccounts, GrainPositions} {
				var got, want [2]float64
				for _, r := range b.IncomeRows(g, amounts, nil, nil) {
					got[0] += r.Received
					got[1] += r.Expected
				}
				want[0], want[1] = rows[12].Received, rows[12].Expected
				if math.Abs(got[0]-want[0]) > 1e-9 || math.Abs(got[1]-want[1]) > 1e-9 {
					t.Errorf("%v %s: %v, the projection %v", a, g, got, want)
				}
			}
		}
	}
}

// A quarterly or annual period is the calendar's, cut by the window; the
// average is over the window's months.
func TestCalendarPeriods(t *testing.T) {
	t.Parallel()
	b, _ := incomeBook()
	w := Window{From: *date(2026, 11, 1), To: date(2027, 10, 31).Add(24*time.Hour - time.Second)}
	amounts := b.Amounts(nil, w, DefaultAssumptions)
	rows := b.ProjectionRows(amounts, w, 3)
	var starts []string
	for _, r := range rows[:len(rows)-2] {
		starts = append(starts, r.Period.Format(time.DateOnly))
	}
	if strings.Join(starts, " ") != "2026-10-01 2027-01-01 2027-04-01 2027-07-01 2027-10-01" {
		t.Errorf("quarters %v", starts)
	}
	// The fourth quarter of 2026 holds November and December alone.
	near(t, "Q4 2026", &rows[0].Expected, 2*30+600, 1e-9)
	near(t, "average", &rows[len(rows)-1].Expected, rows[len(rows)-2].Expected/12, 1e-9)
	if years := b.ProjectionRows(amounts, w, 12); len(years) != 4 || years[0].Period.Year() != 2026 {
		t.Errorf("years %+v", years)
	}
}

// A window that cuts a month takes the share of its days: the deposit
// accrues from November 15 to February 14, and the coupon of December
// 15 counts whole.
func TestAWindowThatCutsMonths(t *testing.T) {
	t.Parallel()
	b, _ := incomeBook()
	w := Window{From: *date(2026, 11, 15), To: date(2027, 2, 14).Add(24*time.Hour - time.Second)}
	rows := b.ProjectionRows(b.Amounts(nil, w, DefaultAssumptions), w, 1)
	total := rows[len(rows)-2]
	near(t, "expected", &total.Expected, 30*(16.0/30+1+1+14.0/28)+600, 1e-9)
	near(t, "average", &rows[len(rows)-1].Expected, total.Expected/(16.0/30+1+1+14.0/28), 1e-9)
}

// The quality column reads the shift and, under the roll rule, the rungs
// of the reinvested income.
func TestTheQualityColumn(t *testing.T) {
	t.Parallel()
	b, _ := incomeBook()
	w := Window{From: *date(2026, 7, 1), To: date(2026, 9, 30).Add(24*time.Hour - time.Second)}
	rows := b.ProjectionRows(b.Amounts(nil, w, Assumptions{Path: PathConstant, Rule: RuleRoll, ShiftBP: -50}), w, 3)
	// The bill rolls along the curve; the deposit floats with the shift.
	if q := rows[0].Quality; q != "roll shift=-50bp" {
		t.Errorf("quality %q", q)
	}
	rows = b.ProjectionRows(b.Amounts(nil, w, DefaultAssumptions), w, 3)
	// Its broker account states no cash rate: the bill's face earns what
	// nothing states.
	if q := rows[0].Quality; q != "cash" {
		t.Errorf("quality %q", q)
	}
}

// Under the forward path a floating amount rests on the curve.
func TestTheForwardBasis(t *testing.T) {
	t.Parallel()
	b, _ := incomeBook()
	w := Window{From: *date(2026, 7, 1), To: date(2027, 6, 30).Add(24*time.Hour - time.Second)}
	rows := b.ProjectionRows(b.Amounts(nil, w, Assumptions{Path: PathForward, Rule: RuleCash}), w, 12)
	if !strings.Contains(rows[0].Basis, "forward=") || strings.Contains(rows[0].Basis, "stated") {
		t.Errorf("basis %q", rows[0].Basis)
	}
}

// A window in the past is all received.
func TestLastYearsProjection(t *testing.T) {
	t.Parallel()
	b, received := incomeBook()
	w := Window{From: *date(2025, 1, 1), To: date(2025, 12, 31).Add(24*time.Hour - time.Second)}
	rows := b.ProjectionRows(b.Amounts(received, w, DefaultAssumptions), w, 3)
	if len(rows) != 6 || rows[3].Received != 600 || rows[3].Status != "received" || rows[4].Expected != 0 ||
		rows[4].Received != 600 {
		t.Errorf("2025: %+v %+v", rows[3], rows[4])
	}
}

// A share prints as a percentage, a sliver as <1%; a basis alone prints
// alone, however the shares round.
func TestShares(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		by   map[Basis]float64
		want string
	}{
		{map[Basis]float64{BasisIndicated: 996, BasisStated: 4}, "indicated=100% stated<1%"},
		{map[Basis]float64{BasisDeclared: 3, BasisIndicated: 1}, "declared=75% indicated=25%"},
		{map[Basis]float64{BasisRealized: 7}, "realized"},
		{map[Basis]float64{BasisUnknown: 0, BasisStated: 5}, "stated"},
		{map[Basis]float64{BasisDeclared: 5, BasisIndicated: 5}, "declared=50% indicated=50%"},
	} {
		total := 0.0
		for _, v := range c.by {
			total += v
		}
		if got := basisShares(c.by, total); got != c.want {
			t.Errorf("%v: %q, want %q", c.by, got, c.want)
		}
	}
}

// Rent received with no holding named repeats on its day of every year
// the projection reaches.
func TestRentRepeatsEveryYear(t *testing.T) {
	t.Parallel()
	b, _ := incomeBook()
	for _, y := range []int{2027, 2028} {
		w := Window{From: *date(y, 3, 1), To: date(y, 3, 31).Add(24*time.Hour - time.Second)}
		rent := 0.0
		for _, a := range b.Amounts(nil, w, DefaultAssumptions) {
			if a.Type == TypeRent {
				rent += a.Value
			}
		}
		if rent != 900 {
			t.Errorf("rent in March %d: %v", y, rent)
		}
	}
}

// Last year's figure is blank where that window is not over, and zero
// where it is and nothing was received.
func TestLastYearIsBlankUntilItIsOver(t *testing.T) {
	t.Parallel()
	b, _ := incomeBook()
	for _, r := range b.IncomeRows(GrainSources, nil, nil, nil) {
		if r.LastYear != nil {
			t.Errorf("not over: %+v", r)
		}
	}
	for _, r := range b.IncomeRows(GrainSources, nil, []Amount{}, nil) {
		if r.LastYear == nil || *r.LastYear != 0 {
			t.Errorf("over: %+v", r)
		}
	}
}

// A rung that earns nothing reads 0%.
func TestARungThatEarnsNothing(t *testing.T) {
	t.Parallel()
	q := tally{rungs: map[Rung]float64{RungRoll: 50, RungRollCash: 0}, reinvested: 50}
	if got := q.quality(); got != "roll=100% roll_cash=0%" {
		t.Errorf("quality %q", got)
	}
}

// An account's rent in two currencies is two holdings, each converted at
// its own currency's rate.
func TestRentInTwoCurrencies(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.FXRates["CHF"] = 1.1
	k := AccountKey{"bank", "S"}
	in.AccountPayments = []AccountPayment{
		{AccountCurrency: AccountCurrency{k, "USD"}, Day: *date(2026, 3, 1), Amount: 1000, Type: TypeRent},
		{AccountCurrency: AccountCurrency{k, "CHF"}, Day: *date(2026, 3, 1), Amount: 1500, Type: TypeRent},
	}
	b := Compute(in)
	w := Window{From: *date(2027, 3, 1), To: date(2027, 3, 31).Add(24*time.Hour - time.Second)}
	got := map[string]float64{}
	for _, a := range b.Amounts(nil, w, DefaultAssumptions) {
		got[a.Holding.PositionKey] += a.Value
	}
	if got["income:rent:USD"] != 1000 || math.Abs(got["income:rent:CHF"]-1650) > 1e-9 {
		t.Errorf("rent %v", got)
	}
}
