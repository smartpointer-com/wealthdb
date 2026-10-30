package gold

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// seedCashHistoryFixture: two bank accounts, one holding several
// currencies (two balance kinds of one of them, a zero balance and a
// metal no rate converts), one holding a single currency, a third
// holding only the metal, and a brokerage account with a stock and an
// open FX forward that reads `XXX` and no value. Two snapshots, the
// second moving one balance. It fills the serving tables and returns
// the two history days.
func seedCashHistoryFixture(t *testing.T, db *sql.DB, ctx context.Context) (day1, day3 int64) {
	t.Helper()
	day1, day3 = dy(2026, time.March, 2), dy(2026, time.March, 4)
	seedFX(t, db, day1, "CHF", "USD", "1.10")
	seedFX(t, db, day1, "USD", "EUR", "0.90")
	seedFX(t, db, day1, "USD", "GBP", "0.80")
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                               display_name, base_currency, tax_wrapper, management_style,
                               first_seen_at, last_seen_at) VALUES
            ('bank-src', 'BANK1', 'cash', 'Bank one', 'CHF', NULL, NULL, 1, 1),
            ('bank-src', 'BANK2', 'cash', 'Bank two', 'CHF', 'taxable_joint', NULL, 1, 1),
            ('bank-src', 'BANK3', 'cash', 'Metal', 'CHF', NULL, NULL, 1, 1),
            ('brk-src', 'BRK1', 'brokerage', 'Brokerage', 'USD', NULL, NULL, 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed accounts: %v", err)
		}
	}
	for _, snap := range []int64{day1, day3} {
		chf := "1000"
		if snap == day3 {
			chf = "1500"
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                       currency, balance_kind, amount) VALUES
                ('bank-src', $1, 'BANK1', 'CHF', 'current',   CAST($2 AS DECIMAL(28,4))),
                ('bank-src', $1, 'BANK1', 'CHF', 'available', CAST('990' AS DECIMAL(28,4))),
                ('bank-src', $1, 'BANK1', 'USD', 'closing',   CAST('200' AS DECIMAL(28,4))),
                ('bank-src', $1, 'BANK1', 'EUR', 'current',   CAST('0' AS DECIMAL(28,4))),
                ('bank-src', $1, 'BANK1', 'XAU', 'current',   CAST('5' AS DECIMAL(28,4))),
                ('bank-src', $1, 'BANK2', 'CHF', 'current',   CAST('300' AS DECIMAL(28,4))),
                ('bank-src', $1, 'BANK3', 'XAU', 'current',   CAST('2' AS DECIMAL(28,4)))`,
			snap, chf); err != nil {
			t.Fatalf("seed balances: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                                   position_key, asset_class, vehicle, currency, market_value) VALUES
                ('brk-src', $1, 'BRK1', 'P1', 'public_equity', 'etf', 'USD', CAST('700' AS DECIMAL(28,4))),
                ('brk-src', $1, 'BRK1', 'F1', 'foreign_exchange', 'forward', 'XXX', NULL)`,
			snap); err != nil {
			t.Fatalf("seed positions: %v", err)
		}
	}
	mustMaterializeServing(t, db, ctx)
	return day1 / 86400 * 86400, day3 / 86400 * 86400
}

// TestCashHistorySumsToTheAccountsHistory: an account's cash rows sum to
// the accounts history's cash columns on every day, in the base and
// every reporting currency, to the cent; an account with no cash has no
// rows, and one whose only balance no rate converts reads NULL in both.
func TestCashHistorySumsToTheAccountsHistory(t *testing.T) {
	db, ctx := openMigrated(t)
	day1, day3 := seedCashHistoryFixture(t, db, ctx)

	var accountDays, differ, orphans int
	if err := db.QueryRowContext(ctx, `
        WITH c AS (
            SELECT as_of_day, silver_source_id, account_external_id,
                   SUM(balance_base) AS b, SUM(balance_usd) AS u, SUM(balance_chf) AS c,
                   SUM(balance_eur) AS e, SUM(balance_gbp) AS g
              FROM serving_cash_history GROUP BY 1, 2, 3)
        SELECT COUNT(*),
               COUNT(*) FILTER (WHERE
                   (c.as_of_day IS NULL AND (h.cash_balance_base <> 0 OR h.cash_balance_usd <> 0
                        OR h.cash_balance_chf <> 0 OR h.cash_balance_eur <> 0 OR h.cash_balance_gbp <> 0))
                   OR (c.as_of_day IS NOT NULL AND NOT (
                        h.cash_balance_base IS NOT DISTINCT FROM c.b AND h.cash_balance_usd IS NOT DISTINCT FROM c.u
                        AND h.cash_balance_chf IS NOT DISTINCT FROM c.c AND h.cash_balance_eur IS NOT DISTINCT FROM c.e
                        AND h.cash_balance_gbp IS NOT DISTINCT FROM c.g))),
               (SELECT COUNT(*) FROM c ANTI JOIN serving_accounts_history h2 USING (as_of_day, silver_source_id, account_external_id))
          FROM serving_accounts_history h
          LEFT JOIN c USING (as_of_day, silver_source_id, account_external_id)`).Scan(&accountDays, &differ, &orphans); err != nil {
		t.Fatal(err)
	}
	if accountDays == 0 {
		t.Fatal("the fixture materialized no account history")
	}
	if differ != 0 || orphans != 0 {
		t.Errorf("%d account-day(s) whose cash differs from the accounts history, %d with no account row", differ, orphans)
	}

	for _, c := range []struct {
		day      int64
		acct     string
		ccy      string
		balance  string
		usdValid bool
	}{
		{day1, "BANK1", "CHF", "1000.0000", true}, // current beats available
		{day3, "BANK1", "CHF", "1500.0000", true},
		{day3, "BANK1", "EUR", "0.0000", true},
		{day3, "BANK1", "XAU", "5.0000", false},
		{day3, "BANK3", "XAU", "2.0000", false},
	} {
		var balance string
		var usd sql.NullFloat64
		if err := db.QueryRowContext(ctx, `
            SELECT CAST(balance AS VARCHAR), balance_usd FROM serving_cash_history
             WHERE as_of_day = ? AND account_external_id = ? AND currency = ?`,
			c.day, c.acct, c.ccy).Scan(&balance, &usd); err != nil {
			t.Fatalf("%s %s: %v", c.acct, c.ccy, err)
		}
		if balance != c.balance || usd.Valid != c.usdValid {
			t.Errorf("%s %s on day %d: balance %s, usd valid %v; want %s, %v",
				c.acct, c.ccy, c.day, balance, usd.Valid, c.balance, c.usdValid)
		}
	}
	if n := countRows(t, db, ctx, "(SELECT 1 FROM serving_cash_history WHERE account_external_id = 'BRK1')"); n != 0 {
		t.Errorf("the account with no cash has %d cash row(s)", n)
	}
	var wrapper string
	if err := db.QueryRowContext(ctx, `SELECT DISTINCT tax_wrapper FROM serving_cash_history
	    WHERE account_external_id = 'BANK1'`).Scan(&wrapper); err != nil || wrapper != "taxable_personal" {
		t.Errorf("an account with no wrapper reads %q (%v), want taxable_personal", wrapper, err)
	}
}

// TestHoldingsLinesCarryCashByCurrency: the history holds one cash line
// per account and currency, with its currency. The currency view sums
// every line, cash included, by currency, and leaves out a line no
// rate values (the metal, the forward) or that names no currency; its
// buckets sum to the asset classes'. The positions view lists each
// account's cash in a currency as its own `Cash <CCY>` row. The latest
// holdings keep net worth equal to positions plus cash on every source.
func TestHoldingsLinesCarryCashByCurrency(t *testing.T) {
	db, ctx := openMigrated(t)
	_, day3 := seedCashHistoryFixture(t, db, ctx)

	var lines, untagged int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*), COUNT(*) FILTER (WHERE currency IS NULL) FROM holdings_lines_history
         WHERE line_kind = 'cash' AND as_of_day = ? AND account_external_id = 'BANK1'`, day3).Scan(&lines, &untagged); err != nil {
		t.Fatal(err)
	}
	if lines != 4 || untagged != 0 {
		t.Errorf("BANK1 has %d cash line(s), %d with no currency; want 4, 0", lines, untagged)
	}

	ccys := scanStrings(t, db, `SELECT currency FROM web_currencies_history
	    WHERE epoch(as_of_day) = ? GROUP BY 1 ORDER BY 1`, day3)
	if got := strings.Join(ccys, ","); got != "CHF,EUR,USD" {
		t.Errorf("web_currencies_history's currencies %s, want CHF,EUR,USD", got)
	}
	var chf, want float64
	if err := db.QueryRowContext(ctx, `SELECT SUM(value_chf) FROM web_currencies_history
	    WHERE epoch(as_of_day) = ? AND currency = 'CHF'`, day3).Scan(&chf); err != nil {
		t.Fatal(err)
	}
	if chf != 1800 {
		t.Errorf("the CHF bucket holds %v CHF, want both banks' 1800", chf)
	}
	var differ int
	if err := db.QueryRowContext(ctx, `
        WITH a AS (SELECT as_of_day, silver_source_id, COALESCE(SUM(value_usd), 0) AS x
                     FROM web_asset_classes_history GROUP BY 1, 2),
             c AS (SELECT as_of_day, silver_source_id, COALESCE(SUM(value_usd), 0) AS x
                     FROM web_currencies_history GROUP BY 1, 2)
        SELECT COUNT(*) FROM a FULL JOIN c USING (as_of_day, silver_source_id)
         WHERE abs(COALESCE(a.x, 0) - COALESCE(c.x, 0)) > 0.005`).Scan(&differ); err != nil {
		t.Fatal(err)
	}
	if differ != 0 {
		t.Errorf("%d source-day(s) whose currencies do not sum to the asset classes", differ)
	}
	var usd float64
	if err := db.QueryRowContext(ctx, `SELECT SUM(value_usd) FROM web_currencies_history
	    WHERE epoch(as_of_day) = ?`, day3).Scan(&usd); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT SUM(total_value_usd) FROM serving_global_history
	    WHERE as_of_day = ?`, day3).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if usd != want {
		t.Errorf("the currencies sum to %v USD, net worth is %v", usd, want)
	}

	var cashRows, keys, symbols, others int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*), COUNT(DISTINCT line_key), COUNT(symbol),
               COUNT(*) FILTER (WHERE vehicle <> 'demand_deposit' OR asset_class <> 'cash')
          FROM web_positions_history
         WHERE epoch(as_of_day) = ? AND name = 'Cash CHF'`, day3).Scan(&cashRows, &keys, &symbols, &others); err != nil {
		t.Fatal(err)
	}
	if cashRows != 2 || keys != 2 || symbols != 0 || others != 0 {
		t.Errorf("Cash CHF: %d row(s), %d key(s), %d symbol(s), %d off the cash class; want 2, 2, 0, 0",
			cashRows, keys, symbols, others)
	}
	if n := countRows(t, db, ctx, "(SELECT 1 FROM web_positions_history WHERE line_key IS NOT NULL AND asset_class <> 'cash')"); n != 0 {
		t.Errorf("%d position row(s) carry a line key", n)
	}

	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM (
            SELECT silver_source_id, SUM(COALESCE(positions_value_usd, 0)) AS p,
                   SUM(COALESCE(cash_balance_usd, 0)) AS c, SUM(COALESCE(total_value_usd, 0)) AS t
              FROM web_sources_latest GROUP BY 1)
         WHERE abs(p + c - t) > 0.005`).Scan(&differ); err != nil {
		t.Fatal(err)
	}
	if differ != 0 {
		t.Errorf("%d source(s) whose net worth is not positions plus cash", differ)
	}
	if n := countRows(t, db, ctx, "web_sources_latest"); n == 0 {
		t.Error("web_sources_latest is empty; the check above proves nothing")
	}

	// No table holds a line with no currency today; the history is
	// pinned and given one, valued, which the currency view leaves out.
	if _, err := db.ExecContext(ctx, `
        CREATE TABLE pinned_lines AS SELECT * FROM holdings_lines_history;
        INSERT INTO pinned_lines (as_of_day, silver_source_id, account_external_id, tax_wrapper,
                                  management_style, line_kind, asset_class, vehicle, value_usd,
                                  value_chf, value_eur, value_gbp)
            VALUES (?, 'bank-src', 'BANK1', 'taxable_personal', 'self_directed', 'position',
                    'other', 'other', 5, 5, 5, 5)`, day3); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE OR REPLACE VIEW holdings_lines_history AS SELECT * FROM pinned_lines`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, ctx, "(SELECT 1 FROM web_currencies_history WHERE currency IS NULL)"); n != 0 {
		t.Errorf("web_currencies_history has %d row(s) with no currency", n)
	}
}

// TestMigration0171DDLIsRerunnable: a replay keeps what the table holds,
// and every view on it still answers.
func TestMigration0171DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCashHistoryFixture(t, db, ctx)
	before := countRows(t, db, ctx, "serving_cash_history")
	if before == 0 {
		t.Fatal("the fixture materialized no cash history")
	}

	rerunMigrationDDL(t, db, ctx, "0171_cash_history_by_currency.sql")

	if after := countRows(t, db, ctx, "serving_cash_history"); after != before {
		t.Errorf("serving_cash_history holds %d rows after a replay, %d before", after, before)
	}
	for _, v := range []string{"holdings_lines_history", "web_positions_history", "web_currencies_history",
		"web_sources_history", "web_asset_classes_history"} {
		countRows(t, db, ctx, v)
	}
}
