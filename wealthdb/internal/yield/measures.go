package yield

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// Kind is what a holding is to the yield report: it decides which
// figures it has and how it earns.
type Kind string

const (
	KindDeposit     Kind = "deposit"
	KindTermDeposit Kind = "term_deposit"
	KindMoneyMarket Kind = "money_market"
	KindBond        Kind = "bond"
	KindBill        Kind = "bill"
	KindFloating    Kind = "floating_note"
	KindNote        Kind = "note"
	KindBondFund    Kind = "bond_fund"
	KindEquityFund  Kind = "equity_fund"
	KindMultiAsset  Kind = "multi_asset"
	KindStock       Kind = "stock"
	KindLiability   Kind = "liability"
	KindOther       Kind = "other"
)

// Tag is a reading a figure rests on (docs/YIELD.md §7).
type Tag string

const (
	TagImplied    Tag = "implied"
	TagStated     Tag = "stated"
	TagIndicated  Tag = "indicated"
	TagRealized   Tag = "realized"
	TagDeclared   Tag = "declared"
	TagAged       Tag = "aged"
	TagStalePrice Tag = "stale_price"
	TagFloating   Tag = "floating"
	TagDefault    Tag = "default"
	TagThin       Tag = "thin"
	TagPartial    Tag = "partial"
	TagUnits      Tag = "units"
	TagUnknown    Tag = "unknown"
	TagMixed      Tag = "mixed"
)

// Tags are the tags in the order a quality column lists them.
var Tags = []Tag{TagImplied, TagStated, TagIndicated, TagRealized, TagDeclared, TagAged, TagStalePrice, TagFloating,
	TagDefault, TagThin, TagPartial, TagUnits, TagUnknown}

// Measures are one holding's figures. A nil figure is one the holding
// does not have, or no source states.
type Measures struct {
	Kind Kind
	// Stated is the coupon, dividend or deposit rate as stated;
	// CurrentYield the stated rate over the price.
	Stated, CurrentYield *float64
	// Implied is the yield the price implies, to worst where a call is
	// known: a bond's compounded at its coupon frequency (its terms',
	// else its market's), a fund's at its market's, a bill's its
	// discount yield; ImpliedAnnual the same as an effective annual rate.
	Implied, ImpliedAnnual *float64
	// Realized is the twelve months' income received over the mean daily
	// value: a holding of an instrument the instrument's, over its mean
	// amount on the days any account held it (realized).
	Realized *float64
	// EarningsYield and DividendYield are a stock's or an equity fund's;
	// a multi-asset fund has a DividendYield too.
	EarningsYield, DividendYield *float64
	// Benchmark is the currency's benchmark rate; Gap the stated or
	// implied yield less it.
	Benchmark, Gap *float64
	// Duration is the modified duration in years; Convexity in years²;
	// DV01 the value's fall, in the output currency, for a rise of one
	// basis point.
	Duration, Convexity, DV01 *float64
	// Maturity is the years to maturity: a fund's weighted average.
	Maturity *float64
	// Income12 is the projected income of the next twelve months, and
	// Income100bp its change for rates one percent higher, both in the
	// output currency.
	Income12, Income100bp *float64
	// Buckets is DV01 per curve currency and tenor, in the output
	// currency.
	Buckets map[string]map[string]float64
	// What a security's own figures say.
	Coupon                                           *float64
	MaturityDate                                     *time.Time
	Price                                            *float64
	CouponKind                                       canonical.CouponKind
	Rating                                           string
	Payout, DividendGrowth, EarningsGrowth, Coverage *float64
	// FiguresAsOf is the day a fund's figures describe; FiguresFrom where
	// they come from (lines, page, filing, ledger).
	FiguresAsOf *time.Time
	FiguresFrom string
	// Tags are the readings the holding's figures rest on.
	Tags map[Tag]bool
	// Spread is the holding's own rate over its currency's benchmark,
	// which the projection keeps along the rate path.
	Spread *float64
	// bondEquivalent is a bill's yield in the convention of the curve it
	// is compared with.
	bondEquivalent *float64
	// rateDeclared says the rate the holding earns is a ledger record's.
	rateDeclared bool
	// statedDividend says the dividend yield is the one a fund's page
	// states, which the projection pays.
	statedDividend bool
	// received is the income of the twelve months to the instant, in the
	// output currency; daily the holding's amount on each of their days.
	received float64
	daily    Daily
	// dividend is the holding's indicated dividend, once dividendRead
	// says it was read (Measures.indicatedDividend).
	dividend     *dividend
	dividendRead bool
}

func ptr(v float64) *float64 { return &v }

// yearsBetween is the years from a to b, actual/365.25.
func yearsBetween(a, b time.Time) float64 {
	return b.Sub(a).Hours() / 24 / 365.25
}

// classify says what a holding is.
func classify(in *Inputs, h Holding) Kind {
	terms := in.Terms[InstrumentKey{h.Source, h.Instrument}]
	switch {
	case h.IsCash || h.Vehicle == "demand_deposit":
		if r := depositRate(in, h, in.Day); r != nil && r.TermEnd != nil {
			return KindTermDeposit
		}
		return KindDeposit
	case h.Vehicle == "mortgage" || (h.Vehicle == "loan" && h.Amount < 0):
		return KindLiability
	case h.Vehicle == "loan" || h.Vehicle == "convertible_note":
		return KindNote
	case h.Vehicle == "bond":
		switch {
		case terms.CouponKind == canonical.CouponFloating:
			return KindFloating
		case terms.Frequency != nil && *terms.Frequency == 0:
			return KindBill
		}
		return KindBond
	case h.Vehicle == "etf" || h.Vehicle == "fund":
		return fundKind(h, in.Funds[h.SecurityID])
	case h.Vehicle == "stock" && h.AssetClass == "public_equity":
		return KindStock
	}
	return KindOther
}

// fundKind says what a fund is. A money-market fund is one of class cash
// or with a seven-day yield, and a multi-asset fund one of that class.
// Any other reads what it looks through to, whatever class a source gives
// the wrapper (keepsClass aside): a bond fund where it files its rate
// sensitivity or its lines are mostly debt, an equity fund where they are
// mostly equity. Else it reads its class.
func fundKind(h Holding, f *Fund) Kind {
	var lines *LineSummary
	if f != nil {
		lines = f.Lines
	}
	switch {
	case h.AssetClass == "cash" || (f != nil && f.SevenDayYield != nil):
		return KindMoneyMarket
	case h.AssetClass == "multi_asset":
		return KindMultiAsset
	case keepsClass(h.AssetClass, lines):
	case f != nil && len(f.Risk) > 0, lines != nil && lines.DebtPct > 50:
		return KindBondFund
	case lines != nil && lines.EquityPct > 50:
		return KindEquityFund
	}
	switch h.AssetClass {
	case "fixed_income":
		return KindBondFund
	case "public_equity", "real_estate":
		return KindEquityFund
	}
	return KindOther
}

// keepsClass says a fund keeps its class whatever its lines hold: a fund
// of class metal, crypto or other (where a commodity fund is), and one
// whose debt all matures within a year beside derivative lines, the
// bills a futures or swap fund holds as collateral.
func keepsClass(class string, l *LineSummary) bool {
	switch class {
	case "metal", "crypto", "other":
		return true
	}
	return l != nil && l.Derivatives && l.DebtPct > 0 && l.DebtPct-l.ShortDebtPct < 1e-9
}

// tiersOf are the tiers of a cash line's rate on day: its account's
// credit rates in its currency for a balance at or above zero, the debit
// rates below it, a term's rate only before its end, by tier.
func tiersOf(in *Inputs, h Holding, day time.Time) []Rate {
	credit := h.Amount >= 0
	var tiers []Rate
	for _, r := range in.Rates[AccountKey{h.Source, h.Account}] {
		if r.Credit == credit && (r.Currency == "" || r.Currency == h.Currency) &&
			(r.TermEnd == nil || r.TermEnd.After(day)) {
			tiers = append(tiers, r)
		}
	}
	slices.SortFunc(tiers, func(a, b Rate) int { return a.Tier - b.Tier })
	return tiers
}

// depositRate is the rate a cash line earns on day, its tiers blended
// over its balance; nil where the account states none.
func depositRate(in *Inputs, h Holding, day time.Time) *Rate {
	tiers := tiersOf(in, h, day)
	if len(tiers) == 0 {
		return nil
	}
	out := tiers[0]
	balance := math.Abs(h.Amount)
	if balance > 0 && len(tiers) > 1 {
		out.Pct = earned(tiers, balance) / balance
	}
	return &out
}

// earned is what a balance earns a year under tiered rates, in percent
// of a unit: each tier's rate on the part of the balance up to its
// limit.
func earned(tiers []Rate, balance float64) float64 {
	sum, below := 0.0, 0.0
	for _, t := range tiers {
		top := balance
		if t.UpTo != nil && *t.UpTo < balance {
			top = *t.UpTo
		}
		if top > below {
			sum += (top - below) * t.Pct
			below = top
		}
		if below >= balance {
			break
		}
	}
	return sum
}

// frequencyOf is a bond's coupon payments a year: its terms', else its
// currency's convention (ConventionFrequency).
func frequencyOf(in *Inputs, h Holding, t Terms) int {
	if t.Frequency != nil && *t.Frequency > 0 {
		return *t.Frequency
	}
	return in.ConventionFrequency(h.Currency)
}

// ConventionFrequency is the coupon payments a year of a bond in ccy by
// its currency's convention, else one.
func (in *Inputs) ConventionFrequency(ccy string) int {
	if c, ok := in.Conventions[ccy]; ok && c.Frequency > 0 {
		return c.Frequency
	}
	return 1
}

func dayCountOf(t Terms) canonical.DayCount {
	if t.DayCount.Valid() {
		return t.DayCount
	}
	return canonical.DayCountActAct
}

// measure computes one holding's figures, all but the projected income.
func measure(in *Inputs, h Holding) Measures {
	m := Measures{Kind: classify(in, h), Tags: map[Tag]bool{}}
	terms := in.Terms[InstrumentKey{h.Source, h.Instrument}]
	m.Rating = terms.Rating
	if h.Quantity != nil && *h.Quantity != 0 && !h.IsCash {
		m.Price = ptr(h.Amount / *h.Quantity)
	}
	switch m.Kind {
	case KindDeposit, KindTermDeposit:
		measureDeposit(in, h, &m)
	case KindBond, KindBill, KindFloating, KindNote:
		measureBond(in, h, terms, &m)
	case KindMoneyMarket, KindBondFund:
		measureFund(in, h, terms, &m)
	case KindMultiAsset:
		measureMultiAsset(in, h, terms, &m)
	case KindEquityFund:
		measureEquityFund(in, h, &m)
	case KindStock:
		measureStock(in, h, &m)
	case KindLiability:
		measureLiability(in, h, terms, &m)
	case KindOther:
		// What it pays repeats last year's; paid nothing, it is unknown.
		if !in.paidWithinYear(in.Payments[h.HoldingKey]) {
			m.Tags[TagUnknown] = true
		}
	}
	if c := in.Curves[h.Currency]; c != nil {
		m.Benchmark = ptr(c.Benchmark)
		if y := gapYield(&m); y != nil {
			m.Gap = ptr(*y - c.Benchmark)
		}
	}
	realized(in, h, &m)
	if m.DV01 != nil && m.Buckets == nil {
		years := 0.0
		if m.Maturity != nil {
			years = *m.Maturity
		}
		if m.Duration != nil && m.Kind != KindDeposit && m.Kind != KindTermDeposit {
			years = *m.Duration
		}
		m.Buckets = map[string]map[string]float64{h.Currency: fixedincome.BucketAt(years, *m.DV01)}
	}
	return m
}

// gapYield is the yield a holding's gap compares with its benchmark: a
// bill's bond-equivalent yield, else the implied yield, else the stated
// rate. A stock and an equity fund have none: their yields are no rate. A
// multi-asset fund's stated rate is a dividend yield, so only its implied
// yield compares.
func gapYield(m *Measures) *float64 {
	switch m.Kind {
	case KindStock, KindEquityFund:
		return nil
	case KindMultiAsset:
		return m.Implied
	}
	return first(m.bondEquivalent, m.Implied, m.Stated)
}

// first is the first of vs that is not nil, nil where none is.
func first[T any](vs ...*T) *T {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// dv01 is what a rise of one basis point takes from a value of a
// modified duration.
func dv01(value, modified float64) *float64 {
	return ptr(value * modified * fixedincome.BasisPoint)
}

// measureDeposit: a deposit yields its stated rate and lasts no time; a
// term deposit lasts to its end.
func measureDeposit(in *Inputs, h Holding, m *Measures) {
	m.Duration, m.Maturity, m.DV01 = ptr(0), ptr(0), ptr(0)
	r := depositRate(in, h, in.Day)
	if r == nil {
		m.Tags[TagUnknown] = true
		return
	}
	m.Stated, m.CurrentYield, m.Implied, m.ImpliedAnnual = ptr(r.Pct), ptr(r.Pct), ptr(r.Pct), ptr(r.Pct)
	m.Tags[readingTag(r.Declared)] = true
	m.rateDeclared = r.Declared
	if m.Kind == KindTermDeposit {
		// One payment at the term's end, at simple interest.
		t := yearsBetween(in.Day, *r.TermEnd)
		mod := fixedincome.OnePayment(t, r.Pct, 0).Modified
		m.Duration, m.Maturity, m.DV01 = ptr(mod), ptr(t), dv01(h.Value, mod)
		m.MaturityDate = r.TermEnd
	}
}

func readingTag(declared bool) Tag {
	if declared {
		return TagDeclared
	}
	return TagStated
}

// measureBond: a fixed bond yields what its price implies, to its call
// where it trades above par and a call is known; a bill its discount; a
// floating note its coupon, with no implied yield; a note its coupon.
func measureBond(in *Inputs, h Holding, t Terms, m *Measures) {
	m.Coupon, m.MaturityDate, m.CouponKind = t.Coupon, t.Maturity, t.CouponKind
	m.Stated, m.rateDeclared = t.Coupon, t.CouponDeclared
	if t.CouponDeclared || t.MaturityDeclared {
		m.Tags[TagDeclared] = true
	}
	if t.Maturity != nil && !t.Maturity.Before(in.Day) {
		m.Maturity = ptr(yearsBetween(in.Day, *t.Maturity))
	}
	face := 0.0
	if h.Quantity != nil {
		face = *h.Quantity
	}
	clean := 0.0
	if m.Kind != KindNote {
		// A bond's price is clean, per 100 of face: its amount includes
		// the interest accrued where the source states it. A quantity
		// that is no face value gives no price.
		m.Price = nil
		if face > 0 {
			clean = h.Amount / face * 100
			if h.Accrued != nil {
				clean = (h.Amount - *h.Accrued) / face * 100
			}
			if clean > 1 && clean < 1000 {
				m.Price = ptr(clean)
			}
		}
	}
	if t.InDefault {
		// A bond in default is left out of the yields.
		m.Tags[TagDefault] = true
		return
	}
	if m.Kind == KindNote {
		if m.Stated != nil {
			m.Implied = m.Stated
			m.Tags[readingTag(t.CouponDeclared)] = true
		} else {
			m.Tags[TagUnknown] = true
		}
		return
	}
	if m.Price == nil || t.Maturity == nil || !t.Maturity.After(in.Day) {
		m.Tags[TagUnknown] = true
		return
	}
	f := frequencyOf(in, h, t)
	switch m.Kind {
	case KindBill:
		m.Implied = ptr(fixedincome.BillDiscount(in.Day, *t.Maturity, clean))
		m.ImpliedAnnual = ptr(fixedincome.BillEffectiveAnnual(in.Day, *t.Maturity, clean))
		m.bondEquivalent = ptr(fixedincome.BillBondEquivalent(in.Day, *t.Maturity, clean))
		r := fixedincome.BillRisk(in.Day, *t.Maturity, clean)
		setRisk(m, h, r)
		m.Tags[TagImplied] = true
		return
	case KindFloating:
		m.Tags[TagFloating] = true
		if t.Coupon != nil {
			m.CurrentYield = ptr(*t.Coupon / clean * 100)
		}
		reset := t.Reset
		if reset == nil || !reset.After(in.Day) {
			ff := f
			if t.Frequency == nil {
				ff = 4
			}
			_, next := fixedincome.Bond{Coupon: 0, Frequency: ff, Maturity: *t.Maturity,
				DayCount: dayCountOf(t)}.Period(in.Day)
			reset = &next
		}
		rate := 0.0
		if t.Coupon != nil {
			rate = *t.Coupon
		}
		setRisk(m, h, fixedincome.FloaterRisk(in.Day, *reset, rate, f))
		return
	}
	coupon := 0.0
	if t.Coupon != nil {
		coupon = *t.Coupon
	} else if t.CouponKind != canonical.CouponZero {
		// A fixed bond whose coupon nothing states implies nothing.
		m.Tags[TagUnknown] = true
		return
	}
	b := fixedincome.Bond{Coupon: coupon, Frequency: f, Maturity: *t.Maturity, DayCount: dayCountOf(t)}
	dirty := clean + b.Accrued(in.Day)
	if h.Accrued != nil {
		dirty = h.Amount / face * 100
	}
	y, err := b.Yield(in.Day, dirty)
	if err != nil {
		m.Tags[TagUnknown] = true
		return
	}
	priced := b
	if t.CallDate != nil && !t.MakeWhole && t.CallDate.After(in.Day) && t.CallDate.Before(*t.Maturity) && clean > 100 {
		call := fixedincome.Bond{Coupon: coupon, Frequency: f, Maturity: *t.CallDate, DayCount: dayCountOf(t)}
		if yc, err := call.Yield(in.Day, dirty); err == nil && yc < y {
			y, priced = yc, call
		}
	}
	m.Implied = ptr(y)
	m.ImpliedAnnual = ptr(fixedincome.EffectiveAnnual(y, f))
	if coupon > 0 {
		m.CurrentYield = ptr(coupon / clean * 100)
	}
	setRisk(m, h, priced.Risk(in.Day, y))
	m.Buckets = map[string]map[string]float64{h.Currency: scaleBuckets(
		fixedincome.Buckets(priced.Flows(in.Day), in.Day, y, f), h.Value/dirty)}
	m.Tags[TagImplied] = true
}

// setRisk stores a risk per 100 face as the holding's: its duration and
// convexity, and its DV01 scaled to its value in the output currency.
func setRisk(m *Measures, h Holding, r fixedincome.Risk) {
	if math.IsNaN(r.Modified) {
		return
	}
	m.Duration, m.Convexity = ptr(r.Modified), ptr(r.Convexity)
	m.DV01 = dv01(h.Value, r.Modified)
}

// scaleBuckets scales a bond's buckets per 100 face by k: the holding's
// value over its dirty price per 100.
func scaleBuckets(b map[string]float64, k float64) map[string]float64 {
	out := make(map[string]float64, len(b))
	for t, v := range b {
		out[t] = v * k
	}
	return out
}

// measureFund: a fund yields a ledger record's figure, else what its
// lines imply, else what its page or filing states; it moves with rates
// as fundRisk says. A money-market fund yields its seven-day yield and
// lasts its weighted average maturity.
func measureFund(in *Inputs, h Holding, t Terms, m *Measures) {
	f := fundOf(in, h)
	if m.Kind == KindMoneyMarket {
		switch {
		case t.Yield != nil:
			m.Implied = figure(m, Dated{*t.Yield, termsDay(t)}, true, "ledger")
			m.rateDeclared = true
		case f.SevenDayYield != nil:
			m.Implied = figure(m, *f.SevenDayYield, false, "filing")
		default:
			m.Tags[TagUnknown] = true
		}
		m.Stated, m.CurrentYield, m.ImpliedAnnual = m.Implied, m.Implied, m.Implied
		if d := f.MaturityYears; d != nil {
			m.Duration, m.Maturity = ptr(d.Value), ptr(d.Value)
		}
		if t.Duration != nil {
			m.Duration = t.Duration
			m.Tags[TagDeclared] = true
		}
		if m.Duration != nil {
			m.DV01 = dv01(h.Value, *m.Duration)
		}
		return
	}
	if d := first(f.DistributionYield, f.SECYield); d != nil {
		m.Stated, m.CurrentYield = ptr(d.Value), ptr(d.Value)
	}
	if d := f.TrailingYield; d != nil {
		m.DividendYield = ptr(d.Value)
	}
	switch l := f.Lines; {
	case t.Yield != nil:
		m.Implied = figure(m, Dated{*t.Yield, termsDay(t)}, true, "ledger")
		m.rateDeclared = true
	case l != nil && l.Yield != nil && l.CoveredPct >= 50:
		m.Implied = l.Yield
		m.Tags[TagImplied], m.Tags[TagStalePrice] = true, true
		if l.Aged {
			m.Tags[TagAged] = true
		}
		day := l.AsOf
		m.FiguresAsOf, m.FiguresFrom = &day, "lines"
	case f.YieldToMaturity != nil || f.YieldToWorst != nil:
		m.Implied = figure(m, *first(f.YieldToWorst, f.YieldToMaturity), false, "page")
	default:
		m.Tags[TagUnknown] = true
	}
	if m.Implied != nil {
		m.ImpliedAnnual = ptr(fixedincome.EffectiveAnnual(*m.Implied, frequencyOf(in, h, Terms{})))
	}
	fundRisk(h, f, t, m)
}

// measureMultiAsset: a multi-asset fund moves with rates as a bond fund
// does (fundRisk), and earns what an equity fund does (equityIncome). Its
// implied yield is a ledger record's, else the yield its page or filing
// states. Its lines imply none: their yield is its debt's alone.
func measureMultiAsset(in *Inputs, h Holding, t Terms, m *Measures) {
	f := fundOf(in, h)
	switch {
	case t.Yield != nil:
		m.Implied = figure(m, Dated{*t.Yield, termsDay(t)}, true, "ledger")
	case f.YieldToMaturity != nil || f.YieldToWorst != nil:
		m.Implied = figure(m, *first(f.YieldToWorst, f.YieldToMaturity), false, "page")
	}
	if m.Implied != nil {
		m.ImpliedAnnual = ptr(fixedincome.EffectiveAnnual(*m.Implied, frequencyOf(in, h, Terms{})))
	}
	fundRisk(h, f, t, m)
	equityIncome(in, h, f, m)
}

// fundOf is what is known of a holding's fund: nothing where no source
// states anything.
func fundOf(in *Inputs, h Holding) *Fund {
	if f := in.Funds[h.SecurityID]; f != nil {
		return f
	}
	return &Fund{}
}

// termsDay is the day a ledger terms record's fund figures describe, zero
// where it states none.
func termsDay(t Terms) time.Time {
	if t.TermsAsOf == nil {
		return time.Time{}
	}
	return *t.TermsAsOf
}

// figure reads a fund's figure: it tags the reading, and names the day
// and the source the fund's figures come from.
func figure(m *Measures, d Dated, declared bool, from string) *float64 {
	m.Tags[readingTag(declared)] = true
	if !d.Day.IsZero() {
		m.FiguresAsOf = &d.Day
	}
	m.FiguresFrom = from
	return ptr(d.Value)
}

// fundRisk gives a bond or multi-asset fund its duration, maturity and
// DV01. The duration is a ledger record's, else its page's or filing's,
// else its lines'. A multi-asset fund's lines count over the whole fund,
// a line without one as zero, and its DV01 sits at its debt's own
// duration on the curve. The filed DV01 per currency and tenor, scaled
// to the holding, wins where no record states a duration. It reads as a
// long fund's, losing value as rates rise, unless the page states a
// negative duration: an inverse or rate-hedged fund's filing reads in
// that sign. A fund is floating where floatingFund says so, and in
// default where any line is.
func fundRisk(h Holding, f *Fund, t Terms, m *Measures) {
	if floatingFund(h, f) {
		m.Tags[TagFloating] = true
	}
	if l := f.Lines; l != nil && l.DefaultPct > 0 {
		m.Tags[TagDefault] = true
	}
	switch {
	case t.Duration != nil:
		m.Duration = t.Duration
		m.Tags[TagDeclared] = true
	case f.Duration != nil:
		m.Duration = ptr(f.Duration.Value)
	case f.Lines != nil && f.Lines.Duration != nil:
		m.Duration = f.Lines.Duration
		if m.Kind == KindMultiAsset {
			m.Duration = ptr(*f.Lines.Duration * f.Lines.DurationPct / 100)
		}
	}
	switch {
	case f.MaturityYears != nil:
		m.Maturity = ptr(f.MaturityYears.Value)
	case f.Lines != nil && f.Lines.MaturityYears != nil:
		m.Maturity = f.Lines.MaturityYears
	}
	if m.Duration != nil {
		m.DV01 = dv01(h.Value, *m.Duration)
		if m.Kind == KindMultiAsset && t.Duration == nil && f.Duration == nil {
			m.Buckets = map[string]map[string]float64{h.Currency: fixedincome.BucketAt(*f.Lines.Duration, *m.DV01)}
		}
	}
	if b := fundBuckets(f, h); b != nil && t.Duration == nil {
		short := f.Duration != nil && f.Duration.Value < 0
		total := 0.0
		for _, tenors := range b {
			for tenor, v := range tenors {
				if short {
					v = -v
					tenors[tenor] = v
				}
				total += v
			}
		}
		m.Buckets, m.DV01 = b, ptr(total)
	}
}

// floatingFund says a fund's income resets with the rates: its lines
// float for half of it or more, so do the lines of its filing at the
// instant, or its name says it is a floating rate fund (floatingName),
// the holding's or the one its filing gives it.
func floatingFund(h Holding, f *Fund) bool {
	return (f.Lines != nil && f.Lines.FloatingPct >= 50) || (f.FilingFloatingPct != nil && *f.FilingFloatingPct >= 50) ||
		floatingName(h.Name) || floatingName(f.FilingName)
}

// floatingNameRE matches the words a fund's name says it floats by. A
// bare FLOAT does not: an index fund's name may say FLOAT ADJUSTED.
var floatingNameRE = regexp.MustCompile(`(?i)\b(floating|floaters?|fltg|frn|float[ -]rate)\b`)

// floatingName says a fund's name names a floating rate fund: FLOATING,
// FLOATER, FLTG, FRN or FLOAT RATE.
func floatingName(name string) bool { return floatingNameRE.MatchString(name) }

// fundBuckets is a fund's filed DV01 per currency and tenor, scaled to
// the holding: the filed figures and the net assets are in one currency,
// so their ratio times the holding's value is the holding's DV01 in the
// output currency. nil where the filing states none, or no net assets.
func fundBuckets(f *Fund, h Holding) map[string]map[string]float64 {
	if len(f.Risk) == 0 || f.NetAssets == nil || *f.NetAssets <= 0 || h.Value == 0 {
		return nil
	}
	share := h.Value / *f.NetAssets
	out := map[string]map[string]float64{}
	for ccy, tenors := range f.Risk {
		if ccy == canonical.NoCurrency {
			continue
		}
		out[ccy] = map[string]float64{}
		for t, v := range tenors {
			out[ccy][t] = v * share
		}
	}
	return out
}

// measureEquityFund: an equity fund's earnings yield is the inverse of
// the P/E ratio its page states; its dividend yield is equityIncome's.
func measureEquityFund(in *Inputs, h Holding, m *Measures) {
	f := in.Funds[h.SecurityID]
	if f != nil && f.PriceEarnings != nil && f.PriceEarnings.Value > 0 {
		m.EarningsYield = ptr(100 / f.PriceEarnings.Value)
	}
	equityIncome(in, h, f, m)
}

// equityIncome gives a fund its dividend yield: its own distributions'
// (dividendReading), else the trailing yield, else the distribution
// yield, its page states; else it paid nothing (paidNothing).
func equityIncome(in *Inputs, h Holding, f *Fund, m *Measures) {
	if dividendReading(in, h, m) {
		return
	}
	if f != nil {
		if d := first(f.TrailingYield, f.DistributionYield); d != nil {
			m.DividendYield, m.Stated, m.statedDividend = ptr(d.Value), ptr(d.Value), true
			m.Tags[TagStated] = true
			return
		}
	}
	paidNothing(in, h, m)
}

// dividendReading sets a holding's dividend yield from its indicated
// dividend over its price; false where none is known.
func dividendReading(in *Inputs, h Holding, m *Measures) bool {
	d := m.indicatedDividend(in, h)
	if d == nil || m.Price == nil || *m.Price <= 0 {
		return false
	}
	m.DividendYield = ptr(d.annual / *m.Price * 100)
	m.Stated = m.DividendYield
	m.Tags[d.tag] = true
	if d.units {
		m.Tags[TagUnits] = true
	}
	if d.partial {
		m.Tags[TagPartial] = true
	}
	return true
}

// paidNothing reads a holding no dividend is known for. One bought
// within the year is unknown, and so is one whose instrument was paid
// within it, whose payment indicates nothing (no rate converts it, no
// quantity is known for it, or no price compares with it). Any other
// paid nothing the dividends tell, and yields zero on that record
// (realized).
func paidNothing(in *Inputs, h Holding, m *Measures) {
	if (h.Acquired != nil && in.withinYear(*h.Acquired)) || in.paidWithinYear(in.instrumentPayments(h)) {
		m.Tags[TagUnknown] = true
		return
	}
	m.DividendYield = ptr(0)
	m.Tags[TagRealized] = true
}

// measureStock: a stock's earnings yield is its trailing twelve months'
// earnings per share over its price, its dividend yield its indicated
// dividend over its price.
func measureStock(in *Inputs, h Holding, m *Measures) {
	f := in.Securities[h.SecurityID]
	price := m.Price
	if price != nil && *price > 0 && (f.Currency == "" || f.Currency == h.Currency) {
		if f.EPS != nil {
			m.EarningsYield = ptr(*f.EPS / *price * 100)
		}
	}
	m.Payout, m.DividendGrowth, m.EarningsGrowth, m.Coverage = ratios(f)
	if !dividendReading(in, h, m) {
		paidNothing(in, h, m)
	}
}

// ratios are a company's payout ratio, dividend and earnings growth over
// the year, and interest coverage, from its reported figures.
func ratios(f Facts) (payout, dividendGrowth, earningsGrowth, coverage *float64) {
	if f.EPS != nil && f.DPS != nil && *f.EPS > 0 {
		payout = ptr(*f.DPS / *f.EPS * 100)
	}
	if f.DPS != nil && f.DPSYearAgo != nil && *f.DPSYearAgo > 0 {
		dividendGrowth = ptr((*f.DPS / *f.DPSYearAgo - 1) * 100)
	}
	if f.EPS != nil && f.EPSYearAgo != nil && *f.EPSYearAgo > 0 {
		earningsGrowth = ptr((*f.EPS / *f.EPSYearAgo - 1) * 100)
	}
	if f.OperatingIncome != nil && f.InterestExpense != nil && *f.InterestExpense > 0 {
		coverage = ptr(*f.OperatingIncome / *f.InterestExpense)
	}
	return payout, dividendGrowth, earningsGrowth, coverage
}

// measureLiability: a mortgage or a loan owed costs its rate; its rate
// risk runs to the end of its fixed period, and it nets against the
// assets' with its negative value. Terms that state no fixed period fix
// the rate to maturity, unless the coupon floats.
func measureLiability(in *Inputs, h Holding, t Terms, m *Measures) {
	m.Coupon, m.MaturityDate = t.Coupon, t.Maturity
	if t.Coupon != nil {
		m.Stated, m.Implied, m.rateDeclared = t.Coupon, t.Coupon, t.CouponDeclared
		m.Tags[readingTag(t.CouponDeclared)] = true
	} else {
		m.Tags[TagUnknown] = true
	}
	if t.Maturity != nil && t.Maturity.After(in.Day) {
		m.Maturity = ptr(yearsBetween(in.Day, *t.Maturity))
	}
	fixed := 0.0
	switch {
	case t.FixedUntil != nil && t.FixedUntil.After(in.Day):
		fixed = yearsBetween(in.Day, *t.FixedUntil)
	case t.FixedUntil == nil && t.CouponKind != canonical.CouponFloating && m.Maturity != nil:
		fixed = *m.Maturity
	}
	rate := 0.0
	if t.Coupon != nil {
		rate = *t.Coupon
	}
	// One payment at the fixed period's end, compounded yearly.
	mod := fixedincome.OnePayment(fixed, rate, 1).Modified
	m.Duration, m.DV01 = ptr(mod), dv01(h.Value, mod)
}

// realized gives a holding the twelve months' income it received and its
// daily amounts, which a row's realized yield pools (summarize), and its
// own realized yield. A holding of an instrument yields its instrument's
// (instrumentPool), over its mean amount on the days any account held it,
// and carries partial where that is not every day; a cash balance its own
// income over its mean daily value. A thin holding has none, and carries
// thin.
func realized(in *Inputs, h Holding, m *Measures) {
	m.received = in.Income[h.HoldingKey].Total()
	if h.IsCash {
		m.received += in.AccountIncome[AccountCurrency{AccountKey{h.Source, h.Account}, h.Currency}][TypeInterest]
	}
	m.daily = in.Daily[h.HoldingKey]
	var p pool
	held := !h.IsCash && in.poolKey(h) != ""
	if held {
		p = instrumentPool(in, h)
	} else {
		p.add(h.FX(), m.received, m.daily)
	}
	var thin bool
	m.Realized, thin = p.realized(held)
	if thin {
		m.Tags[TagThin] = true
	}
	if days, n := p.heldDays(); held && m.Realized != nil && days < n {
		m.Tags[TagPartial] = true
	}
}

// instrumentPool pools what every holding of h's instrument received
// and held (instrumentOf), what its sold positions received (Sold), and
// what the positions of its instruments that are no holding held
// (FormerDaily): its income and its amounts across every account and
// source, an account that sold it included. Those positions' amounts
// convert as a holding of their instrument in their currency does, else
// at the instant's rate. An instrument's pool is read once, for every
// holding of it (Index).
func instrumentPool(in *Inputs, h Holding) pool {
	k := in.poolKey(h)
	if k != "" {
		in.index()
		if p, ok := in.instrumentPools[k]; ok {
			return p
		}
	}
	var p pool
	holdings, instruments, sold := in.instrumentOf(h)
	// fx is each instrument's rate by currency: a holding's of it in that
	// currency, the first holding's for amounts that state none.
	fx := map[InstrumentKey]map[string]float64{}
	for _, o := range holdings {
		p.add(o.FX(), in.Income[o.HoldingKey].Total(), in.Daily[o.HoldingKey])
		ik := in.instrumentKey(o)
		if fx[ik] == nil {
			fx[ik] = map[string]float64{"": o.FX()}
		}
		if fx[ik][o.Currency] == 0 {
			fx[ik][o.Currency] = o.FX()
		}
	}
	slices.SortFunc(instruments, compareInstruments)
	for _, ik := range instruments {
		for _, d := range in.FormerDaily[ik] {
			rate := fx[ik][d.Currency]
			if rate == 0 {
				rate = in.FXOf(d.Currency)
			}
			p.add(rate, 0, d)
		}
	}
	for _, s := range sold {
		p.received += in.Income[s].Total()
	}
	if k != "" {
		in.instrumentPools[k] = p
	}
	return p
}

// heldShare is the share of the twelve months to the instant on which
// h's instrument was held, as its realized yield counts the days
// (instrumentPool): by a holding of it, or by a position of it that is no
// holding, under any id the matching folds into its own and at any
// source. It is one where no daily amount is known.
func (in *Inputs) heldShare(h Holding) float64 {
	p := instrumentPool(in, h)
	days, n := p.heldDays()
	if days == 0 || n <= 0 {
		return 1
	}
	return float64(days) / float64(n)
}

func compareInstruments(a, b InstrumentKey) int {
	return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Instrument, b.Instrument))
}

// pool is what a set of holdings received over the twelve months to the
// instant, and what they held, in the output currency: their income,
// their summed mean daily amount, and their amount on each day as
// differences (day i's amount is the sum of diff[0..i]); and how many
// spans of an amount cover each day, as differences too.
type pool struct {
	received, mean float64
	diff           []float64
	holding        []int
}

// add adds a holding's income and its daily amounts, converted at fx. A
// holding no rate converts, or one with no daily amounts, adds nothing.
func (p *pool) add(fx, received float64, d Daily) {
	if fx == 0 || d.Days == 0 {
		return
	}
	if p.diff == nil {
		p.diff, p.holding = make([]float64, d.Days+1), make([]int, d.Days+1)
	}
	p.received += received
	p.mean += d.Mean() * fx
	for _, s := range d.Spans {
		if s.From >= 0 && s.To <= d.Days && s.From < s.To && s.To < len(p.diff) && s.Amount != 0 {
			p.diff[s.From] += s.Amount * fx
			p.diff[s.To] -= s.Amount * fx
			p.holding[s.From]++
			p.holding[s.To]--
		}
	}
}

// heldDays are the days of the pool's n on which a span of an amount
// covers it.
func (p *pool) heldDays() (days, n int) {
	n = len(p.diff) - 1
	holding := 0
	for _, h := range p.holding[:max(n, 0)] {
		if holding += h; holding > 0 {
			days++
		}
	}
	return days, n
}

// realized is the pool's income over its mean daily amount, in percent;
// nothing received over a debt is zero, not a negative zero. With held,
// the mean is that of the days the pool held anything: a pool held part
// of the year yields what it received in that part. It is nil where the
// mean is zero, or where the pool is thin: under a tenth of its mean, in
// size, on more than half the days, a day not held among them. So a pool
// held under half the days is thin.
func (p *pool) realized(held bool) (pct *float64, thin bool) {
	n := len(p.diff) - 1
	if n <= 0 || p.mean == 0 {
		return nil, false
	}
	mean := p.mean
	if held {
		days, _ := p.heldDays()
		if days == 0 {
			return nil, false
		}
		mean *= float64(n) / float64(days)
	}
	days, day := 0, 0.0
	for _, d := range p.diff[:n] {
		day += d
		if math.Abs(day) < math.Abs(mean)/10 {
			days++
		}
	}
	if 2*days > n {
		return nil, true
	}
	r := p.received / mean * 100
	if r == 0 {
		r = 0 // a negative zero, from a debt's mean, reads as zero
	}
	return ptr(r), false
}
