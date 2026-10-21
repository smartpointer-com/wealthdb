package yield

import (
	"cmp"
	"fmt"
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
// to rates (constant rates, maturing principal to cash).
func Compute(in *Inputs) *Book {
	measures := make(map[HoldingKey]*Measures, len(in.Holdings))
	for _, h := range in.Holdings {
		m := measure(in, h)
		measures[h.HoldingKey] = &m
	}
	p := Project(in, measures)
	income, d100 := map[HoldingKey]float64{}, map[HoldingKey]float64{}
	unknown, realized := map[HoldingKey]bool{}, map[HoldingKey]bool{}
	for _, part := range p.Parts {
		if part.Basis == BasisRealized {
			realized[part.Holding] = true
		}
		if part.Rule != RuleCash || part.Month < 1 || part.Month > 12 {
			continue
		}
		fx := p.FX[part.Holding]
		income[part.Holding] += part.Income(PathConstant, p.Rate(part.Currency, part.Month, PathConstant), 0) * fx
		d100[part.Holding] += (part.Principal + part.RollPrincipal) / 1200 * fx
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
	source, portfolio, account string
}

func entityOf(g Grain, p Position) entity {
	switch g {
	case GrainSources:
		return entity{source: p.Source}
	case GrainPortfolios:
		return entity{source: p.Source, portfolio: p.Account.Portfolio}
	case GrainAccounts:
		return entity{source: p.Source, account: p.HoldingKey.Account}
	}
	return entity{}
}

// Aggregate is the rows of a grain: one per entity and currency, then a
// total over every currency, value-weighted and mixed. converted gives
// one row per entity instead, every rate across currencies and mixed.
func (b *Book) Aggregate(g Grain, converted bool) []Row {
	type key struct {
		e   entity
		ccy string
	}
	groups := map[key][]Position{}
	var order []key
	add := func(k key, p Position) {
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	for _, p := range b.Positions {
		e := entityOf(g, p)
		if converted {
			add(key{e, ""}, p)
		} else {
			add(key{e, p.Currency}, p)
		}
	}
	var rows []Row
	for _, k := range order {
		r := summarize(groups[k])
		r.Source, r.Portfolio, r.AccountID, r.Currency = k.e.source, k.e.portfolio, k.e.account, k.ccy
		if k.e.account != "" || k.e.portfolio != "" {
			first := groups[k][0]
			r.Account, r.PortfolioName = first.Account, first.Account.PortfolioName
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
		return cmp.Compare(b.Value, a.Value)
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

// summarize sums a set of positions into one row. Value and DV01 sum
// every position, a liability's netted. The rates are weighted by value
// over the positions that state them, the liabilities and the lines of
// no positive value left out unless the set holds nothing else: they say
// what the assets earn.
func summarize(ps []Position) Row {
	var r Row
	weighted := ps
	var assets []Position
	for _, p := range ps {
		r.Value += p.Value
		if p.Kind != KindLiability && p.Value > 0 {
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
	r.Realized = realizedOf(weighted)
	if r.Income12 != nil {
		den := 0.0
		for _, p := range weighted {
			den += p.Value
		}
		if den != 0 {
			r.IncomeYield = ptr(*r.Income12 / den * 100)
		}
	}
	r.Quality = quality(ps)
	return r
}

// realizedOf is the positions' income received over their average
// value, as one rate: the value-weighted average of their realized
// yields.
func realizedOf(ps []Position) *float64 {
	num, den := 0.0, 0.0
	for _, p := range ps {
		if p.Realized != nil {
			num += p.Value * *p.Realized
			den += p.Value
		}
	}
	if den == 0 {
		return nil
	}
	return ptr(num / den)
}

// quality is the share of the positions' value each tag covers, in the
// order of Tags, as `tag=NN%`; a tag that covers all of it prints
// alone, and implied, the reading of a computed yield, does not print
// where it is all there is.
func quality(ps []Position) string {
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
	var parts []string
	for _, t := range Tags {
		s, ok := share[t]
		if !ok || s == 0 {
			continue
		}
		pct := math.Round(s / total * 100)
		switch {
		case t == TagImplied && pct >= 100:
			continue
		case pct >= 100:
			parts = append(parts, string(t))
		case pct < 1:
			parts = append(parts, string(t)+"<1%")
		default:
			parts = append(parts, fmt.Sprintf("%s=%.0f%%", t, pct))
		}
	}
	return strings.Join(parts, " ")
}
