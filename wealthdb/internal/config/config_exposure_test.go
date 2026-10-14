package config

import (
	"os"
	"path/filepath"
	"slices"
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
        "reference_funds": ["symbol:idx9", "isin:XX0000000005"],
        "size_funds": {"symbol:all9": 100, "isin:XX0000000006": 85},
        "custody": {"bank": {"custodian": " Example Bank AG ", "country": "CH"}},
        "max_gap_days": 200
    }`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c.Exposure.Tags = TagsPath(writeLedger(t, "silver_source_id,instrument,country,currency,sector,note\n"+
		"manual,pe-fund-1,US,USD,technology,\nmanual,house-1,CH,CHF,,the house\n"))
	rows, err := c.ExposureRows()
	if err != nil {
		t.Fatal(err)
	}
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
	if strings.Join(rows.ReferenceFunds, ",") != "isin:XX0000000005,symbol:IDX9" {
		t.Errorf("reference funds = %v", rows.ReferenceFunds)
	}
	if want := []canonical.ExposureSizeFund{{FundRef: "isin:XX0000000006", CoveragePct: 85},
		{FundRef: "symbol:ALL9", CoveragePct: 100}}; !slices.Equal(rows.SizeFunds, want) {
		t.Errorf("size funds = %+v, want %+v", rows.SizeFunds, want)
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

// Without the block, gold still gets the default settings; a ledger
// path that names no file holds no tags.
func TestExposureRowsDefaults(t *testing.T) {
	for _, c := range []*Config{{}, {Exposure: &ExposureConfig{Tags: TagsPath(filepath.Join(t.TempDir(), "none.csv"))}}} {
		rows, err := c.ExposureRows()
		if err != nil || rows.Settings.MaxGapDays != 400 ||
			len(rows.Proxies)+len(rows.Tags)+len(rows.Custody)+len(rows.ReferenceFunds) != 0 {
			t.Errorf("rows = %+v, err %v", rows, err)
		}
	}
}

func writeLedger(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exposure-tags.csv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The config's path reaches the ledger relative to the config, and a row
// the ledger refuses names its line.
func TestTagLedger(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tags.csv"),
		[]byte("instrument, silver_source_id ,country\nhouse-1,manual,CH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfgPath, []byte(`{"gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "manual", "kind": "manual", "path": "/tmp/m.db"}],
        "exposure": {"tags": "tags.csv"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.ExposureRows()
	if err != nil || len(rows.Tags) != 1 || rows.Tags[0].InstrumentExternalID != "house-1" || rows.Tags[0].Country != "CH" {
		t.Fatalf("tags = %+v, err %v", rows.Tags, err)
	}
	// A spreadsheet's byte-order mark and an empty trailing header cell
	// are no columns.
	c.Exposure.Tags = TagsPath(writeLedger(t, "\ufeffsilver_source_id,instrument,country,\nmanual,house-1,CH,\n"))
	if rows, err := c.ExposureRows(); err != nil || len(rows.Tags) != 1 {
		t.Errorf("BOM and trailing cell: %+v, %v", rows.Tags, err)
	}
	// The map form of tags is refused with what the field takes.
	if err := os.WriteFile(cfgPath, []byte(`{"gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "manual", "kind": "manual", "path": "/tmp/m.db"}],
        "exposure": {"tags": {"manual": {"house-1": {"country": "CH"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil || !strings.Contains(err.Error(), "path to the tags ledger") {
		t.Errorf("map form: %v", err)
	}
	head := "silver_source_id,instrument,country,currency,sector\n"
	for body, want := range map[string]string{
		"silver_source_id,instrument,contry\n":          "unknown column \"contry\"",
		"silver_source_id,instrument,country,country\n": "column \"country\" twice",
		"silver_source_id,country\nmanual,CH\n":         "column \"instrument\" is required",
		head + "nope,x,CH,,\n":                          "line 2: silver_source_id \"nope\"",
		head + "manual,,CH,,\n":                         "line 2: silver_source_id and instrument are required",
		head + "\nmanual,x,,,\n":                        "line 3: at least one of",
		head + "manual,x,,,tech\n":                      "sector \"tech\" is not one of",
		head + "manual,x,ch,,\n":                        "not an ISO 3166",
		head + "manual,x,QQ,,\n":                        "not an ISO 3166",
		head + "manual,x,,chf,\n":                       "ISO 4217",
		head + "manual,x,CH,,\nmanual,x,,CHF,\n":        "line 3: manual x is tagged on line 2 already",
	} {
		c.Exposure.Tags = TagsPath(writeLedger(t, body))
		if _, err := c.ExposureRows(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", body, err, want)
		}
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
		`{"size_funds": {"symbol:A": 100, "symbol:a": 85}}`:                                  "name the same fund",
		`{"size_funds": {"symbol:A": 40}}`:                                                   "50 to 100",
		`{"size_funds": {"A": 100}}`:                                                         "want isin:",
		`{"custody": {"bank": {"custodian": " "}}}`:                                          "custodian is required",
		`{"custody": {"nope": {"custodian": "X"}}}`:                                          "no silver_sources[].id matches",
		`{"max_gap_days": -1}`:                                                               "max_gap_days",
	} {
		if _, err := exposureConfig(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", block, err, want)
		}
	}
}
