package gold

import (
	"testing"
)

// TestStatusIncomeCounters pins the income half of `status -v`: the
// backlog counter counts what a report SHOWS as uncategorised, and the
// catch-all counter is not duplicated per family.
func TestStatusIncomeCounters(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('inc-src', 'chase', '/tmp/s.db', -1, 0, 0);
    `); err != nil {
		t.Fatalf("register the source: %v", err)
	}
	seedIncomeFixture(t, db, ctx)

	var s SourceStatus
	s.SilverSourceID = "inc-src"
	if err := spendDrift(ctx, db, &s); err != nil {
		t.Fatalf("spendDrift: %v", err)
	}

	// The backlog is the base's unplaced rows. A floored dividend is
	// NOT one: the floor placed it, and asking a model about it would
	// be work with a known answer.
	var want int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM income_lines_base(0, ?)
         WHERE income_detailed IS NULL`, MaxEpoch).Scan(&want); err != nil {
		t.Fatalf("count the backlog: %v", err)
	}
	if s.UncategorizedIncomeCount != want {
		t.Errorf("UncategorizedIncomeCount = %d, want %d", s.UncategorizedIncomeCount, want)
	}
	if want == 0 {
		t.Fatal("the fixture has no unplaced income rows; the counter proves nothing")
	}
	var floored int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM income_lines_base(0, ?)
         WHERE income_detailed IS NOT NULL`, MaxEpoch).Scan(&floored); err != nil {
		t.Fatalf("count the placed rows: %v", err)
	}
	if s.UncategorizedIncomeCount >= floored+want {
		t.Error("the backlog counts placed rows; it must count only what a report shows as uncategorised")
	}

	// The catch-all counter is the one number both families share, and
	// it is computed over SPENDING's scope. The fixture's `other` and
	// `journal` rows sit on an account both scopes include, so it sees
	// them.
	if s.ExcludedUnmappedCount != 2 {
		t.Errorf("ExcludedUnmappedCount = %d, want 2 (one `other`, one `journal`)", s.ExcludedUnmappedCount)
	}
}
