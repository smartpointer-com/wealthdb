package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// YieldConfig is the `yield` block of wealthdb.cfg: what the yield
// report needs that no source states. Absent block ⇒ no curves, no
// withholding rates, no rating funds, the built-in conventions, cycles
// and settlement cycles, and the default horizon. The load writes all of
// it but the horizon into gold's yield tables (gold.SyncYield); the
// reports read the horizon from the config (YieldHorizon).
type YieldConfig struct {
	// Curves maps a currency to its public curve: each tenor (3M, 1Y) to
	// the rate series that gives it, as <silver_source_id>:<series_id>.
	Curves map[string]map[string]string `json:"curves,omitempty"`
	// Withholding maps a situs country to the tax it withholds at source
	// on income, in percent, per tax wrapper, and per account where an
	// account's rate is not its wrapper's (WithholdingRates). A country
	// the block does not name has no rate, which reads as unknown, never
	// as zero.
	Withholding WithholdingTable `json:"withholding,omitempty"`
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
	// SettlementCycles maps a market, a country, to its settlement
	// cycles, oldest first. An entry replaces the built-in cycles of its
	// market (canonical.BuiltinSettlementCycles); an empty list leaves the
	// market none.
	SettlementCycles map[string][]SettlementCycleEntry `json:"settlement_cycles,omitempty"`
}

// WithholdingRates is one country's entry of `yield.withholding`: a rate
// per tax wrapper, the wrapper `default` standing for every wrapper it
// names no rate for, and under the key `accounts` the accounts whose
// rate is not their wrapper's, by silver_source_id and account, as
// {"bank-1": {"acct-1": 0}}. An account's rate wins over its wrapper's,
// which wins over the default.
type WithholdingRates struct {
	Wrappers map[string]float64
	Accounts map[string]map[string]float64
}

// withholdingAccountsKey is the key of a country's per-account rates.
const withholdingAccountsKey = "accounts"

// WithholdingTable is `yield.withholding`: each situs country's rates.
type WithholdingTable map[string]WithholdingRates

// UnmarshalJSON reads each country's object in key order, so the first
// error is the same at every run, and names the country of a value that
// does not read.
func (t *WithholdingTable) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return &withholdingError{want: "an object"}
	}
	out := make(WithholdingTable, len(raw))
	for _, country := range slices.Sorted(maps.Keys(raw)) {
		var w WithholdingRates
		if err := w.UnmarshalJSON(raw[country]); err != nil {
			if e, ok := errors.AsType[*withholdingError](err); ok {
				e.at = fmt.Sprintf("[%q]", country) + e.at
			}
			return err
		}
		out[country] = w
	}
	*t = out
	return nil
}

// withholdingError is a value of `yield.withholding` that does not read:
// at is its place in the block, such as ["US"]["default"], and want what
// it should be.
type withholdingError struct {
	at, want string
	err      error
}

func (e *withholdingError) Error() string {
	msg := "yield.withholding" + e.at + ": want " + e.want
	if e.err != nil {
		msg += ": " + e.err.Error()
	}
	return msg
}

func (e *withholdingError) Unwrap() error { return e.err }

// UnmarshalJSON reads a country's object: `accounts` into Accounts, every
// other key a wrapper's rate.
func (w *WithholdingRates) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return &withholdingError{want: "an object"}
	}
	w.Wrappers, w.Accounts = map[string]float64{}, nil
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		if k == withholdingAccountsKey {
			if err := json.Unmarshal(raw[k], &w.Accounts); err != nil {
				return &withholdingError{at: fmt.Sprintf("[%q]", k), want: "{source: {account: percent}}", err: err}
			}
			continue
		}
		var r float64
		if err := json.Unmarshal(raw[k], &r); err != nil {
			return &withholdingError{at: fmt.Sprintf("[%q]", k), want: "a percent", err: err}
		}
		w.Wrappers[k] = r
	}
	return nil
}

// MarshalJSON writes the object UnmarshalJSON reads.
func (w WithholdingRates) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	for k, r := range w.Wrappers {
		out[k] = r
	}
	if len(w.Accounts) > 0 {
		out[withholdingAccountsKey] = w.Accounts
	}
	return json.Marshal(out)
}

// SettlementCycleEntry is one cycle of a `yield.settlement_cycles`
// market: the business days a trade settles after (1 or more), from its
// day on, YYYY-MM-DD. Only a market's first entry may leave the day out:
// it then holds from the first day on.
type SettlementCycleEntry struct {
	From string `json:"from,omitempty"`
	Days int    `json:"days"`
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
// wrapper, fund, rating, market and sector is of its vocabulary, every
// curve's series names a configured source, and each market's settlement
// cycles ascend by day, each of a day or more. The keys are checked in
// order, so the first error is the same at every run.
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
		if len(rates.Wrappers) == 0 && len(rates.Accounts) == 0 {
			return fmt.Errorf("config: yield.withholding[%q]: name a rate for %q, a tax wrapper or an account",
				country, canonical.WithholdingDefault)
		}
		for _, wrapper := range slices.Sorted(maps.Keys(rates.Wrappers)) {
			if wrapper != canonical.WithholdingDefault && !canonical.TaxWrapper(wrapper).Valid() {
				return fmt.Errorf("config: yield.withholding[%q][%q]: want %q, a known tax wrapper or %q",
					country, wrapper, canonical.WithholdingDefault, withholdingAccountsKey)
			}
			if r := rates.Wrappers[wrapper]; !(r >= 0 && r <= 100) {
				return fmt.Errorf("config: yield.withholding[%q][%q] %v: want a percent, 0 to 100", country, wrapper, r)
			}
		}
		for _, source := range slices.Sorted(maps.Keys(rates.Accounts)) {
			accounts := rates.Accounts[source]
			at := fmt.Sprintf("yield.withholding[%q][%q][%q]", country, withholdingAccountsKey, source)
			if !seenIDs[source] {
				return fmt.Errorf("config: %s: no silver_sources[].id matches", at)
			}
			if len(accounts) == 0 {
				return fmt.Errorf("config: %s: name at least one account", at)
			}
			for _, acct := range slices.Sorted(maps.Keys(accounts)) {
				if strings.TrimSpace(acct) == "" {
					return fmt.Errorf("config: %s: an account id is empty", at)
				}
				if strings.TrimSpace(acct) != acct {
					return fmt.Errorf("config: %s[%q]: an account id has no space around it", at, acct)
				}
				if r := accounts[acct]; !(r >= 0 && r <= 100) {
					return fmt.Errorf("config: %s[%q] %v: want a percent, 0 to 100", at, acct, r)
				}
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
	for _, market := range slices.Sorted(maps.Keys(y.SettlementCycles)) {
		if !isCountryCode(market) || market == canonical.EuropeanUnion {
			return fmt.Errorf("config: yield.settlement_cycles[%q]: not a market: want an ISO 3166 alpha-2 code", market)
		}
		var prev time.Time
		for i, c := range y.SettlementCycles[market] {
			if c.Days < 1 {
				return fmt.Errorf("config: yield.settlement_cycles[%q][%d].days %d: want 1 or more business days",
					market, i, c.Days)
			}
			if c.From == "" {
				if i > 0 {
					return fmt.Errorf("config: yield.settlement_cycles[%q][%d]: from is required past the first cycle",
						market, i)
				}
				continue
			}
			from, err := time.Parse(time.DateOnly, c.From)
			if err != nil {
				return fmt.Errorf("config: yield.settlement_cycles[%q][%d].from %q: want YYYY-MM-DD", market, i, c.From)
			}
			if i > 0 && !from.After(prev) {
				return fmt.Errorf("config: yield.settlement_cycles[%q][%d].from %s: the cycles ascend by day, "+
					"each after the one before", market, i, c.From)
			}
			prev = from
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
// every fund reference in the form gold builds, the conventions, cycles
// and settlement cycles the built-in maps with the block's entries over
// them. Call after Validate. An absent block yields the built-in maps
// alone. The horizon stays in the config (YieldHorizon).
func (c *Config) YieldRows() canonical.YieldRows {
	out := canonical.YieldRows{
		Conventions:      canonical.BuiltinConventions(),
		Cycles:           canonical.BuiltinCycles(),
		SettlementCycles: canonical.BuiltinSettlementCycles(),
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
		for _, wrapper := range slices.Sorted(maps.Keys(rates.Wrappers)) {
			out.Withholding = append(out.Withholding, canonical.YieldWithholding{Country: country, Wrapper: wrapper,
				RatePct: rates.Wrappers[wrapper]})
		}
		for _, source := range slices.Sorted(maps.Keys(rates.Accounts)) {
			for _, acct := range slices.Sorted(maps.Keys(rates.Accounts[source])) {
				out.AccountWithholding = append(out.AccountWithholding, canonical.YieldAccountWithholding{
					Country: country, SilverSourceID: source, AccountExternalID: acct,
					RatePct: rates.Accounts[source][acct]})
			}
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
	out.SettlementCycles = slices.DeleteFunc(out.SettlementCycles, func(sc canonical.SettlementCycle) bool {
		_, named := y.SettlementCycles[sc.Market]
		return named
	})
	for market, cycles := range y.SettlementCycles {
		for _, entry := range cycles {
			sc := canonical.SettlementCycle{Market: market, Days: entry.Days, Origin: canonical.OriginConfig}
			if from, err := time.Parse(time.DateOnly, entry.From); err == nil {
				sc.From = &from
			}
			out.SettlementCycles = append(out.SettlementCycles, sc)
		}
	}
	// A market's cycles keep their order, oldest first, whichever map
	// gave them.
	slices.SortStableFunc(out.SettlementCycles, func(a, b canonical.SettlementCycle) int {
		return strings.Compare(a.Market, b.Market)
	})
	return out
}
