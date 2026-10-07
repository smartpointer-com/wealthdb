package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// reportingCurrencies is the set the `_multi` macros emit a column set
// for (migration 0114) — the same set the returns materializer writes.
var reportingCurrencies = materializeCurrencies

// seedReportingFX lays down a rate history that reaches every path the
// conversion takes: rates that change on different days, a currency
// known only against CHF (JPY: through CHF), one known only against USD
// (CAD: through USD), a direct CHF→GBP rate that starts later than the
// USD→GBP one, and nothing at all for XAU.
func seedReportingFX(t *testing.T, db *sql.DB) {
	t.Helper()
	d := func(day int) int64 { return int64(day) * 86400 }
	seedFX(t, db, d(10), "CHF", "USD", "0.90")
	seedFX(t, db, d(20), "CHF", "USD", "0.88")
	seedFX(t, db, d(10), "USD", "EUR", "1.10")
	seedFX(t, db, d(15), "USD", "EUR", "1.12")
	seedFX(t, db, d(12), "USD", "GBP", "1.27")
	seedFX(t, db, d(25), "CHF", "GBP", "1.13")
	seedFX(t, db, d(11), "CHF", "JPY", "0.006")
	seedFX(t, db, d(30), "CAD", "USD", "1.35")
}

// TestFxReportingValueMatchesTheReference holds the shared helper to the
// conversion every report macro spelled out before it existed: for each
// source currency, reporting target and day — before the first rate,
// between changes, after the last — fx_reporting_value over the two
// grid views gives fxConvert's answer to the last decimal, NULL where
// fxConvert finds no path.
func TestFxReportingValueMatchesTheReference(t *testing.T) {
	db, _ := openMigrated(t)
	seedReportingFX(t, db)

	const q = `
        SELECT CAST(CAST(fx_reporting_value(b.amt, b.fromc, b.toc, fxl, fxb) AS DECIMAL(28,4)) AS VARCHAR)
          FROM (SELECT CAST(? AS BIGINT) AS d, CAST(? AS DOUBLE) AS amt,
                       CAST(? AS VARCHAR) AS fromc, CAST(? AS VARCHAR) AS toc) b
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.fromc AND fxl.day <= b.d
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= b.d`
	resolved := 0
	for _, from := range []string{"USD", "CHF", "EUR", "GBP", "JPY", "CAD", "XAU"} {
		for _, to := range reportingCurrencies {
			for _, day := range []int64{0, 5, 10, 11, 12, 14, 15, 19, 20, 24, 25, 29, 30, 40} {
				want, wantOK := fxConvert(t, db, day, 1234.5678, from, to)
				var got sql.NullString
				if err := db.QueryRow(q, day, 1234.5678, from, to).Scan(&got); err != nil {
					t.Fatalf("fx_reporting_value(day %d, %s→%s): %v", day, from, to, err)
				}
				if got.Valid != wantOK || got.String != want {
					t.Errorf("day %d %s→%s: helper %q (ok=%v), reference %q (ok=%v)",
						day, from, to, got.String, got.Valid, want, wantOK)
				}
				if wantOK && from != to {
					resolved++
				}
			}
		}
	}
	// The comparison must have compared conversions, not a grid of NULLs.
	if resolved < 100 {
		t.Errorf("only %d cross-currency conversions resolved; the fixture no longer reaches the paths", resolved)
	}
}

// seedReportingFixture extends the web-view fixture with positions in
// currencies that reach every conversion path, and the rate history above
// placed around the fixture's snapshot day.
func seedReportingFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedWebViewEpochFixture(t, db, ctx)
	snap := spendAt(2026, time.January, 10)
	for i, ccy := range []string{"EUR", "GBP", "JPY", "CAD", "XAU", "CHF"} {
		if _, err := db.Exec(`
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                                   position_key, asset_class, vehicle, currency, market_value)
            VALUES ('test-src', ?, 'BRK1', ?, 'public_equity', 'stock', ?,
                    CAST('1000.5' AS DECIMAL(28,4)))`, snap, fmt.Sprintf("P-%s", ccy), ccy); err != nil {
			t.Fatalf("seed %s position %d: %v", ccy, i, err)
		}
	}
	day := snap / 86400
	for _, r := range []struct {
		off               int64
		base, quote, rate string
	}{
		{-30, "CHF", "USD", "0.90"}, {-3, "CHF", "USD", "0.88"},
		{-30, "USD", "EUR", "1.10"}, {-20, "USD", "GBP", "1.27"},
		{-10, "CHF", "GBP", "1.13"}, {-25, "CHF", "JPY", "0.006"},
		{-5, "CAD", "USD", "1.35"},
	} {
		seedFX(t, db, (day+r.off)*86400, r.base, r.quote, r.rate)
	}
}

// TestMultiReportsMatchTheSingleCurrencyOnes pins what the shared helper
// and the GBP columns promise: every `_<ccy>` column of a `_multi` report
// equals the single-currency report asked for in that currency — the
// figure `wealthdb … -x <CCY>` prints.
func TestMultiReportsMatchTheSingleCurrencyOnes(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportingFixture(t, db, ctx)

	const max = "9223372036854775807"
	v3 := []string{"positions_value", "cash_balance", "total_value"}
	pairs := []struct {
		multi, single string
		keys, vals    []string
	}{
		{"report_sources_multi(" + max + ")", "report_sources(" + max + ", '%s')",
			[]string{"silver_source_id"}, v3},
		{"report_accounts_multi(" + max + ")", "report_accounts(" + max + ", '%s')",
			[]string{"silver_source_id", "account_external_id"}, v3},
		{"report_positions_multi(" + max + ")", "report_positions(" + max + ", '%s')",
			[]string{"silver_source_id", "account_external_id", "position_key"}, []string{"value"}},
		{"report_sources_history_multi()", "report_sources_history('%s')",
			[]string{"as_of_day", "silver_source_id"}, v3},
		{"report_positions_history_multi()", "report_positions_history('%s')",
			[]string{"as_of_day", "silver_source_id", "account_external_id", "position_key"}, []string{"value"}},
		{"report_transactions_multi(0, " + max + ")", "report_transactions(0, " + max + ", '%s')",
			[]string{"silver_source_id", "transaction_external_id"}, []string{"value"}},
		{"spending_lines_multi(0, " + max + ")", "spending_lines_outccy(0, " + max + ", '%s')",
			[]string{"silver_source_id", "transaction_external_id"}, []string{"value"}},
	}
	for _, p := range pairs {
		for _, ccy := range reportingCurrencies {
			keys := strings.Join(p.keys, ", ")
			if keys != "" {
				keys += ", "
			}
			var mv, sv []string
			for _, v := range p.vals {
				mv = append(mv, fmt.Sprintf("CAST(%s_%s AS DECIMAL(28,4))", v, strings.ToLower(ccy)))
				sv = append(sv, fmt.Sprintf("CAST(%s_outccy AS DECIMAL(28,4))", v))
			}
			single := fmt.Sprintf(p.single, ccy)
			m := "SELECT " + keys + strings.Join(mv, ", ") + " FROM " + p.multi
			s := "SELECT " + keys + strings.Join(sv, ", ") + " FROM " + single
			var rows, onlyMulti, onlySingle int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM `+p.multi+`),
                    (SELECT COUNT(*) FROM (`+m+` EXCEPT ALL `+s+`)),
                    (SELECT COUNT(*) FROM (`+s+` EXCEPT ALL `+m+`))`).
				Scan(&rows, &onlyMulti, &onlySingle); err != nil {
				t.Fatalf("%s vs %s: %v", p.multi, single, err)
			}
			if rows == 0 {
				t.Errorf("%s is empty; the comparison proves nothing", p.multi)
			}
			if onlyMulti != 0 || onlySingle != 0 {
				t.Errorf("%s %s columns differ from %s: %d/%d rows",
					p.multi, ccy, single, onlyMulti, onlySingle)
			}
		}
	}

	// GBP resolves where a rate path exists and stays NULL where none does.
	var gbp, xau sql.NullString
	if err := db.QueryRow(`
        SELECT max(CAST(value_gbp AS VARCHAR)) FILTER (WHERE currency = 'JPY'),
               max(CAST(value_gbp AS VARCHAR)) FILTER (WHERE currency = 'XAU')
          FROM report_positions_multi(`+max+`)`).Scan(&gbp, &xau); err != nil {
		t.Fatalf("GBP spot check: %v", err)
	}
	if !gbp.Valid {
		t.Error("a JPY position has no GBP value; the CHF bridge did not resolve")
	}
	if xau.Valid {
		t.Errorf("an XAU position has GBP value %s with no rate to reach it", xau.String)
	}
}

// TestMigration0114DDLIsRerunnable holds the helper and the re-issued
// macros and views to the replay bar, and confirms each still answers.
func TestMigration0114DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportingFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0114_reporting_currencies_gbp.sql")

	for _, q := range []string{
		"fx_reporting_legs", "fx_reporting_bridges", "web_sources_latest",
		"report_sources_history_multi()", "report_transactions_multi(0, 9223372036854775807)",
	} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + q).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", q, err)
		}
	}
}
