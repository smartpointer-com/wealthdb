package gold

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The look-back's parts are the summary's own figures over the same
// window, spread over its months: operating income without the lines
// the projection projects (the dividend), operating outflows positive
// and by class, the vehicles signed. The fixture's lines are invented.
func TestThePlansLookbackIsTheSummarySpread(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	lb, err := CashflowPlanLookback(ctx, db, 0, 3500000, "USD")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := CashflowSummary(ctx, db, 0, 3500000, "USD", "total")
	if err != nil || len(rows) != 1 {
		t.Fatalf("summary: %v %v", rows, err)
	}
	s := rows[0]
	n := float64(lb.Months)
	// From January, the month of the pool's first line, to February,
	// the window's last.
	if lb.Months != 2 {
		t.Errorf("months = %d, want 2", lb.Months)
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	near("other_income", lb.OtherIncome*n, mustFloat(t, s.OperatingIn, "operating_in")-900)
	near("operating_out", lb.OperatingOut*n, mustFloat(t, s.OperatingOut, "operating_out"))
	near("vehicles", lb.Vehicles*n, mustFloat(t, s.Vehicles, "vehicles"))
	classes := 0.0
	for _, c := range PlanOutClasses {
		classes += lb.OutClasses[c]
	}
	near("the classes' sum", classes, lb.OperatingOut)
	near("taxes", lb.OutClasses["taxes"]*n, mustFloat(t, s.Taxes, "taxes"))
	// A window after the last line spreads nothing over its months.
	if empty, err := CashflowPlanLookback(ctx, db, 4000000, 5000000, "USD"); err != nil || empty.Months != 1 ||
		empty.OperatingOut != 0 || math.Signbit(empty.OperatingOut) || math.Signbit(empty.OtherDebt) {
		t.Errorf("an empty window: %+v %v", empty, err)
	}
	// One before the first line has none.
	if none, err := CashflowPlanLookback(ctx, db, -5000000, -4000000, "USD"); err != nil || none.Months != 0 {
		t.Errorf("a window before the first line: %+v %v", none, err)
	}
}

// The other income splits by the operating-in class of its lines and
// the account they arrived on, each over the same months, and the split
// sums to the whole: the wage, the refund and the unemployment benefit
// on CASH are earnings, other receipts and benefits, a staking reward on
// BROK is yield the projection does not read, and the projected dividend
// is in none. A window with no
// other income has no split. The fixture's lines are invented.
func TestThePlansOtherIncomeSplitsByClassAndAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "R-STAKE", account: "BROK", kind: "deposit", amount: 30, income: "INCOME_STAKING"},
		{id: "R-DOLE", account: "CASH", kind: "deposit", amount: 20, income: "INCOME_UNEMPLOYMENT"},
	})
	lb, err := CashflowPlanLookback(ctx, db, 0, 3500000, "USD")
	if err != nil {
		t.Fatal(err)
	}
	n := float64(lb.Months)
	want := map[PlanIncomeKey]float64{
		{Class: "earnings", Source: "cf", Account: "CASH"}:       6000 / n,
		{Class: "other_receipts", Source: "cf", Account: "CASH"}: 50 / n,
		{Class: "yield", Source: "cf", Account: "BROK"}:          30 / n,
		{Class: "benefits", Source: "cf", Account: "CASH"}:       20 / n,
	}
	sum := 0.0
	for k, v := range lb.OtherIncomeBy {
		sum += v
		if math.Abs(v-want[k]) > 1e-9 {
			t.Errorf("%+v = %v, want %v", k, v, want[k])
		}
	}
	if len(lb.OtherIncomeBy) != len(want) {
		t.Errorf("split %v, want %v", lb.OtherIncomeBy, want)
	}
	if math.Abs(sum-lb.OtherIncome) > 1e-6 || math.Abs(lb.OtherIncome-6100/n) > 1e-9 {
		t.Errorf("the split sums to %v, the whole is %v", sum, lb.OtherIncome)
	}
	if keys := lb.IncomeKeys(); len(keys) != 4 || keys[0].Class != "benefits" || keys[3].Class != "yield" {
		t.Errorf("keys out of order: %v", keys)
	}
	if empty, err := CashflowPlanLookback(ctx, db, 4000000, 5000000, "USD"); err != nil ||
		len(empty.OtherIncomeBy) != 0 || empty.OtherIncome != 0 {
		t.Errorf("a window with no other income: %+v %v", empty, err)
	}
}

// A mortgage's instalment is what the look-back paid into it per month,
// interest and amortization together, over its months from its first
// instalment ever: MORT's began before the window, MORT2's inside it.
// An instalment that names no mortgage the product holds counts among
// the other financing outflows. The fixture's days are arbitrary
// ordinals in an impossible era.
func TestThePlansLookbackCarriesEachMortgage(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
	    display_name, base_currency, first_seen_at, last_seen_at) VALUES ('cf', 'MORT2', 'mortgage', 'Second', 'USD', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	seedMortgagePayment(t, db, ctx, "M-EARLY", 100, -9500, "MORT")
	seedMortgagePayment(t, db, ctx, "M-EXTRA", 200, -44000, "MORT")
	seedMortgagePayment(t, db, ctx, "M-SCHED", 200, -5000, "MORT")
	seedMortgagePayment(t, db, ctx, "M-OPEN", 300, -2200, "MORT")
	seedMortgagePayment(t, db, ctx, "M2-A", 240, -1000, "MORT2")
	seedMortgagePayment(t, db, ctx, "M2-B", 300, -1000, "MORT2")
	seedMortgagePayment(t, db, ctx, "M-SERVICER", 250, -350, "")
	// From 1970-05-31 to 1970-11-27: seven months, M2-A's first
	// instalment in the fourth from the end.
	lb, err := CashflowPlanLookback(ctx, db, 150*mortDay, 330*mortDay, "USD")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, m := range lb.Mortgages {
		got[m.Account] = m.Instalment
	}
	if lb.Months != 7 || len(got) != 2 || math.Abs(got["MORT"]-(44000+5000+2200)/7.0) > 1e-6 || math.Abs(got["MORT2"]-500) > 1e-6 {
		t.Errorf("%d months, mortgages %v", lb.Months, got)
	}
	if math.Abs(lb.OtherDebt-50) > 1e-6 {
		t.Errorf("other financing outflows %v a month, want 50", lb.OtherDebt)
	}
}

// The cash flow projection sets the income projection's months against
// the look-back: each month the averages, a standing mortgage's
// instalment to the month its maturity falls in, none for one the book
// no longer holds; net leaves the vehicles' income out, and the
// principal under the roll rule; a quarter sums its months, a window's
// first quarter perhaps short; the tags say what each part rests on.
func TestCashflowPlan(t *testing.T) {
	day := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	maturity := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	lb := PlanLookback{Months: 12, OtherIncome: 1000, OperatingOut: 3000, Vehicles: -200,
		OutClasses: map[string]float64{"consumption": 2500, "taxes": 500},
		Mortgages:  []PlanMortgage{{"bank", "M1", 900}, {"bank", "M2", 400}, {"bank", "M3", 50}}}
	standing := map[[2]string]*time.Time{{"bank", "M1"}: &maturity, {"bank", "M2"}: nil}
	months := []PlanMonth{{day(2026, 11), 100, 0, 0}, {day(2026, 12), 200, 5000, 70}, {day(2027, 1), 300, 0, 0},
		{day(2027, 2), 400, 0, 0}}
	rows := CashflowPlan(lb, standing, months, 1, false)
	if len(rows) != 6 || !rows[4].Total || !rows[5].Average {
		t.Fatalf("rows %+v", rows)
	}
	for i, want := range []float64{1300, 1300, 1300, 400} {
		if rows[i].DebtService != want {
			t.Errorf("%s: debt service %v, want %v", rows[i].Period.Format("2006-01"), rows[i].DebtService, want)
		}
	}
	if r := rows[1]; r.Net != 200+1000-3000-1300-200+5000 || r.VehiclesIncome != 70 ||
		r.OutClasses["taxes"] != 500 || r.Quality != "projected trailing terms" {
		t.Errorf("December: %+v", r)
	}
	if rolled := CashflowPlan(lb, standing, months, 1, true); rolled[1].Net != rows[1].Net-5000 {
		t.Errorf("under roll, net %v, want %v", rolled[1].Net, rows[1].Net-5000)
	}
	if tot, avg := rows[4], rows[5]; tot.InvestmentIncome != 1000 || tot.OperatingOut != 12000 ||
		tot.VehiclesIncome != 70 || avg.InvestmentIncome != 250 || avg.VehiclesIncome != 17.5 ||
		avg.DebtService != (1300*3+400)/4.0 {
		t.Errorf("total %+v, average %+v", tot, avg)
	}
	q := CashflowPlan(lb, standing, months, 3, false)
	if len(q) != 4 || !q[0].Period.Equal(day(2026, 10)) || q[0].OperatingOut != 6000 ||
		q[0].VehiclesIncome != 70 || !q[1].Period.Equal(day(2027, 1)) || q[1].InvestmentIncome != 700 {
		t.Errorf("quarters %+v", q)
	}
	thin := lb
	thin.Months = 3
	if r := CashflowPlan(thin, nil, months[:1], 1, false); r[0].Quality != "projected trailing thin" ||
		r[0].DebtService != 0 {
		t.Errorf("a thin look-back with no mortgage standing: %+v", r[0])
	}
	if r := CashflowPlan(PlanLookback{}, nil, months[:1], 1, false); r[0].Quality != "projected thin" ||
		r[0].Net != 100 {
		t.Errorf("no look-back at all: %+v", r[0])
	}
	if CashflowPlan(lb, standing, nil, 1, false) != nil {
		t.Error("no months, rows")
	}
}

// A mortgage stands while the book holds it, maturing on the latest of
// its tranches' terms; a tranche of no maturity leaves it none.
func TestPlanStanding(t *testing.T) {
	early, late := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	in := &yield.Inputs{Terms: map[yield.InstrumentKey]yield.Terms{
		{Source: "bank", Instrument: "T1"}: {Maturity: &early}, {Source: "bank", Instrument: "T2"}: {Maturity: &late},
		{Source: "bank", Instrument: "T4"}: {Maturity: &late}}}
	hold := func(acct, iid, vehicle string) yield.Holding {
		return yield.Holding{HoldingKey: yield.HoldingKey{Source: "bank", Account: acct, PositionKey: iid},
			Instrument: iid, Vehicle: vehicle}
	}
	in.Holdings = []yield.Holding{hold("M1", "T1", "mortgage"), hold("M1", "T2", "mortgage"),
		hold("M2", "T3", "mortgage"), hold("M2", "T4", "mortgage"), hold("B1", "T4", "bond")}
	got := PlanStanding(&yield.Book{In: in})
	var out []string
	for k, v := range got {
		s := k[1] + "="
		if v != nil {
			s += v.Format("2006")
		}
		out = append(out, s)
	}
	if len(got) != 2 || got[[2]string{"bank", "M1"}] == nil || !got[[2]string{"bank", "M1"}].Equal(late) ||
		got[[2]string{"bank", "M2"}] != nil {
		t.Errorf("standing %s", strings.Join(out, " "))
	}
}

// The look-back window is the lookback months whole before the UTC month
// now falls in, to the last second of the month before: from mid-month,
// a month's first second or its last, across a year's end, and from a
// zone ahead of UTC.
func TestPlanWindow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		now      time.Time
		lookback int
		from, to time.Time
	}{
		{time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC), 12, ymd(2025, 1, 1), ymd(2026, 1, 1)},
		{ymd(2026, 3, 1), 1, ymd(2026, 2, 1), ymd(2026, 3, 1)},
		{time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC), 3, ymd(2025, 11, 1), ymd(2026, 2, 1)},
		{time.Date(2026, 7, 1, 1, 0, 0, 0, time.FixedZone("CEST", 2*3600)), 36, ymd(2023, 6, 1), ymd(2026, 6, 1)},
	} {
		from, to := PlanWindow(c.now, c.lookback)
		if from != c.from.Unix() || to != c.to.Unix()-1 {
			t.Errorf("PlanWindow(%s, %d) = %s to %s, want %s to the second before %s", c.now, c.lookback,
				time.Unix(from, 0).UTC(), time.Unix(to, 0).UTC(), c.from, c.to)
		}
	}
}
