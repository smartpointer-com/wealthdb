package yield

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// Position is one holding, its account and its figures.
type Position struct {
	Holding
	Account Account
	Measures
}

// Book is the holdings of one instant with their figures and the
// projection of their income.
type Book struct {
	In         *Inputs
	Positions  []Position
	Projection *Projection
}

// Compute measures every holding, projects the income, and gives each
// holding its next twelve months' income and that income's sensitivity
// to rates, under DefaultAssumptions.
func Compute(in *Inputs) *Book {
	in.Index()
	measures := make(map[HoldingKey]*Measures, len(in.Holdings))
	for _, h := range in.Holdings {
		m := measure(in, h)
		measures[h.HoldingKey] = &m
	}
	p := project(in, measures)
	rule, path := DefaultAssumptions.Rule, DefaultAssumptions.Path
	income, d100 := map[HoldingKey]float64{}, map[HoldingKey]float64{}
	unknown, realized := map[HoldingKey]bool{}, map[HoldingKey]bool{}
	for _, part := range p.Parts {
		if part.Basis == BasisRealized {
			realized[part.Holding] = true
		}
		if part.Rule != rule || part.Month < 1 || part.Month > 12 {
			continue
		}
		fx := p.FX[part.Holding]
		rate := p.Rate(part.Currency, part.Month, path)
		at := part.Income(path, rate, 0)
		income[part.Holding] += at * fx
		d100[part.Holding] += (part.Income(path, rate, 100) - at) * fx
		// What a principal earns after its maturity is no figure of the
		// holding's own.
		if part.Basis == BasisUnknown && part.Reinvested == "" {
			unknown[part.Holding] = true
		}
	}
	b := &Book{In: in, Projection: p}
	for _, h := range in.Holdings {
		m := measures[h.HoldingKey]
		if m.Kind != KindLiability {
			m.Income12 = ptr(income[h.HoldingKey])
			m.Income100bp = ptr(d100[h.HoldingKey])
		}
		switch {
		case realized[h.HoldingKey]:
			// What it was paid last year stands in for what nothing
			// states.
			delete(m.Tags, TagUnknown)
			m.Tags[TagRealized] = true
		case unknown[h.HoldingKey]:
			m.Tags[TagUnknown] = true
		}
		b.Positions = append(b.Positions, Position{Holding: h, Account: in.Accounts[AccountKey{h.Source, h.Account}],
			Measures: *m})
	}
	return b
}

// Grain is the entity an aggregate row sums over.
type Grain string

const (
	GrainAll        Grain = "global"
	GrainSources    Grain = "sources"
	GrainPortfolios Grain = "portfolios"
	GrainAccounts   Grain = "accounts"
	// GrainPositions is one row per holding, of the income views.
	GrainPositions Grain = "positions"
)

// Row is one aggregate row: an entity's holdings in one currency, or in
// every currency (Currency "" and Mixed). Value, DV01 and the income
// figures are in the output currency.
type Row struct {
	Source, Portfolio, PortfolioName                                    string
	AccountID                                                           string
	Account                                                             Account
	Currency                                                            string
	Total, Mixed                                                        bool
	Value                                                               float64
	IncomeYield, Stated, CurrentYield, Implied, ImpliedAnnual, Realized *float64
	EarningsYield, DividendYield, Benchmark, Gap                        *float64
	Duration, Convexity, Maturity                                       *float64
	DV01, Income12, Income100bp                                         *float64
	Quality                                                             string
}

// entity is the key of a grain's row.
type entity struct {
	source, portfolio, account, position string
}

func entityOf(g Grain, p Position) entity {
	return entityOfKey(g, p.HoldingKey, p.Account)
}

// entityOfKey is the row of grain g a holding of account a belongs to.
func entityOfKey(g Grain, h HoldingKey, a Account) entity {
	switch g {
	case GrainSources:
		return entity{source: h.Source}
	case GrainPortfolios:
		return entity{source: h.Source, portfolio: a.Portfolio}
	case GrainAccounts:
		return entity{source: h.Source, account: h.Account}
	case GrainPositions:
		return entity{source: h.Source, account: h.Account, position: h.PositionKey}
	}
	return entity{}
}

// positionIndex is each holding's position, by its key.
func (b *Book) positionIndex() map[HoldingKey]*Position {
	out := make(map[HoldingKey]*Position, len(b.Positions))
	for i := range b.Positions {
		out[b.Positions[i].HoldingKey] = &b.Positions[i]
	}
	return out
}

// Aggregate is the rows of a grain: one per entity and currency, then a
// total over every currency, value-weighted and mixed. A currency's row
// holds the holdings in it, and the DV01 of every holding of the entity
// that moves with its rates (dv01ByCurrency): a fund in dollars that
// states a DV01 in euros adds it to the euro row. converted gives one row
// per entity instead, every rate across currencies and mixed, and every
// holding's DV01 whole, as the total does.
func (b *Book) Aggregate(g Grain, converted bool) []Row {
	type key struct {
		e   entity
		ccy string
	}
	groups := map[key][]Position{}
	dv01 := map[key]*float64{}
	accounts := map[entity]Account{}
	var order []key
	add := func(k key, p *Position) {
		if _, ok := groups[k]; !ok {
			order = append(order, k)
			groups[k] = nil
		}
		if p != nil {
			groups[k] = append(groups[k], *p)
		}
	}
	for i, p := range b.Positions {
		e := entityOf(g, p)
		if _, ok := accounts[e]; !ok {
			accounts[e] = p.Account
		}
		if converted {
			add(key{e, ""}, &b.Positions[i])
			continue
		}
		add(key{e, p.Currency}, &b.Positions[i])
		byCurrency := dv01ByCurrency(p)
		for _, ccy := range slices.Sorted(maps.Keys(byCurrency)) {
			k := key{e, ccy}
			add(k, nil)
			if dv01[k] == nil {
				dv01[k] = ptr(0)
			}
			*dv01[k] += byCurrency[ccy]
		}
	}
	var rows []Row
	for _, k := range order {
		r := summarize(groups[k])
		if !converted {
			r.DV01 = dv01[k]
		}
		r.Source, r.Portfolio, r.AccountID, r.Currency = k.e.source, k.e.portfolio, k.e.account, k.ccy
		if k.e.account != "" || k.e.portfolio != "" {
			a := accounts[k.e]
			r.Account, r.PortfolioName = a, a.PortfolioName
		}
		if converted {
			r.Mixed = len(currencies(groups[k])) > 1
			if r.Mixed {
				r.Quality = joinQuality(TagMixed, r.Quality)
			}
		}
		rows = append(rows, r)
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if c := cmp.Compare(a.Source+"\x00"+a.Portfolio+"\x00"+a.AccountID, b.Source+"\x00"+b.Portfolio+"\x00"+b.AccountID); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Value, a.Value); c != 0 {
			return c
		}
		return cmp.Compare(a.Currency, b.Currency)
	})
	if !converted && len(b.Positions) > 0 {
		t := summarize(b.Positions)
		t.Total = true
		t.Mixed = len(currencies(b.Positions)) > 1
		if t.Mixed {
			t.Quality = joinQuality(TagMixed, t.Quality)
		}
		rows = append(rows, t)
	}
	return rows
}

// dv01ByCurrency is a position's DV01 by the currency of the rates that
// move it: in the shares its buckets hold in each curve currency, all of
// it in its own currency where they sum to nothing.
func dv01ByCurrency(p Position) map[string]float64 {
	if p.DV01 == nil {
		return nil
	}
	// Summed in key order, so the shares are the same to the last bit at
	// every run.
	per, total := map[string]float64{}, 0.0
	for _, ccy := range slices.Sorted(maps.Keys(p.Buckets)) {
		tenors := p.Buckets[ccy]
		for _, tenor := range slices.Sorted(maps.Keys(tenors)) {
			per[ccy] += tenors[tenor]
			total += tenors[tenor]
		}
	}
	if total == 0 {
		return map[string]float64{p.Currency: *p.DV01}
	}
	for ccy, v := range per {
		per[ccy] = *p.DV01 * (v / total)
	}
	return per
}

func currencies(ps []Position) []string {
	var out []string
	for _, p := range ps {
		if !slices.Contains(out, p.Currency) {
			out = append(out, p.Currency)
		}
	}
	return out
}

func joinQuality(tag Tag, rest string) string {
	if rest == "" {
		return string(tag)
	}
	return string(tag) + " " + rest
}

// Weighs says the position weighs in the rates of a row: an asset of
// positive value, since the rates say what the assets earn.
func (p Position) Weighs() bool { return p.Kind != KindLiability && p.Value > 0 }

// RealizedParts are what a row's realized yield pools of the position
// (pool): its income of the twelve months to the instant and its mean
// daily value, both in the output currency. A position no rate converts,
// or one with no daily amounts, has neither.
func (p Position) RealizedParts() (received, mean float64) {
	fx := p.FX()
	if fx == 0 || p.daily.Days == 0 {
		return 0, 0
	}
	return p.received, p.daily.Mean() * fx
}

// IncomeYield is the next twelve months' projected income over the
// position's value, in percent; nil where the position has no income
// figure (a liability) or its value is zero. An aggregate row's is
// Row.IncomeYield.
func (p Position) IncomeYield() *float64 {
	if p.Income12 == nil || p.Value == 0 {
		return nil
	}
	return ptr(*p.Income12 / p.Value * 100)
}

// summarize sums a set of positions into one row. Value and DV01 sum
// every position, a liability's netted. The rates are weighted by value
// over the positions that state them and weigh (Weighs), or over every
// position where none weighs. The realized yield is not: it is those
// positions' income over the sum of their mean daily values, none where
// they are thin together (pool.realized).
func summarize(ps []Position) Row {
	var r Row
	weighted := ps
	var assets []Position
	for _, p := range ps {
		r.Value += p.Value
		if p.Weighs() {
			assets = append(assets, p)
		}
	}
	if len(assets) > 0 {
		weighted = assets
	}
	avg := func(get func(Measures) *float64) *float64 {
		num, den := 0.0, 0.0
		for _, p := range weighted {
			if v := get(p.Measures); v != nil && !math.IsNaN(*v) {
				num += p.Value * *v
				den += p.Value
			}
		}
		if den == 0 {
			return nil
		}
		return ptr(num / den)
	}
	sum := func(get func(Measures) *float64) *float64 {
		var out *float64
		for _, p := range ps {
			if v := get(p.Measures); v != nil {
				if out == nil {
					out = ptr(0)
				}
				*out += *v
			}
		}
		return out
	}
	r.Stated = avg(func(m Measures) *float64 { return m.Stated })
	r.CurrentYield = avg(func(m Measures) *float64 { return m.CurrentYield })
	r.Implied = avg(func(m Measures) *float64 { return m.Implied })
	r.ImpliedAnnual = avg(func(m Measures) *float64 { return m.ImpliedAnnual })
	r.EarningsYield = avg(func(m Measures) *float64 { return m.EarningsYield })
	r.DividendYield = avg(func(m Measures) *float64 { return m.DividendYield })
	r.Benchmark = avg(func(m Measures) *float64 { return m.Benchmark })
	r.Gap = avg(func(m Measures) *float64 { return m.Gap })
	r.Duration = avg(func(m Measures) *float64 { return m.Duration })
	r.Convexity = avg(func(m Measures) *float64 { return m.Convexity })
	r.Maturity = avg(func(m Measures) *float64 { return m.Maturity })
	r.DV01 = sum(func(m Measures) *float64 { return m.DV01 })
	r.Income12 = sum(func(m Measures) *float64 { return m.Income12 })
	r.Income100bp = sum(func(m Measures) *float64 { return m.Income100bp })
	var pl pool
	for _, p := range weighted {
		pl.add(p.FX(), p.received, p.daily)
	}
	realized, thin := pl.realized(false)
	r.Realized = realized
	if r.Income12 != nil {
		den := 0.0
		for _, p := range weighted {
			den += p.Value
		}
		if den != 0 {
			r.IncomeYield = ptr(*r.Income12 / den * 100)
		}
	}
	r.Quality = quality(ps, thin)
	return r
}

// quality is the share of the positions' value each tag covers, in the
// order of Tags (pctLabel); a tag that covers the whole of it prints
// bare, without a share, and implied, the reading of a computed yield,
// does not print where it covers the whole. thin says the positions are
// thin together: the tag then covers the whole.
func quality(ps []Position, thin bool) string {
	total := 0.0
	share := map[Tag]float64{}
	for _, p := range ps {
		v := math.Abs(p.Value)
		total += v
		for t := range p.Tags {
			share[t] += v
		}
	}
	if total == 0 {
		return ""
	}
	if thin {
		share[TagThin] = total
	}
	var parts []string
	for _, t := range Tags {
		s, ok := share[t]
		switch {
		case !ok || s == 0 || (t == TagImplied && s >= total):
		case s >= total:
			parts = append(parts, string(t))
		default:
			parts = append(parts, pctLabel(string(t), s, total))
		}
	}
	return strings.Join(parts, " ")
}

// pctLabel is part's share of total as `name=NN%`, or `name<1%` for a
// share that rounds to nothing.
func pctLabel(name string, part, total float64) string {
	pct := math.Round(part / total * 100)
	if math.Abs(pct) < 1 {
		return name + "<1%"
	}
	return fmt.Sprintf("%s=%.0f%%", name, pct)
}
