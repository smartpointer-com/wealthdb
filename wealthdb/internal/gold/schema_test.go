package gold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
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

// TestCheckSchemaKnown: a database a later build has migrated past this
// binary's newest migration reads as stale, with or without a build
// stamp; one at the binary's own schema does not.
func TestCheckSchemaKnown(t *testing.T) {
	db, ctx := openMigrated(t)
	if err := CheckSchemaKnown(ctx, db); err != nil {
		t.Fatalf("a database at this binary's schema: %v", err)
	}
	ms, err := listMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_meta (gold_schema_version, applied_at) VALUES (?, 0)`,
		ms[len(ms)-1].version+1); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchemaKnown(ctx, db); !errors.Is(err, ErrStaleBinary) {
		t.Errorf("a database one migration ahead: err = %v, want ErrStaleBinary", err)
	}
}

// dropped0124 are the macros migration 0124 drops.
var dropped0124 = []string{
	"report_global_history", "report_sources_history",
	"report_portfolios_history", "report_positions_history",
	"report_spending_summary_multi", "report_spending_categories_multi",
	"report_income_types_multi",
}

// assertGone0124 checks that what 0124 drops is gone, that nothing left
// in the schema names it, and that instruments keeps both its indexes.
func assertGone0124(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	for _, name := range dropped0124 {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM duckdb_functions() WHERE function_name = ?`, name).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", name, err)
		}
		if n != 0 {
			t.Errorf("macro %s survived", name)
		}
	}
	var cols int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM duckdb_columns()
         WHERE table_name IN ('instruments', 'positions') AND column_name = 'asset_class_new'`).Scan(&cols); err != nil {
		t.Fatalf("look up asset_class_new: %v", err)
	}
	if cols != 0 {
		t.Errorf("asset_class_new survives on %d table(s)", cols)
	}
	// A view or macro still naming a dropped object would fail only when
	// read; the word boundary keeps the _multi siblings out of the match.
	rows, err := db.QueryContext(ctx, `
        SELECT view_name, sql FROM duckdb_views() WHERE NOT internal
        UNION ALL
        SELECT function_name, macro_definition FROM duckdb_functions()
         WHERE function_type IN ('macro', 'table_macro') AND NOT internal`)
	if err != nil {
		t.Fatalf("read definitions: %v", err)
	}
	defer rows.Close()
	names := append([]string{"asset_class_new"}, dropped0124...)
	read := 0
	for rows.Next() {
		read++
		var name string
		var body sql.NullString
		if err := rows.Scan(&name, &body); err != nil {
			t.Fatalf("scan definition: %v", err)
		}
		for _, dropped := range names {
			if regexp.MustCompile(`\b` + dropped + `\b`).MatchString(body.String) {
				t.Errorf("%s still names %s", name, dropped)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate definitions: %v", err)
	}
	if read < 100 {
		t.Fatalf("read %d view and macro definitions; the scan is vacuous", read)
	}
	var idx int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM duckdb_indexes()
         WHERE table_name = 'instruments'
           AND index_name IN ('ix_instruments_isin', 'ix_instruments_symbol')`).Scan(&idx); err != nil {
		t.Fatalf("look up indexes: %v", err)
	}
	if idx != 2 {
		t.Errorf("instruments carries %d of its two indexes", idx)
	}
}

// TestMigration0124DropsOverRows applies 0124 to a database that holds
// instruments and positions, as a live gold does, and keeps their rows.
func TestMigration0124DropsOverRows(t *testing.T) {
	db, ctx := openAtVersion(t, 123)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at)
            VALUES ('test-src', 'schwab', '/tmp/test.db', -1, 0, 0);
        INSERT INTO instruments(silver_source_id, instrument_external_id, asset_class,
            isin, symbol, first_seen_at, last_seen_at, asset_class_new, vehicle)
            VALUES ('test-src', 'I1', 'public_equity', 'XX0000000000', 'VTI', 1, 1, 'public_equity', 'etf');
        INSERT INTO positions(silver_source_id, snapshot_at, account_external_id,
            position_key, instrument_external_id, asset_class, currency, market_value,
            asset_class_new, vehicle)
            VALUES ('test-src', 1000, 'A1', 'I1', 'I1', 'public_equity', 'USD', 100, 'public_equity', 'etf')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertGone0124(t, db, ctx)
	var symbol, vehicle string
	if err := db.QueryRowContext(ctx, `
        SELECT i.symbol, p.vehicle FROM positions p
          JOIN instruments i USING (silver_source_id, instrument_external_id)`).Scan(&symbol, &vehicle); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if symbol != "VTI" || vehicle != "etf" {
		t.Errorf("rows after the drop = %s/%s, want VTI/etf", symbol, vehicle)
	}
}

// TestMigration0124DDLIsRerunnable holds the drops to the replay bar.
func TestMigration0124DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0124_drop_unread_reports.sql")
	assertGone0124(t, db, ctx)
}
