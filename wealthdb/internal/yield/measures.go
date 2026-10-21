package yield

import (
	"math"
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
	TagUnknown    Tag = "unknown"
	TagMixed      Tag = "mixed"
)

// Tags are the tags in the order a quality column lists them.
var Tags = []Tag{TagImplied, TagStated, TagIndicated, TagRealized, TagDeclared, TagAged, TagStalePrice, TagFloating,
	TagDefault, TagUnknown}

// Measures are one holding's figures. A nil figure is one the holding
// does not have, or no source states.
type Measures struct {
	Kind Kind
	// Stated is the coupon, dividend or deposit rate as stated;
	// CurrentYield the stated rate over the price.
	Stated, CurrentYield *float64
	// Implied is the yield the price implies, to worst where a call is
	// known, in the market's convention (compounded Frequency times a
	// year); ImpliedAnnual the same as an effective annual rate.
	Implied, ImpliedAnnual *float64
	Frequency              int
	// Realized is the twelve months' income received over the average
	// value.
	Realized *float64
	// EarningsYield and DividendYield are a stock's or an equity fund's.
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
		f := in.Funds[h.SecurityID]
		switch {
		case h.AssetClass == "cash" || (f != nil && f.SevenDayYield != nil):
			return KindMoneyMarket
		case h.AssetClass == "fixed_income" || h.AssetClass == "multi_asset":
			return KindBondFund
		case h.AssetClass == "public_equity" || h.AssetClass == "real_estate":
			return KindEquityFund
		}
	case h.Vehicle == "stock" && h.AssetClass == "public_equity":
		return KindStock
	}
	return KindOther
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
// currency's convention, else two for a dollar bond and one for any
// other.
func frequencyOf(in *Inputs, h Holding, t Terms) int {
	if t.Frequency != nil && *t.Frequency > 0 {
		return *t.Frequency
	}
	if c, ok := in.Conventions[h.Currency]; ok && c.Frequency > 0 {
		return c.Frequency
	}
	if h.Currency == "USD" {
		return 2
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
	case KindEquityFund:
		measureEquityFund(in, h, &m)
	case KindStock:
		measureStock(in, h, &m)
	case KindLiability:
		measureLiability(in, h, terms, &m)
	case KindOther:
		// What it pays repeats last year's; paid nothing, it is unknown.
		if !paidWithinYear(in, h) {
			m.Tags[TagUnknown] = true
		}
	}
	if c := in.Curves[h.Currency]; c != nil {
		m.Benchmark = ptr(c.Benchmark)
		if y := firstOf(m.bondEquivalent, m.Implied, m.Stated); y != nil && m.Kind != KindStock &&
			m.Kind != KindEquityFund {
			m.Gap = ptr(*y - c.Benchmark)
		}
	}
	m.Realized = realized(in, h)
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

func firstOf(vs ...*float64) *float64 {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
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
		t := yearsBetween(in.Day, *r.TermEnd)
		mod := t / (1 + r.Pct/100*t)
		m.Duration, m.Maturity, m.DV01 = ptr(mod), ptr(t), ptr(h.Value*mod*1e-4)
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
	m.Frequency = f
	switch m.Kind {
	case KindBill:
		m.Implied = ptr(fixedincome.BillDiscount(in.Day, *t.Maturity, clean))
		m.ImpliedAnnual = ptr(fixedincome.BillEffectiveAnnual(in.Day, *t.Maturity, clean))
		m.bondEquivalent = ptr(fixedincome.BillBondEquivalent(in.Day, *t.Maturity, clean))
		r := fixedincome.BillRisk(in.Day, *t.Maturity, clean)
		setRisk(m, h, r)
		m.Frequency = 0
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
	m.DV01 = ptr(h.Value * r.Modified * 1e-4)
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

// measureFund: a fund yields what its lines imply, else what its page or
// filing states; its duration likewise. A money-market fund yields its
// seven-day yield and lasts its weighted average maturity. A ledger
// record's factsheet figures win over all of them.
func measureFund(in *Inputs, h Holding, t Terms, m *Measures) {
	f := &Fund{}
	if known := in.Funds[h.SecurityID]; known != nil {
		f = known
	}
	var day time.Time
	if t.TermsAsOf != nil {
		day = *t.TermsAsOf
	}
	read := func(d Dated, declared bool, from string) *float64 {
		m.Tags[readingTag(declared)] = true
		if !d.Day.IsZero() {
			m.FiguresAsOf = &d.Day
		}
		m.FiguresFrom = from
		return ptr(d.Value)
	}
	if t.Duration != nil {
		m.Tags[TagDeclared] = true
	}
	if m.Kind == KindMoneyMarket {
		switch {
		case t.Yield != nil:
			m.Implied = read(Dated{*t.Yield, day}, true, "ledger")
			m.rateDeclared = true
		case f.SevenDayYield != nil:
			m.Implied = read(*f.SevenDayYield, false, "filing")
		default:
			m.Tags[TagUnknown] = true
		}
		m.Stated, m.CurrentYield, m.ImpliedAnnual = m.Implied, m.Implied, m.Implied
		if d := f.MaturityYears; d != nil {
			m.Duration, m.Maturity = ptr(d.Value), ptr(d.Value)
		}
		if t.Duration != nil {
			m.Duration = t.Duration
		}
		if m.Duration != nil {
			m.DV01 = ptr(h.Value * *m.Duration * 1e-4)
		}
		return
	}
	if d := firstDated(f.DistributionYield, f.SECYield); d != nil {
		m.Stated, m.CurrentYield = ptr(d.Value), ptr(d.Value)
	}
	if d := f.TrailingYield; d != nil {
		m.DividendYield = ptr(d.Value)
	}
	switch l := f.Lines; {
	case t.Yield != nil:
		m.Implied = read(Dated{*t.Yield, day}, true, "ledger")
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
		m.Implied = read(*firstDated(f.YieldToWorst, f.YieldToMaturity), false, "page")
	default:
		m.Tags[TagUnknown] = true
	}
	if l := f.Lines; l != nil {
		if l.FloatingPct >= 50 {
			m.Tags[TagFloating] = true
		}
		if l.DefaultPct > 0 {
			m.Tags[TagDefault] = true
		}
	}
	if m.Implied != nil {
		m.Frequency = frequencyOf(in, h, Terms{})
		m.ImpliedAnnual = ptr(fixedincome.EffectiveAnnual(*m.Implied, m.Frequency))
	}
	switch {
	case t.Duration != nil:
		m.Duration = t.Duration
	case f.Duration != nil:
		m.Duration = ptr(f.Duration.Value)
	case f.Lines != nil && f.Lines.Duration != nil:
		m.Duration = f.Lines.Duration
	}
	switch {
	case f.MaturityYears != nil:
		m.Maturity = ptr(f.MaturityYears.Value)
	case f.Lines != nil && f.Lines.MaturityYears != nil:
		m.Maturity = f.Lines.MaturityYears
	}
	if m.Duration != nil {
		m.DV01 = ptr(h.Value * *m.Duration * 1e-4)
	}
	if b := fundBuckets(f, h); b != nil && t.Duration == nil {
		m.Buckets = b
		total := 0.0
		for _, t := range b {
			for _, v := range t {
				total += v
			}
		}
		m.DV01 = ptr(total)
	}
}

func firstDated(ds ...*Dated) *Dated {
	for _, d := range ds {
		if d != nil {
			return d
		}
	}
	return nil
}

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
// the P/E ratio its page states; its dividend yield its own
// distributions' (dividendReading), else the trailing yield its page
// states.
func measureEquityFund(in *Inputs, h Holding, m *Measures) {
	f := in.Funds[h.SecurityID]
	if f != nil && f.PriceEarnings != nil && f.PriceEarnings.Value > 0 {
		m.EarningsYield = ptr(100 / f.PriceEarnings.Value)
	}
	if dividendReading(in, h, m) {
		return
	}
	if f != nil {
		if d := firstDated(f.TrailingYield, f.DistributionYield); d != nil {
			m.DividendYield, m.Stated = ptr(d.Value), ptr(d.Value)
			m.Tags[TagStated] = true
			return
		}
	}
	paidNothing(in, h, m)
}

// dividendReading sets a holding's dividend yield from its indicated
// dividend over its price; false where none is known.
func dividendReading(in *Inputs, h Holding, m *Measures) bool {
	d := indicatedDividend(in, h)
	if d == nil || m.Price == nil || *m.Price <= 0 {
		return false
	}
	m.DividendYield = ptr(d.annual / *m.Price * 100)
	m.Stated = m.DividendYield
	m.Tags[d.tag] = true
	return true
}

// paidNothing reads a holding no dividend is known for: one held a year
// or more paid nothing in the 24 months the transactions cover, and
// yields zero on that record (realized); one bought within the year is
// unknown.
func paidNothing(in *Inputs, h Holding, m *Measures) {
	if h.Acquired != nil && in.Day.Sub(*h.Acquired) < 365*24*time.Hour {
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
	mod := fixed / (1 + rate/100)
	m.Duration, m.DV01 = ptr(mod), ptr(h.Value*mod*1e-4)
}

// realized is the twelve months' income received over the average
// value, both in the output currency; nil where neither is known.
func realized(in *Inputs, h Holding) *float64 {
	received := in.Income[h.HoldingKey].Total()
	if h.IsCash {
		received += in.AccountIncome[AccountCurrency{AccountKey{h.Source, h.Account}, h.Currency}][TypeInterest]
	}
	avg, ok := in.AverageAmount[h.HoldingKey]
	if !ok || avg == 0 || h.FX() == 0 {
		return nil
	}
	return ptr(received / (avg * h.FX()) * 100)
}
