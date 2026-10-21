package yield

import (
	"math"
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

// book is an empty set of inputs at day, in USD, with the built-in
// conventions.
func book() *Inputs {
	in := &Inputs{AsOf: day.Unix() + 86399, Day: day, Currency: "USD", Accounts: map[AccountKey]Account{},
		Terms: map[InstrumentKey]Terms{}, Rates: map[AccountKey][]Rate{}, Funds: map[string]*Fund{},
		Curves: map[string]*Curve{}, Conventions: map[string]canonical.YieldConvention{},
		Income: map[HoldingKey]Received{}, AccountIncome: map[AccountCurrency]Received{},
		Payments: map[HoldingKey][]Payment{}, Dividends: map[InstrumentKey]canonical.DeclaredDividend{},
		Securities: map[string]Facts{}, AverageAmount: map[HoldingKey]float64{},
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

func flatCurve(ccy string, rate float64) *Curve {
	pts := []fixedincome.Point{{Months: 3, Rate: rate}, {Months: 12, Rate: rate}, {Months: 120, Rate: rate}}
	z, _ := fixedincome.Bootstrap(pts)
	return &Curve{Currency: ccy, Zero: z, Benchmark: rate, BenchmarkMonths: 3}
}

// A fixed bond priced at par on a coupon date implies its coupon; its
// DV01 is its value times its modified duration times a basis point.
func TestAParBondImpliesItsCoupon(t *testing.T) {
	t.Parallel()
	in := book()
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
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
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
	in := book()
	pts := []fixedincome.Point{{Months: 3, Rate: 4}, {Months: 12, Rate: 3}, {Months: 24, Rate: 3}}
	z, err := fixedincome.Bootstrap(pts)
	if err != nil {
		t.Fatal(err)
	}
	in.Curves["USD"] = &Curve{Currency: "USD", Zero: z, Benchmark: 4, BenchmarkMonths: 3}
	h := cash("bank", "S", "USD", 100000)
	in.Holdings = []Holding{h}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 1, UpTo: num(50000)},
		{Credit: true, Tier: 2, Pct: 0.5}, {Credit: false, Tier: 1, Pct: 9}}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	near(t, "stated", p.Stated, 0.75, 1e-12)
	near(t, "gap", p.Gap, 0.75-4, 1e-12)
	near(t, "spread", p.Spread, 0.75-4, 1e-12)
	near(t, "duration", p.Duration, 0, 0)
	m := 5
	constant := incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, 0)
	if math.Abs(constant-100000*0.75/1200) > 1e-9 {
		t.Errorf("constant income %v", constant)
	}
	forward := incomeOf(b, h.HoldingKey, m, RuleCash, PathForward, 0)
	want := 100000 * (z.Forward(m, 3) + 0.75 - 4) / 1200
	if math.Abs(forward-want) > 1e-9 || forward >= constant {
		t.Errorf("forward income %v, want %v below %v", forward, want, constant)
	}
	shifted := incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, -100)
	if math.Abs(shifted-(constant-100000*0.01/12)) > 1e-9 {
		t.Errorf("a shift of -100bp: %v", shifted)
	}
	near(t, "income 100bp", p.Income100bp, 100000*0.01, 1e-6)
}

// A bond maturing inside the window returns its principal, which then
// earns the account's cash rate (cash), nothing (idle), or buys the bond
// again at its own yield over the curve (roll).
func TestTheMaturingRules(t *testing.T) {
	t.Parallel()
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
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
			tags = append(tags, string(p.Rule)+":"+p.Reinvested)
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
	in := book()
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
	in := book()
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
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
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

// The grains reconcile: the total of global is the sum of every source's
// rows, and --converted totals equal the split ones; a liability nets in
// value and DV01 and is left out of the rates.
func TestTheGrainsReconcile(t *testing.T) {
	t.Parallel()
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
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
	sum, dv := 0.0, 0.0
	for _, r := range b.Aggregate(GrainSources, false) {
		if !r.Total {
			sum += r.Value
			dv += *r.DV01
		}
	}
	if sum != total.Value || math.Abs(dv-*total.DV01) > 1e-9 || total.Value != 10000+5000+2200-4000 {
		t.Errorf("sources sum %v dv01 %v, global %v %v", sum, dv, total.Value, *total.DV01)
	}
	conv := b.Aggregate(GrainAll, true)
	if len(conv) != 1 || conv[0].Value != total.Value || *conv[0].Implied != *total.Implied {
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
	if got := quality(ps); got != "implied=39% stated=61%" {
		t.Errorf("quality %q", got)
	}
	if got := quality(ps[1:]); got != "" {
		t.Errorf("implied alone %q", got)
	}
}

// Coverage names what is missing and the ledger record that fills it.
func TestCoverageSkeletons(t *testing.T) {
	t.Parallel()
	in := book()
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
