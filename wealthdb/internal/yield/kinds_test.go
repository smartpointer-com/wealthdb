package yield

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// The books here are invented: every account, holding and figure. The
// day is 2026-06-15; month 0 runs from June 16.

func fund(iid, class string, value float64) Holding {
	return Holding{HoldingKey: HoldingKey{"broker", "A", iid}, Instrument: iid, AssetClass: class, Vehicle: "fund",
		Currency: "USD", Value: value, Amount: value, Quantity: num(value / 10), SecurityID: "symbol:" + iid,
		Key: "symbol:" + iid}
}

func partsOf(b *Book, k HoldingKey, rule Rule) []Part {
	var out []Part
	for _, p := range b.Projection.Parts {
		if p.Holding == k && p.Rule == rule {
			out = append(out, p)
		}
	}
	return out
}

// A bond's amount includes the interest accrued where the source states
// it: the price is clean, and the yield the same as from the clean amount
// with the accrual computed.
func TestAccruedInterestIsInTheAmount(t *testing.T) {
	t.Parallel()
	terms := Terms{Coupon: num(5), Maturity: date(2031, 3, 15), DayCount: canonical.DayCount30360}
	read := func(amount float64, accrued *float64) Position {
		in := book()
		h := bondHolding("broker", "A", "QB", 10000, amount)
		h.Accrued = accrued
		in.Holdings = []Holding{h}
		in.Terms[InstrumentKey{"broker", "QB"}] = terms
		return position(Compute(in), h.HoldingKey)
	}
	// Three months into a half-year period: 1.25 per 100 accrued.
	stated, computed := read(10125, num(125)), read(10000, nil)
	near(t, "clean price", stated.Price, 100, 1e-9)
	near(t, "yield, accrual stated", stated.Implied, *computed.Implied, 1e-9)
	near(t, "near the coupon", stated.Implied, 5, 0.01)
}

// A money-market fund that files nowhere yields what a ledger record
// states, as of the record's day; a fund's page figure is the page's, and
// a record's yield wins over the page and the lines.
func TestALedgerRecordStatesAFundsFigures(t *testing.T) {
	t.Parallel()
	in := book()
	mmf := fund("QMMF", "cash", 10000)
	bf := fund("QBF", "fixed_income", 20000)
	in.Holdings = []Holding{mmf, bf}
	in.Terms[InstrumentKey{"broker", "QMMF"}] = Terms{Yield: num(3.5), Duration: num(0.12), TermsAsOf: date(2026, 5, 31)}
	in.Funds["symbol:QBF"] = &Fund{YieldToMaturity: &Dated{Value: 4.2, Day: *date(2026, 5, 30)}}
	in.Terms[InstrumentKey{"broker", "QBF"}] = Terms{Duration: num(6.5), TermsAsOf: date(2026, 4, 30)}
	b := Compute(in)
	m := position(b, mmf.HoldingKey)
	if m.Kind != KindMoneyMarket || !m.Tags[TagDeclared] || m.Tags[TagUnknown] || m.FiguresFrom != "ledger" ||
		!m.FiguresAsOf.Equal(*date(2026, 5, 31)) {
		t.Errorf("money market: %s %v %s %v", m.Kind, m.Tags, m.FiguresFrom, m.FiguresAsOf)
	}
	near(t, "money market yield", m.Implied, 3.5, 0)
	near(t, "money market duration", m.Duration, 0.12, 0)
	near(t, "money market income", m.Income12, 350, 1e-6)
	f := position(b, bf.HoldingKey)
	if f.FiguresFrom != "page" || !f.FiguresAsOf.Equal(*date(2026, 5, 30)) || !f.Tags[TagStated] || !f.Tags[TagDeclared] {
		t.Errorf("page yield, declared duration: %s %v %v", f.FiguresFrom, f.FiguresAsOf, f.Tags)
	}
	near(t, "page yield", f.Implied, 4.2, 0)
	near(t, "declared duration", f.Duration, 6.5, 0)
	in.Funds["symbol:QBF"].Lines = &LineSummary{AsOf: day, Yield: num(4.4), CoveredPct: 97}
	in.Terms[InstrumentKey{"broker", "QBF"}] = Terms{Yield: num(5), TermsAsOf: date(2026, 4, 30)}
	f = position(Compute(in), bf.HoldingKey)
	near(t, "declared yield", f.Implied, 5, 0)
	if f.FiguresFrom != "ledger" || f.Tags[TagImplied] {
		t.Errorf("declared yield: %s %v", f.FiguresFrom, f.Tags)
	}
}

// A liability's rate risk runs to its fixed period's end; terms that
// state none fix a coupon to maturity, unless it floats.
func TestALiabilitysFixedPeriod(t *testing.T) {
	t.Parallel()
	read := func(kind canonical.CouponKind, fixed *time.Time) Position {
		in := book()
		h := Holding{HoldingKey: HoldingKey{"bank", "M", "QMORT"}, Instrument: "QMORT", AssetClass: "real_estate",
			Vehicle: "mortgage", Currency: "USD", Value: -100000, Amount: -100000}
		in.Holdings = []Holding{h}
		in.Terms[InstrumentKey{"bank", "QMORT"}] = Terms{Coupon: num(2), Maturity: date(2031, 6, 15), CouponKind: kind,
			FixedUntil: fixed}
		return position(Compute(in), h.HoldingKey)
	}
	years := yearsBetween(day, *date(2031, 6, 15))
	p := read("", nil)
	near(t, "to maturity", p.Duration, years/1.02, 1e-9)
	near(t, "dv01", p.DV01, -100000*years/1.02*1e-4, 1e-9)
	near(t, "fixed period", read(canonical.CouponFixed, date(2028, 6, 15)).Duration,
		yearsBetween(day, *date(2028, 6, 15))/1.02, 1e-9)
	near(t, "floating", read(canonical.CouponFloating, nil).Duration, 0, 0)
}

// A bill implies its discount yield; its gap is its bond-equivalent
// yield less the benchmark, the convention the curve is quoted in. Its
// discount is paid on its maturity day, with its face.
func TestABill(t *testing.T) {
	t.Parallel()
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
	h := bondHolding("broker", "A", "QBILL", 10000, 9750)
	in.Holdings = []Holding{h}
	zero := 0
	maturity := day.AddDate(0, 0, 182)
	in.Terms[InstrumentKey{"broker", "QBILL"}] = Terms{Frequency: &zero, Maturity: &maturity}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if p.Kind != KindBill {
		t.Fatalf("kind %s", p.Kind)
	}
	near(t, "discount", p.Implied, fixedincome.BillDiscount(day, maturity, 97.5), 1e-12)
	near(t, "annual", p.ImpliedAnnual, fixedincome.BillEffectiveAnnual(day, maturity, 97.5), 1e-12)
	near(t, "gap", p.Gap, fixedincome.BillBondEquivalent(day, maturity, 97.5)-4, 1e-12)
	ps := partsOf(b, h.HoldingKey, RuleIdle)
	if len(ps) != 1 || ps[0].Fixed != 250 || !ps[0].Day.Equal(maturity) || ps[0].Type != TypeInterest {
		t.Errorf("bill parts %+v", ps)
	}
	if len(b.Projection.Events) != len(Rules) || b.Projection.Events[0].Amount != 10000 {
		t.Errorf("events %+v", b.Projection.Events)
	}
}

// A holding no figure describes repeats what it was paid last year,
// interest too; paid nothing, it is unknown.
func TestAnOtherHolding(t *testing.T) {
	t.Parallel()
	in := book()
	h := Holding{HoldingKey: HoldingKey{"broker", "A", "QNOTE"}, Instrument: "QNOTE", AssetClass: "alternatives",
		Vehicle: "structured_product", Currency: "USD", Value: 5000, Amount: 5000, Quantity: num(5)}
	in.Holdings = []Holding{h}
	p := position(Compute(in), h.HoldingKey)
	if p.Kind != KindOther || !p.Tags[TagUnknown] {
		t.Errorf("paid nothing: %s %v", p.Kind, p.Tags)
	}
	in.Payments[h.HoldingKey] = []Payment{{Day: *date(2025, 9, 30), Amount: 120, Type: TypeInterest}}
	b := Compute(in)
	p = position(b, h.HoldingKey)
	if p.Tags[TagUnknown] || !p.Tags[TagRealized] {
		t.Errorf("paid: %v", p.Tags)
	}
	near(t, "repeated", num(incomeOf(b, h.HoldingKey, 3, RuleCash, PathConstant, 0)), 120, 0)
}

// A deposit no source states a rate for repeats the interest its account
// was paid: realized, no longer unknown, and still missing its rate.
func TestADepositRepeatsItsInterest(t *testing.T) {
	t.Parallel()
	in := book()
	h := cash("bank", "S", "USD", 8000)
	in.Holdings = []Holding{h}
	in.AccountPayments = []AccountPayment{{AccountCurrency: AccountCurrency{AccountKey{"bank", "S"}, "USD"},
		Day: *date(2025, 12, 31), Amount: 40, Type: TypeInterest}}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if !p.Tags[TagRealized] || p.Tags[TagUnknown] {
		t.Errorf("tags %v", p.Tags)
	}
	near(t, "income 12m", p.Income12, 40, 0)
	if c := b.Coverage(); !slices.ContainsFunc(c, func(r CoverageRow) bool { return r.Reason == missingRate }) {
		t.Errorf("coverage %+v", c)
	}
}

// An overdraft costs its debit rate but weighs in no rate: the rates say
// what the assets earn. The cash view lists it after the credit balances.
func TestAnOverdraft(t *testing.T) {
	t.Parallel()
	in := book()
	d, o := cash("bank", "S", "USD", 10000), cash("bank", "O", "USD", -5000)
	in.Holdings = []Holding{o, d}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 2}}
	in.Rates[AccountKey{"bank", "O"}] = []Rate{{Credit: false, Tier: 1, Pct: 9}}
	b := Compute(in)
	usd := b.Aggregate(GrainAll, false)[0]
	near(t, "implied", usd.Implied, 2, 1e-12)
	near(t, "income yield", usd.IncomeYield, 2, 1e-9)
	if usd.Value != 5000 {
		t.Errorf("value %v", usd.Value)
	}
	if c := b.Cash(); c[0].HoldingKey != d.HoldingKey || c[1].HoldingKey != o.HoldingKey {
		t.Errorf("cash order %v %v", c[0].HoldingKey, c[1].HoldingKey)
	}
}

// A coupon later in the current month counts whole; one on the day is
// past.
func TestACouponLaterThisMonth(t *testing.T) {
	t.Parallel()
	in := book()
	late, past := bondHolding("broker", "A", "QL", 10000, 10000), bondHolding("broker", "A", "QP", 10000, 10000)
	in.Holdings = []Holding{late, past}
	in.Terms[InstrumentKey{"broker", "QL"}] = Terms{Coupon: num(4), Maturity: date(2030, 6, 25)}
	in.Terms[InstrumentKey{"broker", "QP"}] = Terms{Coupon: num(4), Maturity: date(2030, 6, 15)}
	b := Compute(in)
	near(t, "later this month", num(incomeOf(b, late.HoldingKey, 0, RuleCash, PathConstant, 0)), 200, 1e-9)
	near(t, "on the day", num(incomeOf(b, past.HoldingKey, 0, RuleCash, PathConstant, 0)), 0, 0)
}

// A bond in default is out of the yields and pays what nobody can say:
// no coupon, no principal.
func TestABondInDefault(t *testing.T) {
	t.Parallel()
	in := book()
	h := bondHolding("broker", "A", "QD", 10000, 3000)
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QD"}] = Terms{Coupon: num(8), Maturity: date(2027, 6, 15), InDefault: true}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if !p.Tags[TagDefault] || p.Implied != nil || len(b.Projection.Events) != 0 {
		t.Errorf("tags %v implied %v events %v", p.Tags, p.Implied, b.Projection.Events)
	}
	for _, part := range b.Projection.Parts {
		if part.Basis != BasisUnknown || part.Income(PathConstant, 0, 0) != 0 {
			t.Errorf("part %+v", part)
		}
	}
	near(t, "income 12m", p.Income12, 0, 0)
}

// A floating note resets quarterly where its terms state no reset, lasts
// to its next reset, and earns the benchmark plus its spread on its face.
// Rolled, it buys again at its own current yield (roll_own); a note
// with no coupon at all rolls into cash (roll_cash).
func TestAFloatingNote(t *testing.T) {
	t.Parallel()
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
	h := bondHolding("broker", "A", "QFRN", 10000, 10000)
	o := bondHolding("broker", "A", "QFRX", 10000, 10000)
	in.Holdings = []Holding{h, o}
	in.Terms[InstrumentKey{"broker", "QFRN"}] = Terms{Coupon: num(5), Maturity: date(2027, 6, 15),
		CouponKind: canonical.CouponFloating}
	in.Terms[InstrumentKey{"broker", "QFRX"}] = Terms{Maturity: date(2027, 6, 15), CouponKind: canonical.CouponFloating}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if p.Kind != KindFloating || p.Implied != nil || !p.Tags[TagFloating] {
		t.Fatalf("kind %s implied %v tags %v", p.Kind, p.Implied, p.Tags)
	}
	// The next quarterly date from its maturity is September 15.
	if *p.Duration < 0.2 || *p.Duration > 0.26 {
		t.Errorf("duration %v", *p.Duration)
	}
	near(t, "dv01", p.DV01, 10000**p.Duration*1e-4, 1e-12)
	near(t, "a month", num(incomeOf(b, h.HoldingKey, 5, RuleCash, PathConstant, 0)), 10000*5.0/1200, 1e-9)
	rung := func(k HoldingKey) []string {
		var out []string
		for _, part := range partsOf(b, k, RuleRoll) {
			if part.Reinvested != "" && !slices.Contains(out, part.Reinvested) {
				out = append(out, part.Reinvested)
			}
		}
		return out
	}
	if got := rung(h.HoldingKey); !slices.Equal(got, []string{"roll_own"}) {
		t.Errorf("rolled %v", got)
	}
	near(t, "rolled", num(incomeOf(b, h.HoldingKey, 14, RuleRoll, PathForward, 0)), 10000*5.0/1200, 1e-9)
	if got := rung(o.HoldingKey); !slices.Equal(got, []string{"roll_cash"}) {
		t.Errorf("no coupon, rolled %v", got)
	}
}

// A term deposit earns its rate to its end day, then its principal
// earns from that day on: its maturity month splits by day. Rolled, it
// buys its original term again, and earns interest.
func TestATermDepositsMaturityMonth(t *testing.T) {
	t.Parallel()
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
	h := cash("bank", "T", "USD", 10000)
	in.Holdings = []Holding{h}
	in.Rates[AccountKey{"bank", "T"}] = []Rate{{Credit: true, Tier: 1, Pct: 3, ValidFrom: date(2026, 3, 15),
		TermEnd: date(2026, 12, 15)}}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	if p.Kind != KindTermDeposit {
		t.Fatalf("kind %s", p.Kind)
	}
	month := 25.0 // 10,000 at 3% a month
	near(t, "the rest of June", num(incomeOf(b, h.HoldingKey, 0, RuleIdle, PathConstant, 0)), month*15/30, 1e-9)
	near(t, "December, idle", num(incomeOf(b, h.HoldingKey, 6, RuleIdle, PathConstant, 0)), month*14/31, 1e-9)
	near(t, "December, rolled", num(incomeOf(b, h.HoldingKey, 6, RuleRoll, PathConstant, 0)), month, 1e-9)
	// Each rung starts nine months after the last, the deposit's own
	// term.
	var starts []string
	for _, part := range partsOf(b, h.HoldingKey, RuleRoll) {
		if part.Reinvested != "roll" {
			continue
		}
		if part.Type != TypeInterest {
			t.Errorf("rolled part %+v", part)
		}
		if part.From.Day() != 1 {
			starts = append(starts, part.From.Format(time.DateOnly))
		}
	}
	if !slices.Equal(starts, []string{"2026-12-15", "2027-09-15", "2028-06-15", "2029-03-15"}) {
		t.Errorf("rungs start %v", starts)
	}
}

// The roll term is the holding's original one: from the day it was
// bought, else the term left today.
func TestTheRollTerm(t *testing.T) {
	t.Parallel()
	in := book()
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	pj := &projector{in: in}
	m := &Measures{Kind: KindBond}
	if got := pj.term(h, m, *date(2027, 6, 15)); got != 12 {
		t.Errorf("term left %d", got)
	}
	h.Acquired = date(2024, 6, 15)
	if got := pj.term(h, m, *date(2027, 6, 15)); got != 36 {
		t.Errorf("term from purchase %d", got)
	}
}

// A loan held pays its interest monthly and returns its principal at
// maturity.
func TestALoanHeldReturnsItsPrincipal(t *testing.T) {
	t.Parallel()
	in := book()
	h := Holding{HoldingKey: HoldingKey{"broker", "A", "QLOAN"}, Instrument: "QLOAN", AssetClass: "fixed_income",
		Vehicle: "loan", Currency: "USD", Value: 5000, Amount: 5000}
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QLOAN"}] = Terms{Coupon: num(6), Maturity: date(2027, 3, 31)}
	b := Compute(in)
	near(t, "a month", num(incomeOf(b, h.HoldingKey, 4, RuleIdle, PathConstant, 0)), 25, 1e-9)
	if len(b.Projection.Events) != len(Rules) || !b.Projection.Events[0].Day.Equal(*date(2027, 3, 31)) ||
		b.Projection.Events[0].Amount != 5000 {
		t.Errorf("events %+v", b.Projection.Events)
	}
}

// A dividend expected later this month counts in it whole.
func TestADividendLaterThisMonth(t *testing.T) {
	t.Parallel()
	in := book()
	h := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
	in.Holdings = []Holding{h}
	for _, d := range []*time.Time{date(2025, 6, 25), date(2025, 9, 25), date(2025, 12, 25), date(2026, 3, 25)} {
		in.Payments[h.HoldingKey] = append(in.Payments[h.HoldingKey], Payment{Day: *d, Amount: 50, Quantity: num(100),
			Type: TypeDividends})
	}
	b := Compute(in)
	near(t, "June", num(incomeOf(b, h.HoldingKey, 0, RuleCash, PathConstant, 0)), 50, 0)
	near(t, "July", num(incomeOf(b, h.HoldingKey, 1, RuleCash, PathConstant, 0)), 0, 0)
	near(t, "September", num(incomeOf(b, h.HoldingKey, 3, RuleCash, PathConstant, 0)), 50, 0)
}

// The lines a fund distributes on one day are one payment: three lines
// of a once-a-year distribution indicate their sum, once a year, in any
// order.
func TestADistributionInSeveralLines(t *testing.T) {
	t.Parallel()
	for _, amounts := range [][]float64{{300, 20, 80}, {80, 300, 20}} {
		in := book()
		h := fund("QBF", "public_equity", 10000)
		h.Vehicle, h.Quantity = "fund", num(100)
		in.Holdings = []Holding{h}
		for i, amt := range amounts {
			in.Payments[h.HoldingKey] = append(in.Payments[h.HoldingKey], Payment{
				Day: date(2025, 12, 20).Add(time.Duration(i) * time.Minute), Amount: amt, Quantity: num(100),
				Type: TypeDistributions})
		}
		b := Compute(in)
		near(t, "income 12m", position(b, h.HoldingKey).Income12, 400, 1e-9)
		near(t, "December", num(incomeOf(b, h.HoldingKey, 6, RuleCash, PathConstant, 0)), 400, 1e-9)
	}
}

// An equity fund yields the inverse of its P/E and the trailing yield
// its page states, and distributes that yield month by month.
func TestAnEquityFund(t *testing.T) {
	t.Parallel()
	in := book()
	h := fund("QEQ", "public_equity", 12000)
	h.Vehicle = "etf"
	in.Holdings = []Holding{h}
	in.Funds["symbol:QEQ"] = &Fund{PriceEarnings: &Dated{Value: 20}, TrailingYield: &Dated{Value: 1.8}}
	b := Compute(in)
	p := position(b, h.HoldingKey)
	near(t, "earnings yield", p.EarningsYield, 5, 1e-12)
	near(t, "dividend yield", p.DividendYield, 1.8, 0)
	near(t, "a month", num(incomeOf(b, h.HoldingKey, 2, RuleCash, PathConstant, 0)), 12000*1.8/1200, 1e-9)
}

// A callable bond above par yields to its call where that is lower; a
// make-whole call does not count. A zero implies its discount.
func TestCallableAndZeroBonds(t *testing.T) {
	t.Parallel()
	read := func(terms Terms, amount float64) Position {
		in := book()
		h := bondHolding("broker", "A", "QB", 10000, amount)
		in.Holdings = []Holding{h}
		in.Terms[InstrumentKey{"broker", "QB"}] = terms
		return position(Compute(in), h.HoldingKey)
	}
	callable := Terms{Coupon: num(6), Maturity: date(2036, 6, 15), CallDate: date(2028, 6, 15)}
	toCall := read(callable, 10500)
	callable.MakeWhole = true
	toMaturity := read(callable, 10500)
	ytm, _ := fixedincome.Bond{Coupon: 6, Frequency: 2, Maturity: *date(2036, 6, 15),
		DayCount: canonical.DayCountActAct}.Yield(day, 105)
	near(t, "make-whole", toMaturity.Implied, ytm, 1e-9)
	if *toCall.Implied >= ytm {
		t.Errorf("to call %v, to maturity %v", *toCall.Implied, ytm)
	}
	z := read(Terms{Maturity: date(2031, 6, 15), CouponKind: canonical.CouponZero}, 8000)
	near(t, "zero", z.Implied, (math.Pow(1.25, 1.0/10)-1)*200, 1e-6)
}

// The grains reconcile for every figure: the sum of each grain's rows is
// the total row and the converted global row, and an entity's converted
// row sums its split ones; an overdraft and a second currency in one
// account among them.
func TestEveryGrainReconciles(t *testing.T) {
	t.Parallel()
	in := book()
	in.Curves["USD"] = flatCurve("USD", 4)
	b1 := bondHolding("broker", "A", "QB", 10000, 10000)
	usd, eur := cash("bank", "S", "USD", 5000), cash("bank", "S", "EUR", 2000)
	eur.Value = 2200
	od := cash("bank", "O", "USD", -1000)
	in.Holdings = []Holding{b1, usd, eur, od}
	in.Accounts[AccountKey{"bank", "S"}] = Account{Portfolio: "P1"}
	in.Accounts[AccountKey{"bank", "O"}] = Account{Portfolio: "P1"}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(5), Maturity: date(2030, 6, 15)}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 1, Currency: "USD"},
		{Credit: true, Tier: 1, Pct: 0.5, Currency: "EUR"}}
	in.Rates[AccountKey{"bank", "O"}] = []Rate{{Credit: false, Tier: 1, Pct: 9}}
	b := Compute(in)
	figures := map[string]func(Row) float64{
		"value": func(r Row) float64 { return r.Value },
		"dv01":  func(r Row) float64 { return *r.DV01 },
		"income": func(r Row) float64 {
			return *r.Income12
		},
		"income_100bp": func(r Row) float64 { return *r.Income100bp },
	}
	global := b.Aggregate(GrainAll, false)
	total, conv := global[len(global)-1], b.Aggregate(GrainAll, true)[0]
	for _, g := range []Grain{GrainSources, GrainPortfolios, GrainAccounts} {
		split, converted := b.Aggregate(g, false), b.Aggregate(g, true)
		for name, get := range figures {
			sum, csum := 0.0, 0.0
			bySplit := map[entity]float64{}
			for _, r := range split {
				if !r.Total {
					sum += get(r)
					bySplit[entity{r.Source, r.Portfolio, r.AccountID}] += get(r)
				}
			}
			for _, r := range converted {
				csum += get(r)
				if e := (entity{r.Source, r.Portfolio, r.AccountID}); math.Abs(bySplit[e]-get(r)) > 1e-9 {
					t.Errorf("%s %s %v: converted %v, split %v", g, name, e, get(r), bySplit[e])
				}
			}
			for _, v := range []float64{sum, csum, get(conv)} {
				if math.Abs(v-get(total)) > 1e-9 {
					t.Errorf("%s %s: %v, total %v", g, name, v, get(total))
				}
			}
		}
	}
}

// A wrapper the withholding table names no rate for reads its country's
// default row; a situs the table does not name withholds nothing.
func TestWithholdingByWrapper(t *testing.T) {
	t.Parallel()
	in := book()
	a, b := bondHolding("broker", "A", "QA", 1000, 1000), bondHolding("broker", "R", "QR", 1000, 1000)
	c := bondHolding("broker", "A", "QC", 1000, 1000)
	in.Holdings = []Holding{a, b, c}
	in.Accounts[AccountKey{"broker", "A"}] = Account{TaxWrapper: string(canonical.TaxWrapperTaxablePersonal)}
	in.Accounts[AccountKey{"broker", "R"}] = Account{TaxWrapper: "roth_ira"}
	in.Situs[a.HoldingKey], in.Situs[b.HoldingKey], in.Situs[c.HoldingKey] = "US", "US", "XX"
	in.Withholding["US"] = map[string]float64{canonical.WithholdingDefault: 30,
		string(canonical.TaxWrapperTaxablePersonal): 15}
	w := Compute(in).Projection.Withholding
	if w[a.HoldingKey] != 15 || w[b.HoldingKey] != 30 {
		t.Errorf("withholding %v", w)
	}
	if _, ok := w[c.HoldingKey]; ok {
		t.Errorf("an unnamed situs withholds %v", w[c.HoldingKey])
	}
}

// Rolled along a sloped curve, a holding buys its term again at that
// day's forward rate for the term plus its spread over the curve today;
// under constant rates, at its own yield.
func TestARollAlongTheCurve(t *testing.T) {
	t.Parallel()
	in := book()
	pts := []fixedincome.Point{{Months: 3, Rate: 3}, {Months: 12, Rate: 4}, {Months: 60, Rate: 5}}
	z, err := fixedincome.Bootstrap(pts)
	if err != nil {
		t.Fatal(err)
	}
	in.Curves["USD"] = &Curve{Currency: "USD", Zero: z, Benchmark: 3, BenchmarkMonths: 3}
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	h.Acquired = date(2025, 12, 15)
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4.5), Maturity: date(2026, 12, 15)}
	b := Compute(in)
	own := *position(b, h.HoldingKey).Implied
	spread := own - z.Zero(12)
	for _, p := range partsOf(b, h.HoldingKey, RuleRoll) {
		if p.Reinvested != "roll" || p.From.Day() != 1 || p.Month != 8 {
			continue
		}
		// The first rung starts in month 6, December: its rate is the
		// forward of month 6 for twelve months.
		near(t, "forward", &p.RollForward, 10000*(z.Forward(6, 12)+spread)/1200, 1e-9)
		near(t, "constant", &p.RollConstant, 10000*own/1200, 1e-9)
		if math.Abs(p.RollForward-p.RollConstant) < 1e-6 {
			t.Errorf("the forward roll equals the constant one on a sloped curve")
		}
		return
	}
	t.Fatal("no rolled part in month 8")
}

// A term deposit's rate ends with its term: under the cash rule its
// principal finds no rate in its account after that, and reads unknown.
func TestATermRateEndsWithItsTerm(t *testing.T) {
	t.Parallel()
	in := book()
	h := cash("bank", "T", "USD", 12000)
	in.Holdings = []Holding{h}
	in.Rates[AccountKey{"bank", "T"}] = []Rate{{Credit: true, Tier: 1, Pct: 3, TermEnd: date(2026, 9, 15)}}
	b := Compute(in)
	near(t, "December, cash", num(incomeOf(b, h.HoldingKey, 6, RuleCash, PathConstant, 0)), 0, 0)
	p := position(b, h.HoldingKey)
	near(t, "income 12m", p.Income12, 30+30+30*14.0/30, 1e-9)
	if p.Tags[TagUnknown] {
		t.Errorf("its own figures are known: %v", p.Tags)
	}
}

// A holding a source lists on or after its maturity returns its
// principal the next day.
func TestABondPastItsMaturity(t *testing.T) {
	t.Parallel()
	in := book()
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4), Maturity: date(2026, 6, 15)}
	b := Compute(in)
	if e := b.Projection.Events; len(e) != len(Rules) || !e[0].Day.Equal(*date(2026, 6, 16)) {
		t.Errorf("events %+v", e)
	}
}

// A described bond maturing into cash that states no rate keeps its own
// readings: what its principal earns after is no figure of the bond's.
func TestABondMaturingIntoUnknownCash(t *testing.T) {
	t.Parallel()
	in := book()
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4), Maturity: date(2026, 12, 15), Rating: "AA"}
	b := Compute(in)
	if p := position(b, h.HoldingKey); p.Tags[TagUnknown] || len(missingOf(p)) != 0 {
		t.Errorf("tags %v missing %v", p.Tags, missingOf(p))
	}
}

// A rate reads declared only where a ledger record states the rate: a
// record of a fund's duration alone leaves its filed yield stated. A
// rate no curve moves stays stated under the forward path.
func TestTheBasisLabels(t *testing.T) {
	t.Parallel()
	in := book()
	mmf := fund("QMMF", "cash", 10000)
	chf := cash("bank", "C", "CHF", 5000)
	chf.Value = 5500
	in.Holdings = []Holding{mmf, chf}
	in.Funds["symbol:QMMF"] = &Fund{SevenDayYield: &Dated{Value: 4, Day: day}}
	in.Terms[InstrumentKey{"broker", "QMMF"}] = Terms{Duration: num(0.1)}
	in.Rates[AccountKey{"bank", "C"}] = []Rate{{Credit: true, Tier: 1, Pct: 1}}
	b := Compute(in)
	w := b.NextMonths(12)
	for _, a := range b.Amounts(nil, w, Assumptions{Path: PathForward, Rule: RuleCash}) {
		if a.Basis != BasisStated {
			t.Errorf("%v: basis %s", a.Holding, a.Basis)
		}
	}
}

// A liability beside a line of no value reads its own rate: the zero
// line weighs nothing.
func TestALiabilityBesideAZeroLine(t *testing.T) {
	t.Parallel()
	in := book()
	m := Holding{HoldingKey: HoldingKey{"bank", "M", "QMORT"}, Instrument: "QMORT", AssetClass: "real_estate",
		Vehicle: "mortgage", Currency: "USD", Value: -100000, Amount: -100000}
	in.Holdings = []Holding{m, cash("bank", "M", "USD", 0)}
	in.Terms[InstrumentKey{"bank", "M"}] = Terms{}
	in.Terms[InstrumentKey{"bank", "QMORT"}] = Terms{Coupon: num(2), Maturity: date(2036, 6, 15)}
	near(t, "stated", Compute(in).Aggregate(GrainAll, false)[0].Stated, 2, 0)
}

// The funds view shows the duration the report reads, a ledger record's
// over the page's; the cash view shows an overdraft's own debit tiers.
func TestTheFundsAndCashViews(t *testing.T) {
	t.Parallel()
	in := book()
	bf := fund("QBF", "fixed_income", 20000)
	o := cash("bank", "O", "USD", -500)
	in.Holdings = []Holding{bf, o}
	in.Funds["symbol:QBF"] = &Fund{YieldToMaturity: &Dated{Value: 4.2}, Duration: &Dated{Value: 5}}
	in.Terms[InstrumentKey{"broker", "QBF"}] = Terms{Duration: num(6.5)}
	in.Rates[AccountKey{"bank", "O"}] = []Rate{{Credit: true, Tier: 1, Pct: 1}, {Credit: true, Tier: 2, Pct: 2},
		{Credit: false, Tier: 1, Pct: 9}}
	b := Compute(in)
	if f := b.Funds(); len(f) != 1 || *f[0].Duration != 6.5 {
		t.Errorf("funds %+v", f)
	}
	if c := b.Cash(); len(c) != 1 || c[0].Tiers != 1 || *c[0].Stated != 9 {
		t.Errorf("cash %+v", c)
	}
}
