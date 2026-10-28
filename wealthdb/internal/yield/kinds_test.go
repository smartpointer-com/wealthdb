package yield

import (
	"math"
	"slices"
	"strings"
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
		in := inputs()
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
	in := inputs()
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
		in := inputs()
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
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
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
	in := inputs()
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
	in := inputs()
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

// A card's credit balance takes no rate, no tag and no expected
// interest, and misses no rate (YIELD.md §3).
func TestACardMissesNoRate(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := cash("bank", "C", "USD", 300)
	in.Holdings = []Holding{h}
	in.Accounts[AccountKey{"bank", "C"}] = Account{Name: "Card", Kind: "card"}
	b := Compute(in)
	if c := b.Coverage(); slices.ContainsFunc(c, func(r CoverageRow) bool {
		return r.Reason == missingRate || r.Reason == string(TagUnknown)
	}) {
		t.Errorf("coverage %+v", c)
	}
	if p := position(b, h.HoldingKey); len(p.Tags) != 0 || p.Stated != nil {
		t.Errorf("a card's balance reads %v, stated %v", p.Tags, p.Stated)
	}
	near(t, "card income", num(incomeOf(b, h.HoldingKey, 3, RuleCash, PathConstant, 0)), 0, 0)
}

// An overdraft costs its debit rate but weighs in no rate: the rates say
// what the assets earn. The cash view lists it after the credit balances.
func TestAnOverdraft(t *testing.T) {
	t.Parallel()
	in := inputs()
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
	in := inputs()
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
	in := inputs()
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

// A floating note resets quarterly where its terms state no frequency,
// lasts to its next reset, and earns the benchmark plus its spread on its
// face. Rolled, it buys again at its own current yield (roll_own); a note
// with no coupon at all rolls into cash (roll_cash).
func TestAFloatingNote(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
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
	rung := func(k HoldingKey) []Rung {
		var out []Rung
		for _, part := range partsOf(b, k, RuleRoll) {
			if part.Reinvested != "" && !slices.Contains(out, part.Reinvested) {
				out = append(out, part.Reinvested)
			}
		}
		return out
	}
	if got := rung(h.HoldingKey); !slices.Equal(got, []Rung{RungRollOwn}) {
		t.Errorf("rolled %v", got)
	}
	near(t, "rolled", num(incomeOf(b, h.HoldingKey, 14, RuleRoll, PathForward, 0)), 10000*5.0/1200, 1e-9)
	if got := rung(o.HoldingKey); !slices.Equal(got, []Rung{RungRollCash}) {
		t.Errorf("no coupon, rolled %v", got)
	}
}

// A term deposit earns its rate to its end day, then its principal
// earns from that day on: its maturity month splits by day. Rolled, it
// buys its original term again, and earns interest.
func TestATermDepositsMaturityMonth(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
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
		if part.Reinvested != RungRoll {
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
	in := inputs()
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
	in := inputs()
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
	in := inputs()
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
		in := inputs()
		h := fund("QBF", "public_equity", 10000)
		h.Quantity = num(100)
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
	in := inputs()
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
		in := inputs()
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
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
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
					bySplit[entity{source: r.Source, portfolio: r.Portfolio, account: r.AccountID}] += get(r)
				}
			}
			for _, r := range converted {
				csum += get(r)
				if e := (entity{source: r.Source, portfolio: r.Portfolio, account: r.AccountID}); math.Abs(bySplit[e]-get(r)) > 1e-9 {
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
	in := inputs()
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

// An account's own rate wins over its wrapper's and the default: of two
// accounts of one wrapper, only the one the block names moves. An
// account's rate also stands where its country names no wrapper.
func TestWithholdingByAccount(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b := bondHolding("broker", "A", "QA", 1000, 1000), bondHolding("broker", "B", "QB", 1000, 1000)
	c, d := bondHolding("broker", "B", "QC", 1000, 1000), bondHolding("broker", "A", "QD", 1000, 1000)
	in.Holdings = []Holding{a, b, c, d}
	for _, acct := range []string{"A", "B"} {
		in.Accounts[AccountKey{"broker", acct}] = Account{TaxWrapper: string(canonical.TaxWrapperCustodialUTMA)}
	}
	in.Situs[a.HoldingKey], in.Situs[b.HoldingKey] = "US", "US"
	in.Situs[c.HoldingKey], in.Situs[d.HoldingKey] = "XX", "XX"
	in.Withholding["US"] = map[string]float64{canonical.WithholdingDefault: 30,
		string(canonical.TaxWrapperCustodialUTMA): 15}
	before := Compute(in).Projection.Withholding
	in.AccountWithholding = map[AccountKey]map[string]float64{{"broker", "B"}: {"US": 0, "XX": 5}}
	after := Compute(in).Projection.Withholding
	if before[a.HoldingKey] != 15 || after[a.HoldingKey] != 15 {
		t.Errorf("account A: %v then %v, want 15 both times", before[a.HoldingKey], after[a.HoldingKey])
	}
	if before[b.HoldingKey] != 15 || after[b.HoldingKey] != 0 {
		t.Errorf("account B: %v then %v, want 15 then its own 0", before[b.HoldingKey], after[b.HoldingKey])
	}
	if w, ok := after[c.HoldingKey]; !ok || w != 5 {
		t.Errorf("account B in XX: %v %v, want its own 5", w, ok)
	}
	if _, ok := after[d.HoldingKey]; ok {
		t.Errorf("account A in XX withholds %v; the block names no rate for it", after[d.HoldingKey])
	}
}

// Rolled along a sloped curve, a holding buys its term again at that
// day's forward rate for the term plus its spread over the curve today;
// under constant rates, at its own yield.
func TestARollAlongTheCurve(t *testing.T) {
	t.Parallel()
	in := inputs()
	pts := []fixedincome.Point{{Months: 3, Rate: 3}, {Months: 12, Rate: 4}, {Months: 60, Rate: 5}}
	z, err := fixedincome.NewCurve(pts, fixedincome.Treasury)
	if err != nil {
		t.Fatal(err)
	}
	in.Curves["USD"] = &Curve{Zero: z, Benchmark: 3, BenchmarkMonths: 3}
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	h.Acquired = date(2025, 12, 15)
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4.5), Maturity: date(2026, 12, 15)}
	b := Compute(in)
	own := *position(b, h.HoldingKey).Implied
	spread := own - z.Zero(12)
	for _, p := range partsOf(b, h.HoldingKey, RuleRoll) {
		if p.Reinvested != RungRoll || p.From.Day() != 1 || p.Month != 8 {
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
	in := inputs()
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
	in := inputs()
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
	in := inputs()
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
	in := inputs()
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
	in := inputs()
	m := Holding{HoldingKey: HoldingKey{"bank", "M", "QMORT"}, Instrument: "QMORT", AssetClass: "real_estate",
		Vehicle: "mortgage", Currency: "USD", Value: -100000, Amount: -100000}
	in.Holdings = []Holding{m, cash("bank", "M", "USD", 0)}
	in.Terms[InstrumentKey{"bank", "QMORT"}] = Terms{Coupon: num(2), Maturity: date(2036, 6, 15)}
	near(t, "stated", Compute(in).Aggregate(GrainAll, false)[0].Stated, 2, 0)
}

// The funds view shows the duration the report reads, a ledger record's
// over the page's; the cash view shows an overdraft's own debit tiers.
func TestTheFundsAndCashViews(t *testing.T) {
	t.Parallel()
	in := inputs()
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

// A security held directly reads its holdings' figures. One held only
// through funds reads its largest line: its price, the yield the price
// implies (implied stale_price), and the earnings and dividend yields
// over that price where its figures' currency is the line's. A line in
// default reads default.
func TestTheSecuritiesView(t *testing.T) {
	t.Parallel()
	in := inputs()
	s := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100), SecurityID: "symbol:QSTK",
		Key: "symbol:QSTK"}
	in.Holdings = []Holding{s}
	in.Securities["symbol:QSTK"] = Facts{EPS: num(5), DPS: num(2), Currency: "USD"}
	in.Securities["isin:QA"] = Facts{EPS: num(2.5), EPSYearAgo: num(2), DPS: num(1), Currency: "USD"}
	in.Securities["isin:QE"] = Facts{EPS: num(4), Currency: "USD"}
	in.Lines = map[string]LineInfo{
		"isin:QA": {Price: num(50), Yield: num(4.2), Duration: num(3), Coupon: num(4), Maturity: date(2030, 1, 15),
			Currency: "USD"},
		"isin:QD": {Default: true, Price: num(30), Currency: "USD"},
		"isin:QE": {Price: num(80), Currency: "EUR"},
	}
	rows := Compute(in).Securities([]SecurityRow{{Key: "symbol:QSTK"}, {Key: "isin:QA"}, {Key: "isin:QD"},
		{Key: "isin:QE"}, {Key: "isin:QX"}})
	stk, qa, qd, qe, qx := rows[0], rows[1], rows[2], rows[3], rows[4]
	near(t, "direct earnings yield", stk.EarningsYield, 5, 1e-9)
	near(t, "direct price", stk.Price, 100, 0)
	near(t, "direct P/E", stk.PriceEarnings, 20, 1e-9)
	near(t, "line price", qa.Price, 50, 0)
	near(t, "line yield", qa.Implied, 4.2, 0)
	near(t, "line earnings yield", qa.EarningsYield, 5, 1e-9)
	near(t, "line dividend yield", qa.DividendYield, 2, 1e-9)
	near(t, "line P/E", qa.PriceEarnings, 20, 1e-9)
	near(t, "line payout", qa.Payout, 40, 1e-9)
	near(t, "line earnings growth", qa.EarningsGrowth, 25, 1e-9)
	if qa.Quality != "implied stale_price" || qd.Quality != "default" || qe.Quality != "" || qx.Quality != "" {
		t.Errorf("qualities %q %q %q %q", qa.Quality, qd.Quality, qe.Quality, qx.Quality)
	}
	if qe.EarningsYield != nil || qe.Price == nil || qx.Price != nil {
		t.Errorf("another currency: %+v; no line: %+v", qe, qx)
	}
}

// The curve view splits each currency's DV01 by what holds it, a
// liability's netted, and its rows sum to the holdings' DV01.
func TestTheCurveView(t *testing.T) {
	t.Parallel()
	in := inputs()
	b1 := bondHolding("broker", "A", "QB", 10000, 10000)
	bf := fund("QBF", "fixed_income", 30000)
	td := cash("bank", "T", "USD", 20000)
	mort := Holding{HoldingKey: HoldingKey{"bank", "M", "QMORT"}, Instrument: "QMORT", AssetClass: "real_estate",
		Vehicle: "mortgage", Currency: "USD", Value: -50000, Amount: -50000}
	in.Holdings = []Holding{b1, bf, td, mort}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4), Maturity: date(2031, 6, 15)}
	in.Terms[InstrumentKey{"bank", "QMORT"}] = Terms{Coupon: num(2), Maturity: date(2036, 6, 15)}
	in.Funds["symbol:QBF"] = &Fund{Risk: map[string]map[string]float64{"USD": {"5Y": 1000, "10Y": 2000}},
		NetAssets: num(1e6)}
	in.Rates[AccountKey{"bank", "T"}] = []Rate{{Credit: true, Tier: 1, Pct: 3, TermEnd: date(2027, 6, 15)}}
	b := Compute(in)
	byHolder, holdings := map[string]float64{}, 0.0
	for _, p := range b.Positions {
		holdings += *p.DV01
		for _, v := range p.Buckets["USD"] {
			byHolder[HeldIn(p.Kind)] += v
		}
	}
	var rows, funds, bonds, cash, liabilities float64
	for _, r := range b.Curve() {
		if r.Currency != "USD" || math.Abs(r.Funds+r.Bonds+r.Cash+r.Liabilities-r.DV01) > 1e-9 ||
			r.DV100 != r.DV01*100 {
			t.Errorf("row %+v", r)
		}
		rows += r.DV01
		funds, bonds, cash, liabilities = funds+r.Funds, bonds+r.Bonds, cash+r.Cash, liabilities+r.Liabilities
	}
	near(t, "rows", &rows, holdings, 1e-9)
	near(t, "funds", &funds, 90, 1e-9)
	near(t, "bonds", &bonds, byHolder[HeldInBonds], 1e-9)
	near(t, "cash", &cash, byHolder[HeldInCash], 1e-9)
	near(t, "liabilities", &liabilities, byHolder[HeldInLiabilities], 1e-9)
	if bonds <= 0 || cash <= 0 || liabilities >= 0 {
		t.Errorf("bonds %v cash %v liabilities %v", bonds, cash, liabilities)
	}
}

// A stock the transactions show paid nothing indicates its filings'
// dividends per share, a quarter of them every quarter from two months
// back. A figure in another currency than the price indicates nothing.
func TestTheFilingsDividend(t *testing.T) {
	t.Parallel()
	read := func(ccy string) (*Book, Holding) {
		in := inputs()
		h := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
			Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100), SecurityID: "symbol:QSTK"}
		in.Holdings = []Holding{h}
		in.Securities["symbol:QSTK"] = Facts{DPS: num(4), Currency: ccy}
		return Compute(in), h
	}
	b, h := read("USD")
	p := position(b, h.HoldingKey)
	near(t, "dividend yield", p.DividendYield, 4, 1e-9)
	near(t, "income 12m", p.Income12, 400, 1e-9)
	if !p.Tags[TagIndicated] {
		t.Errorf("tags %v", p.Tags)
	}
	// Two months back is April 15: July, October, January and April.
	for _, m := range []int{1, 4, 7, 10} {
		near(t, "a quarter", num(incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, 0)), 100, 1e-9)
	}
	b, h = read("EUR")
	if p := position(b, h.HoldingKey); p.Tags[TagIndicated] || *p.DividendYield != 0 {
		t.Errorf("another currency: tags %v yield %v", p.Tags, *p.DividendYield)
	}
}

// A principal that lands on a cash balance earns the account's marginal
// rate: the tiers' rate on the balance with it, less on the balance
// without it.
func TestAPrincipalEarnsTheMarginalTier(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := bondHolding("broker", "A", "QB", 10000, 10000)
	acct := cash("broker", "A", "USD", 40000)
	in.Holdings = []Holding{h, acct}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4), Maturity: date(2026, 12, 15)}
	in.Rates[AccountKey{"broker", "A"}] = []Rate{{Credit: true, Tier: 1, Pct: 2, UpTo: num(45000)},
		{Credit: true, Tier: 2, Pct: 1}}
	b := Compute(in)
	// 5,000 of the principal at 2% and 5,000 at 1%: 1.5% on 10,000.
	near(t, "February", num(incomeOf(b, h.HoldingKey, 8, RuleCash, PathConstant, 0)), 10000*1.5/1200, 1e-9)
}

// A fund's filed DV01 leaves out the rows of no currency: a credit
// spread's move is no move of a curve.
func TestAFundsRiskOfNoCurrency(t *testing.T) {
	t.Parallel()
	in := inputs()
	bf := fund("QBF", "fixed_income", 30000)
	in.Holdings = []Holding{bf}
	in.Funds["symbol:QBF"] = &Fund{Risk: map[string]map[string]float64{"USD": {"5Y": 1000},
		canonical.NoCurrency: {"5Y": 500}}, NetAssets: num(1e6)}
	p := position(Compute(in), bf.HoldingKey)
	if _, ok := p.Buckets[canonical.NoCurrency]; ok || len(p.Buckets) != 1 {
		t.Errorf("buckets %v", p.Buckets)
	}
	near(t, "dv01", p.DV01, 30, 1e-9)
}

// A bond in default lacks no terms: no terms record clears a default.
func TestABondInDefaultLacksNoTerms(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := bondHolding("broker", "A", "QD", 10000, 3000)
	in.Holdings = []Holding{h}
	in.Terms[InstrumentKey{"broker", "QD"}] = Terms{Coupon: num(8), Maturity: date(2027, 6, 15), InDefault: true,
		Rating: "D"}
	b := Compute(in)
	if p := position(b, h.HoldingKey); !p.Tags[TagUnknown] || len(missingOf(p)) != 0 {
		t.Errorf("tags %v missing %v", p.Tags, missingOf(p))
	}
	for _, r := range b.Coverage() {
		if r.Section != "rests_on" || r.Skeleton != nil {
			t.Errorf("coverage %+v", r)
		}
	}
}

// A bond's coupons a year are its terms', else its currency's
// convention, else one.
func TestTheConventionFrequency(t *testing.T) {
	t.Parallel()
	in := inputs()
	four := 4
	for _, c := range []struct {
		ccy   string
		terms Terms
		want  int
	}{
		{"USD", Terms{}, 2},
		{"CHF", Terms{}, 1},
		{"GBP", Terms{}, 1},
		{"GBP", Terms{Frequency: &four}, 4},
	} {
		if got := frequencyOf(in, Holding{Currency: c.ccy}, c.terms); got != c.want {
			t.Errorf("%s %v: %d, want %d", c.ccy, c.terms.Frequency, got, c.want)
		}
	}
	in.Conventions["USD"] = canonical.YieldConvention{Market: "USD", Frequency: 0}
	if got := in.ConventionFrequency("USD"); got != 1 {
		t.Errorf("a convention of no coupons: %d", got)
	}
}

// The year to the instant leaves out the same day a year before: a yearly
// payer paid on the day and on the day a year back pays once a year, and
// a holding no figure describes repeats one payment, not two.
func TestTheYearToTheInstant(t *testing.T) {
	t.Parallel()
	in := inputs()
	s := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
	o := Holding{HoldingKey: HoldingKey{"broker", "A", "QNOTE"}, Instrument: "QNOTE", AssetClass: "alternatives",
		Vehicle: "structured_product", Currency: "USD", Value: 5000, Amount: 5000, Quantity: num(5)}
	in.Holdings = []Holding{s, o}
	for _, d := range []time.Time{day.AddDate(-1, 0, 0), day} {
		in.Payments[s.HoldingKey] = append(in.Payments[s.HoldingKey], Payment{Day: d, Amount: 300, Quantity: num(100),
			Type: TypeDividends})
		in.Payments[o.HoldingKey] = append(in.Payments[o.HoldingKey], Payment{Day: d, Amount: 120, Type: TypeInterest})
	}
	b := Compute(in)
	p := position(b, s.HoldingKey)
	near(t, "dividend yield", p.DividendYield, 3, 1e-9)
	near(t, "income 12m", p.Income12, 300, 1e-9)
	near(t, "repeated", position(b, o.HoldingKey).Income12, 120, 1e-9)
}

// A holding's income yield is its next twelve months' income over its
// value; none where it has no income figure (a liability) or its value
// is zero.
func TestAPositionsIncomeYield(t *testing.T) {
	t.Parallel()
	in := inputs()
	b1 := bondHolding("broker", "A", "QB", 10000, 10000)
	mort := Holding{HoldingKey: HoldingKey{"bank", "M", "QMORT"}, Instrument: "QMORT", AssetClass: "real_estate",
		Vehicle: "mortgage", Currency: "USD", Value: -50000, Amount: -50000}
	zero := cash("bank", "S", "USD", 0)
	in.Holdings = []Holding{b1, mort, zero}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4), Maturity: date(2031, 6, 15)}
	in.Terms[InstrumentKey{"bank", "QMORT"}] = Terms{Coupon: num(2), Maturity: date(2036, 6, 15)}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 1}}
	b := Compute(in)
	p := position(b, b1.HoldingKey)
	near(t, "income yield", p.IncomeYield(), *p.Income12/p.Value*100, 1e-12)
	near(t, "the coupon", p.IncomeYield(), 4, 1e-9)
	if y := position(b, mort.HoldingKey).IncomeYield(); y != nil {
		t.Errorf("a liability: %v", *y)
	}
	if y := position(b, zero.HoldingKey).IncomeYield(); y != nil {
		t.Errorf("a value of zero: %v", *y)
	}
}

// A fund reads what it looks through to, whatever class a source gives
// the wrapper. One that files its rate sensitivity, or whose lines are
// mostly debt, is a bond fund: one classed as equity carries its filed
// DV01 per currency. One whose lines are mostly equity
// is an equity fund. Lines of neither, and no look-through at all, leave
// the fund its class.
func TestAFundsKindIsItsLookThrough(t *testing.T) {
	t.Parallel()
	in := inputs()
	filed, debt := fund("QBF1", "public_equity", 30000), fund("QDBT", "public_equity", 20000)
	equity, mixed := fund("QEQL", "fixed_income", 10000), fund("QMIX", "fixed_income", 8000)
	bonds, stocks := fund("QBND", "fixed_income", 5000), fund("QSTX", "public_equity", 5000)
	in.Holdings = []Holding{filed, debt, equity, mixed, bonds, stocks}
	in.Funds["symbol:QBF1"] = &Fund{Risk: map[string]map[string]float64{"EUR": {"5Y": 400}, "USD": {"10Y": 600}},
		NetAssets: num(1e6)}
	in.Funds["symbol:QDBT"] = &Fund{Lines: &LineSummary{AsOf: day, Duration: num(4), DurationPct: 80, DebtPct: 80,
		EquityPct: 10}}
	in.Funds["symbol:QEQL"] = &Fund{Lines: &LineSummary{AsOf: day, DebtPct: 5, EquityPct: 90},
		TrailingYield: &Dated{Value: 2}}
	in.Funds["symbol:QMIX"] = &Fund{Lines: &LineSummary{AsOf: day, DebtPct: 45, EquityPct: 45}}
	b := Compute(in)
	want := map[string]Kind{"QBF1": KindBondFund, "QDBT": KindBondFund, "QEQL": KindEquityFund, "QMIX": KindBondFund,
		"QBND": KindBondFund, "QSTX": KindEquityFund}
	for _, p := range b.Positions {
		if p.Kind != want[p.Instrument] {
			t.Errorf("%s: kind %s, want %s", p.Instrument, p.Kind, want[p.Instrument])
		}
	}
	// 3% of the fund: its filed DV01 in each currency.
	p := position(b, filed.HoldingKey)
	near(t, "EUR 5Y", num(p.Buckets["EUR"]["5Y"]), 12, 1e-9)
	near(t, "USD 10Y", num(p.Buckets["USD"]["10Y"]), 18, 1e-9)
	near(t, "dv01", p.DV01, 30, 1e-9)
	near(t, "lines' duration", position(b, debt.HoldingKey).Duration, 4, 0)
	near(t, "equity fund's dividend", position(b, equity.HoldingKey).DividendYield, 2, 0)
}

// A multi-asset fund moves with rates as a bond fund does, and earns what
// an equity fund does. One files its DV01, which the holding scales to
// its share; it pays distributions, which indicate its dividend; its
// lines imply no yield of the fund's. Another's lines give its duration
// over the whole fund, a line without one counting zero; its page gives
// its implied yield, which alone it compares with the benchmark, and the
// trailing yield it pays.
func TestAMultiAssetFund(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Curves["USD"] = flatCurve(4)
	filed, page := fund("QMA1", "multi_asset", 30000), fund("QMA2", "multi_asset", 20000)
	in.Holdings = []Holding{filed, page}
	in.Funds["symbol:QMA1"] = &Fund{Risk: map[string]map[string]float64{"USD": {"5Y": 1000, "10Y": 2000}},
		NetAssets: num(1e6), Lines: &LineSummary{AsOf: day, Yield: num(4.4), CoveredPct: 40, Duration: num(6),
			DurationPct: 40, DebtPct: 40, EquityPct: 60}}
	for _, d := range []*time.Time{date(2025, 9, 10), date(2025, 12, 10), date(2026, 3, 10), date(2026, 6, 10)} {
		in.Payments[filed.HoldingKey] = append(in.Payments[filed.HoldingKey], Payment{Day: *d, Amount: 150,
			Quantity: num(3000), Type: TypeDistributions})
	}
	in.Funds["symbol:QMA2"] = &Fund{Lines: &LineSummary{AsOf: day, Duration: num(6), DurationPct: 40, DebtPct: 40,
		EquityPct: 55}, YieldToMaturity: &Dated{Value: 3.9, Day: day}, TrailingYield: &Dated{Value: 2.5}}
	b := Compute(in)
	p := position(b, filed.HoldingKey)
	if p.Kind != KindMultiAsset || p.Implied != nil || p.Gap != nil || !p.Tags[TagIndicated] || p.Tags[TagImplied] {
		t.Errorf("filed: %s implied %v gap %v tags %v", p.Kind, p.Implied, p.Gap, p.Tags)
	}
	near(t, "filed dv01", p.DV01, 90, 1e-9)
	near(t, "filed 10Y", num(p.Buckets["USD"]["10Y"]), 60, 1e-9)
	// 0.05 a share four times a year, on a price of 10.
	near(t, "dividend yield", p.DividendYield, 2, 1e-9)
	near(t, "distributions", p.Income12, 600, 1e-9)
	q := position(b, page.HoldingKey)
	near(t, "whole-fund duration", q.Duration, 2.4, 1e-12)
	near(t, "lines' dv01", q.DV01, 20000*2.4e-4, 1e-9)
	// Its DV01 sits on the curve where its debt's does, at six years.
	for tenor, v := range fixedincome.BucketAt(6, 20000*2.4e-4) {
		near(t, "lines' "+tenor, num(q.Buckets["USD"][tenor]), v, 1e-12)
	}
	if len(q.Buckets["USD"]) != len(fixedincome.BucketAt(6, 1)) {
		t.Errorf("lines' buckets %v", q.Buckets)
	}
	near(t, "page yield", q.Implied, 3.9, 0)
	near(t, "gap", q.Gap, -0.1, 1e-12)
	near(t, "trailing yield", q.DividendYield, 2.5, 0)
	near(t, "stated distributions", q.Income12, 500, 1e-9)
	if q.FiguresFrom != "page" || !q.Tags[TagStated] {
		t.Errorf("page: %s %v", q.FiguresFrom, q.Tags)
	}
	if f := b.Funds(); len(f) != 2 || f[0].Kind != KindMultiAsset {
		t.Errorf("funds %+v", f)
	}
	if HeldIn(KindMultiAsset) != HeldInFunds {
		t.Errorf("held in %s", HeldIn(KindMultiAsset))
	}
	funds := 0.0
	for _, r := range b.Curve() {
		funds += r.Funds
	}
	near(t, "the curve's funds", &funds, 90+20000*2.4e-4, 1e-9)
}

// A holding whose principal matures, under the cash rule, into cash
// whose account states no rate lacks a receiving rate: bonds and a term
// deposit. Its skeleton is a rate record for that account and currency,
// one per account and currency: the cash beside two bonds lacks its rate
// too, and its line carries the one skeleton. A bond whose account
// states a rate in its currency earns it, though the account holds no
// cash today. A rate that names no currency is the cash's own, so it pays
// no principal where the account holds no cash.
func TestAMaturityIntoCashOfNoRate(t *testing.T) {
	t.Parallel()
	in := inputs()
	terms := Terms{Coupon: num(4), Maturity: date(2026, 12, 15), Rating: "AA"}
	a1, a2 := bondHolding("broker", "A", "QB", 10000, 10000), bondHolding("broker", "A", "QB2", 9000, 9000)
	rated, bare := bondHolding("broker", "R", "QR", 10000, 10000), bondHolding("broker", "X", "QX", 8000, 8000)
	term, idle := cash("bank", "T", "USD", 20000), cash("broker", "A", "USD", 500)
	in.Holdings = []Holding{a1, a2, rated, bare, term, idle}
	for _, h := range []Holding{a1, a2, rated, bare} {
		in.Terms[InstrumentKey{h.Source, h.Instrument}] = terms
	}
	in.Rates[AccountKey{"broker", "R"}] = []Rate{{Currency: "USD", Credit: true, Tier: 1, Pct: 1}}
	in.Rates[AccountKey{"broker", "X"}] = []Rate{{Credit: true, Tier: 1, Pct: 1}}
	in.Rates[AccountKey{"bank", "T"}] = []Rate{{Credit: true, Tier: 1, Pct: 3, TermEnd: date(2027, 3, 15)}}
	in.Accounts[AccountKey{"broker", "A"}] = Account{Name: "Brokerage"}
	in.Accounts[AccountKey{"broker", "X"}] = Account{Name: "Other brokerage"}
	in.Accounts[AccountKey{"bank", "T"}] = Account{Name: "Term"}
	b := Compute(in)
	var got []string
	for _, r := range b.Coverage() {
		switch {
		case r.Section == "missing" && r.Reason == missingReceiving:
			if r.Holdings != 4 || r.Value != 47000 {
				t.Errorf("missing %+v", r)
			}
		case r.Section == "holdings" && (r.Reason == missingReceiving || r.Reason == missingRate):
			line := r.Reason + " " + r.Source + "/" + r.Instrument
			if s := r.Skeleton; s != nil {
				line += " " + s.SilverSourceID + "/" + s.Instrument + "/" + s.Account + " " + s.Rate.Currency + " " + s.Note
			}
			got = append(got, line)
		}
	}
	want := "no rate broker/ broker//A USD Brokerage|no receiving rate bank/ bank//T USD Term|" +
		"no receiving rate broker/QB|no receiving rate broker/QB2|no receiving rate broker/QX broker//X USD Other brokerage"
	if strings.Join(got, "|") != want {
		t.Errorf("holdings\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
	// Its principal earns the account's 1%, no curve under it.
	reinvested := 0
	for _, p := range partsOf(b, rated.HoldingKey, RuleCash) {
		if p.Reinvested == RungCash {
			reinvested++
			if p.Basis != BasisReinvested || p.Spread != 1 || p.Principal <= 0 {
				t.Errorf("the rated account's principal: %+v", p)
			}
		}
	}
	if reinvested == 0 {
		t.Error("the rated account's principal earns nothing")
	}
}

// A deposit at a stated 0% has a figure: its row's implied yield is
// zero. A row whose holdings state no yield has none: a deposit whose
// account states no rate, beside a stock, which implies none.
func TestAStatedZeroRateIsAFigure(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.FXRates["CHF"], in.FXRates["GBP"] = 1.1, 1.3
	zero, none := cash("bank", "Z", "CHF", 1000), cash("bank", "N", "GBP", 1000)
	zero.Value, none.Value = 1100, 1300
	stock := func(iid, ccy string, fx float64) Holding {
		return Holding{HoldingKey: HoldingKey{"broker", "A", iid}, Instrument: iid, AssetClass: "public_equity",
			Vehicle: "stock", Currency: ccy, Value: 10000 * fx, Amount: 10000, Quantity: num(100)}
	}
	in.Holdings = []Holding{zero, stock("QCHF", "CHF", 1.1), none, stock("QGBP", "GBP", 1.3)}
	in.Rates[AccountKey{"bank", "Z"}] = []Rate{{Credit: true, Tier: 1, Pct: 0}}
	for _, r := range Compute(in).Aggregate(GrainAll, false) {
		switch r.Currency {
		case "CHF":
			near(t, "a stated 0%", r.Implied, 0, 0)
			near(t, "a stated 0%, stated", r.Stated, 0, 0)
		case "GBP":
			if r.Implied != nil || r.Stated != nil {
				t.Errorf("no figure: implied %v stated %v", r.Implied, r.Stated)
			}
		}
	}
}

// A fund keeps its class whatever its lines hold where it is of class
// metal, crypto or other, and where its debt all matures within a year
// beside derivative lines: the bills a futures fund holds as collateral,
// though it files its rate sensitivity. A fund of bills alone, classed as
// equity, is still a bond fund.
func TestAFundThatKeepsItsClass(t *testing.T) {
	t.Parallel()
	in := inputs()
	futures, metal := fund("QFUT", "public_equity", 10000), fund("QGLD", "metal", 10000)
	commodity, bills := fund("QCOM", "other", 10000), fund("QBIL", "public_equity", 10000)
	in.Holdings = []Holding{futures, metal, commodity, bills}
	collateral := &LineSummary{AsOf: day, DebtPct: 90, ShortDebtPct: 90, Derivatives: true}
	in.Funds["symbol:QFUT"] = &Fund{Lines: collateral, Risk: map[string]map[string]float64{"USD": {"3M": 20}},
		NetAssets: num(1e6)}
	in.Funds["symbol:QGLD"] = &Fund{Lines: &LineSummary{AsOf: day, DebtPct: 80}}
	in.Funds["symbol:QCOM"] = &Fund{Lines: collateral, Risk: map[string]map[string]float64{"USD": {"3M": 20}},
		NetAssets: num(1e6)}
	in.Funds["symbol:QBIL"] = &Fund{Lines: &LineSummary{AsOf: day, DebtPct: 95, ShortDebtPct: 95}}
	want := map[string]Kind{"QFUT": KindEquityFund, "QGLD": KindOther, "QCOM": KindOther, "QBIL": KindBondFund}
	for _, p := range Compute(in).Positions {
		if p.Kind != want[p.Instrument] {
			t.Errorf("%s: kind %s, want %s", p.Instrument, p.Kind, want[p.Instrument])
		}
	}
}

// DV01 belongs to the currency of the rates that move it: a fund held in
// dollars that files its DV01 in euros and yen adds each part to that
// currency's row of every grain, its value staying on the dollar row.
// The rows' DV01 sums to the total's, which with --converted sums every
// holding's whole.
func TestDV01SitsOnItsRatesCurrency(t *testing.T) {
	t.Parallel()
	in := inputs()
	bf := fund("QGLB", "fixed_income", 30000)
	bond := bondHolding("broker", "A", "QB", 10000, 10000)
	in.Holdings = []Holding{bf, bond}
	in.Terms[InstrumentKey{"broker", "QB"}] = Terms{Coupon: num(4), Maturity: date(2031, 6, 15)}
	in.Funds["symbol:QGLB"] = &Fund{Risk: map[string]map[string]float64{"EUR": {"5Y": 1000}, "JPY": {"10Y": 500},
		"USD": {"1Y": 100}}, NetAssets: num(1e6)}
	b := Compute(in)
	bondDV01 := *position(b, bond.HoldingKey).DV01
	for _, g := range []Grain{GrainAll, GrainAccounts} {
		rows := b.Aggregate(g, false)
		by := map[string]Row{}
		sum := 0.0
		for _, r := range rows {
			if !r.Total {
				by[r.Currency] = r
				sum += *r.DV01
			}
		}
		total := rows[len(rows)-1]
		near(t, string(g)+" EUR", by["EUR"].DV01, 30, 1e-9)
		near(t, string(g)+" JPY", by["JPY"].DV01, 15, 1e-9)
		near(t, string(g)+" USD", by["USD"].DV01, 3+bondDV01, 1e-9)
		near(t, string(g)+" rows", &sum, *total.DV01, 1e-9)
		if by["EUR"].Value != 0 || by["USD"].Value != 40000 || len(by) != 3 {
			t.Errorf("%s values %+v", g, by)
		}
		// The rows of equal value come in currency order, every time.
		for i := 0; i < 20; i++ {
			got := b.Aggregate(g, false)
			if got[1].Currency != "EUR" || got[2].Currency != "JPY" {
				t.Fatalf("%s order %s %s", g, got[1].Currency, got[2].Currency)
			}
		}
	}
	converted := b.Aggregate(GrainAll, true)
	near(t, "converted", converted[0].DV01, 48+bondDV01, 1e-9)
}

// A fund's filed DV01 reads as a long fund's, losing value as rates rise,
// unless its page states a negative duration: an inverse fund's filing
// then reads in that sign, along the curve too. One whose ledger record
// states a negative duration reads it from the record.
func TestAnInverseFundsFiling(t *testing.T) {
	t.Parallel()
	in := inputs()
	long, inverse, declared := fund("QLNG", "fixed_income", 30000), fund("QINV", "fixed_income", 30000),
		fund("QDEC", "fixed_income", 30000)
	in.Holdings = []Holding{long, inverse, declared}
	risk := func() map[string]map[string]float64 { return map[string]map[string]float64{"USD": {"10Y": 3000}} }
	in.Funds["symbol:QLNG"] = &Fund{Risk: risk(), NetAssets: num(1e6)}
	in.Funds["symbol:QINV"] = &Fund{Risk: risk(), NetAssets: num(1e6), Duration: &Dated{Value: -16}}
	in.Funds["symbol:QDEC"] = &Fund{Risk: risk(), NetAssets: num(1e6)}
	in.Terms[InstrumentKey{"broker", "QDEC"}] = Terms{Duration: num(-16)}
	b := Compute(in)
	near(t, "long", position(b, long.HoldingKey).DV01, 90, 1e-9)
	p := position(b, inverse.HoldingKey)
	near(t, "inverse", p.DV01, -90, 1e-9)
	near(t, "inverse 10Y", num(p.Buckets["USD"]["10Y"]), -90, 1e-9)
	near(t, "declared", position(b, declared.HoldingKey).DV01, -30000*16e-4, 1e-9)
}
