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
            {"id": "schwab-retail", "kind": "schwab", "path": "/tmp/schwab.db"},
            {"id": "ubs-main",      "kind": "ubs",    "path": "/tmp/ubs.db"}
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

func TestValidateRejectsBadCurrency(t *testing.T) {
	cases := []string{"", "usd", "DOLLAR", "US", "USDX"}
	for _, ccy := range cases {
		c := &Config{GoldDB: "/x", DefaultCurrency: ccy}
		if err := c.Validate(); err == nil {
			t.Errorf("currency %q should fail validation", ccy)
		}
	}
}
