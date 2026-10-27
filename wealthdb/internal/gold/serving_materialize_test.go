package gold

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// mustMaterializeServing fills the serving tables from the macros, as a
// refresh does: a test that reads a serving view calls it after its last
// write.
func mustMaterializeServing(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := MaterializeServingTables(ctx, db); err != nil {
		t.Fatalf("materialize serving tables: %v", err)
	}
}

// countRows returns COUNT(*) of a table, a view or a macro call.
func countRows(t *testing.T, db *sql.DB, ctx context.Context, from string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+from).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

// assertTablesHoldTheMacros checks that every serving table holds its
// macro's rows, no more and no fewer.
func assertTablesHoldTheMacros(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	for _, h := range servingTables {
		if got, want := countRows(t, db, ctx, h.table), countRows(t, db, ctx, h.source); got != want {
			t.Errorf("%s has %d rows, %s %d", h.table, got, h.source, want)
		}
		for _, q := range []string{
			"SELECT * FROM " + h.table + " EXCEPT ALL SELECT * FROM " + h.source,
			"SELECT * FROM " + h.source + " EXCEPT ALL SELECT * FROM " + h.table,
		} {
			if n := countRows(t, db, ctx, "("+q+")"); n != 0 {
				t.Errorf("%s: %d row(s) in one and not the other", h.table, n)
			}
		}
	}
}

// TestMaterializeServingTablesHoldsTheMacrosRows: each table is its
// macro's output verbatim, so a report model that wraps it keeps the
// macro's shape and values. A second run replaces the rows rather than
// adding to them.
func TestMaterializeServingTablesHoldsTheMacrosRows(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	if countRows(t, db, ctx, "serving_positions_history") == 0 || countRows(t, db, ctx, "serving_accounts_history") == 0 {
		t.Fatal("the fixture materialized no history; the comparison below proves nothing")
	}
	if countRows(t, db, ctx, "serving_spending_transactions") == 0 || countRows(t, db, ctx, "serving_card_balances_history") == 0 {
		t.Fatal("the fixture materialized no transaction line; the comparison below proves nothing")
	}
	assertTablesHoldTheMacros(t, db, ctx)
	if _, err := MaterializeServingTables(ctx, db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	assertTablesHoldTheMacros(t, db, ctx)

	// A serving table the schema adds without an entry here would stay
	// empty, and the views on it read nothing.
	var inSchema, listed []string
	if err := scanAll(ctx, db, `SELECT table_name FROM information_schema.tables
	    WHERE table_type = 'BASE TABLE' AND table_name LIKE 'serving\_%' ESCAPE '\' ORDER BY 1`,
		func(rows *sql.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			inSchema = append(inSchema, name)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	for _, s := range servingTables {
		listed = append(listed, s.table)
	}
	slices.Sort(listed)
	if !slices.Equal(inSchema, listed) {
		t.Errorf("the schema's serving tables are %v, servingTables lists %v", inSchema, listed)
	}
}

// TestWebHistoryViewsSumToTheSources: per day and source, every history
// view sums to the per-source history the macro gives, so the dashboard
// tiles that read different views agree with each other and with
// `wealthdb holdings` history.
func TestWebHistoryViewsSumToTheSources(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	src := `SELECT CAST(as_of_day AS BIGINT) AS d, silver_source_id AS s,
                   COALESCE(positions_value_usd, 0) AS p, COALESCE(cash_balance_usd, 0) AS c,
                   COALESCE(total_value_usd, 0) AS t
              FROM report_sources_history_multi()`
	for _, v := range []struct{ name, sum, col string }{
		{"web_sources_history", "COALESCE(SUM(total_value_usd), 0)", "t"},
		{"web_sources_history", "COALESCE(SUM(positions_value_usd), 0)", "p"},
		{"web_sources_history", "COALESCE(SUM(cash_balance_usd), 0)", "c"},
		{"web_asset_classes_history", "COALESCE(SUM(value_usd), 0)", "t"},
		{"web_vehicles_history", "COALESCE(SUM(value_usd), 0)", "t"},
		{"web_accounts_history", "COALESCE(SUM(total_value_usd), 0)", "t"},
		{"web_positions_history", "COALESCE(SUM(value_usd), 0)", "p"},
	} {
		var differ int
		err := db.QueryRowContext(ctx, `
            WITH v AS (SELECT CAST(epoch(as_of_day) AS BIGINT) AS d, silver_source_id AS s, `+v.sum+` AS x
                         FROM `+v.name+` GROUP BY 1, 2),
                 m AS (`+src+`)
            SELECT COUNT(*) FROM m FULL JOIN v USING (d, s)
             WHERE abs(COALESCE(v.x, 0) - COALESCE(m.`+v.col+`, 0)) > 0.005`).Scan(&differ)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if differ != 0 {
			t.Errorf("%s: %d source-day(s) whose %s differs from report_sources_history_multi", v.name, differ, v.col)
		}
	}
}

// TestWebHistoryViewsCarryTheWrapperAndClass: every holdings view splits
// by the account's tax wrapper, cash reads as the class `cash` and the
// vehicle `demand_deposit`, and an account with no wrapper of its own
// reads `taxable_personal`, the default the account history gives it.
// The income flow view carries the wrapper too.
func TestWebHistoryViewsCarryTheWrapperAndClass(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)
	snap := dy(2026, time.January, 10)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, base_currency, tax_wrapper, management_style,
                              first_seen_at, last_seen_at)
        VALUES ('test-src', 'IRA1', 'brokerage', 'Retirement', 'USD',
                'traditional_ira', 'advisory', 1, 1)`); err != nil {
		t.Fatalf("seed retirement account: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, vehicle, currency, market_value)
        VALUES ('test-src', ?, 'IRA1', 'R1', 'fixed_income', 'etf', 'USD', CAST('500' AS DECIMAL(28,4)))`,
		snap); err != nil {
		t.Fatalf("seed retirement position: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount)
        VALUES ('test-src', ?, 'IRA1', 'USD', 'current', CAST('50' AS DECIMAL(28,4)))`, snap); err != nil {
		t.Fatalf("seed retirement cash: %v", err)
	}
	if _, err := MaterializeServingTables(ctx, db); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// The retirement account's lines, on its own wrapper, against the
	// account history's own total for it.
	var day int64
	if err := db.QueryRowContext(ctx, `SELECT MAX(as_of_day) FROM serving_accounts_history`).Scan(&day); err != nil {
		t.Fatalf("latest day: %v", err)
	}
	var want float64
	if err := db.QueryRowContext(ctx, `
        SELECT total_value_usd FROM serving_accounts_history
         WHERE as_of_day = ? AND account_external_id = 'IRA1'`, day).Scan(&want); err != nil {
		t.Fatalf("retirement account total: %v", err)
	}
	for _, q := range []string{
		`SELECT SUM(total_value_usd) FROM web_sources_history`,
		`SELECT SUM(value_usd) FROM web_asset_classes_history`,
		`SELECT SUM(value_usd) FROM web_vehicles_history`,
		`SELECT SUM(total_value_usd) FROM web_accounts_history`,
	} {
		var got float64
		if err := db.QueryRowContext(ctx, q+` WHERE epoch(as_of_day) = ? AND tax_wrapper = 'traditional_ira'`, day).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != want {
			t.Errorf("%s on traditional_ira = %v, want the account's %v", q, got, want)
		}
	}
	var cash, cashVehicle, unwrapped int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FILTER (WHERE asset_class = 'cash' AND tax_wrapper = 'traditional_ira'),
               COUNT(*) FILTER (WHERE vehicle = 'demand_deposit' AND asset_class <> 'cash'),
               COUNT(*) FILTER (WHERE tax_wrapper IS NULL)
          FROM web_vehicles_history`).Scan(&cash, &cashVehicle, &unwrapped); err != nil {
		t.Fatalf("cash lines: %v", err)
	}
	if cash == 0 {
		t.Error("the retirement account's cash reads no `cash` line")
	}
	if cashVehicle != 0 {
		t.Errorf("%d demand_deposit line(s) outside the cash class", cashVehicle)
	}
	if unwrapped != 0 {
		t.Errorf("%d line(s) with no tax wrapper", unwrapped)
	}
	for _, v := range []string{"web_positions_history", "web_sources_latest", "web_income"} {
		if n := countRows(t, db, ctx, "(SELECT tax_wrapper FROM "+v+" WHERE tax_wrapper IS NULL)"); n != 0 {
			t.Errorf("%s: %d row(s) with no tax wrapper", v, n)
		}
	}
	if n := countRows(t, db, ctx, "(SELECT 1 FROM web_positions_history WHERE tax_wrapper = 'traditional_ira')"); n == 0 {
		t.Error("web_positions_history has no row on the retirement account's wrapper")
	}
}

// TestMigration0132DDLIsRerunnable holds the tables and views to the
// replay bar: a replay keeps what the tables hold, and every view still
// answers.
func TestMigration0132DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)
	before := countRows(t, db, ctx, "serving_positions_history")

	rerunMigrationDDL(t, db, ctx, "0132_web_serving_tables.sql")

	if after := countRows(t, db, ctx, "serving_positions_history"); after != before {
		t.Errorf("serving_positions_history holds %d rows after a replay, %d before", after, before)
	}
	for _, v := range []string{"holdings_lines_history", "holdings_lines_latest", "web_sources_latest",
		"web_sources_history", "web_asset_classes_history", "web_vehicles_history", "web_accounts_history",
		"web_positions_history", "web_card_balances_history", "web_income",
		"web_spending", "web_cashflow"} {
		countRows(t, db, ctx, v)
	}
}

// TestWebTransactionsIsDropped: web_transactions is gone (migration
// 0158), no view or macro left names it, and the table it read stays for
// the report_transactions model and web_income_withheld.
func TestWebTransactionsIsDropped(t *testing.T) {
	db, ctx := openMigrated(t)
	assertNothingNames(t, db, ctx, "web_transactions")
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM duckdb_tables() WHERE table_name = 'serving_transactions'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Errorf("serving_transactions tables %d, want 1", tables)
	}
}
