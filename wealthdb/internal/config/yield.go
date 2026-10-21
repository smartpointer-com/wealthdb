package config

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// YieldConfig is the `yield` block of wealthdb.cfg: what the yield
// report needs that no source states. Absent block ⇒ no curves, no
// withholding rates, no rating funds, the built-in conventions and
// cycles, and the default horizon. The load writes all of it but the
// horizon into gold's yield tables (gold.SyncYield); the reports read
// the horizon from the config (YieldHorizon).
type YieldConfig struct {
	// Curves maps a currency to its public curve: each tenor (3M, 1Y) to
	// the rate series that gives it, as <silver_source_id>:<series_id>.
	Curves map[string]map[string]string `json:"curves,omitempty"`
	// Withholding maps a situs country to the tax it withholds at source
	// on income, in percent, per tax wrapper. The wrapper `default`
	// stands for every wrapper the country names no rate for. A country
	// the block does not name has no rate, which reads as unknown, never
	// as zero.
	Withholding map[string]map[string]float64 `json:"withholding,omitempty"`
	// RatingFunds maps a fund sliced by credit rating to the bucket
	// (BBB) or range of buckets, best first (AAA-A), its lines carry.
	// Keyed like exposure.size_funds.
	RatingFunds map[string]string `json:"rating_funds,omitempty"`
	// Conventions maps a market to its coupon frequency and day count. A
	// market is a currency, or a currency and a segment (USD:government,
	// USD:bill). An entry replaces the built-in one of its market
	// (canonical.BuiltinConventions).
	Conventions map[string]YieldConvention `json:"conventions,omitempty"`
	// HorizonMonths is the projection's window when none is given, 1 to
	// canonical.MaxHorizonMonths; default canonical.DefaultHorizonMonths.
	HorizonMonths int `json:"horizon_months,omitempty"`
	// Cycles maps a broad sector to its cycle (cyclical, defensive,
	// sensitive), over the built-in map (canonical.BuiltinCycles).
	Cycles map[string]string `json:"cycles,omitempty"`
}

// YieldHorizon is the income projection's window when none is given:
// yield.horizon_months, else canonical.DefaultHorizonMonths.
func (c *Config) YieldHorizon() int {
	if c.Yield != nil && c.Yield.HorizonMonths > 0 {
		return c.Yield.HorizonMonths
	}
	return canonical.DefaultHorizonMonths
}

// YieldConvention is one `yield.conventions` entry: the coupon payments
// a year (0 for a discount instrument) and the day count. Both are
// required.
type YieldConvention struct {
	Frequency *int   `json:"frequency"`
	DayCount  string `json:"day_count"`
}

// validateYield checks the yield block: every currency, tenor, country,
// wrapper, fund, rating, market and sector is of its vocabulary, and
// every curve's series names a configured source. The keys are checked
// in order, so the first error is the same at every run.
func (c *Config) validateYield(seenIDs map[string]bool) error {
	y := c.Yield
	if y == nil {
		return nil
	}
	for _, ccy := range slices.Sorted(maps.Keys(y.Curves)) {
		curve := y.Curves[ccy]
		if !IsLikelyISO4217(ccy) {
			return fmt.Errorf("config: yield.curves[%q]: not a 3-letter ISO 4217 code", ccy)
		}
		if len(curve) == 0 {
			return fmt.Errorf("config: yield.curves[%q]: a curve names at least one tenor", ccy)
		}
		lengths := map[int]string{}
		for _, tenor := range slices.Sorted(maps.Keys(curve)) {
			months, err := canonical.TenorMonths(tenor)
			if err != nil {
				return fmt.Errorf("config: yield.curves[%q][%q]: %w", ccy, tenor, err)
			}
			if prev, dup := lengths[months]; dup {
				return fmt.Errorf("config: yield.curves[%q]: %q and %q are the same tenor", ccy, prev, tenor)
			}
			lengths[months] = tenor
			source, _, err := splitSeries(curve[tenor])
			if err != nil {
				return fmt.Errorf("config: yield.curves[%q][%q]: %w", ccy, tenor, err)
			}
			if !seenIDs[source] {
				return fmt.Errorf("config: yield.curves[%q][%q]: series %q: no silver_sources[].id matches",
					ccy, tenor, curve[tenor])
			}
		}
	}
	for _, country := range slices.Sorted(maps.Keys(y.Withholding)) {
		rates := y.Withholding[country]
		if !isCountryCode(country) {
			return fmt.Errorf("config: yield.withholding[%q]: not an ISO 3166 alpha-2 code", country)
		}
		if country == canonical.EuropeanUnion {
			return fmt.Errorf("config: yield.withholding[%q]: EU is the European Union, never a situs", country)
		}
		if len(rates) == 0 {
			return fmt.Errorf("config: yield.withholding[%q]: name a rate for %q or a tax wrapper",
				country, canonical.WithholdingDefault)
		}
		for _, wrapper := range slices.Sorted(maps.Keys(rates)) {
			if wrapper != canonical.WithholdingDefault && !canonical.TaxWrapper(wrapper).Valid() {
				return fmt.Errorf("config: yield.withholding[%q][%q]: want %q or a known tax wrapper",
					country, wrapper, canonical.WithholdingDefault)
			}
			if r := rates[wrapper]; !(r >= 0 && r <= 100) {
				return fmt.Errorf("config: yield.withholding[%q][%q] %v: want a percent, 0 to 100", country, wrapper, r)
			}
		}
	}
	rated := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(y.RatingFunds)) {
		ref, err := canonical.ParseFundRef(key)
		if err != nil {
			return fmt.Errorf("config: yield.rating_funds: %w", err)
		}
		if prev, dup := rated[ref]; dup {
			return fmt.Errorf("config: yield.rating_funds: %q and %q name the same fund", prev, key)
		}
		rated[ref] = key
		if _, _, err := canonical.ParseRating(y.RatingFunds[key]); err != nil {
			return fmt.Errorf("config: yield.rating_funds[%q] %q: %w", key, y.RatingFunds[key], err)
		}
	}
	for _, market := range slices.Sorted(maps.Keys(y.Conventions)) {
		cv := y.Conventions[market]
		if err := checkMarket(market); err != nil {
			return fmt.Errorf("config: yield.conventions[%q]: %w", market, err)
		}
		if cv.Frequency == nil {
			return fmt.Errorf("config: yield.conventions[%q]: frequency is required", market)
		}
		if !canonical.ValidCouponFrequency(*cv.Frequency) {
			return fmt.Errorf("config: yield.conventions[%q].frequency %d: want one of %s (0 for a discount instrument)",
				market, *cv.Frequency, frequencyNames(true))
		}
		if cv.DayCount == "" {
			return fmt.Errorf("config: yield.conventions[%q]: day_count is required", market)
		}
		if !canonical.DayCount(cv.DayCount).Valid() {
			return fmt.Errorf("config: yield.conventions[%q].day_count %q: want one of %s", market, cv.DayCount,
				joinNames(canonical.DayCounts()))
		}
	}
	if y.HorizonMonths < 0 || y.HorizonMonths > canonical.MaxHorizonMonths {
		return fmt.Errorf("config: yield.horizon_months %d: want 1 to %d months, or 0 for the default",
			y.HorizonMonths, canonical.MaxHorizonMonths)
	}
	for _, sector := range slices.Sorted(maps.Keys(y.Cycles)) {
		if _, ok := canonical.BuiltinCycle(canonical.Sector(sector)); !ok {
			sectors := make([]canonical.Sector, 0, len(canonical.BuiltinCycles()))
			for _, bc := range canonical.BuiltinCycles() {
				sectors = append(sectors, bc.Sector)
			}
			return fmt.Errorf("config: yield.cycles[%q]: not one of the eleven sectors (%s)", sector, joinNames(sectors))
		}
		if cy := y.Cycles[sector]; !canonical.Cycle(cy).Valid() {
			return fmt.Errorf("config: yield.cycles[%q] %q: want one of %s", sector, cy, joinNames(canonical.Cycles()))
		}
	}
	return nil
}

// splitSeries reads a curve's series, <silver_source_id>:<series_id>.
func splitSeries(s string) (source, id string, err error) {
	source, id, ok := strings.Cut(s, ":")
	if !ok || source == "" || id == "" || strings.ContainsFunc(s, unicode.IsSpace) {
		return "", "", fmt.Errorf("series %q: want <silver_source_id>:<series_id>", s)
	}
	return source, id, nil
}

// checkMarket checks a convention's market: a currency, or a currency, a
// colon and one of canonical.MarketSegments.
func checkMarket(market string) error {
	ccy, segment, hasSegment := strings.Cut(market, ":")
	if !IsLikelyISO4217(ccy) {
		return fmt.Errorf("%q is not a 3-letter ISO 4217 code", ccy)
	}
	if hasSegment && !slices.Contains(canonical.MarketSegments(), segment) {
		return fmt.Errorf("%q is not one of %s", segment, strings.Join(canonical.MarketSegments(), ", "))
	}
	return nil
}

// YieldRows is the yield block as gold stores it, in a stable order:
// every fund reference in the form gold builds, the conventions and
// cycles the built-in maps with the block's entries over them. Call
// after Validate. An absent block yields the built-in maps alone. The
// horizon stays in the config (YieldHorizon).
func (c *Config) YieldRows() canonical.YieldRows {
	out := canonical.YieldRows{
		Conventions: canonical.BuiltinConventions(),
		Cycles:      canonical.BuiltinCycles(),
	}
	y := c.Yield
	if y == nil {
		return out
	}
	for ccy, curve := range y.Curves {
		for tenor, series := range curve {
			months, _ := canonical.TenorMonths(tenor)
			source, id, _ := splitSeries(series)
			out.Curves = append(out.Curves, canonical.YieldCurvePoint{Currency: ccy, Tenor: tenor, TenorMonths: months,
				SilverSourceID: source, SeriesID: id})
		}
	}
	slices.SortFunc(out.Curves, func(a, b canonical.YieldCurvePoint) int {
		return cmp.Or(strings.Compare(a.Currency, b.Currency), cmp.Compare(a.TenorMonths, b.TenorMonths))
	})
	for _, country := range slices.Sorted(maps.Keys(y.Withholding)) {
		rates := y.Withholding[country]
		for _, wrapper := range slices.Sorted(maps.Keys(rates)) {
			out.Withholding = append(out.Withholding, canonical.YieldWithholding{Country: country, Wrapper: wrapper,
				RatePct: rates[wrapper]})
		}
	}
	for key, rating := range y.RatingFunds {
		ref, _ := canonical.ParseFundRef(key)
		high, low, _ := canonical.ParseRating(rating)
		out.RatingFunds = append(out.RatingFunds, canonical.YieldRatingFund{FundRef: ref, Rating: rating,
			BucketHigh: high, BucketLow: low})
	}
	slices.SortFunc(out.RatingFunds, func(a, b canonical.YieldRatingFund) int { return strings.Compare(a.FundRef, b.FundRef) })
	for market, cv := range y.Conventions {
		row := canonical.YieldConvention{Market: market, Frequency: *cv.Frequency, DayCount: canonical.DayCount(cv.DayCount),
			Origin: canonical.OriginConfig}
		if i := slices.IndexFunc(out.Conventions, func(b canonical.YieldConvention) bool { return b.Market == market }); i >= 0 {
			out.Conventions[i] = row
		} else {
			out.Conventions = append(out.Conventions, row)
		}
	}
	slices.SortFunc(out.Conventions, func(a, b canonical.YieldConvention) int { return strings.Compare(a.Market, b.Market) })
	for i, bc := range out.Cycles {
		if cy, ok := y.Cycles[string(bc.Sector)]; ok {
			out.Cycles[i] = canonical.YieldCycle{Sector: bc.Sector, Cycle: canonical.Cycle(cy), Origin: canonical.OriginConfig}
		}
	}
	return out
}
