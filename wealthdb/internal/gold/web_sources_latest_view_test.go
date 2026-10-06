package gold

import "testing"

// TestWebSourcesLatestMatchesTheMacro pins that the view adds a
// rendering and nothing else: per source, the same snapshot and the same
// value trio report_sources_multi returns as of the latest snapshot. The
// Wealth Overview's headline figures read it, and they promise the
// numbers `wealthdb holdings sources` prints.
func TestWebSourcesLatestMatchesTheMacro(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	var n, want, differ int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_sources_latest`).Scan(&n); err != nil {
		t.Fatalf("read web_sources_latest: %v", err)
	}
	if n == 0 {
		t.Fatal("web_sources_latest is empty; the comparison below proves nothing")
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM report_sources_multi(9223372036854775807)`).Scan(&want); err != nil {
		t.Fatalf("read report_sources_multi: %v", err)
	}
	if n != want {
		t.Errorf("web_sources_latest has %d rows, report_sources_multi %d", n, want)
	}
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM (
            SELECT CAST(epoch(snapshot_at) AS BIGINT), silver_source_id,
                   positions_value_usd, cash_balance_usd, total_value_usd,
                   positions_value_chf, cash_balance_chf, total_value_chf,
                   positions_value_eur, cash_balance_eur, total_value_eur
              FROM web_sources_latest
            EXCEPT
            SELECT snapshot_at, silver_source_id,
                   positions_value_usd, cash_balance_usd, total_value_usd,
                   positions_value_chf, cash_balance_chf, total_value_chf,
                   positions_value_eur, cash_balance_eur, total_value_eur
              FROM report_sources_multi(9223372036854775807))`).Scan(&differ); err != nil {
		t.Fatalf("compare with report_sources_multi: %v", err)
	}
	if differ != 0 {
		t.Errorf("%d web_sources_latest row(s) differ from report_sources_multi", differ)
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
