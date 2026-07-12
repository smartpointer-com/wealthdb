package gold

import (
	"context"
	"fmt"
	"testing"
)

// latestSchemaVersion returns the version number of the highest
// embedded migration. Used by tests to assert post-Migrate state
// without hardcoding a version that drifts as new migrations land.
func latestSchemaVersion() (int, error) {
	ms, err := listMigrations()
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, fmt.Errorf("no migrations embedded")
	}
	return ms[len(ms)-1].version, nil
}

// TestMigrateAppliesSchema is the smoke test that proves the
// DuckDB driver loads, the embedded migrations run end-to-end,
// and schema_meta records the latest applied version.
func TestMigrateAppliesSchema(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want, err := latestSchemaVersion()
	if err != nil {
		t.Fatalf("latestSchemaVersion: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(gold_schema_version) FROM schema_meta`).Scan(&version); err != nil {
		t.Fatalf("read schema_meta: %v", err)
	}
	if version != want {
		t.Errorf("schema version = %d, want %d", version, want)
	}
}

// TestMigrateIdempotent verifies that a second Migrate call against
// an already-migrated database is a no-op (no re-application of
// already-applied migrations).
func TestMigrateIdempotent(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	var before int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_meta`).Scan(&before); err != nil {
		t.Fatalf("count schema_meta (before): %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate (should be no-op): %v", err)
	}

	var after int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_meta`).Scan(&after); err != nil {
		t.Fatalf("count schema_meta (after): %v", err)
	}
	if after != before {
		t.Errorf("schema_meta row count = %d, want %d (idempotent)", after, before)
	}
}

// TestMigrateCreatesAllTables verifies every table named in
// docs/DESIGN.md §7.2 is present after migration.
func TestMigrateCreatesAllTables(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := []string{
		"schema_meta",
		"silver_sources",
		"load_audit",
		"accounts",
		"instruments",
		"positions",
		"cash_balances",
		"fx_rates",
		"transactions",
	}
	for _, table := range want {
		var n int
		// COUNT(*) verifies both that the table exists and that
		// any DDL inside it (CHECK constraints, FKs) at least
		// don't block reads of an empty table.
		query := "SELECT COUNT(*) FROM " + table
		if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Errorf("expected table %q to exist: %v", table, err)
			continue
		}
		if n != 0 && table != "schema_meta" {
			t.Errorf("expected table %q to be empty, got %d rows", table, n)
		}
	}
}
