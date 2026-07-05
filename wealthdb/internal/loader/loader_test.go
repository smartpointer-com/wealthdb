package loader_test

// End-to-end tests for the load orchestration. We import the
// loader from a separate _test package so the test honestly
// exercises the public API; cross-imports of internal packages
// happen via canonical paths.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/loader"

	// Blank-import so the Schwab adapter registers itself with
	// the silver registry. The loader looks it up by kind.
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/schwab"
)

// silverFixtureSchema is the same minimal schema the Schwab
// adapter tests use; we duplicate-by-value here (a handful of
// lines) rather than create a cross-package test helper export.
const silverFixtureSchema = `
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
);`

type harness struct {
	t        *testing.T
	gold     *sql.DB
	loader   *loader.Loader
	silver   *sql.DB
	silverPath string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	g, err := gold.Open(":memory:", gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("gold.Open: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	if err := gold.Migrate(context.Background(), g); err != nil {
		t.Fatalf("gold.Migrate: %v", err)
	}

	silverPath := filepath.Join(t.TempDir(), "schwab.db")
	s, err := sql.Open("sqlite", "file:"+silverPath)
	if err != nil {
		t.Fatalf("open silver: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Exec(silverFixtureSchema); err != nil {
		t.Fatalf("apply silver schema: %v", err)
	}

	return &harness{
		t:          t,
		gold:       g,
		loader:     loader.New(g),
		silver:     s,
		silverPath: silverPath,
	}
}

func (h *harness) load(t *testing.T) *loader.LoadResult {
	t.Helper()
	res, err := h.loader.Load(context.Background(), loader.SourceSpec{
		ID: "schwab-test", Kind: "schwab", Path: h.silverPath,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res
}

func (h *harness) silverExec(t *testing.T, q string) {
	t.Helper()
	if _, err := h.silver.Exec(q); err != nil {
		t.Fatalf("silver exec: %v", err)
	}
}

func (h *harness) goldCount(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := h.gold.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (h *harness) goldScalar(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := h.gold.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("scalar %q: %v", q, err)
	}
	return s.String
}

func TestFirstLoadIngestsOneSnapshot(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{"hashValue":"ACC1"}');
        INSERT INTO account_balances(snapshot_at, account_external_id, balance_kind, payload) VALUES
            (1000, 'ACC1', 'current', '{"cashBalance":500.00}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '037833100',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"}}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 900, 'ACC1', 'TRADE',
             '{"netAmount":-1505.00,"transferItems":[
                {"instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"},
                 "amount":10,"cost":-1500.00,"price":150.00}]}');
    `)

	res := h.load(t)

	if res.AlreadyUpToDate {
		t.Fatal("first load should not be up-to-date")
	}
	if res.ChangeNumberBefore != -1 {
		t.Errorf("ChangeNumberBefore = %d, want -1", res.ChangeNumberBefore)
	}
	if res.ChangeNumberAfter != 1000 {
		t.Errorf("ChangeNumberAfter = %d, want 1000", res.ChangeNumberAfter)
	}
	if h.goldCount(t, "accounts") != 1 {
		t.Errorf("accounts = %d, want 1", h.goldCount(t, "accounts"))
	}
	if h.goldCount(t, "positions") != 1 {
		t.Errorf("positions = %d, want 1", h.goldCount(t, "positions"))
	}
	if h.goldCount(t, "cash_balances") != 1 {
		t.Errorf("cash_balances = %d, want 1", h.goldCount(t, "cash_balances"))
	}
	if h.goldCount(t, "transactions") != 1 {
		t.Errorf("transactions = %d, want 1", h.goldCount(t, "transactions"))
	}

	// Watermark advanced and audit row recorded.
	if got := h.goldScalar(t, `SELECT CAST(high_watermark AS VARCHAR) FROM silver_sources WHERE silver_source_id='schwab-test'`); got != "1000" {
		t.Errorf("high_watermark = %q, want 1000", got)
	}
	if h.goldCount(t, "load_audit") != 1 {
		t.Errorf("load_audit rows = %d, want 1", h.goldCount(t, "load_audit"))
	}
}

func TestReloadIsNoop(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{"hashValue":"ACC1"}');
    `)

	first := h.load(t)
	if first.AlreadyUpToDate {
		t.Fatal("first load should not be up-to-date")
	}
	auditAfter1 := h.goldCount(t, "load_audit")

	second := h.load(t)
	if !second.AlreadyUpToDate {
		t.Errorf("second load should be up-to-date")
	}
	if got := h.goldCount(t, "load_audit"); got != auditAfter1 {
		t.Errorf("audit rows after no-op = %d, want %d (no-op should not log)", got, auditAfter1)
	}
}

func TestIncrementalAppendsSecondSnapshot(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{"hashValue":"ACC1"}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '037833100',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"}}');
    `)
	h.load(t)
	if got := h.goldCount(t, "positions"); got != 1 {
		t.Fatalf("after first load: positions=%d want 1", got)
	}

	// Second snapshot: quantity bumped, market_value bumped.
	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 1, '/x/2');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (2000, 'ACC1', '{"hashValue":"ACC1"}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (2000, 'ACC1', '037833100',
             '{"longQuantity":15,"shortQuantity":0,"marketValue":2400.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"}}');
    `)
	res := h.load(t)

	if res.AlreadyUpToDate {
		t.Fatal("second load should pick up new snapshot")
	}
	if h.goldCount(t, "positions") != 2 {
		t.Errorf("positions = %d, want 2 (one row per snapshot)", h.goldCount(t, "positions"))
	}
	if got := h.goldScalar(t, `SELECT CAST(market_value AS VARCHAR) FROM positions WHERE snapshot_at=2000`); got != "2400.0000" {
		t.Errorf("snapshot 2000 market_value = %q, want 2400.0000", got)
	}
}

func TestAmendedWindowDropsPhantom(t *testing.T) {
	h := newHarness(t)

	// Initial silver state: tx A1 at t=1500.
	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 1500, 'ACC1', 'JOURNAL', '{"netAmount":100}'),
            ('A2', 1700, 'ACC1', 'JOURNAL', '{"netAmount":200}');
    `)
	h.load(t)
	if got := h.goldCount(t, "transactions"); got != 2 {
		t.Fatalf("after first load: transactions=%d want 2", got)
	}

	// Silver amends: A2 retracted, new A3 added, new dump_run.
	// The window covers [<= 2000, ...], so the delete-then-insert
	// step should drop A2 (no longer present in silver) and add A3.
	h.silverExec(t, `
        DELETE FROM transactions WHERE activity_id='A2';
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A3', 1800, 'ACC1', 'JOURNAL', '{"netAmount":300}');
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 1, '/x/2');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (2000, 'ACC1', '{}');
    `)
	h.load(t)

	// Expect A1 + A3 only (A2 wiped via window-delete).
	rows, err := h.gold.Query(`SELECT transaction_external_id FROM transactions ORDER BY transaction_external_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, s)
	}
	wantList := []string{"A1", "A3"}
	if len(seen) != len(wantList) {
		t.Fatalf("transactions = %v, want %v", seen, wantList)
	}
	for i, w := range wantList {
		if seen[i] != w {
			t.Errorf("transactions[%d] = %q, want %q", i, seen[i], w)
		}
	}
}

func TestSilverWentBackwardsIsRefused(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 1, '/x/2');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (2000, 'ACC1', '{}');
    `)
	h.load(t)
	if got := h.goldScalar(t, `SELECT CAST(high_watermark AS VARCHAR) FROM silver_sources`); got != "2000" {
		t.Fatalf("watermark after load = %q, want 2000", got)
	}

	// Simulate restoring an older silver: blow away the newer
	// dump_run so the latest change number drops.
	h.silverExec(t, `DELETE FROM dump_runs WHERE snapshot_at=2000;`)
	h.silverExec(t, `DELETE FROM accounts WHERE snapshot_at=2000;`)
	h.silverExec(t, `INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1500, 1, '/x/before');`)

	_, err := h.loader.Load(context.Background(), loader.SourceSpec{
		ID: "schwab-test", Kind: "schwab", Path: h.silverPath,
	})
	if err == nil {
		t.Fatal("expected error on silver-went-backwards")
	}
	if !errors.Is(err, loader.ErrSilverWentBackwards) {
		t.Errorf("err = %v, want ErrSilverWentBackwards", err)
	}
	// Watermark must NOT have moved.
	if got := h.goldScalar(t, `SELECT CAST(high_watermark AS VARCHAR) FROM silver_sources`); got != "2000" {
		t.Errorf("watermark after refused load = %q, want unchanged 2000", got)
	}
}

func TestResetClearsEverything(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '037833100',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"}}');
    `)
	h.load(t)
	if h.goldCount(t, "positions") == 0 {
		t.Fatal("setup: expected positions after load")
	}

	if err := h.loader.Reset(context.Background(), "schwab-test"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	for _, table := range []string{
		"transactions", "fx_rates", "cash_balances",
		"positions", "instruments", "accounts",
		"load_audit", "silver_sources", "symbol_resolutions",
	} {
		if n := h.goldCount(t, table); n != 0 {
			t.Errorf("after reset: %s has %d rows, want 0", table, n)
		}
	}

	// Subsequent Load starts fresh.
	res := h.load(t)
	if res.ChangeNumberBefore != -1 {
		t.Errorf("ChangeNumberBefore after reset+load = %d, want -1", res.ChangeNumberBefore)
	}
	if h.goldCount(t, "positions") != 1 {
		t.Errorf("after reset+load: positions=%d, want 1", h.goldCount(t, "positions"))
	}
}

// TestAccountOverridesApplied confirms the config-file account
// overrides land on the gold accounts row: nickname and category
// fields wired via SourceSpec.Overrides reach the writer, and
// override-less accounts in the same batch are untouched.
func TestAccountOverridesApplied(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{"hashValue":"ACC1","accountNumber":"redacted-1"}'),
            (1000, 'ACC2', '{"hashValue":"ACC2","accountNumber":"redacted-2"}'),
            (1000, 'ACC3', '{"hashValue":"ACC3","accountNumber":"redacted-3"}');
    `)

	_, err := h.loader.Load(context.Background(), loader.SourceSpec{
		ID: "schwab-test", Kind: "schwab", Path: h.silverPath,
		Overrides: map[string]loader.AccountOverride{
			"ACC1": {Nickname: "Main brokerage", Category: "personal"},
			"ACC2": {Nickname: "Education account"},      // partial: only nickname
			"ACC3": {Category: "managed"},        // partial: only category
			"ACCX": {Nickname: "unmatched"},      // no such account in batch
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rows, err := h.gold.Query(`
        SELECT account_external_id, COALESCE(nickname,''), COALESCE(account_category,'')
          FROM accounts
         ORDER BY account_external_id`)
	if err != nil {
		t.Fatalf("query accounts: %v", err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var id, nick, cat string
		if err := rows.Scan(&id, &nick, &cat); err != nil {
			t.Fatal(err)
		}
		got[id] = [2]string{nick, cat}
	}
	want := map[string][2]string{
		"ACC1": {"Main brokerage", "personal"},
		"ACC2": {"Education account", ""},
		"ACC3": {"", "managed"},
	}
	for id, w := range want {
		if g := got[id]; g != w {
			t.Errorf("%s: nickname/category = %q/%q, want %q/%q", id, g[0], g[1], w[0], w[1])
		}
	}
}

func TestListSourceIDs(t *testing.T) {
	h := newHarness(t)
	if got, _ := h.loader.ListSourceIDs(context.Background()); len(got) != 0 {
		t.Errorf("ListSourceIDs on empty gold = %v, want []", got)
	}

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{}');
    `)
	h.load(t)

	got, err := h.loader.ListSourceIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(strings.Join(got, ","), "schwab-test") {
		t.Errorf("ListSourceIDs = %v, want [schwab-test]", got)
	}
}
