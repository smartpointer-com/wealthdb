package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupAngelListPrivate writes an AngelList silver of four deals, a
// config that names it as source venture, and an empty gold, and returns
// the config and the silver's path. Each deal is paid from the wallet.
//   - PLAIN is marked at exactly what was paid in, and states no basis.
//   - ZERO is marked at 0, and the portal states 0 paid in.
//   - SINCE is marked at what was paid in, then takes more at cost: a
//     mark plus cost, worth exactly its book value.
//   - CARRY has no event: the ledger alone carries it, at cost.
func setupAngelListPrivate(t *testing.T) (cfg, silverPath string) {
	t.Helper()
	dir := t.TempDir()
	schema, err := os.ReadFile("../../internal/silver/angellist/testdata/silver_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	silverPath = filepath.Join(dir, "angellist.db")
	db, err := sql.Open("sqlite", "file:"+silverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	day := func(m time.Month, d int) int64 { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC).Unix() }
	hour := func(m time.Month, d int) int64 { return day(m, d) + 3600 }
	seed := fmt.Sprintf(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (%[1]d, 'example-holder');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('PLAIN', 'spv',  'Example Alpha', %[2]d),
            ('ZERO',  'spv',  'Example Zero',  %[2]d),
            ('SINCE', 'spv',  'Example Since', %[2]d),
            ('CARRY', 'fund', 'Example Fund',  %[3]d);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('PLAIN', %[2]d, 'investment', NULL,   1, 'USD', 50000, 'cost', 50000, %[1]d),
            ('PLAIN', %[4]d, 'valuation',  'live', 1, 'USD', 50000, 'fmv',  50000, %[1]d),
            ('ZERO',  %[2]d, 'investment', NULL,   1, 'USD', 30000, 'cost', 30000, %[1]d),
            ('ZERO',  %[4]d, 'valuation',  'live', 1, 'USD', 0,     'fmv',  0,     %[1]d),
            ('SINCE', %[2]d, 'valuation',  'live', 1, 'USD', 20000, 'fmv',  20000, %[1]d),
            ('SINCE', %[5]d, 'investment', NULL,   1, 'USD', NULL,  'cost', 30000, %[1]d);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t0', %[6]d, 'deposit',    200000, 'USD', NULL),
            ('t1', %[7]d, 'investment', -50000, 'USD', 'PLAIN'),
            ('t2', %[7]d, 'investment', -30000, 'USD', 'ZERO'),
            ('t3', %[7]d, 'investment', -20000, 'USD', 'SINCE'),
            ('t4', %[8]d, 'investment', -40000, 'USD', 'CARRY'),
            ('t5', %[9]d, 'investment', -10000, 'USD', 'SINCE');`,
		day(time.June, 1), day(time.January, 5), day(time.February, 2), day(time.March, 31), day(time.April, 15),
		hour(time.January, 2), hour(time.January, 5), hour(time.February, 2), hour(time.April, 15))
	if _, err := db.Exec(string(schema) + seed); err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD",
        "silver_sources": [{"id": "venture", "kind": "angellist", "path": %q}]}`,
		goldPathFromCfg(cfg), silverPath)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	freshGoldFor(t, cfg)
	return cfg, silverPath
}

// privateAsOf are the days the AngelList fixture reads its private view
// on: the day of each mark and payment, read at its end.
var privateAsOf = []string{"2026-01-05", "2026-02-02", "2026-03-31", "2026-04-15"}

// privateColumns are the private view's columns the AngelList fixture
// reads: the holding, its mark, its NAV and what its quality says.
const privateColumns = "holding_id,marked_at,nav,quality"

// privateCSV is the private view's privateColumns on each day of
// privateAsOf, each day's CSV after a line that names it.
func privateCSV(t *testing.T, cfg string) string {
	t.Helper()
	var b strings.Builder
	for _, d := range privateAsOf {
		out := mustRun(t, "-c", cfg, "returns", "private", "-d", d, "-C", privateColumns, "-f", "csv")
		fmt.Fprintf(&b, "# %s\n%s", d, out)
	}
	return b.String()
}

// privateAngelListWant is the AngelList fixture's private view. PLAIN and
// CARRY read at_cost throughout. ZERO does until its mark of 0, and SINCE
// until the payment after its mark. The row with no holding_id is the
// total.
const privateAngelListWant = `# 2026-01-05
holding_id,marked_at,nav_USD,quality
PLAIN,2026-01-05,500.00,at_cost;mwr_no_sign_change
SINCE,2026-01-05,200.00,at_cost;mwr_no_sign_change
ZERO,2026-01-05,300.00,at_cost;mwr_no_sign_change
,2026-01-05,1000.00,at_cost=3;mwr_no_sign_change
# 2026-02-02
holding_id,marked_at,nav_USD,quality
PLAIN,2026-01-05,500.00,at_cost
SINCE,2026-01-05,200.00,at_cost
ZERO,2026-01-05,300.00,at_cost
CARRY,2026-02-02,400.00,at_cost;mwr_no_sign_change
,2026-01-05,1400.00,at_cost=4
# 2026-03-31
holding_id,marked_at,nav_USD,quality
PLAIN,2026-03-31,500.00,at_cost
SINCE,2026-01-05,200.00,at_cost
ZERO,2026-03-31,0.00,mwr_no_sign_change
CARRY,2026-02-02,400.00,at_cost
,2026-01-05,1100.00,at_cost=3
# 2026-04-15
holding_id,marked_at,nav_USD,quality
PLAIN,2026-03-31,500.00,at_cost
SINCE,2026-01-05,300.00,call_after_mark
ZERO,2026-03-31,0.00,mwr_no_sign_change
CARRY,2026-02-02,400.00,at_cost
,2026-01-05,1200.00,at_cost=2;call_after_mark=1
`

// The AngelList adapter states cost on a mark worth exactly its nonzero
// book value: the private view reads at_cost on each day as
// privateAngelListWant says.
func TestPrivateAngelListAtBookReadsAtCost(t *testing.T) {
	t.Parallel()
	cfg, _ := setupAngelListPrivate(t)
	mustRun(t, "-c", cfg, "load", "venture")
	if got := privateCSV(t, cfg); got != privateAngelListWant {
		t.Errorf("private view:\n%s", got)
	}
}

// A plain load re-emits nothing below the source's watermark, so gold
// keeps the lines it holds. `reload` re-emits the source whole, and so
// does the load of a dump past the watermark. The test strips every
// payload off PLAIN's lines and watches what each command does with them.
func TestALoadKeepsTheStampsAReloadRestates(t *testing.T) {
	t.Parallel()
	cfg, silverPath := setupAngelListPrivate(t)
	goldPath := goldPathFromCfg(cfg)
	mustRun(t, "-c", cfg, "load", "venture")
	stamped := `SELECT COUNT(*) FROM positions WHERE position_key = 'PLAIN'
                   AND json_extract_string(payload, '$.valuation_basis') = 'cost'`
	lines := countGold(t, goldPath, `SELECT COUNT(*) FROM positions WHERE position_key = 'PLAIN'`)
	if n := countGold(t, goldPath, stamped); n != lines || n == 0 {
		t.Fatalf("%d of PLAIN's %d lines state cost after the load", n, lines)
	}
	unstamp := func() {
		execGold(t, goldPath, `UPDATE positions SET payload = NULL WHERE position_key = 'PLAIN'`)
	}

	unstamp()
	mustRun(t, "-c", cfg, "load", "venture")
	if n := countGold(t, goldPath, stamped); n != 0 {
		t.Errorf("a plain load restamped %d line(s): it re-emits nothing below the watermark", n)
	}
	if got := privateCSV(t, cfg); got == privateAngelListWant {
		t.Error("the private view reads PLAIN at cost on its stored lines")
	}

	mustRun(t, "-c", cfg, "reload", "venture")
	if n := countGold(t, goldPath, stamped); n != lines {
		t.Errorf("reload stamped %d of %d lines", n, lines)
	}
	if got := privateCSV(t, cfg); got != privateAngelListWant {
		t.Errorf("private view after the reload:\n%s", got)
	}

	unstamp()
	sdb, err := sql.Open("sqlite", "file:"+silverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	next := time.Date(2026, time.June, 2, 0, 0, 0, 0, time.UTC).Unix()
	if _, err := sdb.Exec(`INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (?, 'example-holder')`,
		next); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "-c", cfg, "load", "venture")
	if n := countGold(t, goldPath, stamped); n != lines {
		t.Errorf("the load of a new dump stamped %d of %d lines: it re-emits the source whole", n, lines)
	}
}
