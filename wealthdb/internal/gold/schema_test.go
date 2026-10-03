package gold

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// rerunMigrationDDL re-executes everything in a migration ahead of its
// schema_meta stamp — that INSERT would collide on the version primary
// key, and schema.go's Migrate contract puts it last. It pins that an
// additive migration's DDL can be replayed against an already-migrated
// database.
func rerunMigrationDDL(t *testing.T, db *sql.DB, ctx context.Context, file string) {
	t.Helper()
	body, err := fs.ReadFile(migrationsFS, "migrations/"+file)
	if err != nil {
		t.Fatalf("read embedded migration %s: %v", file, err)
	}
	ddl, _, found := strings.Cut(string(body), "INSERT INTO schema_meta")
	if !found {
		t.Fatalf("migration %s has no schema_meta stamp", file)
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Errorf("re-applying %s DDL: %v", file, err)
	}
}

// openAtVersion opens an in-memory gold database migrated through
// version and no further: the database the next migration meets. Open
// would carry it to the latest version, so the migrations are applied
// here one by one, each in its own transaction as Migrate applies it.
func openAtVersion(t *testing.T, version int) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	ms, err := listMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	reached := 0
	for _, m := range ms {
		if m.version > version {
			break
		}
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply migration %04d: %v", m.version, err)
		}
		reached = m.version
	}
	if reached != version {
		t.Fatalf("migrated through %04d; there is no migration %04d", reached, version)
	}
	return db, ctx
}

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

// TestAccountKindCheckAdmitsCard exercises migration 0037's widened
// CHECK at the DDL level, below Writer's Go-side validation: 'card'
// is admitted alongside the kinds 0036 already allowed, and an
// unrecognised kind is still refused by the constraint.
func TestAccountKindCheckAdmitsCard(t *testing.T) {
	db, ctx := openMigrated(t)

	insert := func(id, kind string) error {
		_, err := db.ExecContext(ctx, `
            INSERT INTO accounts (
                silver_source_id, account_external_id, account_kind,
                first_seen_at, last_seen_at
            ) VALUES ('test-src', ?, ?, 1, 1)`, id, kind)
		return err
	}

	for _, kind := range []string{"card", "cash", "mortgage", "donor_advised_fund"} {
		if err := insert("ACC-"+kind, kind); err != nil {
			t.Errorf("account_kind %q rejected by CHECK: %v", kind, err)
		}
	}
	for _, kind := range []string{"credit_card", "garbage", ""} {
		if err := insert("REJ-"+kind, kind); err == nil {
			t.Errorf("account_kind %q accepted, want CHECK violation", kind)
		}
	}
}

// TestTransactionEnrichmentColumns pins migration 0038: `counterparty`
// and `provider_category` exist on transactions as nullable TEXT.
// Nullable matters — every pre-existing row and every non-card source
// leaves them unset.
func TestTransactionEnrichmentColumns(t *testing.T) {
	db, ctx := openMigrated(t)

	for _, col := range []string{"counterparty", "provider_category"} {
		var dataType, nullable string
		err := db.QueryRowContext(ctx, `
            SELECT data_type, is_nullable
              FROM information_schema.columns
             WHERE table_name = 'transactions' AND column_name = ?`, col).
			Scan(&dataType, &nullable)
		if err != nil {
			t.Errorf("transactions.%s: %v", col, err)
			continue
		}
		if dataType != "VARCHAR" {
			t.Errorf("transactions.%s data_type = %q, want VARCHAR", col, dataType)
		}
		if nullable != "YES" {
			t.Errorf("transactions.%s is_nullable = %q, want YES", col, nullable)
		}
	}
}

// TestMigration0038DDLIsRerunnable proves the IF NOT EXISTS on 0038's
// ALTERs is load-bearing rather than decorative: a bare ADD COLUMN
// would raise "column already exists" when the body is replayed.
// Re-executing the migration's DDL against an already-migrated DB must
// stay clean.
func TestMigration0038DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0038_transactions_enrichment_columns.sql")
}

// TestMigration0039DDLIsRerunnable holds 0039 to the same bar: it drops and
// re-creates web_transactions around the macro swap, so the DROP must be
// IF EXISTS and the CREATEs OR REPLACE for a replay to be harmless.
func TestMigration0039DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0039_report_transactions_account_kind.sql")

	// The view must still be there and still expose account_kind.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_name = 'web_transactions' AND column_name = 'account_kind'`).Scan(&n); err != nil {
		t.Fatalf("web_transactions.account_kind: %v", err)
	}
	if n != 1 {
		t.Errorf("web_transactions.account_kind columns = %d, want 1", n)
	}
}
