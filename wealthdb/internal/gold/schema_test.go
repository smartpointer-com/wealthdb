package gold

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"
)

// mustExec runs one statement and fails the test on an error.
func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// execEach runs each statement in turn (mustExec).
func execEach(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		mustExec(t, db, q)
	}
}

// utcMidnight is the epoch second of the day's UTC midnight.
func utcMidnight(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// fmtFloatPtr is a nullable float for a failure message, "-" for NULL.
// It keeps every digit, so two renders compare bit for bit.
func fmtFloatPtr(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'g', 17, 64)
}

// fmtStrPtr is a nullable string for a failure message, "-" for NULL.
func fmtStrPtr(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// scanStrings is the one column q reads, a string a row.
func scanStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	var out []string
	if err := scanAll(context.Background(), db, q, func(rows *sql.Rows) error {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	}, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// scanPairs is the two columns q reads, key and value, as a map.
func scanPairs(t *testing.T, db *sql.DB, q string, args ...any) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := scanAll(context.Background(), db, q, func(rows *sql.Rows) error {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		out[k] = v
		return nil
	}, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// rowsDigest is every row sqlText reads, each cast to text, in text
// order, joined: two reads of one answer compare as one string. "" for
// none.
func rowsDigest(t *testing.T, ctx context.Context, q querier, sqlText string) string {
	t.Helper()
	var s sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT string_agg(CAST(x AS VARCHAR), ';' ORDER BY CAST(x AS VARCHAR))
	    FROM (`+sqlText+`) x`).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	return s.String
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

	ms, err := migrations()
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

// TestMultiStatementExecRunsEachOnce pins the driver behaviour Migrate
// rests on: one Exec of several statements runs each of them exactly
// once.
func TestMultiStatementExecRunsEachOnce(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mustExec(t, db, `CREATE TABLE exec_once (n INTEGER);
		INSERT INTO exec_once VALUES (1);
		INSERT INTO exec_once VALUES (2);`)
	got := scanStrings(t, db, `SELECT n::VARCHAR FROM exec_once ORDER BY n`)
	if !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("rows = %v, want [1 2]", got)
	}
}

// TestMigrateCreatesAllTables verifies every table named in
// docs/DESIGN.md §7.2 is present after migration.
func TestMigrateCreatesAllTables(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
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

// currentSchemaVersion reads a database with no schema_meta as version
// 0, and returns any other failure to read it: a schema_meta it cannot
// read is no version 0, which would have Migrate replay every
// migration over it.
func TestCurrentSchemaVersionReturnsAnUnreadableSchemaMeta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := currentSchemaVersion(ctx, db); v != 0 || err != nil {
		t.Errorf("no schema_meta: version %d, err %v; want 0, nil", v, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_meta (applied_at BIGINT)`); err != nil {
		t.Fatal(err)
	}
	if v, err := currentSchemaVersion(ctx, db); err == nil {
		t.Errorf("a schema_meta without its version column: version %d, err nil; want an error", v)
	}
}

// TestAccountKindCheckAdmitsCard exercises migration 0037's widened
// CHECK at the DDL level, below Writer's Go-side validation: 'card'
// is admitted alongside the kinds 0036 already allowed, and an
// unrecognised kind is still refused by the constraint.
func TestAccountKindCheckAdmitsCard(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	assertNothingNames(t, db, ctx, append([]string{"asset_class_new"}, dropped0124...)...)
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

// assertNothingNames checks that no view or macro definition names any
// of names, as a whole word. A view or macro still naming a dropped
// object would fail only when read, and a view's own definition names
// the view, so a surviving view fails too. The word boundary keeps a
// longer sibling (a _multi macro) out of the match.
func assertNothingNames(t *testing.T, db *sql.DB, ctx context.Context, names ...string) {
	t.Helper()
	res := make([]*regexp.Regexp, len(names))
	for i, n := range names {
		res[i] = regexp.MustCompile(`\b` + n + `\b`)
	}
	rows, err := db.QueryContext(ctx, `
        SELECT view_name, sql FROM duckdb_views() WHERE NOT internal
        UNION ALL
        SELECT function_name, macro_definition FROM duckdb_functions()
         WHERE function_type IN ('macro', 'table_macro') AND NOT internal`)
	if err != nil {
		t.Fatalf("read definitions: %v", err)
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		read++
		var name string
		var body sql.NullString
		if err := rows.Scan(&name, &body); err != nil {
			t.Fatalf("scan definition: %v", err)
		}
		for i, re := range res {
			if re.MatchString(body.String) {
				t.Errorf("%s still names %s", name, names[i])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate definitions: %v", err)
	}
	if read < 100 {
		t.Fatalf("read %d view and macro definitions; the scan is vacuous", read)
	}
}

// TestMigration0124DropsOverRows applies 0124 to a database that holds
// instruments and positions, as a live gold does, and keeps their rows.
func TestMigration0124DropsOverRows(t *testing.T) {
	t.Parallel()
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
