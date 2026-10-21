package yield

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// FundRow is one fund held directly: what its lines, its page or filing,
// or a ledger record say of it.
type FundRow struct {
	Key, Name, Symbol, Currency string
	Value                       float64
	Kind                        Kind
	// What its lines imply, as of LinesAsOf.
	LinesYield, LinesDuration, LinesMaturity           *float64
	CoveredPct, FloatingPct, DefaultPct, PaidInKindPct *float64
	LinesAsOf                                          *time.Time
	// What its page or filing states. YieldToMaturity is the yield to
	// worst where one is stated.
	YieldToMaturity, DistributionYield, SECYield, SevenDayYield, TrailingYield *float64
	// The figures the report reads (§3 of docs/YIELD.md gives each one's
	// sources), and where they come from.
	Implied, Duration, Maturity, DV01 *float64
	FiguresAsOf                       *time.Time
	FiguresFrom                       string
	RiskAsOf                          *time.Time
	Quality                           string
}

// Funds is one row per fund held directly, the largest first.
func (b *Book) Funds() []FundRow {
	byKey := map[string]*FundRow{}
	var order []string
	var ps = map[string][]Position{}
	for _, p := range b.Positions {
		if p.Kind != KindBondFund && p.Kind != KindMoneyMarket && p.Kind != KindEquityFund && p.Kind != KindMultiAsset {
			continue
		}
		key := p.Key
		r := byKey[key]
		if r == nil {
			r = &FundRow{Key: key, Name: p.Name, Symbol: p.Symbol, Currency: p.Currency, Kind: p.Kind,
				FiguresAsOf: p.FiguresAsOf, FiguresFrom: p.FiguresFrom}
			if f := b.In.Funds[p.SecurityID]; f != nil {
				fillFund(r, f)
			}
			byKey[key] = r
			order = append(order, key)
		}
		r.Value += p.Value
		ps[key] = append(ps[key], p)
	}
	out := make([]FundRow, 0, len(order))
	for _, k := range order {
		r := byKey[k]
		s := summarize(ps[k])
		r.Implied, r.Duration, r.Maturity, r.DV01, r.Quality = s.Implied, s.Duration, s.Maturity, s.DV01, s.Quality
		out = append(out, *r)
	}
	slices.SortStableFunc(out, func(a, b FundRow) int { return cmp.Compare(b.Value, a.Value) })
	return out
}

func fillFund(r *FundRow, f *Fund) {
	v := func(d *Dated) *float64 {
		if d == nil {
			return nil
		}
		return ptr(d.Value)
	}
	r.YieldToMaturity, r.DistributionYield = v(first(f.YieldToWorst, f.YieldToMaturity)), v(f.DistributionYield)
	r.SECYield, r.SevenDayYield, r.TrailingYield = v(f.SECYield), v(f.SevenDayYield), v(f.TrailingYield)
	r.RiskAsOf = f.RiskAsOf
	if l := f.Lines; l != nil {
		day := l.AsOf
		r.LinesYield, r.LinesDuration, r.LinesMaturity, r.LinesAsOf = l.Yield, l.Duration, l.MaturityYears, &day
		r.CoveredPct, r.FloatingPct = ptr(l.CoveredPct), ptr(l.FloatingPct)
		r.DefaultPct, r.PaidInKindPct = ptr(l.DefaultPct), ptr(l.PaidInKindPct)
	}
}

// CashRow is one cash balance, term deposit or money-market holding.
type CashRow struct {
	Position
	// Tiers is the number of tiers its rate has; UpTo the balance its
	// first tier runs to.
	Tiers     int
	UpTo      *float64
	ValidFrom *time.Time
	TermEnd   *time.Time
}

// Cash is one row per cash balance, term deposit and money-market
// holding, the largest gap to its benchmark last, so the worst-paid
// cash comes first; the debit balances follow, which pay their rate.
func (b *Book) Cash() []CashRow {
	var out []CashRow
	for _, p := range b.Positions {
		if p.Kind != KindDeposit && p.Kind != KindTermDeposit && p.Kind != KindMoneyMarket {
			continue
		}
		r := CashRow{Position: p}
		if p.Kind != KindMoneyMarket {
			tiers := tiersOf(b.In, p.Holding, b.In.Day)
			r.Tiers = len(tiers)
			if len(tiers) > 0 {
				r.UpTo, r.ValidFrom, r.TermEnd = tiers[0].UpTo, tiers[0].ValidFrom, tiers[0].TermEnd
			}
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b CashRow) int {
		if c := cmp.Compare(boolInt(a.Amount < 0), boolInt(b.Amount < 0)); c != 0 {
			return c
		}
		ga, gb := math.Inf(-1), math.Inf(-1)
		if a.Gap != nil {
			ga = *a.Gap
		}
		if b.Gap != nil {
			gb = *b.Gap
		}
		if c := cmp.Compare(ga, gb); c != 0 {
			return c
		}
		return cmp.Compare(b.Value, a.Value)
	})
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CurveRow is the rate sensitivity of the holdings in one currency at one
// tenor, in the output currency: what a rise of one basis point at that
// tenor takes from their value, by what holds it, a liability's netted.
type CurveRow struct {
	Currency, Tenor                 string
	Funds, Bonds, Cash, Liabilities float64
	DV01, DV100                     float64
}

// HeldIn is what holds a holding of kind k in the curve view: funds,
// liabilities, cash or bonds.
func HeldIn(k Kind) string {
	switch k {
	case KindBondFund, KindMoneyMarket, KindMultiAsset:
		return HeldInFunds
	case KindLiability:
		return HeldInLiabilities
	case KindDeposit, KindTermDeposit:
		return HeldInCash
	}
	return HeldInBonds
}

// What holds a holding in the curve view (HeldIn).
const (
	HeldInFunds       = "funds"
	HeldInBonds       = "bonds"
	HeldInCash        = "cash"
	HeldInLiabilities = "liabilities"
)

// Curve is one row per currency and tenor of canonical.RiskTenors.
func (b *Book) Curve() []CurveRow {
	rows := map[string]map[string]*CurveRow{}
	var ccys []string
	for _, p := range b.Positions {
		for ccy, tenors := range p.Buckets {
			if rows[ccy] == nil {
				rows[ccy] = map[string]*CurveRow{}
				ccys = append(ccys, ccy)
				for _, t := range canonical.RiskTenors {
					rows[ccy][t] = &CurveRow{Currency: ccy, Tenor: t}
				}
			}
			for t, v := range tenors {
				r := rows[ccy][t]
				if r == nil {
					continue
				}
				switch HeldIn(p.Kind) {
				case HeldInFunds:
					r.Funds += v
				case HeldInLiabilities:
					r.Liabilities += v
				case HeldInCash:
					r.Cash += v
				default:
					r.Bonds += v
				}
				r.DV01 += v
			}
		}
	}
	slices.Sort(ccys)
	var out []CurveRow
	for _, ccy := range ccys {
		for _, t := range canonical.RiskTenors {
			r := rows[ccy][t]
			r.DV100 = r.DV01 * 100
			out = append(out, *r)
		}
	}
	return out
}

// SecurityRow is one security held directly or through funds, as
// `exposure securities` lists it, with what the report knows of it.
type SecurityRow struct {
	Key, Name, Ticker, AssetClass, Sector, Currency  string
	Value, Direct, ViaFunds                          float64
	Funds                                            int64
	SharePct                                         *float64
	Coupon                                           *float64
	MaturityDate                                     *time.Time
	CouponKind                                       string
	Price, Implied, Duration                         *float64
	Rating                                           string
	EarningsYield, PriceEarnings, DividendYield      *float64
	Payout, DividendGrowth, EarningsGrowth, Coverage *float64
	Quality                                          string
}

// Securities fills each security row: a security held directly reads
// its holdings' figures, weighted by value; one held only through funds
// what its largest line says (its coupon, maturity, price and the yield
// its price implies) and its reported figures (its earnings and
// dividend yields over the line's price, and the ratios).
func (b *Book) Securities(rows []SecurityRow) []SecurityRow {
	direct := map[string][]Position{}
	for _, p := range b.Positions {
		if p.Key != "" {
			direct[p.Key] = append(direct[p.Key], p)
		}
	}
	for i := range rows {
		r := &rows[i]
		if ps := direct[r.Key]; len(ps) > 0 {
			s := summarize(ps)
			p := ps[0]
			r.Coupon, r.MaturityDate, r.CouponKind, r.Rating = p.Coupon, p.MaturityDate, string(p.CouponKind), p.Rating
			r.Price, r.Implied, r.Duration = p.Price, s.Implied, s.Duration
			r.EarningsYield, r.DividendYield = s.EarningsYield, s.DividendYield
			r.Payout, r.DividendGrowth, r.EarningsGrowth, r.Coverage = p.Payout, p.DividendGrowth, p.EarningsGrowth,
				p.Coverage
			r.Quality = s.Quality
		} else if l, ok := b.In.Lines[r.Key]; ok {
			r.Coupon, r.MaturityDate, r.CouponKind, r.Price = l.Coupon, l.Maturity, l.CouponKind, l.Price
			r.Implied, r.Duration = l.Yield, l.Duration
			if f, ok := b.In.Securities[r.Key]; ok && l.Price != nil && *l.Price > 0 &&
				(f.Currency == "" || f.Currency == l.Currency) {
				if f.EPS != nil {
					r.EarningsYield = ptr(*f.EPS / *l.Price * 100)
				}
				if f.DPS != nil {
					r.DividendYield = ptr(*f.DPS / *l.Price * 100)
				}
				r.Payout, r.DividendGrowth, r.EarningsGrowth, r.Coverage = ratios(f)
			}
			switch {
			case l.Default:
				r.Quality = string(TagDefault)
			case r.Implied != nil:
				r.Quality = string(TagImplied) + " " + string(TagStalePrice)
			}
		}
		if r.EarningsYield != nil && *r.EarningsYield > 0 {
			r.PriceEarnings = ptr(100 / *r.EarningsYield)
		}
	}
	return rows
}

// CoverageRow is one line of the coverage view: a section, a reason, the
// value it covers (a liability's netted), its share of the holdings'
// gross value (a liability's counted whole), and how many holdings; a
// holding's line names it.
type CoverageRow struct {
	Section, Reason                   string
	Value                             float64
	gross                             float64
	SharePct                          *float64
	Holdings                          int
	Source, Instrument, Account, Name string
	// Skeleton is the ledger record that fills the gap, for a holding's
	// line.
	Skeleton *Skeleton
}

// Skeleton is a ledger record with its holding (or account) named and
// what is missing left empty; it loads once filled in.
type Skeleton struct {
	SilverSourceID string        `json:"silver_source_id"`
	Instrument     string        `json:"instrument,omitempty"`
	Account        string        `json:"account,omitempty"`
	Rate           *RateSkeleton `json:"rate,omitempty"`
	Rating         *struct{}     `json:"rating,omitempty"`
	Dividend       *struct{}     `json:"dividend,omitempty"`
	Terms          *struct{}     `json:"terms,omitempty"`
	Note           string        `json:"note,omitempty"`
}

// RateSkeleton is a rate record with its currency named and its rate
// left empty.
type RateSkeleton struct {
	Currency string `json:"currency,omitempty"`
}

// The missing figures the coverage view names (missingOf says which
// kinds each applies to).
const (
	missingTerms     = "no terms"
	missingRate      = "no rate"
	missingReceiving = "no receiving rate"
	missingFigures   = "no figures"
	missingDividend  = "no dividend"
	missingRating    = "no rating"
)

// missingReasons are the missing figures in the order the coverage view
// lists them.
var missingReasons = []string{missingTerms, missingRate, missingReceiving, missingFigures, missingDividend,
	missingRating}

// Coverage is the coverage view: what the figures rest on (`rests_on`,
// one line per tag, each its holdings' share of the gross value),
// then each kind of figure missing (`missing`), then every holding a
// figure is missing on, with the ledger record that fills it.
func (b *Book) Coverage() []CoverageRow {
	total := 0.0
	tagged := map[Tag]*CoverageRow{}
	for _, p := range b.Positions {
		total += math.Abs(p.Value)
		for t := range p.Tags {
			r := tagged[t]
			if r == nil {
				r = &CoverageRow{Section: "rests_on", Reason: string(t)}
				tagged[t] = r
			}
			r.Value += p.Value
			r.gross += math.Abs(p.Value)
			r.Holdings++
		}
	}
	share := func(gross float64) *float64 {
		if total == 0 {
			return nil
		}
		return ptr(gross / total * 100)
	}
	var out []CoverageRow
	for _, t := range Tags {
		if r := tagged[t]; r != nil {
			r.SharePct = share(r.gross)
			out = append(out, *r)
		}
	}
	missing := map[string]*CoverageRow{}
	var holdings []CoverageRow
	receiving := b.unknownReceiving()
	for _, p := range b.Positions {
		reasons := missingOf(p)
		if receiving[p.HoldingKey] {
			reasons = append(reasons, missingReceiving)
		}
		for _, reason := range reasons {
			r := missing[reason]
			if r == nil {
				r = &CoverageRow{Section: "missing", Reason: reason}
				missing[reason] = r
			}
			r.Value += p.Value
			r.gross += math.Abs(p.Value)
			r.Holdings++
			holdings = append(holdings, CoverageRow{Section: "holdings", Reason: reason, Value: p.Value,
				SharePct: share(math.Abs(p.Value)), Holdings: 1, Source: p.Source, Instrument: p.Instrument,
				Account: p.HoldingKey.Account, Name: p.Name, Skeleton: skeletonOf(p, reason)})
		}
	}
	for _, reason := range missingReasons {
		if r := missing[reason]; r != nil {
			r.SharePct = share(r.gross)
			out = append(out, *r)
		}
	}
	slices.SortStableFunc(holdings, func(a, b CoverageRow) int {
		if c := cmp.Compare(a.Reason, b.Reason); c != 0 {
			return c
		}
		return cmp.Compare(math.Abs(b.Value), math.Abs(a.Value))
	})
	// An account takes one rate per currency: the first line that lacks
	// it carries its skeleton.
	rated := map[AccountCurrency]bool{}
	for i := range holdings {
		if s := holdings[i].Skeleton; s != nil && s.Rate != nil {
			k := AccountCurrency{AccountKey{s.SilverSourceID, s.Account}, s.Rate.Currency}
			if rated[k] {
				holdings[i].Skeleton = nil
			}
			rated[k] = true
		}
	}
	return append(out, holdings...)
}

// unknownReceiving are the holdings whose principal matures, under the
// default maturing rule, into cash whose account states no rate: their
// projection earns an unknown rate after the maturity.
func (b *Book) unknownReceiving() map[HoldingKey]bool {
	out := map[HoldingKey]bool{}
	for _, p := range b.Projection.Parts {
		if p.Rule == DefaultAssumptions.Rule && p.Reinvested == RungCash && p.Basis == BasisUnknown {
			out[p.Holding] = true
		}
	}
	return out
}

// missingOf names what a holding lacks that a ledger record could
// state. A bond in default reads unknown, but no terms record clears a
// default: it lacks no terms.
func missingOf(p Position) []string {
	var out []string
	unknown := p.Tags[TagUnknown]
	switch p.Kind {
	case KindBond, KindBill, KindFloating, KindNote, KindLiability:
		if unknown && !p.Tags[TagDefault] {
			out = append(out, missingTerms)
		}
		if p.Kind != KindLiability && p.Kind != KindNote && p.Rating == "" {
			out = append(out, missingRating)
		}
	case KindDeposit, KindTermDeposit:
		if p.Stated == nil && p.Amount > 0 {
			out = append(out, missingRate)
		}
	case KindBondFund, KindMoneyMarket:
		if unknown {
			out = append(out, missingFigures)
		}
	case KindStock, KindEquityFund, KindMultiAsset:
		if unknown {
			out = append(out, missingDividend)
		}
	}
	return out
}

// skeletonOf is the record that fills a holding's gap: its terms, the
// rate of its account in its currency (the cash's own, or the cash a
// maturity lands in), its rating, its dividend, a fund's factsheet
// figures. A holding its source names by no instrument takes none, but a
// rate names its account.
func skeletonOf(p Position, reason string) *Skeleton {
	s := &Skeleton{SilverSourceID: p.Source, Instrument: p.Instrument, Note: p.Name}
	empty := &struct{}{}
	switch reason {
	case missingRate, missingReceiving:
		s.Instrument, s.Account, s.Rate = "", p.HoldingKey.Account, &RateSkeleton{Currency: p.Currency}
		s.Note = p.Account.Name
		return s
	case missingTerms, missingFigures:
		s.Terms = empty
	case missingRating:
		s.Rating = empty
	case missingDividend:
		s.Dividend = empty
	}
	if p.Instrument == "" {
		return nil
	}
	return s
}
