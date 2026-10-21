package yield

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// Rule is what a maturing holding's principal does after maturity.
type Rule string

const (
	// RuleCash: it lands in the cash of the holding's own account and
	// earns that cash's rate.
	RuleCash Rule = "cash"
	// RuleIdle: it earns nothing.
	RuleIdle Rule = "idle"
	// RuleRoll: it buys the same kind of holding again, for the same
	// term, at each maturity.
	RuleRoll Rule = "roll"
)

// Rules are the maturing rules, the default (DefaultAssumptions) first.
var Rules = []Rule{RuleCash, RuleIdle, RuleRoll}

// Path is how the floating rates move over the window.
type Path string

const (
	// PathConstant holds today's rates.
	PathConstant Path = "constant"
	// PathForward moves them along the currency's forward curve.
	PathForward Path = "forward"
)

// Paths are the rate paths, the default (DefaultAssumptions) first.
var Paths = []Path{PathConstant, PathForward}

// Basis is what an expected amount rests on.
type Basis string

const (
	BasisDeclared   Basis = "declared"
	BasisIndicated  Basis = "indicated"
	BasisStated     Basis = "stated"
	BasisForward    Basis = "forward"
	BasisReinvested Basis = "reinvested"
	BasisRealized   Basis = "realized"
	BasisUnknown    Basis = "unknown"
)

// Bases are the bases in the order a basis column lists them.
var Bases = []Basis{BasisDeclared, BasisIndicated, BasisStated, BasisForward, BasisReinvested, BasisRealized,
	BasisUnknown}

// Rung is a way a principal earns after a maturity (docs/INCOME.md §12,
// the maturing rules).
type Rung string

const (
	// RungCash: the cash rule's, at the rate of the account's cash.
	RungCash Rung = "cash"
	// RungRoll: the roll rule's, at the forward rate of the holding's
	// term plus its own spread.
	RungRoll Rung = "roll"
	// RungRollConstant: a roll at the holding's own implied yield, held
	// constant, where its currency has no curve.
	RungRollConstant Rung = "roll_constant"
	// RungRollOwn: a roll at its current yield, else its stated rate,
	// held constant, where it implies none.
	RungRollOwn Rung = "roll_own"
	// RungRollCash: a roll into cash, where the holding has no yield at
	// all.
	RungRollCash Rung = "roll_cash"
)

// Rungs are the rungs in the order a quality column lists them: the cash
// rule's, then the roll rule's and its fallbacks.
var Rungs = []Rung{RungCash, RungRoll, RungRollConstant, RungRollOwn, RungRollCash}

// Months is the longest window the projection computes: every view and
// dashboard horizon cuts it.
const Months = canonical.MaxHorizonMonths

// Part is part of one holding's expected income in one month under one
// maturing rule, in the holding's own currency:
//
//	Fixed + Roll[path] + Principal × (rate + Spread + shift) / 1200
//	      + RollPrincipal × shift / 1200
//
// where rate is the month's benchmark rate of Currency on the path, and
// shift is in percent. Fixed is a coupon, a declared or indicated
// dividend, a term deposit's rate. Principal earns the floating rate: a
// balance, a money-market holding, a floating note. A rolled holding
// earns the rate of its term on the day it rolls, which differs by path.
//
// A part either accrues over From..To, the days of its month it covers
// (To exclusive), its amounts that span's share of a month's, or is a
// payment on Day.
type Part struct {
	Holding  HoldingKey
	Month    int
	Rule     Rule
	Type     IncomeType
	Basis    Basis
	Currency string
	From, To time.Time
	Day      *time.Time
	// MonthOnly says a payment's day is an estimate: only its month is
	// known.
	MonthOnly bool
	Fixed     float64
	// Principal earns the benchmark rate plus Spread.
	Principal, Spread float64
	// RollConstant and RollForward are a rolled holding's income under
	// each path; RollPrincipal the principal a shift moves it by.
	RollConstant, RollForward, RollPrincipal float64
	// Reinvested is "" for a holding's own income, else how its
	// principal earns after a maturity.
	Reinvested Rung
}

// Income is the part's income under a path and a parallel shift in
// basis points, given the month's benchmark rate.
func (p Part) Income(path Path, rate, shiftBP float64) float64 {
	roll := p.RollConstant
	if path == PathForward {
		roll = p.RollForward
	}
	shift := shiftBP / 100
	return p.Fixed + roll + p.Principal*(rate+p.Spread+shift)/1200 + p.RollPrincipal*shift/1200
}

// Floating says the part's income moves with the rates.
func (p Part) Floating() bool { return p.Principal != 0 || p.RollPrincipal != 0 }

// share is the part of p that falls in [from, to]: an accrual its share
// of its days, a payment whole or not at all.
func (p Part) share(from, to time.Time) float64 {
	if p.Day != nil {
		if p.Day.Before(from) || p.Day.After(to) {
			return 0
		}
		return 1
	}
	a, b := later(p.From, from), earlier(p.To, to.Add(time.Second))
	if !b.After(a) {
		return 0
	}
	return b.Sub(a).Seconds() / p.To.Sub(p.From).Seconds()
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type rateKey struct {
	ccy   string
	month int
	path  Path
}

// Event is a principal a maturity returns, under one rule.
type Event struct {
	Holding HoldingKey
	Month   int
	Day     time.Time
	Rule    Rule
	// Type is the holding's own income type.
	Type   IncomeType
	Amount float64 // in the holding's own currency
	Basis  Basis
}

// Projection is the expected income of the holdings, month by month from
// the instant's month (month 0, from the day after the instant).
type Projection struct {
	// Start is the first day of month 0; From the first day expected.
	Start, From time.Time
	Parts       []Part
	rates       map[rateKey]float64
	Events      []Event
	// Withholding is the percent withheld at source from each holding's
	// income, and from a sold position's of a security held (Sold), where
	// the withholding table names its situs.
	Withholding map[HoldingKey]float64
	// Currency of each holding the parts name, and its rate to the
	// output currency.
	Currency map[HoldingKey]string
	FX       map[HoldingKey]float64
}

// MonthStart is the first day of month m of the projection.
func (p *Projection) MonthStart(m int) time.Time {
	return p.Start.AddDate(0, m, 0)
}

// End is the day after the projection's last.
func (p *Projection) End() time.Time { return p.MonthStart(Months + 1) }

// rateOf is the benchmark rate of a currency in a month on a path; 0 for
// a currency with no curve, whose holdings' spreads carry their whole
// rate.
func rateOf(curves map[string]*Curve, ccy string, month int, path Path) float64 {
	c := curves[ccy]
	if c == nil {
		return 0
	}
	if path == PathConstant || c.Zero == nil {
		return c.Benchmark
	}
	return c.Zero.Forward(month, c.BenchmarkMonths)
}

// rateTable fills the benchmark rate of every currency the parts name,
// per month and path.
func (p *Projection) rateTable(curves map[string]*Curve) {
	p.rates = map[rateKey]float64{}
	for _, part := range p.Parts {
		if _, ok := p.rates[rateKey{part.Currency, 0, PathConstant}]; ok {
			continue
		}
		for _, path := range Paths {
			for m := 0; m <= Months; m++ {
				p.rates[rateKey{part.Currency, m, path}] = rateOf(curves, part.Currency, m, path)
			}
		}
	}
}

// Currencies are the currencies the rate table holds, sorted.
func (p *Projection) Currencies() []string {
	var out []string
	for k := range p.rates {
		if k.month == 0 && k.path == PathConstant {
			out = append(out, k.ccy)
		}
	}
	slices.Sort(out)
	return out
}

// Rate is the benchmark rate of ccy in month m on path, as the rate
// table holds it.
func (p *Projection) Rate(ccy string, m int, path Path) float64 {
	return p.rates[rateKey{ccy, m, path}]
}

// projector builds the projection of one set of inputs.
type projector struct {
	in       *Inputs
	out      *Projection
	measures map[HoldingKey]*Measures
	// cash is each account's cash line per currency, for the cash rule.
	cash map[AccountCurrency]Holding
}

// monthOf is the projection month a day falls in, below 0 before the
// start.
func (pj *projector) monthOf(d time.Time) int {
	y, m, _ := d.Date()
	sy, sm, _ := pj.out.Start.Date()
	return (y-sy)*12 + int(m) - int(sm)
}

// project computes the projection of the holdings, given each holding's
// measures (its spread, its yields, its kind).
func project(in *Inputs, measures map[HoldingKey]*Measures) *Projection {
	out := &Projection{Start: monthStart(in.Day), From: in.Day.AddDate(0, 0, 1), Withholding: map[HoldingKey]float64{},
		Currency: map[HoldingKey]string{}, FX: map[HoldingKey]float64{}}
	pj := &projector{in: in, out: out, measures: measures, cash: map[AccountCurrency]Holding{}}
	for _, h := range in.Holdings {
		if h.IsCash {
			pj.cash[AccountCurrency{AccountKey{h.Source, h.Account}, h.Currency}] = h
		}
	}
	for _, h := range in.Holdings {
		m := measures[h.HoldingKey]
		if m == nil || h.Amount == 0 {
			continue
		}
		out.Currency[h.HoldingKey], out.FX[h.HoldingKey] = h.Currency, h.FX()
		if w, ok := pj.withholding(in.Situs[h.HoldingKey], AccountKey{h.Source, h.Account}); ok {
			out.Withholding[h.HoldingKey] = w
		}
		pj.holding(h, m)
	}
	pj.soldWithholding()
	pj.accountIncome()
	out.rateTable(in.Curves)
	return out
}

// withholding is the percent income of situs is withheld at in account
// acct, by the account's tax wrapper, the country's default where it
// names none for the wrapper.
func (pj *projector) withholding(situs string, acct AccountKey) (float64, bool) {
	rates := pj.in.Withholding[situs]
	if situs == "" || rates == nil {
		return 0, false
	}
	if r, ok := rates[pj.in.Accounts[acct].TaxWrapper]; ok {
		return r, true
	}
	r, ok := rates[canonical.WithholdingDefault]
	return r, ok
}

// soldWithholding gives each sold position of a holding's instrument
// (Inputs.Sold) the percent its received income is withheld at: by the
// situs of the holding, at its own account's wrapper.
func (pj *projector) soldWithholding() {
	for _, h := range pj.in.Holdings {
		situs := pj.in.Situs[h.HoldingKey]
		if situs == "" {
			continue
		}
		_, sold := pj.in.instrumentOf(h)
		for _, k := range sold {
			if _, ok := pj.out.Withholding[k]; ok {
				continue
			}
			if w, ok := pj.withholding(situs, AccountKey{k.Source, k.Account}); ok {
				pj.out.Withholding[k] = w
			}
		}
	}
}

func (pj *projector) add(p Part) { pj.out.Parts = append(pj.out.Parts, p) }

// part is an empty part of a holding's income.
func part(h Holding, typ IncomeType, basis Basis) Part {
	return Part{Holding: h.HoldingKey, Currency: h.Currency, Type: typ, Basis: basis}
}

// accrue adds p as accruing from from to to (exclusive; nil for the
// projection's end), no earlier than the first day expected, under
// rules (every rule where none is named). p's amounts are a month's; each
// month the span meets takes the share of its days the span covers.
func (pj *projector) accrue(p Part, from time.Time, to *time.Time, rules ...Rule) {
	end := pj.out.End()
	if to != nil && to.Before(end) {
		end = *to
	}
	from = later(from, pj.out.From)
	if len(rules) == 0 {
		rules = Rules
	}
	for m := max(pj.monthOf(from), 0); m <= Months; m++ {
		ms, me := pj.out.MonthStart(m), pj.out.MonthStart(m+1)
		a, b := later(ms, from), earlier(me, end)
		if !b.After(a) {
			break
		}
		k := b.Sub(a).Hours() / me.Sub(ms).Hours()
		for _, r := range rules {
			q := p
			q.Month, q.Rule, q.From, q.To = m, r, a, b
			q.Fixed *= k
			q.Principal *= k
			q.RollConstant *= k
			q.RollForward *= k
			q.RollPrincipal *= k
			pj.add(q)
		}
	}
}

// pay adds p as a payment on day under every rule, where the day falls
// after the instant and within the projection.
func (pj *projector) pay(p Part, day time.Time, monthOnly bool) {
	if day.Before(pj.out.From) || !day.Before(pj.out.End()) {
		return
	}
	for _, r := range Rules {
		q := p
		q.Month, q.Rule, q.Day, q.MonthOnly = pj.monthOf(day), r, &day, monthOnly
		pj.add(q)
	}
}

// unknown adds a holding's income as unknown over the whole projection.
func (pj *projector) unknown(h Holding, typ IncomeType) {
	pj.accrue(part(h, typ, BasisUnknown), pj.out.From, nil)
}

// floating adds the income of a principal that earns a floating rate,
// the benchmark plus the holding's spread to it, to until (nil for the
// projection's end); unknown where no rate is known.
func (pj *projector) floating(h Holding, m *Measures, rate *float64, typ IncomeType, basis Basis, principal float64,
	until *time.Time) {
	if rate == nil {
		pj.unknown(h, typ)
		return
	}
	bench := 0.0
	if c := pj.in.Curves[h.Currency]; c != nil {
		bench = c.Benchmark
	}
	p := part(h, typ, basis)
	p.Principal, p.Spread = principal, *rate-bench
	m.Spread = ptr(p.Spread)
	pj.accrue(p, pj.out.From, until)
}

// fixed adds a fixed rate's income on a principal, to until.
func (pj *projector) fixed(h Holding, typ IncomeType, basis Basis, principal, rate float64, until *time.Time) {
	p := part(h, typ, basis)
	p.Fixed = principal * rate / 1200
	pj.accrue(p, pj.out.From, until)
}

func (pj *projector) holding(h Holding, m *Measures) {
	terms := pj.in.Terms[InstrumentKey{h.Source, h.Instrument}]
	switch m.Kind {
	case KindBond, KindBill, KindFloating, KindNote:
		if terms.InDefault {
			// A bond in default pays what nobody can say.
			pj.unknown(h, TypeCoupons)
			return
		}
	}
	switch m.Kind {
	case KindDeposit:
		switch {
		case h.Amount <= 0:
		case m.Stated != nil:
			pj.floating(h, m, m.Stated, TypeInterest, basisOf(m), h.Amount, nil)
		case !pj.repeatInterest(h):
			pj.unknown(h, TypeInterest)
		}
	case KindTermDeposit:
		pj.fixed(h, TypeInterest, basisOf(m), h.Amount, *m.Stated, m.MaturityDate)
		pj.matures(h, m, *m.MaturityDate, h.Amount, TypeInterest)
	case KindMoneyMarket:
		pj.floating(h, m, m.Stated, TypeDistributions, basisOf(m), h.Amount, nil)
	case KindFloating:
		if h.Quantity == nil {
			pj.unknown(h, TypeCoupons)
			return
		}
		pj.floating(h, m, terms.Coupon, TypeCoupons, BasisStated, *h.Quantity, m.MaturityDate)
		if m.MaturityDate != nil {
			pj.matures(h, m, *m.MaturityDate, *h.Quantity, TypeCoupons)
		}
	case KindBond:
		pj.bond(h, m, terms)
	case KindBill:
		pj.discount(h, m)
	case KindNote:
		// A loan held pays its interest monthly; a convertible note pays
		// nothing.
		if h.Vehicle != "loan" {
			return
		}
		if m.Stated == nil {
			pj.unknown(h, TypeInterest)
			return
		}
		pj.fixed(h, TypeInterest, basisOf(m), h.Amount, *m.Stated, m.MaturityDate)
		if m.MaturityDate != nil {
			pj.matures(h, m, *m.MaturityDate, h.Amount, TypeInterest)
		}
	case KindBondFund:
		switch {
		case m.Tags[TagFloating] && m.Implied != nil:
			pj.floating(h, m, m.Implied, TypeDistributions, BasisStated, h.Amount, nil)
		case pj.indicated(h, TypeDistributions):
		case m.Stated != nil:
			pj.fixed(h, TypeDistributions, basisOf(m), h.Amount, *m.Stated, nil)
		default:
			pj.unknown(h, TypeDistributions)
		}
	case KindEquityFund, KindMultiAsset:
		if !pj.indicated(h, TypeDistributions) && m.statedDividend {
			pj.fixed(h, TypeDistributions, BasisStated, h.Amount, *m.DividendYield, nil)
		}
	case KindStock:
		pj.indicated(h, TypeDividends)
	case KindOther:
		if !pj.realizedPattern(h) {
			pj.unknown(h, TypeDistributions)
		}
	}
}

// basisOf is the basis of a holding's stated rate: declared where a
// ledger record states the rate.
func basisOf(m *Measures) Basis {
	if m.rateDeclared {
		return BasisDeclared
	}
	return BasisStated
}

// bond adds a fixed bond's coupons on its coupon dates, then its
// maturity.
func (pj *projector) bond(h Holding, m *Measures, t Terms) {
	if h.Quantity == nil || m.MaturityDate == nil || (t.Coupon == nil && t.CouponKind != canonical.CouponZero) {
		pj.unknown(h, TypeCoupons)
		return
	}
	face := *h.Quantity
	coupon := 0.0
	if t.Coupon != nil {
		coupon = *t.Coupon
	}
	if coupon == 0 {
		pj.discount(h, m)
		return
	}
	b := fixedincome.Bond{Coupon: coupon, Frequency: frequencyOf(pj.in, h, t), Maturity: *m.MaturityDate,
		DayCount: dayCountOf(t)}
	for _, f := range b.Flows(pj.in.Day) {
		if f.Coupon != 0 {
			p := part(h, TypeCoupons, BasisDeclared)
			p.Fixed = f.Coupon * face / 100
			pj.pay(p, f.Date, false)
		}
	}
	pj.matures(h, m, *m.MaturityDate, face, TypeCoupons)
}

// discount adds a bill's or a zero's income: its face less its price,
// at maturity, then its maturity.
func (pj *projector) discount(h Holding, m *Measures) {
	if m.MaturityDate == nil || h.Quantity == nil {
		pj.unknown(h, TypeInterest)
		return
	}
	p := part(h, TypeInterest, BasisDeclared)
	p.Fixed = *h.Quantity - h.Amount
	pj.pay(p, *m.MaturityDate, false)
	pj.matures(h, m, *m.MaturityDate, *h.Quantity, TypeInterest)
}

// matures returns a holding's principal on its maturity day, and adds
// what it earns from that day on under each rule (docs/INCOME.md §12,
// the maturing rules): cash earns the rate of the account's cash in its
// currency; idle nothing; roll buys the same holding again for its
// term, at the forward rate of that term plus its own spread, falling
// back to today's rate (roll_constant) where its currency has no curve,
// its own current yield (roll_own) where it implies none, and cash
// (roll_cash) where it has no yield at all. typ is the income type the
// holding earns, which a roll keeps.
func (pj *projector) matures(h Holding, m *Measures, day time.Time, principal float64, typ IncomeType) {
	// A holding a source still lists on or after its maturity returns
	// its principal the next day.
	day = later(day, pj.out.From)
	if !day.Before(pj.out.End()) || principal <= 0 {
		return
	}
	for _, r := range Rules {
		pj.out.Events = append(pj.out.Events, Event{Holding: h.HoldingKey, Month: pj.monthOf(day), Day: day, Rule: r,
			Type: typ, Amount: principal, Basis: BasisDeclared})
	}
	pj.cashAfter(h, day, principal, RuleCash, RungCash)
	own := first(m.Implied, m.CurrentYield, m.Stated)
	c := pj.in.Curves[h.Currency]
	switch {
	case own == nil:
		pj.cashAfter(h, day, principal, RuleRoll, RungRollCash)
	case m.Implied == nil:
		pj.rollAt(h, day, principal, *own, typ, RungRollOwn)
	case c == nil || c.Zero == nil:
		pj.rollAt(h, day, principal, *own, typ, RungRollConstant)
	default:
		pj.rollCurve(h, c, day, pj.term(h, m, day), principal, *own, typ)
	}
}

// term is the months a holding rolls for, at least one: its original
// term, maturity less the day the term began (a term deposit's rate's
// first day), else less the day it was bought, else the term left today.
func (pj *projector) term(h Holding, m *Measures, maturity time.Time) int {
	from := pj.in.Day
	if h.Acquired != nil {
		from = *h.Acquired
	}
	if m.Kind == KindTermDeposit {
		if r := depositRate(pj.in, h, pj.in.Day); r != nil && r.ValidFrom != nil {
			from = *r.ValidFrom
		}
	}
	if from.After(pj.in.Day) {
		from = pj.in.Day
	}
	return max(int(math.Round(yearsBetween(from, maturity)*12)), 1)
}

// cashAfter adds the principal's income from day first on, at what the
// cash of the holding's account in its currency earns on it that day:
// the tiers' rate on the balance with it, less on the balance without
// it, a term's rate only before its end. Where the account holds no cash
// in that currency today, only a rate that names the currency counts:
// one that names none is the rate of the cash the account holds. Unknown
// where the account states no rate for that cash.
func (pj *projector) cashAfter(h Holding, first time.Time, principal float64, rule Rule, rung Rung) {
	p := part(h, TypeInterest, BasisReinvested)
	p.Reinvested = rung
	balance := 0.0
	cash, held := pj.cash[AccountCurrency{AccountKey{h.Source, h.Account}, h.Currency}]
	if held && cash.HoldingKey != h.HoldingKey && cash.Amount > 0 {
		balance = cash.Amount
	}
	tiers := tiersOf(pj.in, Holding{HoldingKey: HoldingKey{h.Source, h.Account, "cash:" + h.Currency},
		Currency: h.Currency, Amount: principal}, first)
	if !held {
		tiers = slices.DeleteFunc(tiers, func(r Rate) bool { return r.Currency == "" })
	}
	if len(tiers) == 0 {
		p.Basis = BasisUnknown
	} else {
		bench := 0.0
		if c := pj.in.Curves[h.Currency]; c != nil {
			bench = c.Benchmark
		}
		rate := (earned(tiers, balance+principal) - earned(tiers, balance)) / principal
		p.Principal, p.Spread = principal, rate-bench
	}
	pj.accrue(p, first, nil, rule)
}

// rollAt adds a rolled holding's income from day first on at one rate
// under either path, rolling again every term at the same rate.
func (pj *projector) rollAt(h Holding, first time.Time, principal, rate float64, typ IncomeType, rung Rung) {
	p := part(h, typ, BasisReinvested)
	p.Reinvested, p.RollConstant, p.RollForward, p.RollPrincipal = rung, principal*rate/1200, principal*rate/1200,
		principal
	pj.accrue(p, first, nil, RuleRoll)
}

// rollCurve rolls along the curve: at each maturity the holding buys its
// term again at that day's rate for the term plus its spread over the
// curve today; under constant rates, today's rate for the term plus the
// spread, its own yield.
func (pj *projector) rollCurve(h Holding, c *Curve, first time.Time, term int, principal, own float64, typ IncomeType) {
	spread := own - c.Zero.Zero(term)
	for i := 0; ; i++ {
		start, end := first.AddDate(0, term*i, 0), first.AddDate(0, term*(i+1), 0)
		if !start.Before(pj.out.End()) {
			return
		}
		forward := c.Zero.Forward(max(pj.monthOf(start), 0), term) + spread
		p := part(h, typ, BasisReinvested)
		p.Reinvested, p.RollConstant, p.RollForward, p.RollPrincipal = RungRoll, principal*own/1200,
			principal*forward/1200, principal
		pj.accrue(p, start, &end, RuleRoll)
	}
}

// indicated adds a stock's or a fund's dividends from the one after the
// last paid on: its instrument's indicated dividend (indicatedDividend) on
// the holding's own quantity, at the instrument's frequency, each payment
// in turn. Only their months are known. false where none is.
func (pj *projector) indicated(h Holding, typ IncomeType) bool {
	d := indicatedDividend(pj.in, h)
	if d == nil || h.Quantity == nil {
		return false
	}
	p := part(h, typ, BasisIndicated)
	if d.tag == TagDeclared {
		p.Basis = BasisDeclared
	}
	step := 12 / d.frequency
	for i := 1; ; i++ {
		day := d.last.AddDate(0, step*i, 0)
		if !day.Before(pj.out.End()) {
			return true
		}
		p.Fixed = d.payments[(i-1)%len(d.payments)] * *h.Quantity
		pj.pay(p, day, true)
	}
}

// dividend is a holding's indicated dividend: per share of its next
// payments in turn, from the one after the last, the payments a year, the
// day of the last one, and what it rests on. units says a payment it
// rests on is in its day's units (Payment).
type dividend struct {
	payments  []float64
	frequency int
	annual    float64 // per share
	last      time.Time
	tag       Tag
	units     bool
}

// indicatedDividend is a holding's indicated dividend per share, in its
// own currency: its instrument's, which every holding of it shares. A
// ledger record's comes first. Else its pay days (payDays) give it, by
// month:
//
//   - A month's rate is the sum of its pay days' rates, specials aside:
//     two lines of a month on different days are one payment.
//   - The rate is the last month's that has one, where it lies within
//     400 days, at the frequency the pay months of the twelve months to
//     the instant show (1, 2, 4 or 12). A payer twice a year pays its
//     interim and its final, which often differ, each in its own half:
//     the last month's rate, and the rate of the newest month before it
//     within 400 days in the other half of the year, where one has one.
//   - A special counts in no rate, but its month counts in the
//     frequency. A month whose only pay days are specials counts where
//     the same month a year earlier paid: the special fell on the
//     payer's own schedule, as a trimmed holding's payment does. An
//     extra payment off it, such as a yearly payer's return of capital,
//     does not count.
//
// Else the filings' dividends per share serve, quarterly. A ledger record
// in a currency no rate converts counts nothing. The payments go on from
// the last pay day that is no special.
func indicatedDividend(in *Inputs, h Holding) *dividend {
	days := payDays(in, h)
	paid := map[int]bool{}
	for _, d := range days {
		paid[monthOf(d.day)] = true
	}
	last := in.Day
	months := map[int]bool{}
	type monthly struct {
		month int
		day   time.Time
		rate  float64
		units bool
	}
	var rated []monthly
	for i := 0; i < len(days); {
		m := monthOf(days[i].day)
		var regular []payDay
		for ; i < len(days) && monthOf(days[i].day) == m; i++ {
			if !days[i].special {
				regular, last = append(regular, days[i]), days[i].day
			}
		}
		if in.withinYear(days[i-1].day) && (len(regular) > 0 || paid[m-12]) {
			months[m] = true
		}
		if r, u := monthRate(regular); r != nil {
			rated = append(rated, monthly{m, regular[len(regular)-1].day, *r, u})
		}
	}
	if d, ok := in.Dividends[InstrumentKey{h.Source, h.Instrument}]; ok && d.Frequency > 0 {
		if per, ok := in.inOwn(h, d.Amount, d.Currency); ok {
			return &dividend{payments: []float64{per}, frequency: d.Frequency, annual: per * float64(d.Frequency),
				last: last, tag: TagDeclared}
		}
	}
	recent := func(r monthly) bool { return in.Day.Sub(r.day) < 400*24*time.Hour }
	if n := len(rated); n > 0 && recent(rated[n-1]) {
		r, f := rated[n-1], nearestFrequency(len(months))
		d := &dividend{payments: []float64{r.rate}, frequency: f, annual: r.rate * float64(f), last: last,
			tag: TagIndicated, units: r.units}
		for j := n - 2; f == 2 && j >= 0 && recent(rated[j]); j-- {
			if o := rated[j]; (r.month-o.month)%12 != 0 {
				d.payments, d.annual, d.units = []float64{o.rate, r.rate}, o.rate+r.rate, r.units || o.units
				break
			}
		}
		return d
	}
	if f := in.Securities[h.SecurityID]; f.DPS != nil && *f.DPS > 0 && (f.Currency == "" || f.Currency == h.Currency) {
		return &dividend{payments: []float64{*f.DPS / 4}, frequency: 4, annual: *f.DPS,
			last: in.Day.AddDate(0, -2, 0), tag: TagIndicated}
	}
	return nil
}

// monthOf numbers a day's month: twelve a year.
func monthOf(d time.Time) int { return d.Year()*12 + int(d.Month()) - 1 }

// monthRate is the rate per share of a month's regular pay days: each
// account's rates summed, the median of the accounts'. An account with a
// pay day of unknown rate among them has none. nil where no account has
// one; units where a rate it rests on does.
func monthRate(days []payDay) (*float64, bool) {
	sums, unknown := map[string]float64{}, map[string]bool{}
	units := false
	for _, d := range days {
		for _, a := range d.accounts {
			if a.per == nil {
				unknown[a.account] = true
				continue
			}
			sums[a.account] += *a.per
			units = units || a.units
		}
	}
	var rates []float64
	for a, r := range sums {
		if !unknown[a] {
			rates = append(rates, r)
		}
	}
	if len(rates) == 0 {
		return nil, false
	}
	return ptr(median(rates)), units
}

// payDay is one day an instrument paid a dividend or a distribution, as
// payDays reads it: each account's payment, and the day's rate.
type payDay struct {
	day      time.Time
	accounts []accountPay
	// per is the day's rate per share, in today's units, the median of
	// the accounts'; nil where no account paid knew its quantity.
	per     *float64
	special bool
}

// accountPay is one account's payment of a pay day: its rate per share,
// nil where unknown, and whether that rate is in its day's units
// (Payment).
type accountPay struct {
	account string
	per     *float64
	units   bool
}

// payDays are the days the instrument of a holding paid a dividend or a
// distribution, in any account, oldest first, each payment in the
// holding's own currency. A payment no rate converts counts nothing.
//
//   - An account's lines of one day are one payment: a fund's
//     distribution often comes as several, income and capital gains
//     apart. Their sum over the quantity the account held that day is
//     its rate per share. A payment whose quantity is unknown has none.
//   - So has a payment on under two thirds, or over one and a half
//     times, what its account held RecordLagDays before: a holding
//     trimmed or topped up between the record day and the pay day was
//     paid on another quantity than it held that day.
//   - The accounts paid on one day pay one rate: the day's is the median
//     of their rates.
//   - A day whose rate is above twice the median of the rates of the
//     twelve months to the instant is a special: a special dividend or a
//     return of capital. With fewer than three rates in them, the median
//     is that of every pay day, the 24 months of the payments. With fewer
//     than four there, a special is above four times their median, the
//     smaller one's with two: a payer twice a year whose final is two or
//     three times its interim has no special among a few payments.
func payDays(in *Inputs, h Holding) []payDay {
	type paid struct {
		day     time.Time
		account string
	}
	type sum struct {
		amount      float64
		qty, before *float64
		units       bool
	}
	sums := map[paid]*sum{}
	var order []paid
	for _, p := range in.instrumentPayments(h) {
		if p.Type != TypeDividends && p.Type != TypeDistributions {
			continue
		}
		amount, ok := in.inOwn(h, p.Amount, p.Currency)
		if !ok {
			continue
		}
		k := paid{p.Day.Truncate(24 * time.Hour), p.Account}
		s := sums[k]
		if s == nil {
			s = &sum{}
			sums[k], order = s, append(order, k)
		}
		s.amount += amount
		if p.Quantity != nil && *p.Quantity > 0 {
			s.qty, s.before, s.units = p.Quantity, p.Before, s.units || p.Units
		}
	}
	slices.SortStableFunc(order, func(a, b paid) int {
		return cmp.Or(a.day.Compare(b.day), cmp.Compare(a.account, b.account))
	})
	for _, s := range sums {
		if s.qty != nil && s.before != nil && (*s.qty < *s.before/1.5 || *s.qty > *s.before*1.5) {
			s.qty = nil
		}
	}
	var out []payDay
	var all, year []float64
	for i := 0; i < len(order); {
		d := payDay{day: order[i].day}
		var rates []float64
		for ; i < len(order) && order[i].day.Equal(d.day); i++ {
			s, a := sums[order[i]], accountPay{account: order[i].account}
			if s.qty != nil {
				a.per, a.units = ptr(s.amount / *s.qty), s.units
				rates = append(rates, *a.per)
			}
			d.accounts = append(d.accounts, a)
		}
		if len(rates) > 0 {
			d.per = ptr(median(rates))
			all = append(all, *d.per)
			if in.withinYear(d.day) {
				year = append(year, *d.per)
			}
		}
		out = append(out, d)
	}
	if len(all) >= 2 {
		m, bar := slices.Min(all), 4.0
		switch {
		case len(year) >= 3:
			m, bar = median(year), 2
		case len(all) >= 4:
			m, bar = median(all), 2
		case len(all) == 3:
			m = median(all)
		}
		for i := range out {
			out[i].special = out[i].per != nil && *out[i].per > bar*m
		}
	}
	return out
}

// median is the middle of vs, the mean of the two middle ones where
// their count is even. It sorts vs.
func median(vs []float64) float64 {
	slices.Sort(vs)
	n := len(vs)
	return (vs[(n-1)/2] + vs[n/2]) / 2
}

// nearestFrequency is the coupon frequency of a bond nearest to n
// payments a year (canonical.BondFrequencies).
func nearestFrequency(n int) int {
	fs := canonical.BondFrequencies()
	best := fs[0]
	for _, f := range fs {
		if math.Abs(float64(n-f)) < math.Abs(float64(n-best)) {
			best = f
		}
	}
	return best
}

// withinYear says day lies in the year to the instant: after the same
// day a year before, as gold's received income counts it.
func (in *Inputs) withinYear(day time.Time) bool {
	return day.After(in.Day.AddDate(-1, 0, 0))
}

// paidWithinYear says one of ps falls in the twelve months to the
// instant.
func (in *Inputs) paidWithinYear(ps []Payment) bool {
	return slices.ContainsFunc(ps, func(p Payment) bool { return in.withinYear(p.Day) })
}

// repeat adds a payment of the twelve months to the instant again on its
// day of every later year the projection reaches.
func (pj *projector) repeat(p Part, day time.Time) {
	for y := 1; ; y++ {
		next := day.AddDate(y, 0, 0)
		if !next.Before(pj.out.End()) {
			return
		}
		pj.pay(p, next, true)
	}
}

// realizedPattern repeats a holding's payments of the twelve months to
// the instant every year, each in the holding's own currency: what it
// paid last year, where nothing else is known. false where it was paid
// nothing a rate converts.
func (pj *projector) realizedPattern(h Holding) bool {
	found := false
	for _, pay := range pj.in.Payments[h.HoldingKey] {
		if !pj.in.withinYear(pay.Day) {
			continue
		}
		amount, ok := pj.in.inOwn(h, pay.Amount, pay.Currency)
		if !ok {
			continue
		}
		found = true
		p := part(h, pay.Type, BasisRealized)
		p.Fixed = amount
		pj.repeat(p, pay.Day)
	}
	return found
}

// repeatInterest repeats the interest a cash balance's account received
// in its currency over the twelve months to the instant, every year:
// what a balance whose rate no source states earned last year (basis
// realized). false where the account received none.
func (pj *projector) repeatInterest(h Holding) bool {
	found := false
	for _, r := range pj.in.AccountPayments {
		if r.Type != TypeInterest || r.Source != h.Source || r.Account != h.Account || r.Currency != h.Currency ||
			!pj.in.withinYear(r.Day) {
			continue
		}
		found = true
		p := part(h, TypeInterest, BasisRealized)
		p.Fixed = r.Amount
		pj.repeat(p, r.Day)
	}
	return found
}

// AccountIncomeKey is the holding an account's income of one type in
// one currency is named by where no holding is: `income:<type>:<CCY>`.
func AccountIncomeKey(k AccountCurrency, t IncomeType) HoldingKey {
	return HoldingKey{k.Source, k.Account, "income:" + string(t) + ":" + k.Currency}
}

// accountIncome repeats what each account received with no holding
// named, rent and pensions, every year.
func (pj *projector) accountIncome() {
	for _, r := range pj.in.AccountPayments {
		if r.Type == TypeInterest {
			continue
		}
		k := r.AccountCurrency
		h := Holding{HoldingKey: AccountIncomeKey(k, r.Type), Currency: k.Currency}
		p := part(h, r.Type, BasisRealized)
		p.Fixed = r.Amount
		pj.repeat(p, r.Day)
		pj.out.Currency[h.HoldingKey] = k.Currency
		if _, ok := pj.out.FX[h.HoldingKey]; !ok {
			pj.out.FX[h.HoldingKey] = pj.in.fxOf(k.Currency)
		}
	}
}
