package yield

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// The books here are invented: every account, holding and figure.

var day = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

func date(y, m, d int) *time.Time {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return &t
}

func num(v float64) *float64 { return &v }

func near(t *testing.T, what string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", what, want)
		return
	}
	if math.Abs(*got-want) > tol {
		t.Errorf("%s = %.8f, want %.8f (±%g)", what, *got, want, tol)
	}
}

// inputs is an empty set of inputs at day, in USD, with the built-in
// conventions.
func inputs() *Inputs {
	in := &Inputs{AsOf: day.Unix() + 86399, Day: day, Currency: "USD", Accounts: map[AccountKey]Account{},
		Terms: map[InstrumentKey]Terms{}, Rates: map[AccountKey][]Rate{}, Funds: map[string]*Fund{},
		Curves: map[string]*Curve{}, Conventions: map[string]canonical.YieldConvention{},
		Income: map[HoldingKey]Received{}, AccountIncome: map[AccountCurrency]Received{},
		Payments: map[HoldingKey][]Payment{}, Dividends: map[InstrumentKey]canonical.DeclaredDividend{},
		Securities: map[string]Facts{}, Daily: map[HoldingKey]Daily{},
		FormerDaily: map[InstrumentKey][]Daily{}, Held: map[InstrumentKey][]string{},
		HeldSecurity: map[InstrumentKey]string{}, Aliases: map[AliasKey]string{},
		Withholding: map[string]map[string]float64{}, Situs: map[HoldingKey]string{}, FXRates: map[string]float64{}}
	for _, c := range canonical.BuiltinConventions() {
		in.Conventions[c.Market] = c
	}
	return in
}

func cash(src, acct, ccy string, amount float64) Holding {
	return Holding{HoldingKey: HoldingKey{src, acct, "cash:" + ccy}, AssetClass: "cash", Vehicle: "demand_deposit",
		Currency: ccy, IsCash: true, Value: amount, Amount: amount}
}

func bondHolding(src, acct, iid string, face, amount float64) Holding {
	return Holding{HoldingKey: HoldingKey{src, acct, iid}, Instrument: iid, AssetClass: "fixed_income",
		Vehicle: "bond", Currency: "USD", Value: amount, Amount: amount, Quantity: num(face)}
}

func position(b *Book, k HoldingKey) Position {
	for _, p := range b.Positions {
		if p.HoldingKey == k {
			return p
		}
	}
	return Position{}
}

// incomeOf is a holding's expected income in month m under a rule, a
// path and a shift.
func incomeOf(b *Book, k HoldingKey, m int, rule Rule, path Path, shift float64) float64 {
	total := 0.0
	for _, p := range b.Projection.Parts {
		if p.Holding == k && p.Month == m && p.Rule == rule {
			total += p.Income(path, b.Projection.Rate(p.Currency, m, path), shift)
		}
	}
	return total
}

// monthIndex is the projection month of a month's first day.
func monthIndex(b *Book, m time.Time) int {
	return (m.Year()-b.Projection.Start.Year())*12 + int(m.Month()) - int(b.Projection.Start.Month())
}

// flatCurve is a flat Treasury curve at rate.
func flatCurve(rate float64) *Curve {
	pts := []fixedincome.Point{{Months: 3, Rate: rate}, {Months: 12, Rate: rate}, {Months: 120, Rate: rate}}
	z, _ := fixedincome.NewCurve(pts, fixedincome.Treasury)
	return &Curve{Zero: z, Benchmark: rate, BenchmarkMonths: 3}
}

// A fixed bond priced at par on a coupon date implies its coupon; its
// DV01 is its value times its modified duration times a basis point.
func TestAParBondImpliesItsCoupon(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := bondHolding("broker", "A", "QBOND", 100000, 100000)
	in.Holdings = []Holding{h}
	freq := 2
	in.Terms[InstrumentKey{"broker", "QBOND"}] = Terms{Coupon: num(4.5), Maturity: date(2031, 6, 15),
		Frequency: &freq, DayCount: canonical.DayCount30360, CouponKind: canonical.CouponFixed}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if p.Kind != KindBond || !p.Tags[TagImplied] {
		t.Fatalf("kind %s tags %v", p.Kind, p.Tags)
	}
	near(t, "implied", p.Implied, 4.5, 1e-9)
	near(t, "annual", p.ImpliedAnnual, (math.Pow(1.0225, 2)-1)*100, 1e-9)
	near(t, "current yield", p.CurrentYield, 4.5, 1e-9)
	near(t, "dv01", p.DV01, 100000**p.Duration*1e-4, 1e-9)
	if *p.Duration <= 4 || *p.Duration >= 5 {
		t.Errorf("modified duration of a five-year par bond = %v", *p.Duration)
	}
	// Its coupons: 2,250 every December and June; the next twelve months
	// hold two.
	near(t, "income 12m", p.Income12, 4500, 1e-6)
	near(t, "income yield", num(*p.Income12/p.Value*100), 4.5, 1e-9)
	near(t, "income 100bp", p.Income100bp, 0, 0)
	sum := 0.0
	for _, part := range b.Projection.Parts {
		if part.Rule == RuleIdle {
			sum += part.Fixed
		}
	}
	// Six coupons in the window, which ends with month 36 (2029-06):
	// every December and June from 2026-12.
	near(t, "coupons in the window", &sum, 6*2250, 1e-6)
}

// A pure-fixed book projects exactly its coupon schedule under constant
// rates, and each month's projection equals the sum of its calendar
// rows.
func TestAFixedBookProjectsItsCouponSchedule(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
	a := bondHolding("broker", "A", "QB1", 50000, 49000)
	c := bondHolding("broker", "A", "QB2", 20000, 20400)
	in.Holdings = []Holding{a, c}
	in.Terms[InstrumentKey{"broker", "QB1"}] = Terms{Coupon: num(3), Maturity: date(2034, 9, 30)}
	in.Terms[InstrumentKey{"broker", "QB2"}] = Terms{Coupon: num(6), Maturity: date(2040, 1, 15)}
	b := Compute(in)
	byMonth := map[int]float64{}
	w := Window{From: b.Projection.From, To: b.Last()}
	for _, r := range b.Calendar(b.Amounts(nil, w, DefaultAssumptions)) {
		if r.Day == nil || r.Principal {
			t.Errorf("a coupon without its day: %+v", r)
		}
		byMonth[monthIndex(b, r.Month)] += r.Value
	}
	for m := 0; m <= Months; m++ {
		got := incomeOf(b, a.HoldingKey, m, RuleCash, PathConstant, 0) +
			incomeOf(b, c.HoldingKey, m, RuleCash, PathConstant, 0)
		forward := incomeOf(b, a.HoldingKey, m, RuleCash, PathForward, 100) +
			incomeOf(b, c.HoldingKey, m, RuleCash, PathForward, 100)
		want := byMonth[m]
		if math.Abs(got-want) > 1e-9 || math.Abs(forward-want) > 1e-9 {
			t.Errorf("month %d: projected %v (forward, shifted %v), calendar %v", m, got, forward, want)
		}
	}
	// QB1 pays 750 each March and September, QB2 600 each January and
	// July (its maturity's months).
	if byMonth[3] != 750 || byMonth[1] != 600 || byMonth[7] != 600 {
		t.Errorf("calendar months %v", byMonth)
	}
}

// A deposit earns its tiered rate on its balance; it keeps its spread to
// the benchmark along the forward path, and a shift moves it.
func TestADepositEarnsItsTieredRateAlongThePath(t *testing.T) {
	t.Parallel()
	in := inputs()
	pts := []fixedincome.Point{{Months: 3, Rate: 4}, {Months: 12, Rate: 3}, {Months: 24, Rate: 3}}
	z, err := fixedincome.NewCurve(pts, fixedincome.Treasury)
	if err != nil {
		t.Fatal(err)
	}
	in.Curves["USD"] = &Curve{Zero: z, Benchmark: 4, BenchmarkMonths: 3}
	h := cash("bank", "S", "USD", 100000)
	in.Holdings = []Holding{h}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 3.5, UpTo: num(50000)},
		{Credit: true, Tier: 2, Pct: 3}, {Credit: false, Tier: 1, Pct: 9}}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	near(t, "stated", p.Stated, 3.25, 1e-12)
	near(t, "gap", p.Gap, 3.25-4, 1e-12)
	near(t, "spread", p.Spread, 3.25-4, 1e-12)
	near(t, "duration", p.Duration, 0, 0)
	m := 5
	constant := incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, 0)
	if math.Abs(constant-100000*3.25/1200) > 1e-9 {
		t.Errorf("constant income %v", constant)
	}
	forward := incomeOf(b, h.HoldingKey, m, RuleCash, PathForward, 0)
	want := 100000 * (z.Forward(m, 3) + 3.25 - 4) / 1200
	if math.Abs(forward-want) > 1e-9 || forward >= constant {
		t.Errorf("forward income %v, want %v below %v", forward, want, constant)
	}
	shifted := incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, -100)
	if math.Abs(shifted-(constant-100000*0.01/12)) > 1e-9 {
		t.Errorf("a shift of -100bp: %v", shifted)
	}
	near(t, "income 100bp", p.Income100bp, 100000*0.01, 1e-6)
}

// A part's floating rates floor at zero, its roll's too, each with the
// shift: a roll at a quarter point earns nothing 100bp lower, and a loan
// at half a point costs nothing, rather than paying, 100bp lower.
func TestAPartsRatesFloorAtZero(t *testing.T) {
	t.Parallel()
	roll := Part{RollConstant: 100000 * 0.25 / 1200, RollForward: 100000 * 0.5 / 1200, RollPrincipal: 100000}
	for _, c := range []struct {
		what  string
		part  Part
		path  Path
		shift float64
		want  float64
	}{
		{"a roll at its rate", roll, PathConstant, 0, 100000 * 0.25 / 1200},
		{"a roll 100bp higher", roll, PathConstant, 100, 100000 * 1.25 / 1200},
		{"a roll 100bp lower", roll, PathConstant, -100, 0},
		{"a roll on the forward path 40bp lower", roll, PathForward, -40, 100000 * 0.1 / 1200},
		{"a loan 100bp lower", Part{Principal: -100000}, PathConstant, -100, 0},
		{"a loan at its rate", Part{Principal: -100000}, PathConstant, 0, -100000 * 0.5 / 1200},
		{"a fixed coupon", Part{Fixed: 50}, PathConstant, -500, 50},
	} {
		if got := c.part.Income(c.path, 0.5, c.shift); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", c.what, got, c.want)
		}
	}
}

// A floating rate never earns below zero. A franc deposit the bank
// charges half a percent earns nothing, not a charge: on the constant
// path, along a forward path that falls below zero, and at -100bp. Its
// change for rates one percent higher is what the floor leaves of the
// point: half of it.
func TestANegativeFrancPathFloorsADepositAtZero(t *testing.T) {
	t.Parallel()
	in := inputs()
	pts := []fixedincome.Point{{Months: 3, Rate: 0.25}, {Months: 12, Rate: -0.5}, {Months: 24, Rate: -0.75}}
	z, err := fixedincome.NewCurve(pts, fixedincome.Treasury)
	if err != nil {
		t.Fatal(err)
	}
	in.Curves["CHF"] = &Curve{Zero: z, Benchmark: 0.25, BenchmarkMonths: 3}
	h := cash("bank", "C", "CHF", 100000)
	in.Holdings = []Holding{h}
	in.Rates[AccountKey{"bank", "C"}] = []Rate{{Credit: true, Tier: 1, Pct: -0.5}}
	b := Compute(in)
	if constant := incomeOf(b, h.HoldingKey, 5, RuleCash, PathConstant, 0); constant != 0 {
		t.Errorf("constant income %v, want 0 for a rate of -0.5%%", constant)
	}
	floored := 0
	for m := 1; m <= 12; m++ {
		forward := incomeOf(b, h.HoldingKey, m, RuleCash, PathForward, 0)
		if forward < 0 {
			t.Errorf("month %d: forward income %v below zero", m, forward)
		}
		if z.Forward(m, 3)-0.75 < 0 {
			floored++
			if forward != 0 {
				t.Errorf("month %d: forward income %v, want 0 where the path is negative", m, forward)
			}
		}
	}
	if floored == 0 {
		t.Fatal("the path never goes below zero: the case pins nothing")
	}
	if shifted := incomeOf(b, h.HoldingKey, 5, RuleCash, PathConstant, -100); shifted != 0 {
		t.Errorf("a shift of -100bp: %v, want 0", shifted)
	}
	p := position(b, h.HoldingKey)
	near(t, "income 100bp", p.Income100bp, 100000*0.005, 1e-6)
}

// A bond maturing inside the window returns its principal, which then
// earns the account's cash rate (cash), nothing (idle), or buys the bond
// again at its own yield over the curve (roll).
func TestTheMaturingRules(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	acct := cash("broker", "A", "USD", 500)
	in.Holdings = []Holding{h, acct}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(5), Maturity: date(2027, 6, 15)}
	in.Rates[AccountKey{"broker", "A"}] = []Rate{{Credit: true, Tier: 1, Pct: 2}}
	b := Compute(in)
	var principal *Event
	for i, e := range b.Projection.Events {
		if e.Holding == h.HoldingKey && e.Rule == RuleCash {
			principal = &b.Projection.Events[i]
		}
	}
	if principal == nil || principal.Month != 12 || principal.Amount != 10000 || !principal.Day.Equal(*date(2027, 6, 15)) {
		t.Fatalf("principal event %+v", principal)
	}
	after := 14
	near(t, "cash", num(incomeOf(b, h.HoldingKey, after, RuleCash, PathConstant, 0)), 10000*2.0/1200, 1e-9)
	near(t, "idle", num(incomeOf(b, h.HoldingKey, after, RuleIdle, PathConstant, 0)), 0, 0)
	own := *position(b, h.HoldingKey).Implied
	near(t, "roll", num(incomeOf(b, h.HoldingKey, after, RuleRoll, PathConstant, 0)), 10000*own/1200, 1e-9)
	var tags []string
	for _, p := range b.Projection.Parts {
		if p.Holding == h.HoldingKey && p.Month == after && p.Reinvested != "" {
			tags = append(tags, string(p.Rule)+":"+string(p.Reinvested))
		}
	}
	if strings.Join(tags, " ") != "cash:cash roll:roll" {
		t.Errorf("reinvested tags %v", tags)
	}
	// With no curve the roll is at today's rate, and the forward path
	// gives the same.
	delete(in.Curves, "USD")
	b = Compute(in)
	c := incomeOf(b, h.HoldingKey, after, RuleRoll, PathConstant, 0)
	f := incomeOf(b, h.HoldingKey, after, RuleRoll, PathForward, 0)
	if c != f || c == 0 {
		t.Errorf("roll without a curve: constant %v forward %v", c, f)
	}
}

// A stock's indicated dividend is its last payment per share at the
// frequency of its payments; the projection repeats it on that rhythm.
func TestAStockPaysItsIndicatedDividend(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 20000, Amount: 20000, Quantity: num(200), SecurityID: "symbol:QSTK"}
	in.Holdings = []Holding{h}
	for i, d := range []*time.Time{date(2025, 9, 10), date(2025, 12, 10), date(2026, 3, 10), date(2026, 6, 10)} {
		amt := 100.0 + float64(i)*10
		in.Payments[h.HoldingKey] = append(in.Payments[h.HoldingKey], Payment{Day: *d, Amount: amt, Quantity: num(100),
			Type: TypeDividends})
	}
	in.Securities["symbol:QSTK"] = Facts{EPS: num(5), EPSYearAgo: num(4), DPS: num(4.4), DPSYearAgo: num(4),
		Currency: "USD"}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	// The last payment, 130 on 100 shares, is 1.30 a share; four a year.
	near(t, "dividend yield", p.DividendYield, 1.3*4/100*100, 1e-9)
	near(t, "earnings yield", p.EarningsYield, 5.0, 1e-9)
	near(t, "payout", p.Payout, 88, 1e-9)
	near(t, "dividend growth", p.DividendGrowth, 10, 1e-9)
	near(t, "income 12m", p.Income12, 4*1.3*200, 1e-9)
	if !p.Tags[TagIndicated] {
		t.Errorf("tags %v", p.Tags)
	}
	for _, m := range []int{3, 6, 9, 12} {
		if got := incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, 0); math.Abs(got-260) > 1e-9 {
			t.Errorf("month %d pays %v, want 260", m, got)
		}
	}
	// A ledger record's dividend wins.
	in.Dividends[InstrumentKey{"broker", "QSTK"}] = canonical.DeclaredDividend{Amount: 2, Frequency: 2}
	p = position(Compute(in), h.HoldingKey)
	near(t, "declared income", p.Income12, 2*2*200, 1e-9)
	if !p.Tags[TagDeclared] {
		t.Errorf("declared tags %v", p.Tags)
	}
}

// A stock held a year that paid nothing yields zero on that record; one
// bought within the year is unknown.
func TestAStockThatPaidNothing(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := Holding{HoldingKey: HoldingKey{"broker", "A", "QGRO"}, Instrument: "QGRO", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 1000, Amount: 1000, Quantity: num(10)}
	in.Holdings = []Holding{h}
	p := position(Compute(in), h.HoldingKey)
	if !p.Tags[TagRealized] || *p.DividendYield != 0 {
		t.Errorf("held long: tags %v yield %v", p.Tags, p.DividendYield)
	}
	in.Holdings[0].Acquired = date(2026, 3, 1)
	p = position(Compute(in), h.HoldingKey)
	if !p.Tags[TagUnknown] || p.DividendYield != nil {
		t.Errorf("bought lately: tags %v yield %v", p.Tags, p.DividendYield)
	}
}

// A money-market fund yields its seven-day yield as a floating holding
// and lasts its weighted average maturity; a bond fund reads its lines,
// whose prices are their filing day's.
func TestFunds(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
	mmf := Holding{HoldingKey: HoldingKey{"broker", "A", "QMMF"}, Instrument: "QMMF", AssetClass: "cash",
		Vehicle: "fund", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(10000), SecurityID: "symbol:QMMF"}
	bf := Holding{HoldingKey: HoldingKey{"broker", "A", "QBF"}, Instrument: "QBF", AssetClass: "fixed_income",
		Vehicle: "etf", Currency: "USD", Value: 30000, Amount: 30000, Quantity: num(300), SecurityID: "symbol:QBF"}
	in.Holdings = []Holding{mmf, bf}
	in.Funds["symbol:QMMF"] = &Fund{SevenDayYield: &Dated{Value: 4.1, Day: day}, MaturityYears: &Dated{Value: 0.1}}
	in.Funds["symbol:QBF"] = &Fund{DistributionYield: &Dated{Value: 3.8}, Duration: &Dated{Value: 6},
		Lines: &LineSummary{AsOf: day.AddDate(0, -2, 0), Yield: num(4.4), Duration: num(5.8), CoveredPct: 97},
		Risk:  map[string]map[string]float64{"USD": {"5Y": 1000, "10Y": 2000}}, NetAssets: num(1e6)}
	b := Compute(in)
	m := position(b, mmf.HoldingKey)
	if m.Kind != KindMoneyMarket {
		t.Fatalf("kind %s", m.Kind)
	}
	near(t, "seven-day", m.Implied, 4.1, 0)
	near(t, "duration", m.Duration, 0.1, 0)
	near(t, "income 12m", m.Income12, 10000*0.041, 1e-6)
	near(t, "income 100bp", m.Income100bp, 100, 1e-6)
	f := position(b, bf.HoldingKey)
	near(t, "fund implied", f.Implied, 4.4, 0)
	near(t, "fund stated", f.Stated, 3.8, 0)
	near(t, "fund duration", f.Duration, 6, 0)
	if !f.Tags[TagImplied] || !f.Tags[TagStalePrice] {
		t.Errorf("fund tags %v", f.Tags)
	}
	// Its filed DV01, scaled to the holding: 3% of the fund.
	near(t, "fund dv01", f.DV01, 90, 1e-9)
	near(t, "fund 10Y", num(f.Buckets["USD"]["10Y"]), 60, 1e-9)
	near(t, "fund distributions", f.Income12, 30000*0.038, 1e-6)
}

// A liability nets in value and DV01 and is left out of the rates; the
// total of several currencies is mixed, and --converted reads the same
// rate as the total row.
func TestALiabilityNetsAndWeighsNoRate(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
	b1 := bondHolding("broker", "A", "QB", 10000, 10000)
	d := cash("bank", "S", "USD", 5000)
	e := cash("bank", "E", "EUR", 2000)
	e.Value = 2200
	mort := Holding{HoldingKey: HoldingKey{"bank", "M", "QMORT"}, Instrument: "QMORT", AssetClass: "real_estate",
		Vehicle: "mortgage", Currency: "USD", Value: -4000, Amount: -4000}
	in.Holdings = []Holding{b1, d, e, mort}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(5), Maturity: date(2030, 6, 15)}
	in.Terms[InstrumentKey{"bank", "QMORT"}] = Terms{Coupon: num(2), FixedUntil: date(2028, 6, 15), CouponDeclared: true}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 1}}
	in.Rates[AccountKey{"bank", "E"}] = []Rate{{Credit: true, Tier: 1, Pct: 0.5}}
	b := Compute(in)
	global := b.Aggregate(GrainAll, false)
	total := global[len(global)-1]
	if !total.Total || !total.Mixed || !strings.HasPrefix(total.Quality, "mixed") {
		t.Fatalf("total row %+v", total)
	}
	if total.Value != 10000+5000+2200-4000 {
		t.Errorf("total value %v", total.Value)
	}
	conv := b.Aggregate(GrainAll, true)
	if len(conv) != 1 || *conv[0].Implied != *total.Implied {
		t.Errorf("converted %+v vs %+v", conv, total)
	}
	// The mortgage nets: negative DV01, and no weight in the rates.
	m := position(b, mort.HoldingKey)
	if *m.DV01 >= 0 || m.Income12 != nil || !m.Tags[TagDeclared] {
		t.Errorf("mortgage %+v", m.Measures)
	}
	usd := global[0]
	assets := 10000 + 5000.0
	want := (10000**position(b, b1.HoldingKey).Implied + 5000*1) / assets
	near(t, "USD implied, the mortgage left out", usd.Implied, want, 1e-9)
	// A row of the mortgage alone reads its own rate.
	for _, r := range b.Aggregate(GrainAccounts, false) {
		if r.AccountID == "M" {
			near(t, "mortgage row", r.Implied, 2, 0)
		}
	}
}

// Quality is the share of value each tag covers; implied alone prints
// nothing.
func TestQuality(t *testing.T) {
	t.Parallel()
	ps := []Position{
		{Holding: Holding{Value: 61}, Measures: Measures{Tags: map[Tag]bool{TagStated: true}}},
		{Holding: Holding{Value: 39}, Measures: Measures{Tags: map[Tag]bool{TagImplied: true}}},
	}
	if got := quality(ps, false); got != "implied=39% stated=61%" {
		t.Errorf("quality %q", got)
	}
	if got := quality(ps[1:], false); got != "" {
		t.Errorf("implied alone %q", got)
	}
	// A tag that covers all but a sliver prints its share: a bare tag is
	// the whole row's.
	ps = []Position{
		{Holding: Holding{Value: 996}, Measures: Measures{Tags: map[Tag]bool{TagImplied: true, TagStated: true}}},
		{Holding: Holding{Value: -4}, Measures: Measures{Tags: map[Tag]bool{TagStated: true}}},
	}
	if got := quality(ps, false); got != "implied=100% stated" {
		t.Errorf("all but a sliver %q", got)
	}
}

// Coverage names what is missing and the ledger record that fills it.
func TestCoverageSkeletons(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := bondHolding("broker", "A", "QB", 1000, 1000)
	h.Name = "QORVANT CORP NOTE"
	d := cash("bank", "S", "CHF", 100)
	in.Accounts[AccountKey{"bank", "S"}] = Account{Name: "Savings"}
	in.Holdings = []Holding{h, d}
	b := Compute(in)
	var got []string
	for _, r := range b.Coverage() {
		if r.Section == "holdings" {
			s := r.Skeleton
			got = append(got, r.Reason+" "+s.SilverSourceID+"/"+s.Instrument+"/"+s.Account+" "+s.Note+
				map[bool]string{true: " rate"}[s.Rate != nil]+map[bool]string{true: " terms"}[s.Terms != nil]+
				map[bool]string{true: " rating"}[s.Rating != nil])
		}
	}
	want := "no rate bank//S Savings rate|no rating broker/QB/ QORVANT CORP NOTE rating|" +
		"no terms broker/QB/ QORVANT CORP NOTE terms"
	if strings.Join(got, "|") != want {
		t.Errorf("skeletons:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
}

// A transaction belongs to the holding in its own account, though a
// sibling in its portfolio sorts first. An account that held the
// instrument and sold it has none: its transactions are its sold
// position's. Any other belongs to the first holding in its portfolio,
// as a cash account beside a custody account books it; where the
// portfolio holds none but held it once, to the sold position too; else
// to the first at its source.
func TestHoldingOf(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b, c := bondHolding("broker", "A", "QB", 1000, 1000), bondHolding("broker", "B", "QB", 1000, 1000),
		bondHolding("broker", "C", "QB", 1000, 1000)
	in.Holdings = []Holding{a, b, c}
	in.Accounts[AccountKey{"broker", "A"}] = Account{Portfolio: "P1"}
	for _, acct := range []string{"B", "C", "K", "S"} {
		in.Accounts[AccountKey{"broker", acct}] = Account{Portfolio: "P2"}
	}
	for _, acct := range []string{"D", "E"} {
		in.Accounts[AccountKey{"broker", acct}] = Account{Portfolio: "P3"}
	}
	in.Held[InstrumentKey{"broker", "QB"}] = []string{"A", "B", "C", "D", "S"}
	for _, tc := range []struct {
		acct, iid, want string
	}{
		{"C", "QB", "C"},
		{"K", "QB", "B"},
		{"S", "QB", ""},
		{"E", "QB", ""},
		{"X", "QB", "A"},
		{"C", "QZ", ""},
		{"C", "", ""},
	} {
		got, ok := in.HoldingOf("broker", tc.acct, tc.iid)
		if ok != (tc.want != "") || (ok && got != HoldingKey{"broker", tc.want, "QB"}) {
			t.Errorf("%s %q: %v %v, want %q", tc.acct, tc.iid, got, ok, tc.want)
		}
	}
	for _, acct := range []string{"S", "E"} {
		if k, ok := in.Sold("broker", acct, "QB"); !ok || k != (HoldingKey{"broker", acct, "QB"}) {
			t.Errorf("%s sold %v %v", acct, k, ok)
		}
	}
	if _, ok := in.Sold("broker", "X", "QB"); ok {
		t.Error("X sold what it never held")
	}
}

// The ids a source keys one instrument by fold into one where the lines
// meet the holdings (Fold): an account's own alias first, then its
// source's. A line under the ticker belongs to the holding under the
// CUSIP id, and the reverse. An account's own alias covers the lines its
// portfolio's other accounts book for it too, and no other's: a cash
// account's line under a custody account's key belongs to the custody
// account's holding, or to the cash account's sold position under the
// custody account's id; another portfolio's line under that key belongs
// to a holding of it as it stands. A line of an account whose portfolio
// never held the instrument, where one account of the source alone did,
// is that account's sold position; where two did, none's. A
// line of an account that held it under the other id is its sold
// position's, under the id it folds into, and the holding pools it: its
// income, and its amounts (FormerDaily).
func TestTheAliasFold(t *testing.T) {
	t.Parallel()
	in := inputs()
	for k, v := range map[AliasKey]string{
		{"broker", "", "000000AA0"}: "QALS", {"broker", "", "000000BB0"}: "QTKR",
		{"broker", "K", "QOVR"}: "QALS", {"broker", "", "QOVR"}: "QOTH",
		{"broker", "A", "XOVA"}: "QALS", {"broker", "D", "XOVD"}: "QDD", {"broker", "AA", "XK"}: "QY",
	} {
		in.Aliases[k] = v
	}
	for acct, p := range map[string]string{"A": "P", "C": "P", "D": "Q", "E": "Q", "G": "R", "AA": "PA", "BB": "PB",
		"CC": "PC"} {
		in.Accounts[AccountKey{"broker", acct}] = Account{Portfolio: p}
	}
	cusip, ticker := bondHolding("broker", "A", "000000AA0", 1000, 1000), bondHolding("broker", "B", "QTKR", 1000, 1000)
	aliased, direct := bondHolding("broker", "AA", "QY", 1000, 1000), bondHolding("broker", "BB", "XK", 1000, 1000)
	in.Holdings = []Holding{cusip, ticker, aliased, direct}
	in.Held[InstrumentKey{"broker", "QALS"}] = []string{"A", "S"}
	in.Held[InstrumentKey{"broker", "QTKR"}] = []string{"B"}
	in.Held[InstrumentKey{"broker", "QDD"}] = []string{"D"}
	in.Held[InstrumentKey{"broker", "QTWO"}] = []string{"D", "S"}
	for _, tc := range []struct {
		acct, iid string
		want      HoldingKey
	}{
		{"A", "QALS", cusip.HoldingKey},
		{"A", "000000AA0", cusip.HoldingKey},
		{"B", "000000BB0", ticker.HoldingKey},
		{"K", "QOVR", cusip.HoldingKey},
		{"X", "QOVR", HoldingKey{}},
		{"S", "000000AA0", HoldingKey{}},
		{"C", "XOVA", cusip.HoldingKey},
		{"G", "XOVA", HoldingKey{}},
		{"E", "XOVD", HoldingKey{}},
		{"CC", "XK", direct.HoldingKey},
	} {
		if got, ok := in.HoldingOf("broker", tc.acct, tc.iid); got != tc.want || ok != (tc.want != HoldingKey{}) {
			t.Errorf("%s %s: %v %v, want %v", tc.acct, tc.iid, got, ok, tc.want)
		}
	}
	sold := HoldingKey{"broker", "S", "QALS"}
	for _, tc := range []struct {
		acct, iid string
		want      HoldingKey
	}{
		{"S", "000000AA0", sold},
		{"E", "XOVD", HoldingKey{"broker", "E", "QDD"}},
		{"G", "QDD", HoldingKey{"broker", "D", "QDD"}},
	} {
		if k, ok := in.Sold("broker", tc.acct, tc.iid); !ok || k != tc.want {
			t.Errorf("%s %s sold %v %v, want %v", tc.acct, tc.iid, k, ok, tc.want)
		}
	}
	if k, ok := in.Sold("broker", "G", "QTWO"); ok {
		t.Errorf("G sold %v, which two accounts held", k)
	}
	in.Daily[cusip.HoldingKey] = held(Span{From: 0, To: year, Amount: 1000})
	in.FormerDaily[InstrumentKey{"broker", "QALS"}] = []Daily{held(Span{From: 0, To: year - 165, Amount: 3000})}
	in.Income[cusip.HoldingKey] = Received{TypeCoupons: 50}
	in.Income[sold] = Received{TypeCoupons: 60}
	mean := (1000*year + 3000*(year-165.0)) / year
	near(t, "pooled", position(Compute(in), cusip.HoldingKey).Realized, 110/mean*100, 1e-9)
}

// A payment in another currency than the holding's counts at the
// instant's rates: a dollar stock paid in francs indicates its dividend
// in dollars, and a holding no figure describes repeats its payment in
// dollars. A payment no rate converts counts nothing: both read unknown.
func TestAPaymentInAnotherCurrency(t *testing.T) {
	t.Parallel()
	read := func(ccy string) (*Book, Holding, Holding) {
		in := inputs()
		in.FXRates["CHF"] = 1.25
		s := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
			Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
		o := Holding{HoldingKey: HoldingKey{"broker", "A", "QNOTE"}, Instrument: "QNOTE", AssetClass: "alternatives",
			Vehicle: "structured_product", Currency: "USD", Value: 5000, Amount: 5000, Quantity: num(5)}
		in.Holdings = []Holding{s, o}
		for _, d := range []*time.Time{date(2025, 9, 10), date(2025, 12, 10), date(2026, 3, 10), date(2026, 6, 10)} {
			in.Payments[s.HoldingKey] = append(in.Payments[s.HoldingKey], Payment{Day: *d, Amount: 80, Currency: ccy,
				Quantity: num(100), Type: TypeDividends})
		}
		in.Payments[o.HoldingKey] = []Payment{{Day: *date(2025, 9, 30), Amount: 120, Currency: ccy, Type: TypeInterest}}
		return Compute(in), s, o
	}
	b, s, o := read("CHF")
	p := position(b, s.HoldingKey)
	// 80 francs on 100 shares is a dollar a share; four a year.
	near(t, "dividend yield", p.DividendYield, 4, 1e-9)
	near(t, "income 12m", p.Income12, 400, 1e-9)
	near(t, "repeated", num(incomeOf(b, o.HoldingKey, 3, RuleCash, PathConstant, 0)), 150, 1e-9)
	b, s, o = read("GBP")
	if p := position(b, s.HoldingKey); !p.Tags[TagUnknown] || p.DividendYield != nil {
		t.Errorf("paid in a currency no rate converts: tags %v yield %v", p.Tags, p.DividendYield)
	}
	if p := position(b, o.HoldingKey); !p.Tags[TagUnknown] {
		t.Errorf("repeats a payment no rate converts: tags %v", p.Tags)
	}
}

// A ledger dividend stated in another currency counts at the instant's
// rates; one no rate converts gives way to the payments.
func TestALedgerDividendInAnotherCurrency(t *testing.T) {
	t.Parallel()
	read := func(ccy string) Position {
		in := inputs()
		in.FXRates["CHF"] = 1.25
		h := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
			Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
		in.Holdings = []Holding{h}
		in.Dividends[InstrumentKey{"broker", "QSTK"}] = canonical.DeclaredDividend{Amount: 0.8, Frequency: 4,
			Currency: ccy}
		in.Payments[h.HoldingKey] = []Payment{{Day: *date(2026, 3, 10), Amount: 50, Quantity: num(100),
			Type: TypeDividends}}
		return position(Compute(in), h.HoldingKey)
	}
	p := read("CHF")
	near(t, "declared", p.DividendYield, 4, 1e-9)
	if !p.Tags[TagDeclared] {
		t.Errorf("declared tags %v", p.Tags)
	}
	// The payment of 0.50 a share, once in the year.
	p = read("GBP")
	near(t, "indicated", p.DividendYield, 0.5, 1e-9)
	if !p.Tags[TagIndicated] || p.Tags[TagDeclared] {
		t.Errorf("indicated tags %v", p.Tags)
	}
}

// The defaults are the first rule and the first path, which the CLI, the
// MCP server and the dashboard's pickers start from.
func TestTheDefaultsComeFirst(t *testing.T) {
	t.Parallel()
	if d := DefaultAssumptions; Rules[0] != d.Rule || Paths[0] != d.Path || d.ShiftBP != 0 {
		t.Errorf("defaults %+v, rules %v, paths %v", d, Rules, Paths)
	}
}

// A skeleton's keys are the ledger's: a yield record's kind, or what
// names its holding and the note.
func TestSkeletonKeysAreTheLedgers(t *testing.T) {
	t.Parallel()
	empty := &struct{}{}
	raw, err := json.Marshal(Skeleton{SilverSourceID: "s", Instrument: "i", Account: "a",
		Rate: &RateSkeleton{Currency: "USD"}, Rating: empty, Dividend: empty, Terms: empty, Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	named := []string{"silver_source_id", "instrument", canonical.DeclarationAccount, "note"}
	for k := range keys {
		if !slices.Contains(canonical.DeclarationYieldKinds, k) && !slices.Contains(named, k) {
			t.Errorf("key %q is no ledger key", k)
		}
	}
	if len(keys) != len(canonical.DeclarationYieldKinds)+len(named) {
		t.Errorf("keys %v", keys)
	}
}
