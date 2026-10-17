package gold

import "testing"

// TestWebSourcesLatestMatchesTheMacro pins that the view splits each
// source by tax wrapper and asset class and nothing else: summed per
// source, the same snapshot and the same value columns
// report_sources_multi returns as of the latest snapshot, and every
// source the macro reports with lines. The Wealth
// Overview's headline figures read it, and they promise the numbers
// `wealthdb holdings sources` prints.
func TestWebSourcesLatestMatchesTheMacro(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	var n, differ int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT silver_source_id) FROM web_sources_latest`).Scan(&n); err != nil {
		t.Fatalf("read web_sources_latest: %v", err)
	}
	if n == 0 {
		t.Fatal("web_sources_latest is empty; the comparison below proves nothing")
	}
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM (
            SELECT CAST(epoch(MAX(snapshot_at)) AS BIGINT), silver_source_id,
                   COALESCE(SUM(positions_value_usd), 0), COALESCE(SUM(cash_balance_usd), 0),
                   COALESCE(SUM(total_value_usd), 0), COALESCE(SUM(positions_value_chf), 0),
                   COALESCE(SUM(cash_balance_chf), 0), COALESCE(SUM(total_value_chf), 0),
                   COALESCE(SUM(positions_value_eur), 0), COALESCE(SUM(cash_balance_eur), 0),
                   COALESCE(SUM(total_value_eur), 0), COALESCE(SUM(positions_value_gbp), 0),
                   COALESCE(SUM(cash_balance_gbp), 0), COALESCE(SUM(total_value_gbp), 0)
              FROM web_sources_latest
             GROUP BY silver_source_id
            EXCEPT
            SELECT snapshot_at, silver_source_id,
                   COALESCE(positions_value_usd, 0), COALESCE(cash_balance_usd, 0),
                   COALESCE(total_value_usd, 0), COALESCE(positions_value_chf, 0),
                   COALESCE(cash_balance_chf, 0), COALESCE(total_value_chf, 0),
                   COALESCE(positions_value_eur, 0), COALESCE(cash_balance_eur, 0),
                   COALESCE(total_value_eur, 0), COALESCE(positions_value_gbp, 0),
                   COALESCE(cash_balance_gbp, 0), COALESCE(total_value_gbp, 0)
              FROM report_sources_multi(9223372036854775807))`).Scan(&differ); err != nil {
		t.Fatalf("compare with report_sources_multi: %v", err)
	}
	if differ != 0 {
		t.Errorf("%d web_sources_latest row(s) differ from report_sources_multi", differ)
	}

	// The reverse direction: no source the macro reports with lines may be
	// missing from the view. A source with no lines at all reads snapshot_at
	// 0 in the macro and has no row to split, so it is left out.
	var dropped int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM (
            SELECT silver_source_id FROM report_sources_multi(9223372036854775807)
             WHERE snapshot_at > 0
            EXCEPT
            SELECT DISTINCT silver_source_id FROM web_sources_latest)`).Scan(&dropped); err != nil {
		t.Fatalf("compare report_sources_multi with the view: %v", err)
	}
	if dropped != 0 {
		t.Errorf("%d source(s) report_sources_multi reports are missing from web_sources_latest", dropped)
	}
}

// TestMigration0113DDLIsRerunnable holds the serving view to the replay
// bar, and confirms it still answers afterwards.
func TestMigration0113DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0113_web_sources_latest.sql")

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_sources_latest`).Scan(&n); err != nil {
		t.Errorf("web_sources_latest after re-run: %v", err)
	}
}
