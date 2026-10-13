package config

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func exposureConfig(t *testing.T, block string) (*Config, error) {
	t.Helper()
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "manual", "kind": "manual", "path": "/tmp/m.db"},
                           {"id": "bank", "kind": "ubs", "path": "/tmp/b.db"}],
        "exposure": `+block+`}`)
	return Load(path)
}

// The block expands into gold's rows: every fund reference in gold's own
// form, a proxy's leverage defaulting to 1, rows in a stable order.
func TestExposureRows(t *testing.T) {
	c, err := exposureConfig(t, `{
        "proxies": {
            "isin:xx0000000001": {"fund": "cusip:00000aa01"},
            "symbol:lev3":       {"fund": "symbol:IDX1", "leverage": 3},
            "isin:XX0000000003": {"fund": "isin:XX0000000004", "always": true}
        },
        "reference_funds": ["symbol:totl", "isin:XX0000000005"],
        "tags": {"manual": {"house-1": {"country": "CH", "currency": "CHF"},
                            "pe-fund-1": {"country": "US", "currency": "USD", "sector": "technology"}}},
        "custody": {"bank": {"custodian": " Example Bank AG ", "country": "CH"}},
        "max_gap_days": 200
    }`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows := c.ExposureRows()
	wantProxies := []canonical.ExposureProxy{
		{FundRef: "isin:XX0000000001", ProxyRef: "cusip:00000AA01", Leverage: 1},
		{FundRef: "isin:XX0000000003", ProxyRef: "isin:XX0000000004", Always: true, Leverage: 1},
		{FundRef: "symbol:LEV3", ProxyRef: "symbol:IDX1", Leverage: 3},
	}
	if len(rows.Proxies) != len(wantProxies) {
		t.Fatalf("proxies = %+v", rows.Proxies)
	}
	for i, p := range wantProxies {
		if rows.Proxies[i] != p {
			t.Errorf("proxy %d = %+v, want %+v", i, rows.Proxies[i], p)
		}
	}
	if strings.Join(rows.ReferenceFunds, ",") != "isin:XX0000000005,symbol:TOTL" {
		t.Errorf("reference funds = %v", rows.ReferenceFunds)
	}
	if len(rows.Tags) != 2 || rows.Tags[0].InstrumentExternalID != "house-1" || rows.Tags[0].Country != "CH" ||
		rows.Tags[1].Sector != "technology" {
		t.Errorf("tags = %+v", rows.Tags)
	}
	if len(rows.Custody) != 1 || rows.Custody[0].Custodian != "Example Bank AG" {
		t.Errorf("custody = %+v", rows.Custody)
	}
	if rows.Settings != (canonical.ExposureSettings{MaxGapDays: 200, AgedDays: canonical.DefaultAgedDays,
		PartialPct: canonical.DefaultPartialPct}) {
		t.Errorf("settings = %+v", rows.Settings)
	}
}

// Without the block, gold still gets the default settings.
func TestExposureRowsDefaults(t *testing.T) {
	c := &Config{}
	rows := c.ExposureRows()
	if rows.Settings.MaxGapDays != 400 || len(rows.Proxies)+len(rows.Tags)+len(rows.Custody)+len(rows.ReferenceFunds) != 0 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestExposureRejects(t *testing.T) {
	for block, want := range map[string]string{
		`{"proxies": {"XX0000000001": {"fund": "isin:XX0000000002"}}}`:                       "want isin:",
		`{"proxies": {"isin:XX0000000001": {"fund": "isin:bad"}}}`:                           "not an ISIN",
		`{"proxies": {"isin:XX0000000001": {"fund": "isin:xx0000000001"}}}`:                  "its own proxy",
		`{"proxies": {"symbol:A": {"fund": "symbol:B"}, "symbol:a ": {"fund": "symbol:C"}}}`: "name the same fund",
		`{"proxies": {"symbol:A": {"fund": "symbol:B"}, "symbol:B": {"fund": "symbol:C"}}}`:  "a proxy of its own",
		`{"proxies": {"symbol:A": {"fund": "symbol:B", "leverage": 0}}}`:                     "non-zero factor",
		`{"proxies": {"symbol:A": {"fund": "symbol:B", "leverage": 25}}}`:                    "non-zero factor",
		`{"reference_funds": ["symbol:A", "symbol:a"]}`:                                      "listed twice",
		`{"tags": {"nope": {"x": {"country": "CH"}}}}`:                                       "no silver_sources[].id matches",
		`{"tags": {"manual": {"x": {}}}}`:                                                    "at least one of",
		`{"tags": {"manual": {"x": {"sector": "tech"}}}}`:                                    "sector \"tech\" is not one of",
		`{"tags": {"manual": {"x": {"country": "ch"}}}}`:                                     "not an ISO 3166",
		`{"tags": {"manual": {"x": {"country": "QQ"}}}}`:                                     "not an ISO 3166",
		`{"tags": {"manual": {"x": {"currency": "chf"}}}}`:                                   "ISO 4217",
		`{"custody": {"bank": {"custodian": " "}}}`:                                          "custodian is required",
		`{"custody": {"nope": {"custodian": "X"}}}`:                                          "no silver_sources[].id matches",
		`{"max_gap_days": -1}`:                                                               "max_gap_days",
	} {
		if _, err := exposureConfig(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", block, err, want)
		}
	}
}
