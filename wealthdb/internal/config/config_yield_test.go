package config

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func yieldConfig(t *testing.T, block string) (*Config, error) {
	t.Helper()
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "rates", "kind": "fred", "path": "/tmp/r.db"},
                           {"id": "demo", "kind": "synthetic", "path": "/tmp/d.db"}],
        "yield": `+block+`}`)
	return Load(path)
}

// The block expands into gold's rows in a stable order: a curve by
// tenor length, each fund reference in gold's form, and the conventions
// and cycles the built-in maps with the block's entries over them. The
// horizon stays in the config.
func TestYieldRows(t *testing.T) {
	t.Parallel()
	c, err := yieldConfig(t, `{
        "curves": {"USD": {"10Y": "rates:DGS10", "1M": "rates:DGS1MO", "1Y": "rates:DGS1", "3M": "rates:DGS3MO"},
                   "CHF": {"6M": "demo:CHF6M"}},
        "withholding": {"US": {"default": 30, "taxable_personal": 15, "accounts": {"demo": {"acct-2": 0, "acct-1": 30}}},
                        "CH": {"default": 35}, "IE": {"roth_ira": 0}, "FR": {"accounts": {"rates": {"acct-3": 12.8}}}},
        "rating_funds": {"isin:xx0000000004": "BBB", "symbol:exmp": "AAA-A"},
        "conventions": {"USD": {"frequency": 4, "day_count": "ACT/365"}, "JPY:government": {"frequency": 2, "day_count": "ACT/365"}},
        "horizon_months": 24,
        "cycles": {"utilities": "sensitive", "energy": "cyclical"}
    }`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if h := c.YieldHorizon(); h != 24 {
		t.Errorf("horizon = %d, want 24", h)
	}
	rows := c.YieldRows()
	wantCurves := []canonical.YieldCurvePoint{
		{Currency: "CHF", Tenor: "6M", TenorMonths: 6, SilverSourceID: "demo", SeriesID: "CHF6M"},
		{Currency: "USD", Tenor: "1M", TenorMonths: 1, SilverSourceID: "rates", SeriesID: "DGS1MO"},
		{Currency: "USD", Tenor: "3M", TenorMonths: 3, SilverSourceID: "rates", SeriesID: "DGS3MO"},
		{Currency: "USD", Tenor: "1Y", TenorMonths: 12, SilverSourceID: "rates", SeriesID: "DGS1"},
		{Currency: "USD", Tenor: "10Y", TenorMonths: 120, SilverSourceID: "rates", SeriesID: "DGS10"},
	}
	if !slices.Equal(rows.Curves, wantCurves) {
		t.Errorf("curves = %+v, want %+v", rows.Curves, wantCurves)
	}
	wantWithholding := []canonical.YieldWithholding{
		{Country: "CH", Wrapper: "default", RatePct: 35},
		{Country: "IE", Wrapper: "roth_ira", RatePct: 0},
		{Country: "US", Wrapper: "default", RatePct: 30},
		{Country: "US", Wrapper: "taxable_personal", RatePct: 15},
	}
	if !slices.Equal(rows.Withholding, wantWithholding) {
		t.Errorf("withholding = %+v, want %+v", rows.Withholding, wantWithholding)
	}
	wantAccounts := []canonical.YieldAccountWithholding{
		{Country: "FR", SilverSourceID: "rates", AccountExternalID: "acct-3", RatePct: 12.8},
		{Country: "US", SilverSourceID: "demo", AccountExternalID: "acct-1", RatePct: 30},
		{Country: "US", SilverSourceID: "demo", AccountExternalID: "acct-2", RatePct: 0},
	}
	if !slices.Equal(rows.AccountWithholding, wantAccounts) {
		t.Errorf("account withholding = %+v, want %+v", rows.AccountWithholding, wantAccounts)
	}
	wantRating := []canonical.YieldRatingFund{
		{FundRef: "isin:XX0000000004", Rating: "BBB", BucketHigh: canonical.RatingBBB, BucketLow: canonical.RatingBBB},
		{FundRef: "symbol:EXMP", Rating: "AAA-A", BucketHigh: canonical.RatingAAA, BucketLow: canonical.RatingA},
	}
	if !slices.Equal(rows.RatingFunds, wantRating) {
		t.Errorf("rating funds = %+v, want %+v", rows.RatingFunds, wantRating)
	}
	// The built-in conventions, the block's USD entry over the built-in
	// one and its JPY:government entry added, by market.
	wantConventions := []canonical.YieldConvention{{Market: "JPY:government", Frequency: 2,
		DayCount: canonical.DayCountAct365, Origin: canonical.OriginConfig}}
	for _, b := range canonical.BuiltinConventions() {
		if b.Market == "USD" {
			b = canonical.YieldConvention{Market: "USD", Frequency: 4, DayCount: canonical.DayCountAct365,
				Origin: canonical.OriginConfig}
		}
		wantConventions = append(wantConventions, b)
	}
	slices.SortFunc(wantConventions, func(a, b canonical.YieldConvention) int { return strings.Compare(a.Market, b.Market) })
	if !slices.Equal(rows.Conventions, wantConventions) {
		t.Errorf("conventions = %+v, want %+v", rows.Conventions, wantConventions)
	}
	if len(rows.Cycles) != 11 {
		t.Fatalf("cycles = %+v, want all eleven sectors", rows.Cycles)
	}
	for _, cy := range rows.Cycles {
		want := canonical.YieldCycle{Sector: cy.Sector, Origin: canonical.OriginBuiltin}
		want.Cycle, _ = canonical.BuiltinCycle(cy.Sector)
		switch cy.Sector {
		case canonical.SectorUtilities:
			want.Cycle, want.Origin = canonical.CycleSensitive, canonical.OriginConfig
		case canonical.SectorEnergy:
			want.Cycle, want.Origin = canonical.CycleCyclical, canonical.OriginConfig
		}
		if cy != want {
			t.Errorf("cycle %+v, want %+v", cy, want)
		}
	}
	// The built-in map is the block's to override, not to change.
	if cy, _ := canonical.BuiltinCycle(canonical.SectorUtilities); cy != canonical.CycleDefensive {
		t.Error("the config's entry changed the built-in cycle of utilities")
	}
}

// Without the block, or with an empty one, gold gets the built-in
// conventions and cycles, and nothing else; a horizon of 0 reads the
// default.
func TestYieldRowsDefaults(t *testing.T) {
	t.Parallel()
	empty, err := yieldConfig(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := yieldConfig(t, `{"horizon_months": 0}`)
	if err != nil {
		t.Fatal(err)
	}
	if h := zero.YieldHorizon(); h != canonical.DefaultHorizonMonths {
		t.Errorf("a horizon of 0 = %d, want %d", h, canonical.DefaultHorizonMonths)
	}
	for _, c := range []*Config{{}, empty, zero} {
		rows := c.YieldRows()
		if len(rows.Curves)+len(rows.Withholding)+len(rows.RatingFunds) != 0 ||
			!slices.Equal(rows.Conventions, canonical.BuiltinConventions()) ||
			!slices.Equal(rows.Cycles, canonical.BuiltinCycles()) {
			t.Errorf("rows = %+v", rows)
		}
	}
}

func TestYieldRejects(t *testing.T) {
	t.Parallel()
	for block, want := range map[string]string{
		// Keys the block and its entries do not know.
		`{"curve": {}}`:   `unknown field "curve"`,
		`{"horizon": 12}`: `unknown field "horizon"`,
		`{"conventions": {"USD": {"frequency": 2, "day_count": "30/360", "coupon": 5}}}`: `unknown field "coupon"`,
		`{"curves": {"USD": {"3M": {"series": "rates:DGS3MO"}}}}`:                        "cannot unmarshal",
		`{"withholding": {"US": {"default": "30"}}}`:                                     `yield.withholding["US"]["default"]: want a percent: json: cannot unmarshal`,
		`{"horizon_months": 6.5}`:                                                        "cannot unmarshal",

		// curves
		`{"curves": {"usd": {"3M": "rates:DGS3MO"}}}`:                       `yield.curves["usd"]: not a 3-letter ISO 4217 code`,
		`{"curves": {"USD": {}}}`:                                           `yield.curves["USD"]: a curve names at least one tenor`,
		`{"curves": {"USD": {"3X": "rates:DGS3MO"}}}`:                       `config: yield.curves["USD"]["3X"]: a tenor is a whole number and M or Y`,
		`{"curves": {"USD": {"3m": "rates:DGS3MO"}}}`:                       `yield.curves["USD"]["3m"]: a tenor is a whole number`,
		`{"curves": {"USD": {"0M": "rates:DGS3MO"}}}`:                       `yield.curves["USD"]["0M"]: a tenor is a whole number`,
		`{"curves": {"USD": {"51Y": "rates:DGS51"}}}`:                       `yield.curves["USD"]["51Y"]: a tenor is at most 50 years`,
		`{"curves": {"USD": {"12M": "rates:A", "1Y": "rates:B"}}}`:          `yield.curves["USD"]: "12M" and "1Y" are the same tenor`,
		`{"curves": {"USD": {"3M": "DGS3MO"}}}`:                             `yield.curves["USD"]["3M"]: series "DGS3MO": want <silver_source_id>:<series_id>`,
		`{"curves": {"USD": {"3M": "rates:"}}}`:                             `series "rates:": want`,
		`{"curves": {"USD": {"3M": ":DGS3MO"}}}`:                            `series ":DGS3MO": want`,
		`{"curves": {"USD": {"3M": "rates: DGS3MO"}}}`:                      `series "rates: DGS3MO": want`,
		`{"curves": {"USD": {"3M": "nope:DGS3MO"}}}`:                        `yield.curves["USD"]["3M"]: series "nope:DGS3MO": no silver_sources[].id matches`,
		`{"curves": {"USD": {"1M": "rates:DGS1MO", "3M": "Rates:DGS3MO"}}}`: `series "Rates:DGS3MO": no silver_sources[].id matches`,

		// withholding
		`{"withholding": {"us": {"default": 30}}}`:                      `yield.withholding["us"]: not an ISO 3166 alpha-2 code`,
		`{"withholding": {"QQ": {"default": 30}}}`:                      `yield.withholding["QQ"]: not an ISO 3166`,
		`{"withholding": {"EU": {"default": 30}}}`:                      `yield.withholding["EU"]: EU is the European Union, never a situs`,
		`{"withholding": {"US": {}}}`:                                   `yield.withholding["US"]: name a rate for "default", a tax wrapper or an account`,
		`{"withholding": {"US": {"ira": 0}}}`:                           `yield.withholding["US"]["ira"]: want "default", a known tax wrapper or "accounts"`,
		`{"withholding": {"US": {"accounts": {"nope": {"a-1": 0}}}}}`:   `yield.withholding["US"]["accounts"]["nope"]: no silver_sources[].id matches`,
		`{"withholding": {"US": {"accounts": {"demo": {}}}}}`:           `yield.withholding["US"]["accounts"]["demo"]: name at least one account`,
		`{"withholding": {"US": {"accounts": {"demo": {" ": 0}}}}}`:     `yield.withholding["US"]["accounts"]["demo"]: an account id is empty`,
		`{"withholding": {"US": {"accounts": {"demo": {"a-1 ": 0}}}}}`:  `["a-1 "]: an account id has space around it`,
		`{"withholding": {"US": {"accounts": {"demo": {"a-1": 101}}}}}`: `yield.withholding["US"]["accounts"]["demo"]["a-1"] 101: want a percent, 0 to 100`,
		`{"withholding": {"US": {"accounts": {"demo": 0}}}}`:            `yield.withholding["US"]["accounts"]: want {source: {account: percent}}`,
		`{"withholding": {"US": {"default": "x"}}}`:                     `yield.withholding["US"]["default"]: want a percent`,
		`{"withholding": {"US": 30}}`:                                   `yield.withholding["US"]: want an object`,
		`{"withholding": 30}`:                                           `yield.withholding: want an object`,
		`{"withholding": {"CH": {"default": 35}, "US": [30]}}`:          `yield.withholding["US"]: want an object`,
		`{"withholding": {"US": {"Default": 30}}}`:                      `yield.withholding["US"]["Default"]: want "default"`,
		`{"withholding": {"US": {"default": -1}}}`:                      `yield.withholding["US"]["default"] -1: want a percent, 0 to 100`,
		`{"withholding": {"US": {"taxable_personal": 100.5}}}`:          `yield.withholding["US"]["taxable_personal"] 100.5: want a percent`,
		`{"withholding": {"CH": {"default": 35}, "US": {"x": 1}}}`:      `yield.withholding["US"]["x"]`,

		// rating_funds
		`{"rating_funds": {"XX0000000004": "BBB"}}`:                     "config: yield.rating_funds: fund reference \"XX0000000004\": want isin:",
		`{"rating_funds": {"isin:bad": "BBB"}}`:                         "not an ISIN",
		`{"rating_funds": {"symbol:EXMP": "BBB", "symbol:exmp ": "A"}}`: `yield.rating_funds: "symbol:EXMP" and "symbol:exmp " name the same fund`,
		`{"rating_funds": {"isin:XX0000000004": "A-AAA"}}`:              `yield.rating_funds["isin:XX0000000004"] "A-AAA": a range runs from its better end to its worse: AAA-A`,
		`{"rating_funds": {"isin:XX0000000004": "CCC"}}`:                `yield.rating_funds["isin:XX0000000004"] "CCC": want a rating bucket (AAA, AA, A, BBB, BB, B)`,
		`{"rating_funds": {"isin:XX0000000004": "bbb"}}`:                `"bbb": want a rating bucket`,
		`{"rating_funds": {"isin:XX0000000004": "AA+"}}`:                `"AA+": want a rating bucket`,
		`{"rating_funds": {"isin:XX0000000004": ""}}`:                   `"": want a rating bucket`,

		// conventions
		`{"conventions": {"usd": {"frequency": 2, "day_count": "30/360"}}}`:          `yield.conventions["usd"]: "usd" is not a 3-letter ISO 4217 code`,
		`{"conventions": {"USD:treasury": {"frequency": 2, "day_count": "30/360"}}}`: `yield.conventions["USD:treasury"]: "treasury" is not one of government, municipal, corporate, securitised, bill`,
		`{"conventions": {"USD:": {"frequency": 2, "day_count": "30/360"}}}`:         `yield.conventions["USD:"]: "" is not one of`,
		`{"conventions": {"USD:bill:x": {"frequency": 0, "day_count": "ACT/360"}}}`:  `"bill:x" is not one of`,
		`{"conventions": {"USD": {"day_count": "30/360"}}}`:                          `yield.conventions["USD"]: frequency is required`,
		`{"conventions": {"USD": {"frequency": 3, "day_count": "30/360"}}}`:          `yield.conventions["USD"].frequency 3: want one of 0, 1, 2, 4, 12 (0 for a discount instrument)`,
		`{"conventions": {"USD": {"frequency": -1, "day_count": "30/360"}}}`:         `frequency -1: want one of 0,`,
		`{"conventions": {"USD": {"frequency": 2}}}`:                                 `yield.conventions["USD"]: day_count is required`,
		`{"conventions": {"USD": {"frequency": 2, "day_count": "act/act"}}}`:         `yield.conventions["USD"].day_count "act/act": want one of 30/360, 30E/360, ACT/ACT, ACT/360, ACT/365`,
		`{"conventions": {"USD": {"frequency": 2, "day_count": "ACT/364"}}}`:         `day_count "ACT/364": want one of`,

		// horizon_months
		`{"horizon_months": -1}`: "config: yield.horizon_months -1: want 1 to 36 months, or 0 for the default",
		`{"horizon_months": 37}`: "yield.horizon_months 37: want 1 to 36 months",

		// cycles
		`{"cycles": {"tech": "sensitive"}}`:       `yield.cycles["tech"]: not one of the eleven sectors (energy, materials, industrials, consumer_cyclical, consumer_staples, health_care, financials, technology, communication, utilities, real_estate)`,
		`{"cycles": {"government": "defensive"}}`: `yield.cycles["government"]: not one of the eleven sectors`,
		`{"cycles": {"cash": "defensive"}}`:       `yield.cycles["cash"]: not one of the eleven sectors`,
		`{"cycles": {"utilities": "growth"}}`:     `yield.cycles["utilities"] "growth": want one of cyclical, defensive, sensitive`,
		`{"cycles": {"utilities": "Defensive"}}`:  `yield.cycles["utilities"] "Defensive": want one of`,
	} {
		if _, err := yieldConfig(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", block, err, want)
		}
	}
}

// A country's entry reads back as it was written: the wrappers' rates
// beside the accounts'.
func TestWithholdingRatesRoundTrip(t *testing.T) {
	t.Parallel()
	in := WithholdingRates{Wrappers: map[string]float64{"default": 30, "custodial_utma": 15},
		Accounts: map[string]map[string]float64{"demo": {"acct-1": 0}}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"accounts":{"demo":{"acct-1":0}},"custodial_utma":15,"default":30}` {
		t.Errorf("marshal = %s", b)
	}
	var out WithholdingRates
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(out, in) {
		t.Errorf("unmarshal = %+v (%v), want %+v", out, err, in)
	}
	// A country that names accounts alone.
	only := WithholdingRates{Wrappers: map[string]float64{}, Accounts: map[string]map[string]float64{"demo": {"a": 5}}}
	b, err = json.Marshal(only)
	if err != nil || string(b) != `{"accounts":{"demo":{"a":5}}}` {
		t.Errorf("marshal = %s (%v)", b, err)
	}
	out = WithholdingRates{}
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(out, only) {
		t.Errorf("unmarshal = %+v (%v), want %+v", out, err, only)
	}
}

// A rate no JSON can carry is refused all the same in a config built by
// hand.
func TestYieldRejectsNonFiniteRate(t *testing.T) {
	t.Parallel()
	for _, r := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := &Config{Yield: &YieldConfig{Withholding: WithholdingTable{
			"US": {Wrappers: map[string]float64{"default": r}}}}}
		if err := c.validateYield(nil); err == nil || !strings.Contains(err.Error(), "want a percent, 0 to 100") {
			t.Errorf("%v: err %v", r, err)
		}
	}
}

// The checks run in key order, so a block with two errors names the
// same one at every run.
func TestYieldRejectsFirstInOrder(t *testing.T) {
	t.Parallel()
	block := `{"curves": {"USD": {"1M": "nope:A", "3M": "nope:B", "6M": "nope:C"}, "EUR": {"1Y": "nope:D"}}}`
	for range 20 {
		if _, err := yieldConfig(t, block); err == nil || !strings.Contains(err.Error(), `yield.curves["EUR"]["1Y"]`) {
			t.Fatalf("err %v, want the EUR curve's 1Y first", err)
		}
	}
}

// The projection's default window is the block's horizon, else the
// built-in one.
func TestYieldHorizon(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		cfg  Config
		want int
	}{
		{Config{}, canonical.DefaultHorizonMonths},
		{Config{Yield: &YieldConfig{}}, canonical.DefaultHorizonMonths},
		{Config{Yield: &YieldConfig{HorizonMonths: 24}}, 24},
	} {
		if got := c.cfg.YieldHorizon(); got != c.want {
			t.Errorf("%+v: %d, want %d", c.cfg.Yield, got, c.want)
		}
	}
}
