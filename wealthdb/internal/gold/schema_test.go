package gold

import (
	"context"
	"database/sql"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// enumChecksDropped is the migration that recreates silver_sources and
// accounts without their enum CHECKs.
const enumChecksDropped = 188

// enumCheckTables are the two tables that migration recreates.
const enumCheckTables = `('silver_sources', 'accounts')`

// TestEnumChecksDroppedKeepsRowsAndShape applies the migration to a gold
// one version below it that holds rows in both tables and the derived
// set, as a live gold does. Every row, every column's place, type,
// default and NOT NULL, and every constraint but the CHECKs stay equal.
// A value outside a vocabulary then reaches the table, and the writer
// still refuses it.
func TestEnumChecksDroppedKeepsRowsAndShape(t *testing.T) {
	t.Parallel()
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(m migration) bool { return m.version == enumChecksDropped })
	if i < 1 {
		t.Fatalf("no migration %04d above another", enumChecksDropped)
	}
	db, ctx := openAtVersion(t, ms[i-1].version)
	set, err := derived()
	if err != nil {
		t.Fatal(err)
	}
	// The derived set reads the vocabulary's roles and the snapshot
	// coverage, which later migrations add; they touch neither table.
	for _, suffix := range []string{"_vocabulary_roles.sql", "_snapshot_coverage.sql"} {
		j := slices.IndexFunc(ms, func(m migration) bool { return strings.HasSuffix(m.name, suffix) })
		if j < 0 {
			t.Fatalf("no migration *%s", suffix)
		}
		if err := applyMigration(ctx, db, ms[j]); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyDerived(ctx, db, set, ms[i-1].version, enumChecksDropped-1); err != nil {
		t.Fatalf("apply the derived set: %v", err)
	}
	execEach(t, db,
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
		    high_watermark, first_loaded_at, last_loaded_at)
		    VALUES ('src-a', 'schwab', '/silver/a.db', -1, 10, 20)`,
		`INSERT INTO silver_sources VALUES ('src-b', 'viac', '/silver/b.db', 7, 11, 21, 2, 30, 40)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
		    first_seen_at, last_seen_at)
		    VALUES ('src-a', 'ACC-1', 'brokerage', 1, 2)`,
		`INSERT INTO accounts VALUES ('src-b', 'ACC-2', 'custody', 'Name', 'CHF', 'REL',
		    'Nick', 'Category', 'PF-1', 'pillar_3a', 'automated', 3, 4, '{"k": [1, "v"]}')`,
	)

	shape := func() map[string]string {
		return map[string]string{
			"columns": rowsDigest(t, ctx, db, `SELECT table_name, column_index, column_name,
			    data_type, is_nullable, column_default
			    FROM duckdb_columns() WHERE table_name IN `+enumCheckTables),
			"constraints": rowsDigest(t, ctx, db, `SELECT table_name, constraint_type,
			    constraint_column_names
			    FROM duckdb_constraints()
			    WHERE table_name IN `+enumCheckTables+` AND constraint_type <> 'CHECK'`),
			"indexes": rowsDigest(t, ctx, db, `SELECT table_name, index_name
			    FROM duckdb_indexes() WHERE table_name IN `+enumCheckTables),
			"silver_sources": rowsDigest(t, ctx, db, `SELECT * FROM silver_sources`),
			"accounts":       rowsDigest(t, ctx, db, `SELECT * FROM accounts`),
		}
	}
	checks := func() string {
		return rowsDigest(t, ctx, db, `SELECT table_name, constraint_column_names
		    FROM duckdb_constraints()
		    WHERE table_name IN `+enumCheckTables+` AND constraint_type = 'CHECK'`)
	}
	want := shape()
	if checks() == "" {
		t.Fatal("no CHECK before the migration: the test reads nothing")
	}

	if err := applyMigration(ctx, db, ms[i]); err != nil {
		t.Fatalf("apply migration %04d: %v", enumChecksDropped, err)
	}

	got := shape()
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if got[k] != want[k] {
			t.Errorf("%s changed:\n got %s\nwant %s", k, got[k], want[k])
		}
	}
	if c := checks(); c != "" {
		t.Errorf("CHECKs survive: %s", c)
	}

	// The gate is the writer's: the tables take any text.
	execEach(t, db,
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
		    high_watermark, first_loaded_at, last_loaded_at)
		    VALUES ('src-c', 'not_a_kind', '/silver/c.db', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
		    tax_wrapper, management_style, first_seen_at, last_seen_at)
		    VALUES ('src-c', 'ACC-3', 'not_a_kind', 'not_a_wrapper', 'not_a_style', 1, 1)`,
	)
	bad, wrapper, style := canonical.AccountKind("not_a_kind"),
		canonical.TaxWrapper("not_a_wrapper"), canonical.ManagementStyle("not_a_style")
	for name, a := range map[string]canonical.AccountChange{
		"account_kind":     {AccountKind: bad},
		"tax_wrapper":      {AccountKind: canonical.AccountKindCash, TaxWrapper: &wrapper},
		"management_style": {AccountKind: canonical.AccountKindCash, ManagementStyle: &style},
	} {
		a.SilverSourceID, a.AccountExternalID, a.FirstSeenAt, a.LastSeenAt = "src-a", "ACC-4", 1, 1
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewWriter(tx).UpsertAccounts(ctx, []canonical.AccountChange{a}); err == nil {
			t.Errorf("the writer took an invalid %s", name)
		}
		_ = tx.Rollback()
	}
}

// transactionAction is the migration that adds transactions.action and
// fills it for the rows already loaded.
const transactionAction = 192

// TestTransactionActionFillsLoadedRows applies the migration to a gold
// one version below it, holding rows as a load wrote them before: their
// action is in their own words. Each row of the three kinds that state
// one gets the action its adapter writes, a ledger row and every other
// kind's row none.
func TestTransactionActionFillsLoadedRows(t *testing.T) {
	t.Parallel()
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(m migration) bool { return m.version == transactionAction })
	if i < 1 {
		t.Fatalf("no migration %04d above another", transactionAction)
	}
	db, ctx := openAtVersion(t, ms[i-1].version)
	execEach(t, db,
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at,
		    last_loaded_at) VALUES ('ct', 'cointracking', '/s/ct', -1, 0, 0), ('fid', 'fidelity', '/s/f', -1, 0, 0),
		    ('sch', 'schwab', '/s/s', -1, 0, 0), ('pla', 'plaid', '/s/p', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
		    VALUES ('ct', 'A', 'crypto', 1, 1), ('fid', 'A', 'brokerage', 1, 1), ('sch', 'A', 'brokerage', 1, 1),
		    ('pla', 'A', 'brokerage', 1, 1)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, description, payload) VALUES
		    ('ct', 'dep', 1, 'A', 'transfer_in', 'BTC', NULL, '{"type": "Deposit"}'),
		    ('ct', 'drop', 1, 'A', 'transfer_in', 'XYZ', NULL, '{"type": "Airdrop"}'),
		    ('ct', 'dust-in', 1, 'A', 'transfer_in', 'BTC', NULL,
		     '{"type": "Income (non taxable)", "comment": " dust SWEEPING\u00a0"}'),
		    ('ct', 'move-in', 1, 'A', 'transfer_in', 'BTC', NULL, '{"type": "Income (non taxable)", "comment": "x"}'),
		    ('ct', 'dust-out', 1, 'A', 'other', 'BNB', 'Other Expense',
		     '{"type": "Other Expense", "comment": "Dust Sweeping"}'),
		    ('ct', 'stolen', 1, 'A', 'other', 'BTC', 'Stolen', '{"type": "Stolen"}'),
		    ('ct', 'fee-own', 1, 'A', 'buy', 'USD', NULL,
		     '{"type": "Trade", "buy_currency": "BTC", "sell_currency": "USD", "fee_amount": 1, "fee_currency": "usd"}'),
		    ('ct', 'fee-third', 1, 'A', 'sell', 'USD', NULL,
		     '{"type": "Trade", "buy_currency": "USD", "sell_currency": "ETH", "fee_amount": 1, "fee_currency": "BNB"}'),
		    ('ct', 'split:s', 1, 'A', 'sell', 'USD', NULL,
		     '{"type": "Trade", "buy_currency": "ETH", "sell_currency": "BTC", "fee_amount": 1, "fee_currency": "BNB"}'),
		    ('ct', 'split:b', 1, 'A', 'buy', 'USD', NULL,
		     '{"type": "Trade", "buy_currency": "ETH", "sell_currency": "BTC", "fee_amount": 1, "fee_currency": "BNB"}'),
		    ('ct', 'new-type', 1, 'A', 'other', 'USD', 'Margin Profit', '{"type": "Margin Profit"}'),
		    ('ct', 'ledger', 1, 'A', 'transfer_in', 'BTC', NULL,
		     '{"type": "Airdrop", "equity_transfer_ledger": "true"}'),
		    ('fid', 'roc', 1, 'A', 'corporate_action', 'USD', 'RETURN OF CAPITAL EXAMPLE CORP', NULL),
		    ('fid', 'lieu', 1, 'A', 'corporate_action', 'USD', 'In Lieu Of FRX SHARE', NULL),
		    ('fid', 'merger', 1, 'A', 'corporate_action', 'USD', 'MERGER MER FROM 000000000', NULL),
		    ('fid', 'bare', 1, 'A', 'corporate_action', 'USD', NULL, NULL),
		    ('fid', 'option', 1, 'A', 'sell', 'USD', 'YOU SOLD CALL (XYZ) EXAMPLE CORP', NULL),
		    ('fid', 'sale', 1, 'A', 'sell', 'USD', 'YOU SOLD EXAMPLE CORP', NULL),
		    ('fid', 'ledger', 1, 'A', 'transfer_in', 'USD', 'PUT (XYZ) MOVED', '{"equity_transfer_ledger": "true"}'),
		    ('sch', 'sys', 1, 'A', 'other', 'USD', 'SYSTEM TRANSFER', '{"type": "TRADE"}'),
		    ('sch', 'memo', 1, 'A', 'other', 'USD', NULL, '{"type": "MEMORANDUM"}'),
		    ('sch', 'jnl-lieu', 1, 'A', 'corporate_action', 'USD', 'CASH IN LIEU OF FRACTIONAL SHARES',
		     '{"type": "JOURNAL"}'),
		    ('sch', 'web-lieu', 1, 'A', 'corporate_action', 'USD', 'EXAMPLE CORP', '{"Action": "Cash In Lieu"}'),
		    ('sch', 'stmt-roc', 1, 'A', 'corporate_action', 'USD', 'EXAMPLE CORP',
		     '{"Action": null, "action": "Return Of Capital"}'),
		    ('sch', 'rights', 1, 'A', 'corporate_action', 'USD', 'EXAMPLE CORP', '{"Action": "Litigation"}'),
		    ('sch', 'exchange', 1, 'A', 'corporate_action', 'USD', 'Exchange CALL EXAMPLE', '{}'),
		    ('sch', 'expired', 1, 'A', 'corporate_action', 'USD', 'Removed due to Expiration CALL', '{}'),
		    ('sch', 'split', 1, 'A', 'corporate_action', 'USD', 'EXAMPLE REVERSE SPLIT', '{"type": "RECEIVE_AND_DELIVER"}'),
		    ('sch', 'sale', 1, 'A', 'sell', 'USD', NULL, '{"type": "TRADE"}'),
		    ('pla', 'merger', 1, 'A', 'corporate_action', 'USD', 'RETURN OF CAPITAL', '{"type": "TRADE"}')`,
	)
	if err := applyMigration(ctx, db, ms[i]); err != nil {
		t.Fatalf("apply migration %04d: %v", transactionAction, err)
	}
	want := map[string]string{
		"ct/dep": "in", "ct/drop": "income", "ct/dust-in": "acquired", "ct/move-in": "in", "ct/dust-out": "converted",
		"ct/stolen": "lost", "ct/fee-third": "third_currency_fee", "ct/split:b": "third_currency_fee",
		"fid/roc": "return_of_capital", "fid/lieu": "cash_in_lieu", "fid/merger": "reorganisation",
		"fid/bare": "reorganisation", "fid/option": "ignore",
		"sch/sys": "ignore", "sch/jnl-lieu": "cash_in_lieu", "sch/web-lieu": "cash_in_lieu",
		"sch/stmt-roc": "return_of_capital", "sch/rights": "cash_only", "sch/exchange": "cash_only",
		"sch/expired": "expiry", "sch/split": "reorganisation",
	}
	rows, err := db.QueryContext(ctx, `SELECT silver_source_id || '/' || transaction_external_id, action FROM transactions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id string
		var a sql.NullString
		if err := rows.Scan(&id, &a); err != nil {
			t.Fatal(err)
		}
		if a.Valid {
			got[id] = a.String
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("actions\n got %v\nwant %v", got, want)
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
