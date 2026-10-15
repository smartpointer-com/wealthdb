package config

import (
	"fmt"
	"math"
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
	t.Parallel()
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
	c.Exposure.Declarations = DeclarationsPath(writeLedger(t,
		`{"silver_source_id":"manual","instrument":"pe-fund-1","sector":{"technology":100},"country":{"US":100}}`+"\n"+
			`{"silver_source_id":"manual","instrument":"house-1","country":{"CH":100},"note":"the house"}`+"\n"))
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
	if d := rows.Declarations; len(d) != 2 || d[0].Line != 1 || d[1].InstrumentExternalID != "house-1" ||
		d[1].Note != "the house" || strings.Join(d[0].Dimensions, ",") != "country,sector" ||
		d[0].Leaves[0] != (canonical.ExposureDeclaredLeaf{Share: 1, Country: "US", Sector: "technology"}) {
		t.Errorf("declarations = %+v", d)
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
// path that names no file holds no declarations.
func TestExposureRowsDefaults(t *testing.T) {
	t.Parallel()
	none := DeclarationsPath(filepath.Join(t.TempDir(), "none.ndjson"))
	for _, c := range []*Config{{}, {Exposure: &ExposureConfig{Declarations: none}}} {
		rows, err := c.ExposureRows()
		if err != nil || rows.Settings.MaxGapDays != canonical.DefaultMaxGapDays ||
			len(rows.Proxies)+len(rows.Declarations)+len(rows.Custody)+len(rows.ReferenceFunds)+len(rows.SizeFunds) != 0 {
			t.Errorf("rows = %+v, err %v", rows, err)
		}
	}
}

func writeLedger(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exposure-declarations.ndjson")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// declarations reads a ledger body through the config of one source,
// manual.
func declarations(t *testing.T, body string) ([]canonical.ExposureDeclaration, error) {
	t.Helper()
	c := &Config{SilverSources: []SilverSource{{ID: "manual"}}, Exposure: &ExposureConfig{
		Declarations: DeclarationsPath(writeLedger(t, body))}}
	rows, err := c.ExposureRows()
	return rows.Declarations, err
}

// leafText is a declaration's leaves, one per line: share, then each
// dimension's value or "-".
func leafText(leaves []canonical.ExposureDeclaredLeaf) string {
	var b strings.Builder
	for _, l := range leaves {
		dash := func(s string) string {
			if s == "" {
				return "-"
			}
			return s
		}
		fmt.Fprintf(&b, "%.6f %s %s %s %s %s\n", l.Share, dash(l.Region), dash(l.Country), dash(l.Currency),
			dash(l.Sector), dash(l.AssetClass))
	}
	return b.String()
}

// The config's path reaches the ledger relative to the config; a record
// keeps its date, source and note, and its dimensions in a fixed order.
func TestDeclarationsLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "declarations.ndjson"), []byte("\ufeff"+
		`{"silver_source_id":"manual","instrument":"house-1","currency":{"CHF":100},"country":{"CH":100}}`+"\n\n"+
		`{"silver_source_id":"manual","instrument":"fund-1","as_of":"2024-06-30","region":{"europe":100},`+
		`"source":{"document": "factsheet", "page": 2},"note":"stated as ~100%"}`+"\n"+
		`{"silver_source_id":"manual","instrument":"fund-1","as_of":"2025-06-30","region":{"europe":100}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"fund-1","region":{"rest":100}}`+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfgPath, []byte(`{"gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "manual", "kind": "manual", "path": "/tmp/m.db"}],
        "exposure": {"declarations": "declarations.ndjson"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.ExposureRows()
	if err != nil {
		t.Fatal(err)
	}
	d := rows.Declarations
	if len(d) != 4 {
		t.Fatalf("declarations = %+v", d)
	}
	if d[0].Line != 1 || d[0].AsOf != nil || strings.Join(d[0].Dimensions, ",") != "country,currency" ||
		leafText(d[0].Leaves) != "1.000000 - CH CHF - -\n" {
		t.Errorf("the house: %+v", d[0])
	}
	if d[1].Line != 3 || d[1].AsOf == nil || *d[1].AsOf != 1719705600 || d[1].Source != `{"document":"factsheet","page":2}` ||
		d[1].Note != "stated as ~100%" {
		t.Errorf("the dated record: %+v", d[1])
	}
	if d[3].Line != 5 || leafText(d[3].Leaves) != "1.000000 other - - - -\n" {
		t.Errorf("rest reads other: %+v", d[3])
	}
}

// The leaves are the cross product of the declared dimensions, each
// padded with its unclassified part, so their shares sum to one.
func TestDeclarationLeaves(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]string{
		// A split of three regions by two classes, one currency: six leaves.
		`"region":{"north_america":55,"europe":30,"rest":15},"asset_class":{"private_equity":72,"private_debt":28},` +
			`"currency":{"USD":100}`: "0.396000 north_america - USD - private_equity\n" +
			"0.154000 north_america - USD - private_debt\n" +
			"0.216000 europe - USD - private_equity\n" +
			"0.084000 europe - USD - private_debt\n" +
			"0.108000 other - USD - private_equity\n" +
			"0.042000 other - USD - private_debt\n",
		// What the shares leave of 100 is unclassified; a share of zero
		// states nothing.
		`"sector":{"technology":33.3,"health_care":33.3,"financials":33.3,"energy":0}`: "0.333000 - - - technology -\n" +
			"0.333000 - - - health_care -\n0.333000 - - - financials -\n0.001000 - - - unclassified -\n",
		// What they take beyond it too, as a negative part.
		`"country":{"US":60.2,"unclassified":40.1}`: "0.602000 - US - - -\n0.398000 - unclassified - - -\n",
		`"country":{"US":50.2,"CA":50.1}`:           "0.502000 - US - - -\n0.501000 - CA - - -\n-0.003000 - unclassified - - -\n",
	} {
		d, err := declarations(t, `{"silver_source_id":"manual","instrument":"x",`+body+"}\n")
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if got := leafText(d[0].Leaves); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", body, got, want)
		}
		sum := 0.0
		for _, l := range d[0].Leaves {
			sum += l.Share
		}
		if math.Abs(sum-1) > 1e-12 {
			t.Errorf("%s: the shares sum to %v", body, sum)
		}
	}
}

func TestDeclarationRejects(t *testing.T) {
	t.Parallel()
	rec := func(body string) string { return `{"silver_source_id":"manual","instrument":"x",` + body + "}\n" }
	for body, want := range map[string]string{
		rec(`"contry":{"CH":100}`):                       `line 1: unknown key "contry"`,
		rec(`"country":{"CH":100},"country":{"CH":100}`): `key "country" twice`,
		rec(`"country":{"CH":60,"CH":40}`):               `country: key "CH" twice`,
		`{"silver_source_id":"manual",` + "\n" + `"instrument":"x","country":{"CH":100}}` + "\n": "line 1: not JSON",
		`["manual","x"]` + "\n": "want a JSON object",
		rec(`"country":{"CH":100}`) + `{"silver_source_id":"manual","instrument":"y"} {}` + "\n": "line 2: something follows",
		rec(`"note":"no dimension"`):                                               "line 1: a record declares at least one of",
		rec(`"region":{"europe":100},"country":{"CH":100}`):                        "region and country are one dimension",
		rec(`"region":{"europe":60,"rest":39.4}`):                                  "region: the shares sum to 99.4%, want 100",
		rec(`"region":{"europe":60,"rest":40.6}`):                                  "region: the shares sum to 100.6%",
		rec(`"region":{"europe":60,"rest":39.499}`):                                "region: the shares sum to 99.499%",
		rec(`"region":{}`):                                                         "region: the shares sum to 0%",
		rec(`"region":{"europe":"55-65","rest":40}`):                               `region: europe: a share is a number in percent, not "55-65"`,
		rec(`"region":{"europe":-10,"rest":110}`):                                  "a share is a number from 0 to 100, not -10",
		rec(`"region":{"europe":150}`):                                             "not 150",
		rec(`"region":"europe"`):                                                   "region: want a JSON object",
		rec(`"region":{"other":100}`):                                              `"other" is not a region`,
		rec(`"region":{"Europe":100}`):                                             `"Europe" is not a region`,
		rec(`"country":{"ch":100}`):                                                "not an ISO 3166",
		rec(`"country":{"QQ":100}`):                                                "not an ISO 3166",
		rec(`"currency":{"chf":100}`):                                              "ISO 4217",
		rec(`"sector":{"tech":100}`):                                               `"tech" is not one of`,
		rec(`"asset_class":{"private_fund":100}`):                                  `"private_fund" is not one of`,
		rec(`"as_of":"2024-6-30","country":{"CH":100}`):                            `as_of "2024-6-30" is not a date`,
		rec(`"as_of":20240630,"country":{"CH":100}`):                               "as_of is a string",
		rec(`"note":7,"country":{"CH":100}`):                                       "note is a string",
		`{"silver_source_id":"nope","instrument":"x","country":{"CH":100}}` + "\n": `silver_source_id "nope"`,
		`{"silver_source_id":"manual","country":{"CH":100}}` + "\n":                "silver_source_id and instrument are required",
		rec(`"country":{"CH":100}`) + rec(`"currency":{"CHF":100}`):                "line 2: manual x is declared without a date on line 1 already",
		rec(`"as_of":"2024-06-30","country":{"CH":100}`) + rec(`"as_of":"2024-06-30","country":{"DE":100}`): "line 2: manual x is declared as of 2024-06-30 on line 1",
	} {
		if _, err := declarations(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", body, err, want)
		}
	}
	// The path is named, and a record split over two lines says how a
	// record is written.
	_, err := declarations(t, `{"silver_source_id":"manual",`+"\n"+`"instrument":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "exposure.declarations: ") ||
		!strings.Contains(err.Error(), "one JSON object on one line") {
		t.Errorf("split record: %v", err)
	}
}

// The tags key the declarations ledger replaces is refused with where it
// went, and the ledger's path takes a string alone.
func TestDeclarationsConfigKey(t *testing.T) {
	t.Parallel()
	for block, want := range map[string]string{
		`{"tags": "overrides/exposure_tags.csv"}`:       "the declarations ledger replaces the tags ledger",
		`{"declarations": {"manual": {"house-1": {}}}}`: "path to the declarations ledger",
	} {
		if _, err := exposureConfig(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", block, err, want)
		}
	}
}

func TestExposureRejects(t *testing.T) {
	t.Parallel()
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
		`{"custody": {"bank": {"custodian": "X", "country": "ch"}}}`:                         "not an ISO 3166",
		`{"custody": {"bank": {"custodian": "X", "country": "QQ"}}}`:                         "not an ISO 3166",
		`{"reference_funds": ["XX1"]}`:                                                       "reference_funds[0]",
		`{"max_gap_days": -1}`:                                                               "max_gap_days",
		`{"max_gap_days": 4000}`:                                                             "max_gap_days",
	} {
		if _, err := exposureConfig(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", block, err, want)
		}
	}
}
