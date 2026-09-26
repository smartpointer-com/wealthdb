package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
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
	t.Parallel()
	_, se, code := run(t, "nope")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unknown subcommand") {
		t.Errorf("stderr missing 'unknown subcommand': %s", se)
	}
}

func TestNoArgsShowsUsage(t *testing.T) {
	t.Parallel()
	_, se, code := run(t)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(se, "wealthdb — gold-layer portfolio CLI") {
		t.Errorf("stderr missing usage banner: %s", se)
	}
}

func TestInitLoadPositionsEndToEnd(t *testing.T) {
	t.Parallel()
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
	so, se, code = run(t, "-c", cfg, "holdings", "positions")
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
	t.Parallel()
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
	t.Parallel()
	cfg := setupCLITest(t)
	_, se, code := run(t, "-c", cfg, "holdings", "positions")
	if code != 3 {
		t.Errorf("positions before init exit = %d, want 3 (ExitMissingDB)", code)
	}
	if !strings.Contains(se, "does not exist") {
		t.Errorf("stderr missing 'does not exist': %s", se)
	}
}

func TestLoadAllNoSources(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// Default columns include "symbol".
	so, _, code := run(t, "-c", cfg, "holdings", "positions")
	if code != 0 {
		t.Fatal("positions default failed")
	}
	if !strings.Contains(so, "symbol") || !strings.Contains(so, "market_value") {
		t.Errorf("default columns missing 'symbol' or 'market_value':\n%s", so)
	}

	// Explicit narrow list.
	so, _, code = run(t, "-c", cfg, "holdings", "positions", "--columns", "silver_source,symbol,market_value")
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
	so, _, code = run(t, "-c", cfg, "holdings", "positions", "--columns", "all")
	if code != 0 {
		t.Fatal("positions all failed")
	}
	for _, name := range []string{"account_id", "name", "relationship_id"} {
		if !strings.Contains(strings.SplitN(so, "\n", 2)[0], name) {
			t.Errorf("'all' missing column %q", name)
		}
	}

	// Unknown column → exit 2 with helpful message listing all.
	_, se, code := run(t, "-c", cfg, "holdings", "positions", "--columns", "silver_source,bogus")
	if code != 2 {
		t.Errorf("unknown column exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unknown column") || !strings.Contains(se, "available") {
		t.Errorf("unknown-column stderr lacks guidance: %s", se)
	}
}

func TestPositionsCurrencyConversion(t *testing.T) {
	t.Parallel()
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
	so, _, code := run(t, "-c", cfg, "holdings", "positions", "--columns", "currency,market_value,value")
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

	// -x CHF with no USD↔CHF rate in this fixture: the SQL FX layer
	// leaves the value cell empty (NULL) rather than erroring. The
	// command still succeeds and the natural-currency market_value is
	// untouched.
	so, se, code := run(t, "-c", cfg, "holdings", "positions", "-x", "CHF", "-f", "csv",
		"--columns", "currency,market_value,value")
	if code != 0 {
		t.Fatalf("positions -x CHF should succeed with empty values; exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "value_CHF") {
		t.Errorf("CHF header should show value_CHF: %s", so)
	}
	var emptyCHF bool
	for _, ln := range strings.Split(so, "\n") {
		// Data rows start with the currency; value_CHF is the last,
		// empty field, so the row ends with a trailing comma.
		if strings.HasPrefix(ln, "USD,") && strings.HasSuffix(ln, ",") {
			emptyCHF = true
		}
	}
	if !emptyCHF {
		t.Errorf("expected a USD row with an empty value_CHF cell: %s", so)
	}

	// Bad currency → exit 2.
	_, _, code = run(t, "-c", cfg, "holdings", "positions", "-x", "DOLLAR")
	if code != 2 {
		t.Errorf("bad currency exit = %d, want 2", code)
	}
}

// TestAccountsBasicRollup checks the happy path of `wealthdb
// accounts`: one row per account, the derived total_value cell
// reflects the sum of positions + cash (and equals
// total_value_<CCY> when output and base match), and the default
// column set includes all the right names.
func TestAccountsBasicRollup(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	so, _, code := run(t, "-c", cfg, "holdings", "accounts")
	if code != 0 {
		t.Fatalf("accounts failed; code=%d so=%q", code, so)
	}
	// One row + header + "(1 row)" footer.
	if !strings.Contains(so, "(1 row)") {
		t.Errorf("missing (1 row) footer: %s", so)
	}
	for _, col := range []string{"silver_source", "account", "base_currency",
		"positions_value", "cash_balance", "total_value", "total_value_USD"} {
		if !strings.Contains(so, col) {
			t.Errorf("default columns missing %q:\n%s", col, so)
		}
	}
	// Single position is 1500 USD. Base currency is USD so
	// total_value == total_value_USD == "1500.00".
	if !strings.Contains(so, "1500.00") {
		t.Errorf("expected 1500.00 total in output:\n%s", so)
	}
}

// TestPortfoliosBasic checks the happy path of `wealthdb
// portfolios`. The fixture has one Schwab account (no portfolio)
// so the sentinel row carries its value; non-zero output proves
// the orphan-aggregation path works.
func TestPortfoliosBasic(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	so, _, code := run(t, "-c", cfg, "holdings", "portfolios")
	if code != 0 {
		t.Fatalf("portfolios failed; code=%d so=%q", code, so)
	}
	// Schwab fixture has no real portfolios → one sentinel row only.
	if !strings.Contains(so, "(no portfolio)") {
		t.Errorf("expected sentinel '(no portfolio)' row: %s", so)
	}
	// Value of the single Schwab position (1500 USD) is the
	// sentinel's total.
	if !strings.Contains(so, "1500.00") {
		t.Errorf("expected 1500.00 sentinel total: %s", so)
	}
}

// TestAccountsAllColumnsAndBadColumn exercises the `all` preset
// and the unknown-column error path.
func TestAccountsAllColumnsAndBadColumn(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	so, _, code := run(t, "-c", cfg, "holdings", "accounts", "--columns", "all")
	if code != 0 {
		t.Fatalf("accounts --columns all failed: %d", code)
	}
	header := strings.SplitN(so, "\n", 2)[0]
	for _, name := range []string{"account_kind", "relationship_id", "account_nickname",
		"account_category", "positions_value_USD", "cash_balance_USD"} {
		if !strings.Contains(header, name) {
			t.Errorf("'all' header missing %q: %s", name, header)
		}
	}

	_, se, code := run(t, "-c", cfg, "holdings", "accounts", "--columns", "bogus")
	if code != 2 {
		t.Errorf("unknown column exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unknown column") {
		t.Errorf("missing guidance: %s", se)
	}
}

func TestResetClearsSource(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// positions should have rows before reset.
	so, _, code := run(t, "-c", cfg, "holdings", "positions")
	if code != 0 || !strings.Contains(so, "(1 row)") {
		t.Fatalf("expected one position before reset; got code=%d so=%q", code, so)
	}

	so, _, code = run(t, "-c", cfg, "reset", "schwab-test")
	if code != 0 {
		t.Fatalf("reset failed; code=%d", code)
	}
	if !strings.Contains(so, "cleared") {
		t.Errorf("reset stdout: %q", so)
	}

	// After reset, positions should produce zero rows.
	so, _, code = run(t, "-c", cfg, "holdings", "positions")
	if code != 0 {
		t.Fatalf("positions post-reset failed; code=%d", code)
	}
	if !strings.Contains(so, "(0 rows)") {
		t.Errorf("expected (0 rows) after reset; got %q", so)
	}

	// Re-load should re-register and re-populate.
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("re-load failed")
	}
	so, _, code = run(t, "-c", cfg, "holdings", "positions")
	if code != 0 || !strings.Contains(so, "(1 row)") {
		t.Fatalf("expected one position after re-load; got code=%d", code)
	}
}

// TestReloadIsResetThenLoad confirms 'wealthdb reload <id>' clears
// gold and reloads in one step. The output line mentions both
// halves; positions are present after, just as if reset+load were
// invoked separately.
func TestReloadIsResetThenLoad(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	so, _, code := run(t, "-c", cfg, "reload", "schwab-test")
	if code != 0 {
		t.Fatalf("reload failed; code=%d so=%q", code, so)
	}
	if !strings.Contains(so, "reset +") {
		t.Errorf("reload stdout missing 'reset +' marker: %q", so)
	}

	// Positions are present after the reload.
	so, _, code = run(t, "-c", cfg, "holdings", "positions")
	if code != 0 || !strings.Contains(so, "(1 row)") {
		t.Fatalf("expected (1 row) after reload; got code=%d so=%q", code, so)
	}
}

// TestReloadAll covers the -a path. Empty config + -a should error
// (mirrors load -a) rather than silently succeed.
func TestReloadAllNoSources(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := tmp + "/wealthdb.cfg"
	body := `{"gold_db":"` + tmp + `/g.db","default_currency":"USD","silver_sources":[]}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	_, se, code := run(t, "-c", cfg, "reload", "-a")
	if code != 1 {
		t.Errorf("reload -a on empty config exit = %d, want 1", code)
	}
	if !strings.Contains(se, "no silver sources are configured") {
		t.Errorf("guidance missing: %s", se)
	}
}

func TestResetMissingDB(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	_, se, code := run(t, "-c", cfg, "reset", "schwab-test")
	if code != 3 {
		t.Errorf("reset on missing DB exit = %d, want 3 (ExitMissingDB)", code)
	}
	if !strings.Contains(se, "does not exist") {
		t.Errorf("missing-DB guidance absent: %s", se)
	}
}

func TestSnapshotsListsLoadedTimes(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	so, _, code := run(t, "-c", cfg, "snapshots", "schwab-test")
	if code != 0 {
		t.Fatalf("snapshots failed; code=%d so=%q", code, so)
	}
	// Each row is "<source>  <YYYY-MM-DD>"; fixture has one
	// snapshot at epoch 1000 → 1970-01-01.
	if !strings.Contains(so, "schwab-test") {
		t.Errorf("missing source prefix: %s", so)
	}
	if !strings.Contains(so, "1970-01-01") {
		t.Errorf("expected 1970-01-01 in output: %s", so)
	}
}

func TestSnapshotsAll(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	so, _, code := run(t, "-c", cfg, "snapshots", "-a")
	if code != 0 {
		t.Fatalf("snapshots -a failed; code=%d", code)
	}
	if !strings.Contains(so, "schwab-test") {
		t.Errorf("missing source prefix in -a output: %s", so)
	}
}

func TestStatusOverviewAndDetailed(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}

	// Status with no load yet — overview should still work.
	so, _, code := run(t, "-c", cfg, "status")
	if code != 0 {
		t.Fatalf("status (no load) failed; code=%d", code)
	}
	if !strings.Contains(so, "schwab-test") {
		t.Errorf("overview missing source: %s", so)
	}
	if !strings.Contains(so, "not loaded yet") {
		t.Errorf("expected 'not loaded yet' for fresh source: %s", so)
	}

	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// Status overview after load — fixture seeds 1 position and
	// 0 transactions.
	so, _, code = run(t, "-c", cfg, "status")
	if code != 0 {
		t.Fatalf("status (post-load) failed; code=%d", code)
	}
	if !strings.Contains(so, "1 pos") || !strings.Contains(so, "0 tx") {
		t.Errorf("expected '1 pos, 0 tx' in overview: %s", so)
	}

	// Status detailed.
	so, _, code = run(t, "-c", cfg, "status", "schwab-test")
	if code != 0 {
		t.Fatalf("status detailed failed; code=%d", code)
	}
	for _, want := range []string{"high_watermark:", "gold-side counts:", "positions:", "transactions:", "silver-side:"} {
		if !strings.Contains(so, want) {
			t.Errorf("detailed status missing %q: %s", want, so)
		}
	}

	// Status -v adds drift counts. Note: Go's stdlib flag parser
	// stops at the first positional, so flags must come before the
	// silver_source_id.
	so, _, code = run(t, "-c", cfg, "status", "-v", "schwab-test")
	if code != 0 {
		t.Fatalf("status -v failed; code=%d, stderr-via-stdout=%s", code, so)
	}
	if !strings.Contains(so, "taxonomy drift") {
		t.Errorf("status -v missing taxonomy drift section: %s", so)
	}
}

func TestPositionsCSVAndJSON(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// CSV with header
	so, _, code := run(t, "-c", cfg, "holdings", "positions", "-f", "csv", "--columns", "silver_source,symbol,market_value")
	if code != 0 {
		t.Fatalf("csv failed; code=%d", code)
	}
	lines := strings.Split(strings.TrimRight(so, "\n"), "\n")
	if lines[0] != "silver_source,symbol,market_value" {
		t.Errorf("csv header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "schwab-test") {
		t.Errorf("csv first data row missing source: %q", lines[1])
	}

	// CSV plain — no header
	so, _, code = run(t, "-c", cfg, "holdings", "positions", "-f", "csv_plain", "--columns", "silver_source,symbol")
	if code != 0 {
		t.Fatalf("csv_plain failed; code=%d", code)
	}
	if strings.HasPrefix(so, "silver_source") {
		t.Errorf("csv_plain should not have header: %q", so)
	}

	// JSON
	so, _, code = run(t, "-c", cfg, "holdings", "positions", "-f", "json", "--columns", "silver_source,symbol,market_value")
	if code != 0 {
		t.Fatalf("json failed; code=%d", code)
	}
	if !strings.Contains(so, `"silver_source": "schwab-test"`) {
		t.Errorf("json missing expected key/value: %s", so)
	}
	if !strings.HasPrefix(strings.TrimSpace(so), "[") {
		t.Errorf("json should be a top-level array: %s", so)
	}
}

func TestConfigWizardWritesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "wealthdb.cfg")
	silverPath := filepath.Join(dir, "silver.db")

	// Write a minimal silver SQLite so probeSilverDB inside the
	// wizard passes.
	sdb, _ := sql.Open("sqlite", "file:"+silverPath)
	sdb.Exec(`CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY)`)
	sdb.Close()

	// Explicit gold_db path so the validator doesn't probe the
	// host $XDG_DATA_HOME/wealthdb/ default (which may not exist inside
	// the test sandbox).
	goldPath := filepath.Join(dir, "gold.db")
	stdin := strings.NewReader(strings.Join([]string{
		goldPath,   // gold_db
		"USD",      // currency
		"src-a",    // id
		"schwab",   // kind
		silverPath, // path
		"n",        // no more sources
	}, "\n") + "\n")
	var so, se bytes.Buffer
	exit := Run([]string{"-c", cfgPath, "config"}, stdin, &so, &se)
	if exit != 0 {
		t.Fatalf("config exit=%d stdout=%s stderr=%s", exit, so.String(), se.String())
	}
	if !strings.Contains(so.String(), "Wrote config to") {
		t.Errorf("missing 'Wrote config to' confirmation: %s", so.String())
	}

	// File now exists with valid JSON.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), `"src-a"`) {
		t.Errorf("config file missing src-a id: %s", string(data))
	}
	if !strings.Contains(string(data), `"default_currency": "USD"`) {
		t.Errorf("config file missing currency: %s", string(data))
	}

	// Re-running refuses to clobber: exit 4.
	stdin = strings.NewReader("\n")
	so.Reset()
	se.Reset()
	exit = Run([]string{"-c", cfgPath, "config"}, stdin, &so, &se)
	if exit != 4 {
		t.Errorf("re-run exit = %d, want 4", exit)
	}
	if !strings.Contains(se.String(), "already exists") {
		t.Errorf("missing 'already exists' message: %s", se.String())
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	_, se, code := run(t, "help")
	if code != 0 {
		t.Errorf("help exit = %d, want 0", code)
	}
	for _, want := range []string{"init", "load", "holdings"} {
		if !strings.Contains(se, want) {
			t.Errorf("help missing %q: %s", want, se)
		}
	}
}

// TestHoldingsDispatch covers the `holdings` parent command: routing
// to a view, the no-view and unknown-view error paths, and `-h`.
func TestHoldingsDispatch(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	// A view routed through holdings produces that view's output.
	so, _, code := run(t, "-c", cfg, "holdings", "global")
	if code != 0 {
		t.Fatalf("holdings global exit=%d", code)
	}
	if !strings.Contains(so, "total_value_USD") {
		t.Errorf("holdings global missing total column: %s", so)
	}
	so, _, code = run(t, "-c", cfg, "holdings", "sources")
	if code != 0 || !strings.Contains(so, "schwab-test") {
		t.Errorf("holdings sources failed: code=%d so=%q", code, so)
	}

	// No view → exit 2 with the holdings usage.
	_, se, code := run(t, "-c", cfg, "holdings")
	if code != 2 {
		t.Errorf("bare holdings exit = %d, want 2", code)
	}
	if !strings.Contains(se, "point-in-time portfolio views") {
		t.Errorf("bare holdings missing usage: %s", se)
	}

	// Unknown view → exit 2.
	_, se, code = run(t, "-c", cfg, "holdings", "bogus")
	if code != 2 {
		t.Errorf("unknown view exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unknown view") {
		t.Errorf("unknown view missing guidance: %s", se)
	}

	// `holdings -h` prints the group usage and exits 0.
	_, se, code = run(t, "-c", cfg, "holdings", "-h")
	if code != 0 {
		t.Errorf("holdings -h exit = %d, want 0", code)
	}
	if !strings.Contains(se, "wealthdb holdings <view>") {
		t.Errorf("holdings -h missing usage: %s", se)
	}
}
