package yield

import (
	"math"
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

// Rules are the maturing rules, the default first.
var Rules = []Rule{RuleCash, RuleIdle, RuleRoll}

// Path is how the floating rates move over the window.
type Path string

const (
	// PathConstant holds today's rates.
	PathConstant Path = "constant"
	// PathForward moves them along the currency's forward curve.
	PathForward Path = "forward"
)

// Paths are the rate paths, the default first.
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

// Rungs are the ways a principal earns after a maturity, in the order a
// quality column lists them: the cash rule's, then the roll rule's rung
// and its fallbacks (docs/INCOME.md §12).
var Rungs = []string{"cash", "roll", "roll_constant", "roll_own", "roll_cash"}

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
	// principal earns after a maturity, one of Rungs.
	Reinvested string
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
	// income, where the withholding table names its situs.
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

// Project computes the projection of the holdings, given each holding's
// measures (its spread, its yields, its kind).
func Project(in *Inputs, measures map[HoldingKey]*Measures) *Projection {
	start := time.Date(in.Day.Year(), in.Day.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := &Projection{Start: start, From: in.Day.AddDate(0, 0, 1), Withholding: map[HoldingKey]float64{},
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
		if w, ok := pj.withholding(h); ok {
			out.Withholding[h.HoldingKey] = w
		}
		pj.holding(h, m)
	}
	pj.accountIncome()
	out.rateTable(in.Curves)
	return out
}

// withholding is the percent a holding's income is withheld at, by its
// situs and its account's tax wrapper, the country's default where it
// names none for the wrapper.
func (pj *projector) withholding(h Holding) (float64, bool) {
	situs := pj.in.Situs[h.HoldingKey]
	rates := pj.in.Withholding[situs]
	if situs == "" || rates == nil {
		return 0, false
	}
	if r, ok := rates[pj.in.Accounts[AccountKey{h.Source, h.Account}].TaxWrapper]; ok {
		return r, true
	}
	r, ok := rates[canonical.WithholdingDefault]
	return r, ok
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
	case KindEquityFund:
		if !pj.indicated(h, TypeDistributions) && m.DividendYield != nil && m.Tags[TagStated] {
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
	pj.cashAfter(h, day, principal, RuleCash, "cash")
	own := firstOf(m.Implied, m.CurrentYield, m.Stated)
	c := pj.in.Curves[h.Currency]
	switch {
	case own == nil:
		pj.cashAfter(h, day, principal, RuleRoll, "roll_cash")
	case m.Implied == nil:
		pj.rollAt(h, day, principal, *own, typ, "roll_own")
	case c == nil || c.Zero == nil:
		pj.rollAt(h, day, principal, *own, typ, "roll_constant")
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
// it, a term's rate only before its end. Unknown where that cash states
// no rate.
func (pj *projector) cashAfter(h Holding, first time.Time, principal float64, rule Rule, rung string) {
	p := part(h, TypeInterest, BasisReinvested)
	p.Reinvested = rung
	var tiers []Rate
	balance := 0.0
	if cash, ok := pj.cash[AccountCurrency{AccountKey{h.Source, h.Account}, h.Currency}]; ok {
		tiers = tiersOf(pj.in, Holding{HoldingKey: cash.HoldingKey, Currency: cash.Currency, Amount: principal}, first)
		if cash.HoldingKey != h.HoldingKey && cash.Amount > 0 {
			balance = cash.Amount
		}
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
func (pj *projector) rollAt(h Holding, first time.Time, principal, rate float64, typ IncomeType, rung string) {
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
		p.Reinvested, p.RollConstant, p.RollForward, p.RollPrincipal = "roll", principal*own/1200,
			principal*forward/1200, principal
		pj.accrue(p, start, &end, RuleRoll)
	}
}

// indicated adds a stock's or a fund's dividends from the one after the
// last paid on: a ledger record's (declared), else the last payment per
// share at its frequency (indicated), else the filings' dividends per
// share (indicated). Only their months are known. false where none is.
func (pj *projector) indicated(h Holding, typ IncomeType) bool {
	d := indicatedDividend(pj.in, h)
	if d == nil || h.Quantity == nil {
		return false
	}
	p := part(h, typ, BasisIndicated)
	if d.tag == TagDeclared {
		p.Basis = BasisDeclared
	}
	p.Fixed = d.perPayment * *h.Quantity
	step := 12 / d.frequency
	for i := 1; ; i++ {
		day := d.last.AddDate(0, step*i, 0)
		if !day.Before(pj.out.End()) {
			return true
		}
		pj.pay(p, day, true)
	}
}

// dividend is a holding's indicated dividend: per share of one payment,
// the payments a year, the day of the last one, and what it rests on.
type dividend struct {
	perPayment float64
	frequency  int
	annual     float64 // per share
	last       time.Time
	tag        Tag
}

// indicatedDividend is a holding's indicated dividend per share. The
// lines paid on one day are one payment: a fund's distribution often
// comes as several, income and capital gains apart.
func indicatedDividend(in *Inputs, h Holding) *dividend {
	type payDay struct {
		day time.Time
		per float64 // per share where a quantity is known, else the amount
	}
	var pays []payDay
	for _, p := range in.Payments[h.HoldingKey] {
		if p.Type != TypeDividends && p.Type != TypeDistributions {
			continue
		}
		per := p.Amount
		if p.Quantity != nil && *p.Quantity > 0 {
			per = p.Amount / *p.Quantity
		} else if h.Quantity != nil && *h.Quantity > 0 {
			per = p.Amount / *h.Quantity
		}
		day := p.Day.Truncate(24 * time.Hour)
		if n := len(pays); n > 0 && pays[n-1].day.Equal(day) {
			pays[n-1].per += per
		} else {
			pays = append(pays, payDay{day, per})
		}
	}
	last := in.Day
	if len(pays) > 0 {
		last = pays[len(pays)-1].day
	}
	if d, ok := in.Dividends[InstrumentKey{h.Source, h.Instrument}]; ok && d.Frequency > 0 {
		return &dividend{perPayment: d.Amount, frequency: d.Frequency, annual: d.Amount * float64(d.Frequency),
			last: last, tag: TagDeclared}
	}
	if len(pays) > 0 && in.Day.Sub(last) < 400*24*time.Hour {
		n := 0
		for _, p := range pays {
			if in.Day.Sub(p.day) <= 365*24*time.Hour {
				n++
			}
		}
		f, per := nearestFrequency(n), pays[len(pays)-1].per
		return &dividend{perPayment: per, frequency: f, annual: per * float64(f), last: last, tag: TagIndicated}
	}
	if f := in.Securities[h.SecurityID]; f.DPS != nil && *f.DPS > 0 && (f.Currency == "" || f.Currency == h.Currency) {
		return &dividend{perPayment: *f.DPS / 4, frequency: 4, annual: *f.DPS, last: in.Day.AddDate(0, -2, 0),
			tag: TagIndicated}
	}
	return nil
}

// nearestFrequency is the coupon frequency nearest to n payments a year:
// 1, 2, 4 or 12.
func nearestFrequency(n int) int {
	best := 1
	for _, f := range []int{1, 2, 4, 12} {
		if math.Abs(float64(n-f)) < math.Abs(float64(n-best)) {
			best = f
		}
	}
	return best
}

// paidWithinYear says the holding received a payment in the twelve
// months to the instant.
func paidWithinYear(in *Inputs, h Holding) bool {
	for _, p := range in.Payments[h.HoldingKey] {
		if in.Day.Sub(p.Day) <= 365*24*time.Hour {
			return true
		}
	}
	return false
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
// the instant every year: what it paid last year, where nothing else is
// known. false where it was paid nothing.
func (pj *projector) realizedPattern(h Holding) bool {
	for _, pay := range pj.in.Payments[h.HoldingKey] {
		if pj.in.Day.Sub(pay.Day) <= 365*24*time.Hour {
			p := part(h, pay.Type, BasisRealized)
			p.Fixed = pay.Amount
			pj.repeat(p, pay.Day)
		}
	}
	return paidWithinYear(pj.in, h)
}

// repeatInterest repeats the interest a cash balance's account received
// in its currency over the twelve months to the instant, every year:
// what a balance whose rate no source states earned last year (basis
// realized). false where the account received none.
func (pj *projector) repeatInterest(h Holding) bool {
	found := false
	for _, r := range pj.in.AccountPayments {
		if r.Type != TypeInterest || r.Source != h.Source || r.Account != h.Account || r.Currency != h.Currency ||
			pj.in.Day.Sub(r.Day) > 365*24*time.Hour {
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
