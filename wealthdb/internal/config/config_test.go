package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
                "5E6F7G8H": {"nickname": "Goal account", "category": "esa"}
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
			{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "XD1396017463", Delete: true},
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
