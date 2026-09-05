package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/wealthdb.db",
        "default_currency": "USD",
        "silver_sources": [
            {"id": "schwab-retail", "kind": "schwab", "path": "/tmp/schwab-api.db"},
            {"id": "ubs-main",      "kind": "ubs",    "path": "/tmp/ubs-psn.db"}
        ]
    }`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GoldDB != "/tmp/wealthdb.db" {
		t.Errorf("gold_db = %q", c.GoldDB)
	}
	if c.DefaultCurrency != "USD" {
		t.Errorf("default_currency = %q", c.DefaultCurrency)
	}
	if len(c.SilverSources) != 2 {
		t.Errorf("silver_sources len = %d, want 2", len(c.SilverSources))
	}
	if got, ok := c.Lookup("ubs-main"); !ok || got.Kind != "ubs" {
		t.Errorf("Lookup(ubs-main) = %v, %v", got, ok)
	}
	if _, ok := c.Lookup("nope"); ok {
		t.Error("Lookup(nope) should return false")
	}
}

func TestLoadInceptionOverrides(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/wealthdb.db",
        "default_currency": "USD",
        "silver_sources": [{"id": "cointracking", "kind": "cointracking", "path": "/tmp/ct.db"}],
        "inception_overrides": {
            "sources":    {"cointracking": "2017-07-01"},
            "portfolios": {"cointracking": {"cu_000001": "2019-09-24"}},
            "accounts":   {"cointracking": {"WALLET1": "2020-01-01"}}
        }
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.InceptionOverrides == nil {
		t.Fatal("InceptionOverrides nil")
	}
	s, p, a := c.InceptionOverrides.Epochs()
	wantS, _ := parseYYYYMMDD("2017-07-01")
	wantP, _ := parseYYYYMMDD("2019-09-24")
	wantA, _ := parseYYYYMMDD("2020-01-01")
	if s["cointracking"] != wantS {
		t.Errorf("sources epoch = %d, want %d", s["cointracking"], wantS)
	}
	if p["cointracking"]["cu_000001"] != wantP {
		t.Errorf("portfolios epoch = %d, want %d", p["cointracking"]["cu_000001"], wantP)
	}
	if a["cointracking"]["WALLET1"] != wantA {
		t.Errorf("accounts epoch = %d, want %d", a["cointracking"]["WALLET1"], wantA)
	}
}

func TestLoadReturnsExclude(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/wealthdb.db",
        "default_currency": "USD",
        "silver_sources": [{"id": "cointracking", "kind": "cointracking", "path": "/tmp/ct.db"}],
        "returns_exclude": {
            "portfolios": {"cointracking": ["cu_000001", "cu_000002"]},
            "accounts":   {"cointracking": ["WALLET1"]}
        }
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pf, ac := c.ReturnsExclude.Sets()
	if !pf["cointracking"]["cu_000001"] || !pf["cointracking"]["cu_000002"] {
		t.Errorf("portfolio exclude set missing entries: %v", pf)
	}
	if pf["cointracking"]["cu_999"] {
		t.Error("unexpected portfolio in exclude set")
	}
	if !ac["cointracking"]["WALLET1"] {
		t.Errorf("account exclude set missing entry: %v", ac)
	}

	// rejects: unknown source + empty id
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"cointracking","kind":"cointracking","path":"/tmp/ct.db"}],`
	for name, block := range map[string]string{
		"unknown source": `"returns_exclude":{"portfolios":{"nope":["P"]}}}`,
		"empty id":       `"returns_exclude":{"accounts":{"cointracking":[""]}}}`,
	} {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

// TestValidateSpendingAccountScope pins the two rejections the
// spending account scope needs that the shared id-list check cannot
// make. Both would otherwise surface as a spend_account_scope
// primary-key violation mid-load — after every source has been
// written — with no message naming the offending entry.
//
// The narrowing is deliberate: a repeated id in returns_exclude is
// harmless (those lists fold into sets) and must keep loading.
func TestValidateSpendingAccountScope(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"bank","kind":"chase","path":"/tmp/b.db"}],`
	for name, block := range map[string]string{
		"duplicate in include": `"spending":{"accounts":{"include":{"bank":["ACCT0001","ACCT0001"]}}}}`,
		"duplicate in exclude": `"spending":{"accounts":{"exclude":{"bank":["ACCT0002","ACCT0002"]}}}}`,
		"both sides":           `"spending":{"accounts":{"include":{"bank":["ACCT0003"]},"exclude":{"bank":["ACCT0003"]}}}}`,
	} {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}

	// A unique list on both sides loads.
	ok := `"spending":{"accounts":{"include":{"bank":["ACCT0001","ACCT0002"]},"exclude":{"bank":["ACCT0003"]}}}}`
	if _, err := Load(writeConfig(t, base+ok)); err != nil {
		t.Errorf("a unique account scope must load: %v", err)
	}

	// The same repeat in returns_exclude stays legal.
	dup := `"returns_exclude":{"accounts":{"bank":["ACCT0001","ACCT0001"]}}}`
	if _, err := Load(writeConfig(t, base+dup)); err != nil {
		t.Errorf("returns_exclude folds its list into a set; a repeat must still load: %v", err)
	}
}

func TestLoadInceptionOverridesRejects(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"cointracking","kind":"cointracking","path":"/tmp/ct.db"}],`
	cases := map[string]string{
		"bad source date":       `"inception_overrides":{"sources":{"cointracking":"2019-13-99"}}}`,
		"unknown source":        `"inception_overrides":{"sources":{"nope":"2019-01-01"}}}`,
		"unknown nested source": `"inception_overrides":{"portfolios":{"nope":{"P":"2019-01-01"}}}}`,
		"empty inner key":       `"inception_overrides":{"accounts":{"cointracking":{"":"2019-01-01"}}}}`,
		"bad nested date":       `"inception_overrides":{"accounts":{"cointracking":{"A":"not-a-date"}}}}`,
	}
	for name, block := range cases {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestLoadExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no $HOME")
	}
	path := writeConfig(t, `{
        "gold_db": "~/wealthdb.db",
        "default_currency": "USD",
        "silver_sources": []
    }`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, "wealthdb.db")
	if c.GoldDB != want {
		t.Errorf("gold_db = %q, want %q", c.GoldDB, want)
	}
}

func TestLoadExpandsHomeEnv(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no $HOME")
	}
	path := writeConfig(t, `{
        "gold_db": "$HOME/wealthdb.db",
        "default_currency": "USD",
        "silver_sources": []
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GoldDB != filepath.Join(home, "wealthdb.db") {
		t.Errorf("gold_db = %q", c.GoldDB)
	}
}

func TestLoadResolvesRelative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(path, []byte(`{
        "gold_db": "./gold.db",
        "default_currency": "USD",
        "silver_sources": [{"id":"x","kind":"schwab","path":"./silver/x.db"}]
    }`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GoldDB != filepath.Join(dir, "gold.db") {
		t.Errorf("gold_db = %q, want under %q", c.GoldDB, dir)
	}
	if c.SilverSources[0].Path != filepath.Join(dir, "silver", "x.db") {
		t.Errorf("silver path = %q", c.SilverSources[0].Path)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [], "wibble": true
    }`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func TestValidateRejectsBadID(t *testing.T) {
	c := &Config{
		GoldDB: "/x", DefaultCurrency: "USD",
		SilverSources: []SilverSource{{ID: "bad id", Kind: "schwab", Path: "/x"}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("err = %v, want pattern complaint", err)
	}
}

func TestValidateRejectsDuplicateID(t *testing.T) {
	c := &Config{
		GoldDB: "/x", DefaultCurrency: "USD",
		SilverSources: []SilverSource{
			{ID: "a", Kind: "schwab", Path: "/x"},
			{ID: "a", Kind: "ubs", Path: "/y"},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate complaint", err)
	}
}

func TestLoadParsesAccountOverrides(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [
            {"id": "schwab-main", "kind": "schwab", "path": "/tmp/s.db"},
            {"id": "swissquote",  "kind": "swissquote", "path": "/tmp/q.db"}
        ],
        "account_overrides": {
            "schwab-main": {
                "1A2B3C4D": {"nickname": "Main brokerage", "category": "personal"},
                "5E6F7G8H": {"nickname": "ESA One", "category": "esa"}
            },
            "swissquote": {
                "1234567": {"nickname": "CHF trading"}
            }
        }
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.AccountOverrides["schwab-main"]["1A2B3C4D"]
	if got.Nickname != "Main brokerage" || got.Category != "personal" {
		t.Errorf("schwab-main/1A2B3C4D = %+v", got)
	}
	gotSQ := c.AccountOverrides["swissquote"]["1234567"]
	if gotSQ.Nickname != "CHF trading" || gotSQ.Category != "" {
		t.Errorf("swissquote/1234567 = %+v (Category should be empty)", gotSQ)
	}
}

func TestValidateRejectsOrphanOverride(t *testing.T) {
	c := &Config{
		GoldDB: "/x", DefaultCurrency: "USD",
		SilverSources: []SilverSource{{ID: "a", Kind: "schwab", Path: "/x"}},
		AccountOverrides: map[string]map[string]AccountOverride{
			"unknown-source": {"ACC": {Nickname: "x"}},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown-source") {
		t.Fatalf("err = %v, want orphan-source complaint", err)
	}
}

func TestValidateRejectsEmptyOverride(t *testing.T) {
	c := &Config{
		GoldDB: "/x", DefaultCurrency: "USD",
		SilverSources: []SilverSource{{ID: "a", Kind: "schwab", Path: "/x"}},
		AccountOverrides: map[string]map[string]AccountOverride{
			"a": {"ACC": {}}, // both fields empty
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("err = %v, want both-empty complaint", err)
	}
}

func TestInstrumentOverrides(t *testing.T) {
	base := func() *Config {
		return &Config{
			GoldDB: "/x", DefaultCurrency: "USD",
			SilverSources: []SilverSource{{ID: "a", Kind: "schwab", Path: "/x"}},
		}
	}
	t.Run("accepts valid pair", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"a": {"GLD": {AssetClass: "metal", Vehicle: "etf"}},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("rejects orphan source", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"unknown-source": {"GLD": {AssetClass: "metal", Vehicle: "etf"}},
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "unknown-source") {
			t.Fatalf("err = %v, want orphan-source complaint", err)
		}
	})
	t.Run("rejects missing vehicle", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"a": {"GLD": {AssetClass: "metal"}},
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must both be set") {
			t.Fatalf("err = %v, want both-set complaint", err)
		}
	})
	t.Run("rejects legacy value in asset_class", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"a": {"GLD": {AssetClass: "etf", Vehicle: "etf"}},
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "invalid asset_class") {
			t.Fatalf("err = %v, want invalid-asset_class complaint", err)
		}
	})
	t.Run("rejects invalid vehicle", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"a": {"GLD": {AssetClass: "metal", Vehicle: "spaceship"}},
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "invalid vehicle") {
			t.Fatalf("err = %v, want invalid-vehicle complaint", err)
		}
	})
	t.Run("rejects nonsensical pair", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"a": {"GLD": {AssetClass: "crypto", Vehicle: "mortgage"}},
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "not an admitted taxonomy pair") {
			t.Fatalf("err = %v, want admitted-pair complaint", err)
		}
	})
	t.Run("rejects empty instrument key", func(t *testing.T) {
		c := base()
		c.InstrumentOverrides = map[string]map[string]InstrumentOverride{
			"a": {"": {AssetClass: "metal", Vehicle: "etf"}},
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "empty instrument_external_id") {
			t.Fatalf("err = %v, want empty-key complaint", err)
		}
	})
}

func TestValidateSymbolOverrides(t *testing.T) {
	base := func() *Config {
		return &Config{
			GoldDB: "/x", DefaultCurrency: "USD",
			SilverSources: []SilverSource{{ID: "ubs", Kind: "ubs", Path: "/y"}},
		}
	}
	t.Run("accepts upsert", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "CH0000000020", Symbol: "NOVN"},
		}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("accepts delete", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "XD0000000001", Delete: true},
		}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rejects unknown source", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "binance", LookupKind: "name", LookupValue: "BTC", Symbol: "BTC"},
		}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "binance") {
			t.Fatalf("err = %v, want source-not-found complaint", err)
		}
	})
	t.Run("rejects unknown lookup_kind", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "ubs", LookupKind: "isin", LookupValue: "CH0000000020", Symbol: "NOVN"},
		}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "lookup_kind") {
			t.Fatalf("err = %v, want lookup_kind complaint", err)
		}
	})
	t.Run("rejects bad symbol shape", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "CH0000000020", Symbol: "not a ticker"},
		}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "symbol") {
			t.Fatalf("err = %v, want symbol-shape complaint", err)
		}
	})
	t.Run("rejects delete + symbol mutually exclusive", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "ubs", LookupKind: "name", LookupValue: "X", Symbol: "X", Delete: true},
		}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v, want delete-symbol-conflict complaint", err)
		}
	})
	t.Run("rejects duplicate tuple", func(t *testing.T) {
		c := base()
		c.SymbolResolution = &SymbolResolutionConfig{Overrides: []SymbolOverride{
			{SilverSourceID: "ubs", LookupKind: "name", LookupValue: "FOO", Symbol: "F"},
			{SilverSourceID: "ubs", LookupKind: "name", LookupValue: "FOO", Symbol: "G"},
		}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("err = %v, want duplicate complaint", err)
		}
	})
}

func TestValidateRejectsBadCurrency(t *testing.T) {
	cases := []string{"", "usd", "DOLLAR", "US", "USDX"}
	for _, ccy := range cases {
		c := &Config{GoldDB: "/x", DefaultCurrency: ccy}
		if err := c.Validate(); err == nil {
			t.Errorf("currency %q should fail validation", ccy)
		}
	}
}

func TestLoadParsesWebBlock(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [],
        "web": {"enabled": true, "port": 4444}
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Web == nil || !c.Web.Enabled || c.Web.Port != 4444 {
		t.Errorf("web = %+v, want {enabled:true port:4444}", c.Web)
	}
}

func TestLoadWebOmittedIsNil(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": []
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Web != nil {
		t.Errorf("web = %+v, want nil when omitted", c.Web)
	}
}

func TestValidateRejectsBadWebPort(t *testing.T) {
	c := &Config{
		GoldDB: "/x", DefaultCurrency: "USD",
		Web: &WebConfig{Enabled: true, Port: 70000},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "web.port") {
		t.Fatalf("err = %v, want web.port range complaint", err)
	}
}

func TestLoadReturnsPolicyOverrides(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"carta","kind":"carta","path":"/tmp/c.db"},{"id":"ct","kind":"cointracking","path":"/tmp/ct.db"},{"id":"mx","kind":"manual","path":"/tmp/m.db"}],`
	c, err := Load(writeConfig(t, base+`"returns_policy_overrides":{"carta":{"flow_regime":"nav_only"},"ct":{"accounts_grain":"normal"},"mx":{}}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, ok := c.ReturnsPolicyOverrides["carta"].Regime(); !ok || r != returns.RegimeNavOnly {
		t.Errorf("carta Regime() = %v, %v; want nav_only, true", r, ok)
	}
	ct := c.ReturnsPolicyOverrides["ct"]
	if _, ok := ct.Regime(); ok {
		t.Error("ct sets no flow_regime; Regime() must report unset")
	}
	if m, ok := ct.AccountsGrainMode(); !ok || m != returns.AccountsGrainNormal {
		t.Errorf("ct AccountsGrainMode() = %v, %v; want normal, true", m, ok)
	}
	// An empty override object is accepted as a no-op.
	if mx, ok := c.ReturnsPolicyOverrides["mx"]; !ok || mx == nil {
		t.Errorf("mx empty override must parse as a present no-op, got %v, %v", mx, ok)
	}
	// The nil receiver reports unset.
	if _, ok := c.ReturnsPolicyOverrides["absent"].Regime(); ok {
		t.Error("nil override must report no regime")
	}
}

func TestLoadReturnsPolicyOverridesRejects(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"carta","kind":"carta","path":"/tmp/c.db"}],`
	cases := map[string]string{
		"unknown source": `"returns_policy_overrides":{"nope":{"flow_regime":"nav_only"}}}`,
		"bad regime":     `"returns_policy_overrides":{"carta":{"flow_regime":"freeform"}}}`,
		"bad grain mode": `"returns_policy_overrides":{"carta":{"accounts_grain":"invisible"}}}`,
	}
	for name, block := range cases {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestLoadReturnsHide(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"sq","kind":"swissquote","path":"/tmp/sq.db"}],`
	c, err := Load(writeConfig(t, base+`"returns_hide":{"accounts":{"sq":["A1","A2"]},"portfolios":{"sq":["P1"]}}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pf, ac := c.ReturnsHide.Sets()
	if !ac["sq"]["A1"] || !ac["sq"]["A2"] || !pf["sq"]["P1"] {
		t.Errorf("Sets() = %v, %v; want the listed ids", pf, ac)
	}
	// The nil receiver returns nil sets.
	var nilHide *ReturnsHide
	if p2, a2 := nilHide.Sets(); p2 != nil || a2 != nil {
		t.Error("nil ReturnsHide must yield nil sets")
	}

	cases := map[string]string{
		"unknown source": `"returns_hide":{"accounts":{"nope":["A"]}}}`,
		"empty inner id": `"returns_hide":{"accounts":{"sq":[""]}}}`,
	}
	for name, block := range cases {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestLoadReturnsTransferMatching(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"sq","kind":"swissquote","path":"/tmp/sq.db"}],`

	// Enabled with explicit knobs.
	c, err := Load(writeConfig(t, base+`"returns_transfer_matching":{"enabled":true,"window_days":3,"tolerance_pct":1.5}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := c.ReturnsTransferMatching
	if m == nil || !m.Enabled || m.Window() != 3 || m.Tolerance() != 1.5 {
		t.Errorf("explicit knobs: got %+v (window %d, tol %g)", m, m.Window(), m.Tolerance())
	}

	// Enabled with knobs omitted: the documented defaults apply.
	c2, err := Load(writeConfig(t, base+`"returns_transfer_matching":{"enabled":true}}`))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	m2 := c2.ReturnsTransferMatching
	if m2.Window() != DefaultTransferMatchWindowDays || m2.Tolerance() != DefaultTransferMatchTolerancePct {
		t.Errorf("defaults: window %d tol %g", m2.Window(), m2.Tolerance())
	}

	// Absent block: nil (feature off).
	c3, err := Load(writeConfig(t, base[:len(base)-1]+`}`))
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if c3.ReturnsTransferMatching != nil {
		t.Error("absent block must stay nil")
	}

	// Nil-receiver accessors report the defaults (safe on the absent block).
	var nilM *ReturnsTransferMatching
	if nilM.Window() != DefaultTransferMatchWindowDays || nilM.Tolerance() != DefaultTransferMatchTolerancePct {
		t.Error("nil receiver must yield the defaults")
	}

	// Out-of-range knobs fail the load — even when the block is disabled.
	cases := map[string]string{
		"window too wide":    `"returns_transfer_matching":{"enabled":true,"window_days":31}}`,
		"window negative":    `"returns_transfer_matching":{"enabled":true,"window_days":-1}}`,
		"tolerance too big":  `"returns_transfer_matching":{"enabled":true,"tolerance_pct":6}}`,
		"tolerance negative": `"returns_transfer_matching":{"enabled":true,"tolerance_pct":-0.1}}`,
		"disabled but bad":   `"returns_transfer_matching":{"enabled":false,"window_days":99}}`,
	}
	for name, block := range cases {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestLoadSpending(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"sq","kind":"swissquote","path":"/tmp/sq.db"}],`

	c, err := Load(writeConfig(t, base+`"spending":{
        "accounts":{"include":{"sq":["W-1"]},"exclude":{"sq":["C-9"]}},
        "internal_transfer_matching":{"window_days":3,"tolerance_pct":1.5}}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	include, exclude := c.SpendAccountScope()
	if len(include["sq"]) != 1 || include["sq"][0] != "W-1" {
		t.Errorf("include = %v", include)
	}
	if len(exclude["sq"]) != 1 || exclude["sq"][0] != "C-9" {
		t.Errorf("exclude = %v", exclude)
	}
	if m := c.SpendMatching(); m.Window() != 3 || m.Tolerance() != 1.5 {
		t.Errorf("knobs = (window %d, tol %g)", m.Window(), m.Tolerance())
	}

	// Omitted knobs take the defaults, which are deliberately the same
	// as the returns matcher's — one matching core, one banding.
	c2, err := Load(writeConfig(t, base+`"spending":{}}`))
	if err != nil {
		t.Fatalf("Load empty block: %v", err)
	}
	if m := c2.SpendMatching(); m.Window() != DefaultTransferMatchWindowDays ||
		m.Tolerance() != DefaultTransferMatchTolerancePct {
		t.Errorf("defaults = (window %d, tol %g)", m.Window(), m.Tolerance())
	}

	// An absent block is safe on every accessor.
	c3, err := Load(writeConfig(t, base[:len(base)-1]+`}`))
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if c3.Spending != nil {
		t.Error("absent block must stay nil")
	}
	if include, exclude := c3.SpendAccountScope(); include != nil || exclude != nil {
		t.Error("absent block must scope nothing")
	}
	if m := c3.SpendMatching(); m.Window() != DefaultSpendMatchWindowDays {
		t.Errorf("absent block window = %d", m.Window())
	}

	cases := map[string]string{
		"unknown source":     `"spending":{"accounts":{"include":{"nope":["A"]}}}}`,
		"empty account id":   `"spending":{"accounts":{"exclude":{"sq":[""]}}}}`,
		"both sides at once": `"spending":{"accounts":{"include":{"sq":["A"]},"exclude":{"sq":["A"]}}}}`,
		"window too wide":    `"spending":{"internal_transfer_matching":{"window_days":31}}}`,
		"window negative":    `"spending":{"internal_transfer_matching":{"window_days":-1}}}`,
		"tolerance too big":  `"spending":{"internal_transfer_matching":{"tolerance_pct":6}}}`,
		"tolerance negative": `"spending":{"internal_transfer_matching":{"tolerance_pct":-0.1}}}`,
	}
	for name, block := range cases {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestLoadSpendingCategorization(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"sq","kind":"swissquote","path":"/tmp/sq.db"}],`

	c, err := Load(writeConfig(t, base+`"spending":{"categorization":{
        "model":{"baseUrl":"http://127.0.0.1:1234/v1","api":"openai-completions","name":"a-model"},
        "context":"descriptor",
        "descriptor_samples":5}}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cz := c.SpendCategorization()
	if cz == nil {
		t.Fatal("categorization block must parse")
	}
	if m := cz.CategorizationModel(); m == nil || m.Name != "a-model" {
		t.Errorf("model = %+v", m)
	}
	if cz.ContextLevel() != SpendContextDescriptor {
		t.Errorf("context = %q", cz.ContextLevel())
	}
	if cz.Samples() != 5 {
		t.Errorf("descriptor_samples = %d", cz.Samples())
	}

	// The default is the most private level, and an empty block must
	// resolve to it rather than to anything wider.
	c2, err := Load(writeConfig(t, base+`"spending":{"categorization":{}}}`))
	if err != nil {
		t.Fatalf("Load empty categorization: %v", err)
	}
	cz2 := c2.SpendCategorization()
	if cz2.ContextLevel() != SpendContextMerchant {
		t.Errorf("empty block context = %q, want %q", cz2.ContextLevel(), SpendContextMerchant)
	}
	if cz2.Samples() != DefaultSpendDescriptorSamples {
		t.Errorf("empty block samples = %d", cz2.Samples())
	}
	if cz2.CategorizationModel() != nil {
		t.Error("empty block must carry no model")
	}

	// Every accessor is nil-safe, and a config with no spending block
	// at all must still report the private default.
	c3, err := Load(writeConfig(t, base[:len(base)-1]+`}`))
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if c3.SpendCategorization() != nil {
		t.Error("absent block must stay nil")
	}
	if c3.SpendCategorization().ContextLevel() != SpendContextMerchant {
		t.Error("nil receiver must report the default context")
	}
	if c3.SpendCategorization().CategorizationModel() != nil {
		t.Error("nil receiver must report no model")
	}

	cases := map[string]string{
		"unknown context":      `"spending":{"categorization":{"context":"everything"}}}`,
		"near-miss context":    `"spending":{"categorization":{"context":"descriptors"}}}`,
		"samples negative":     `"spending":{"categorization":{"descriptor_samples":-1}}}`,
		"samples out of range": `"spending":{"categorization":{"descriptor_samples":21}}}`,
	}
	for name, block := range cases {
		if _, err := Load(writeConfig(t, base+block)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}

	// The sample cap is range-checked at the default context too, where
	// nothing reads it — a bad value must not lie in wait for the day
	// the context is widened.
	if _, err := Load(writeConfig(t, base+`"spending":{"categorization":{
        "context":"merchant","descriptor_samples":99}}}`)); err == nil {
		t.Error("out-of-range samples must fail even at the merchant context")
	}
}

// TestLoadSpendingRules pins the one deployment-specific input to the
// rule tier: rules compile case-insensitively at load, an empty list is
// the default and marks nothing, and a bad rule fails the load with a
// message that names it by index and text. The category may be any
// value the taxonomy knows — a delta or a vendored consumption
// category, since a rule is the local route for the wires the fence
// keeps from the model — but an unknown string, a known value in the
// wrong case included, is refused. Every pattern here is invented.
func TestLoadSpendingRules(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"sq","kind":"swissquote","path":"/tmp/sq.db"}],`

	c, err := Load(writeConfig(t, base+`"spending":{"rules":[
		{"match":"SAMPLE HOLDER","category":"internal_transfer"},
		{"match":"EXAMPLE VENTURES FUND","category":"investment"},
		{"match":"CASH DESK","category":"cash_withdrawal"},
		{"match":"EXAMPLE LAW OFFICE","category":"GENERAL_SERVICES_CONSULTING_AND_LEGAL"}]}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rules := c.SpendRules()
	if len(rules) != 4 {
		t.Fatalf("compiled %d rules, want 4", len(rules))
	}
	if !rules[0].Match.MatchString("wire to sample holder") || rules[0].Category != "internal_transfer" {
		t.Errorf("rule 0 = %+v, want a case-insensitive internal_transfer rule", rules[0])
	}
	if !rules[1].Match.MatchString("Subscription Example Ventures Fund II") || rules[1].Category != "investment" {
		t.Errorf("rule 1 = %+v, want a case-insensitive investment rule", rules[1])
	}
	// A vendored consumption category is accepted: a wire to a lawyer
	// is fenced from the model, and a rule is its local route.
	if !rules[3].Match.MatchString("SEPA transfer Example Law Office") || rules[3].Category != "GENERAL_SERVICES_CONSULTING_AND_LEGAL" {
		t.Errorf("rule 3 = %+v, want a case-insensitive consumption-category rule", rules[3])
	}
	if rules[0].Match.MatchString("Corner Market") {
		t.Error("a rule must not fire on an unrelated narrative")
	}

	// An empty list, and an absent block, compile to nothing.
	c2, err := Load(writeConfig(t, base+`"spending":{"rules":[]}}`))
	if err != nil {
		t.Fatalf("Load empty list: %v", err)
	}
	if c2.SpendRules() != nil {
		t.Error("an empty list must compile to nil")
	}
	c3, err := Load(writeConfig(t, base[:len(base)-1]+`}`))
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if c3.SpendRules() != nil {
		t.Error("an absent block must yield nil rules")
	}

	// A bad rule fails the load and the error names it, by index and
	// text, so the user can find it in a list of several.
	cases := map[string]struct{ block, want string }{
		"unbalanced paren":  {`"spending":{"rules":[{"match":"SAMPLE (HOLDER","category":"internal_transfer"}]}}`, `rules[0].match "SAMPLE (HOLDER"`},
		"second is bad":     {`"spending":{"rules":[{"match":"SAMPLE HOLDER","category":"internal_transfer"},{"match":"[","category":"investment"}]}}`, `rules[1].match "["`},
		"empty pattern":     {`"spending":{"rules":[{"match":"","category":"internal_transfer"}]}}`, `matches the empty string`},
		"match-all pattern": {`"spending":{"rules":[{"match":".*","category":"internal_transfer"}]}}`, `rules[0].match ".*" matches the empty string`},
		"missing category":  {`"spending":{"rules":[{"match":"SAMPLE HOLDER"}]}}`, `rules[0].category ""`},
		"unknown category":  {`"spending":{"rules":[{"match":"CORNER MARKET","category":"NOT_A_CATEGORY"}]}}`, `rules[0].category "NOT_A_CATEGORY" is not a spend_detailed value`},
		"wrong case":        {`"spending":{"rules":[{"match":"SAMPLE HOLDER","category":"INVESTMENT"}]}}`, `rules[0].category "INVESTMENT" is not a spend_detailed value: case-sensitive`},
	}
	for name, tc := range cases {
		_, err := Load(writeConfig(t, base+tc.block))
		if err == nil {
			t.Errorf("%s: Load should have failed", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name the rule (want substring %q)", name, err, tc.want)
		}
	}
}

// TestLoadSpendingPins pins the ledger path's handling: expanded like
// equity_transfers — a relative path resolves against the config file's
// directory — and "" when absent, which the parser reads as "no pins".
func TestLoadSpendingPins(t *testing.T) {
	base := `{"gold_db":"/tmp/x","default_currency":"USD","silver_sources":[{"id":"sq","kind":"swissquote","path":"/tmp/sq.db"}],`

	path := writeConfig(t, base+`"spending":{"pins":"pins.csv"}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(filepath.Dir(path), "pins.csv"); c.SpendPins() != want {
		t.Errorf("spending.pins = %q, want %q (resolved against the config directory)", c.SpendPins(), want)
	}

	c2, err := Load(writeConfig(t, base+`"spending":{}}`))
	if err != nil {
		t.Fatalf("Load without pins: %v", err)
	}
	if c2.SpendPins() != "" {
		t.Errorf("absent pins = %q, want empty", c2.SpendPins())
	}
	c3, err := Load(writeConfig(t, base[:len(base)-1]+`}`))
	if err != nil {
		t.Fatalf("Load absent block: %v", err)
	}
	if c3.SpendPins() != "" {
		t.Errorf("absent spending block pins = %q, want empty", c3.SpendPins())
	}
}
