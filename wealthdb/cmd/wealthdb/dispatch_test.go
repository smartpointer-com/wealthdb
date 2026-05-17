package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// silverFixture is the minimal Schwab silver schema, duplicated
// here so this end-to-end test doesn't reach across packages.
const silverFixture = `
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE TABLE account_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    balance_kind        TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, balance_kind)
);
CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    instrument_key      TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);
CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{}');
INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
    (1000, 'ACC1', '037833100',
     '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
       "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL","description":"Apple Inc"}}');
`

// setupCLITest writes a wealthdb.cfg + a Schwab silver fixture
// under a tmpdir and returns the config path. Used by every
// end-to-end CLI test below.
func setupCLITest(t *testing.T) (configPath string) {
	t.Helper()
	dir := t.TempDir()
	silverPath := filepath.Join(dir, "schwab.db")

	sdb, err := sql.Open("sqlite", "file:"+silverPath)
	if err != nil {
		t.Fatalf("open silver: %v", err)
	}
	defer sdb.Close()
	if _, err := sdb.Exec(silverFixture); err != nil {
		t.Fatalf("apply silver fixture: %v", err)
	}

	cfgPath := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{
        "gold_db": %q,
        "default_currency": "USD",
        "silver_sources": [{"id":"schwab-test","kind":"schwab","path":%q}]
    }`, filepath.Join(dir, "wealthdb.db"), silverPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfgPath
}

// run invokes Run() with stdio captured into buffers. The string
// returns are stdout and stderr trimmed of trailing whitespace.
func run(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	var so, se bytes.Buffer
	exit = Run(args, nil, &so, &se)
	return so.String(), se.String(), exit
}

func TestUnknownSubcommand(t *testing.T) {
	_, se, code := run(t, "nope")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unknown subcommand") {
		t.Errorf("stderr missing 'unknown subcommand': %s", se)
	}
}

func TestNoArgsShowsUsage(t *testing.T) {
	_, se, code := run(t)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(se, "wealthdb — gold-layer portfolio CLI") {
		t.Errorf("stderr missing usage banner: %s", se)
	}
}

func TestInitLoadPositionsEndToEnd(t *testing.T) {
	cfg := setupCLITest(t)

	// init
	so, se, code := run(t, "-c", cfg, "init")
	if code != 0 {
		t.Fatalf("init exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "created gold database") {
		t.Errorf("init stdout missing creation message: %s", so)
	}

	// load
	so, se, code = run(t, "-c", cfg, "load", "schwab-test")
	if code != 0 {
		t.Fatalf("load exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "schwab-test") {
		t.Errorf("load stdout missing source id: %s", so)
	}

	// positions
	so, se, code = run(t, "-c", cfg, "positions")
	if code != 0 {
		t.Fatalf("positions exit=%d stderr=%s", code, se)
	}
	// Expect a table with the Apple position visible.
	if !strings.Contains(so, "037833100") {
		t.Errorf("positions stdout missing instrument key: %s", so)
	}
	if !strings.Contains(so, "equity") {
		t.Errorf("positions stdout missing asset class: %s", so)
	}
	if !strings.Contains(so, "(1 row)") {
		t.Errorf("positions stdout missing single-row footer: %s", so)
	}
}

func TestInitFailsOnExistingDB(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("first init failed: code=%d", code)
	}
	_, se, code := run(t, "-c", cfg, "init")
	if code != 4 {
		t.Errorf("second init exit = %d, want 4 (ExitInitExisting)", code)
	}
	if !strings.Contains(se, "already exists") {
		t.Errorf("stderr missing 'already exists': %s", se)
	}
}

func TestPositionsFailsBeforeInit(t *testing.T) {
	cfg := setupCLITest(t)
	_, se, code := run(t, "-c", cfg, "positions")
	if code != 3 {
		t.Errorf("positions before init exit = %d, want 3 (ExitMissingDB)", code)
	}
	if !strings.Contains(se, "does not exist") {
		t.Errorf("stderr missing 'does not exist': %s", se)
	}
}

func TestLoadAllNoSources(t *testing.T) {
	// Config with empty silver_sources array
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{
        "gold_db": %q, "default_currency": "USD", "silver_sources": []
    }`, filepath.Join(dir, "wealthdb.db"))
	os.WriteFile(cfg, []byte(body), 0o644)

	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init failed")
	}
	_, se, code := run(t, "-c", cfg, "load", "-a")
	if code == 0 {
		t.Errorf("load -a with empty sources should fail; stderr=%s", se)
	}
}

func TestPositionsColumnsFlag(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// Default columns include "symbol".
	so, _, code := run(t, "-c", cfg, "positions")
	if code != 0 {
		t.Fatal("positions default failed")
	}
	if !strings.Contains(so, "symbol") || !strings.Contains(so, "market_value") {
		t.Errorf("default columns missing 'symbol' or 'market_value':\n%s", so)
	}

	// Explicit narrow list.
	so, _, code = run(t, "-c", cfg, "positions", "--columns", "silver_source,symbol,market_value")
	if code != 0 {
		t.Fatal("positions narrow failed")
	}
	// Header should contain only the chosen three; quantity/asset_class absent.
	header := strings.SplitN(so, "\n", 2)[0]
	if !strings.Contains(header, "silver_source") || !strings.Contains(header, "symbol") || !strings.Contains(header, "market_value") {
		t.Errorf("narrow header missing chosen columns: %q", header)
	}
	if strings.Contains(header, "quantity") || strings.Contains(header, "asset_class") {
		t.Errorf("narrow header has unwanted columns: %q", header)
	}

	// `all` preset.
	so, _, code = run(t, "-c", cfg, "positions", "--columns", "all")
	if code != 0 {
		t.Fatal("positions all failed")
	}
	for _, name := range []string{"account_id", "name", "relationship_id"} {
		if !strings.Contains(strings.SplitN(so, "\n", 2)[0], name) {
			t.Errorf("'all' missing column %q", name)
		}
	}

	// Unknown column → exit 2 with helpful message listing all.
	_, se, code := run(t, "-c", cfg, "positions", "--columns", "silver_source,bogus")
	if code != 2 {
		t.Errorf("unknown column exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unknown column") || !strings.Contains(se, "available") {
		t.Errorf("unknown-column stderr lacks guidance: %s", se)
	}
}

func TestPositionsCurrencyConversion(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// Default output currency from config is USD; the Schwab
	// fixture is USD-only, so the value column should equal
	// market_value (rate is 1.0 when from == to).
	so, _, code := run(t, "-c", cfg, "positions", "--columns", "currency,market_value,value")
	if code != 0 {
		t.Fatalf("positions default-ccy failed; stderr=...")
	}
	if !strings.Contains(so, "value_USD") {
		t.Errorf("default ccy header should show value_USD: %s", so)
	}
	if !strings.Contains(so, "1500.00") {
		// Schwab fixture position has market_value 1500.00
		t.Errorf("expected 1500.00 in output: %s", so)
	}

	// -x with no FX rates available → an error (no silvers ship
	// USD↔CHF in this fixture).
	_, se, code := run(t, "-c", cfg, "positions", "-x", "CHF")
	if code == 0 {
		t.Errorf("expected non-zero exit when no rates; stderr=%s", se)
	}
	if !strings.Contains(se, "FX rates available") {
		t.Errorf("missing FX-rates guidance: %s", se)
	}

	// Bad fx-mode → exit 2.
	_, se, code = run(t, "-c", cfg, "positions", "--fx-mode", "yolo")
	if code != 2 {
		t.Errorf("bad fx-mode exit = %d, want 2", code)
	}
	if !strings.Contains(se, "fx-mode") {
		t.Errorf("missing fx-mode guidance: %s", se)
	}

	// Bad currency → exit 2.
	_, se, code = run(t, "-c", cfg, "positions", "-x", "DOLLAR")
	if code != 2 {
		t.Errorf("bad currency exit = %d, want 2", code)
	}
}

func TestHelp(t *testing.T) {
	_, se, code := run(t, "help")
	if code != 0 {
		t.Errorf("help exit = %d, want 0", code)
	}
	for _, want := range []string{"init", "load", "positions"} {
		if !strings.Contains(se, want) {
			t.Errorf("help missing %q: %s", want, se)
		}
	}
}
