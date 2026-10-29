package gold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// rerunMigrationDDL re-executes everything in a migration ahead of its
// schema_meta stamp — that INSERT would collide on the version primary
// key, and schema.go's Migrate contract puts it last. It pins that an
// additive migration's DDL can be replayed against an already-migrated
// database. A replayed macro or view goes back to that migration's body,
// so each later migration that re-issues one replays after it, in
// version order, as Migrate applied them: a rerun test asserts against
// the schema at head. A later migration that drops a replayed macro or
// view replays after it too, so the dropped object is gone again. The
// re-issues and drops are read off the embedded migrations, so a new
// one replays with no edit here. A later migration that only undoes a
// seed (0111 deletes rows 0069 seeds) re-issues nothing: the seed's
// rerun test replays it itself.
func rerunMigrationDDL(t *testing.T, db *sql.DB, ctx context.Context, file string) {
	t.Helper()
	ms, err := listMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	i := slices.IndexFunc(ms, func(m migration) bool { return m.name == file })
	if i < 0 {
		t.Fatalf("no embedded migration %s", file)
	}
	replayed := map[string]bool{}
	for j, m := range ms[i:] {
		ddl, _, found := strings.Cut(m.body, "INSERT INTO schema_meta")
		if !found {
			t.Fatalf("migration %s has no schema_meta stamp", m.name)
		}
		issued := reissuedNames(ddl)
		if j > 0 && !slices.ContainsFunc(issued, func(n string) bool { return replayed[n] }) {
			continue
		}
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Errorf("re-applying %s DDL: %v", m.name, err)
		}
		for _, n := range issued {
			replayed[n] = true
		}
	}
}

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

// reissueRE matches a macro or a view a migration's DDL creates,
// replaces or drops, and sqlCommentRE a comment, which names none.
var (
	reissueRE    = regexp.MustCompile(`(?i)\b(?:CREATE\s+OR\s+REPLACE\s+(?:MACRO|VIEW)|DROP\s+(?:MACRO\s+TABLE|MACRO|VIEW)\s+IF\s+EXISTS)\s+(\w+)`)
	sqlCommentRE = regexp.MustCompile(`--[^\n]*`)
)

// reissuedNames are the macros and views ddl creates, replaces or
// drops, lower case.
func reissuedNames(ddl string) []string {
	var out []string
	for _, m := range reissueRE.FindAllStringSubmatch(sqlCommentRE.ReplaceAllString(ddl, ""), -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
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

// TestMultiStatementExecRunsEachOnce pins the driver behaviour
// Migrate's replay contract rests on: one Exec of several statements
// runs each of them exactly once.
func TestMultiStatementExecRunsEachOnce(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatalf("Open: %v", err)
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
// IF EXISTS and the CREATEs OR REPLACE for a replay to be harmless. 0158
// drops the view again after it.
func TestMigration0039DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0039_report_transactions_account_kind.sql")
	assertTransactionsMacroAfterReplay(t, db, ctx)
}

// assertTransactionsMacroAfterReplay checks the schema at head after a
// replay of a migration that swaps the transaction macros around
// web_transactions: the macro still exposes account_kind, and nothing
// names the view 0158 drops.
func assertTransactionsMacroAfterReplay(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(account_kind) FROM report_transactions_multi(0, 9223372036854775807)`).Scan(&n); err != nil {
		t.Fatalf("report_transactions_multi.account_kind: %v", err)
	}
	assertNothingNames(t, db, ctx, "web_transactions")
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
