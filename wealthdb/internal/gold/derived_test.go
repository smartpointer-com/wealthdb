package gold

import (
	"context"
	"database/sql"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// mustDerived is the embedded derived set.
func mustDerived(t *testing.T) derivedSet {
	t.Helper()
	set, err := derived()
	if err != nil {
		t.Fatalf("derived set: %v", err)
	}
	return set
}

// From 0180 on, the migrations and the derived versions share one number
// line: together their numbers are unique and run from 180 to the newest
// with no gap. Migrate applies the migrations above a database's
// version, so a migration that took a derived version's number, or a
// number below a derived version already applied, would be skipped on
// every database at that version.
func TestMigrationsAndDerivedVersionsFormOneRun(t *testing.T) {
	t.Parallel()
	lock, err := readLock(derivedFS)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := listMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, m := range ms {
		if m.version >= 180 {
			numbers = append(numbers, m.version)
		}
	}
	for _, l := range lock {
		numbers = append(numbers, l.version)
	}
	sort.Ints(numbers)
	for i, n := range numbers {
		if n != 180+i {
			t.Fatalf("the numbers from 180 on are %v, want one run 180, 181, … with each number once", numbers)
		}
	}
}

// The LOCK's last line is the hash of the embedded files.
func TestDerivedLockHoldsTheFiles(t *testing.T) {
	t.Parallel()
	set := mustDerived(t)
	lock, err := readLock(derivedFS)
	if err != nil {
		t.Fatal(err)
	}
	if last := lock[len(lock)-1]; last.hash != set.hash {
		t.Errorf("derived/LOCK ends at %d %s; the files hash to %s: append \"%d %s\"",
			last.version, last.hash, set.hash, last.version+1, set.hash)
	}
}

// readDerived refuses a LOCK whose versions do not rise and a file
// statement that is no CREATE MACRO or CREATE VIEW.
func TestReadDerivedRefusesAMalformedSet(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("0", 64)
	for name, files := range map[string]map[string]string{
		"falling LOCK": {"derived/10_a.sql": "CREATE MACRO a() AS 1;\n", "derived/LOCK": "182 " + hash + "\n181 " + hash + "\n"},
		"short hash":   {"derived/10_a.sql": "CREATE MACRO a() AS 1;\n", "derived/LOCK": "181 abc\n"},
		"no number":    {"derived/10_a.sql": "CREATE MACRO a() AS 1;\n", "derived/LOCK": "next " + hash + "\n"},
		"a table":      {"derived/10_a.sql": "CREATE TABLE a (x INTEGER);\n", "derived/LOCK": "181 " + hash + "\n"},
		"no ;":         {"derived/10_a.sql": "CREATE MACRO a() AS 1\n", "derived/LOCK": "181 " + hash + "\n"},
	} {
		fsys := fstest.MapFS{}
		for p, body := range files {
			fsys[p] = &fstest.MapFile{Data: []byte(body)}
		}
		if _, err := readDerived(fsys); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
}

// sqlCode is sql with its comments and its string literals blanked out:
// the text that can name an object.
func sqlCode(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case sql[i] == '\'':
			for i++; i < len(sql); i++ {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
			b.WriteString("''")
		default:
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}

var sqlWordRE = regexp.MustCompile(`[A-Za-z_][A-Za-z_0-9]*`)

// sqlWords are the lower-case words of sql's code.
func sqlWords(sql string) map[string]bool {
	out := map[string]bool{}
	for _, w := range sqlWordRE.FindAllString(sqlCode(sql), -1) {
		out[strings.ToLower(w)] = true
	}
	return out
}

// Every derived object names only objects above it, in its own file or an
// earlier one, and each is defined once.
func TestDerivedObjectsNameOnlyEarlierOnes(t *testing.T) {
	t.Parallel()
	set := mustDerived(t)
	at := map[string]int{}
	for i, s := range set.stmts {
		if _, dup := at[s.name]; dup {
			t.Errorf("%s: %s is defined twice", s.file, s.name)
		}
		at[s.name] = i
	}
	if len(at) < 200 {
		t.Fatalf("read %d derived objects; the scan is vacuous", len(at))
	}
	for i, s := range set.stmts {
		for w := range sqlWords(derivedHeadRE.ReplaceAllString(stripComments(s.sql), "")) {
			if j, ok := at[w]; ok && j > i {
				t.Errorf("%s: %s names %s, which %s defines later", s.file, s.name, w, set.stmts[j].file)
			}
		}
	}
}

// migrationDerivedRE matches a statement that creates or drops a macro or
// a view.
var migrationDerivedRE = regexp.MustCompile(`(?i)\b(?:CREATE|DROP)\s+(?:OR\s+REPLACE\s+)?(?:TEMP(?:ORARY)?\s+)?(?:MACRO|VIEW)\b`)

// tableNameRE matches a table a migration creates.
var tableNameRE = regexp.MustCompile(`(?i)\bCREATE\s+(?:OR\s+REPLACE\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)

// migrationDerivedUses is what body does with the derived set: a macro or
// view it creates or drops, and each derived name it reads. A name that
// is also a table's is no derived object's.
func migrationDerivedUses(body string, derivedNames, tables map[string]bool) []string {
	var out []string
	if m := migrationDerivedRE.FindString(sqlCode(body)); m != "" {
		out = append(out, m)
	}
	for w := range sqlWords(body) {
		if derivedNames[w] && !tables[w] {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

// From 0180 on a migration holds tables and data moves only: it creates,
// drops and reads no macro and no view, which live in derived/.
func TestNoMigrationFrom0180TouchesADerivedObject(t *testing.T) {
	t.Parallel()
	set := mustDerived(t)
	names := map[string]bool{}
	for _, s := range set.stmts {
		names[s.name] = true
	}
	ms, err := listMigrations()
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]bool{}
	for _, m := range ms {
		for _, tm := range tableNameRE.FindAllStringSubmatch(sqlCode(m.body), -1) {
			tables[strings.ToLower(tm[1])] = true
		}
	}
	for _, m := range ms {
		if m.version < 180 {
			continue
		}
		if uses := migrationDerivedUses(m.body, names, tables); len(uses) > 0 {
			t.Errorf("%s touches the derived set: %v", m.name, uses)
		}
	}
	// The scan refuses what it is there to refuse.
	for _, body := range []string{
		"CREATE OR REPLACE MACRO x() AS 1;",
		"CREATE VIEW v AS SELECT 1;",
		"INSERT INTO t SELECT * FROM report_accounts_multi(0);",
	} {
		if len(migrationDerivedUses(body, names, tables)) == 0 {
			t.Errorf("the scan passes %q", body)
		}
	}
	if uses := migrationDerivedUses("-- report_accounts_multi\nALTER TABLE holding_aliases ADD COLUMN x TEXT;", names, tables); len(uses) > 0 {
		t.Errorf("the scan refuses a table change: %v", uses)
	}
}

// derivedCatalog is every macro and view of db's main schema: a key of
// kind, name and parameters, and the definition DuckDB gives back,
// normalized (normalizeDefinition).
func derivedCatalog(t *testing.T, ctx context.Context, q querier) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := scanAll(ctx, q, `
        SELECT function_type || ' ' || function_name || '(' || array_to_string(parameters, ', ') || ')',
               COALESCE(macro_definition, '')
          FROM duckdb_functions()
         WHERE database_name = current_database() AND schema_name = 'main' AND NOT internal
           AND function_type IN ('macro', 'table_macro')
        UNION ALL
        SELECT 'view ' || view_name, sql
          FROM duckdb_views()
         WHERE database_name = current_database() AND schema_name = 'main' AND NOT internal`,
		func(rows *sql.Rows) error {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			out[k] = normalizeDefinition(v)
			return nil
		}); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	return out
}

var (
	starListRE  = regexp.MustCompile(`(?i)\*\s*(?:EXCLUDE|REPLACE|RENAME)\s*\(`)
	nextListRE  = regexp.MustCompile(`(?i)^\s*(?:EXCLUDE|REPLACE|RENAME)\s*\(`)
	exponentRE  = regexp.MustCompile(`(\d)e([+-]?)0+(\d)`)
	textTypeRE  = regexp.MustCompile(`\bTEXT\b`)
	timestampRE = regexp.MustCompile(`\bTIMESTAMP_US\b`)
)

// normalizeDefinition maps the spellings DuckDB does not keep stable when
// it re-creates a definition: the order of the items of a star's
// EXCLUDE, REPLACE and RENAME lists, an exponent's leading zeros
// (1e-06, 1e-6), TEXT for VARCHAR and TIMESTAMP_US for TIMESTAMP.
func normalizeDefinition(def string) string {
	var b strings.Builder
	for {
		loc := starListRE.FindStringIndex(def)
		if loc == nil {
			b.WriteString(def)
			break
		}
		b.WriteString(def[:loc[1]])
		def = def[loc[1]:]
		for {
			items, rest := splitParenList(def)
			sort.Strings(items)
			b.WriteString(strings.Join(items, ", ") + ")")
			def = rest
			m := nextListRE.FindStringIndex(def)
			if m == nil {
				break
			}
			b.WriteString(def[:m[1]])
			def = def[m[1]:]
		}
	}
	s := exponentRE.ReplaceAllString(b.String(), "${1}e${2}${3}")
	s = textTypeRE.ReplaceAllString(s, "VARCHAR")
	return timestampRE.ReplaceAllString(s, "TIMESTAMP")
}

// splitParenList splits s, which follows a list's '(', into the list's
// top-level items, trimmed, and the text after its ')'.
func splitParenList(s string) ([]string, string) {
	var items []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"':
			q := s[i]
			for i++; i < len(s) && s[i] != q; i++ {
			}
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return append(items, strings.TrimSpace(s[start:i])), s[i+1:]
			}
			depth--
		case ',':
			if depth == 0 {
				items = append(items, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(items, strings.TrimSpace(s[start:])), ""
}

func TestNormalizeDefinition(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"SELECT * EXCLUDE (b, a) REPLACE ((x + 1) AS y, f(1, 2) AS c) FROM t": "SELECT * EXCLUDE (a, b) REPLACE ((x + 1) AS y, f(1, 2) AS c) FROM t",
		"SELECT t.* EXCLUDE (z, y) FROM t":                                    "SELECT t.* EXCLUDE (y, z) FROM t",
		"CAST(x AS TEXT) + 1e-06 + CAST(y AS TIMESTAMP_US)":                   "CAST(x AS VARCHAR) + 1e-6 + CAST(y AS TIMESTAMP)",
		"replace(s, 'b', 'a')":                                                "replace(s, 'b', 'a')",
	} {
		if got := normalizeDefinition(in); got != want {
			t.Errorf("normalizeDefinition(%q) = %q, want %q", in, got, want)
		}
	}
}

// schemaRows is schema_meta's versions and the derived hash.
func schemaRows(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows := scanStrings(t, db, `SELECT gold_schema_version::VARCHAR FROM schema_meta ORDER BY 1`)
	h, err := derivedHash(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return append(rows, h)
}

// assertCatalogsEqual fails on every object only one catalog holds or
// the two define apart.
func assertCatalogsEqual(t *testing.T, what string, a, b map[string]string) {
	t.Helper()
	for k, v := range a {
		w, ok := b[k]
		switch {
		case !ok:
			t.Errorf("%s: %s only before", what, k)
		case v != w:
			t.Errorf("%s: %s differs:\n  %s\n  %s", what, k, firstDiff(v, w), firstDiff(w, v))
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("%s: %s only after", what, k)
		}
	}
}

// firstDiff is a's text around where it first parts from b.
func firstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return a[max(0, i-60):min(len(a), i+60)]
}

// applyDerived over a database that already holds the set changes no
// object, no version and no hash.
func TestApplyDerivedTwiceChangesNothing(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	set := mustDerived(t)
	before, meta := derivedCatalog(t, ctx, db), schemaRows(t, ctx, db)
	if len(before) < 200 {
		t.Fatalf("the catalog holds %d objects; the comparison is vacuous", len(before))
	}
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	oids := servingOids(t, db)
	if len(oids) != len(set.serving) {
		t.Fatalf("%d serving tables, %d listed", len(oids), len(set.serving))
	}
	for range 2 {
		if err := applyDerived(ctx, db, set, current, current); err != nil {
			t.Fatalf("applyDerived: %v", err)
		}
	}
	assertCatalogsEqual(t, "applied twice", before, derivedCatalog(t, ctx, db))
	if after := schemaRows(t, ctx, db); !slices.Equal(after, meta) {
		t.Errorf("schema rows %v, want %v", after, meta)
	}
	if after := servingOids(t, db); !maps.Equal(after, oids) {
		t.Errorf("serving tables re-created: %v, want %v", after, oids)
	}
}

// servingOids is each serving table's oid, which a re-created table
// changes.
func servingOids(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	return scanPairs(t, db, `SELECT table_name, table_oid::VARCHAR FROM duckdb_tables()
	    WHERE database_name = current_database() AND table_name LIKE 'serving\_%' ESCAPE '\'`)
}

// A serving table takes its macro's columns from applyDerived with no
// migration: it is created where absent, kept with its rows where its
// columns hold, and re-created empty where the macro gains a column.
func TestApplyDerivedReshapesAServingTable(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	set := mustDerived(t)
	with := func(body string) derivedSet {
		s := set
		s.stmts = append(slices.Clone(set.stmts), derivedStmt{file: "derived/99_probe.sql", name: "probe_rows",
			sql: "CREATE MACRO probe_rows() AS TABLE " + body + ";\n"})
		s.serving = []servingTable{{"serving_probe", "probe_rows()"}}
		return s
	}
	columns := func() []string {
		t.Helper()
		cols, err := columnsOf(ctx, db, `SELECT * FROM serving_probe`)
		if err != nil {
			t.Fatal(err)
		}
		return cols
	}
	apply := func(s derivedSet) {
		t.Helper()
		if err := applyDerived(ctx, db, s, current, current); err != nil {
			t.Fatalf("applyDerived: %v", err)
		}
	}
	apply(with(`SELECT 1 AS a`))
	if got := columns(); !slices.Equal(got, []string{"a INTEGER"}) {
		t.Fatalf("created with %v, want [a INTEGER]", got)
	}
	mustExec(t, db, `INSERT INTO serving_probe VALUES (7)`)
	apply(with(`SELECT 2 AS a`))
	if n := countRows(t, db, ctx, "serving_probe"); n != 1 {
		t.Errorf("a table whose columns hold has %d rows, want its 1", n)
	}
	apply(with(`SELECT 1 AS a, 'x' AS b`))
	if got := columns(); !slices.Equal(got, []string{"a INTEGER", "b VARCHAR"}) {
		t.Errorf("reshaped to %v, want [a INTEGER b VARCHAR]", got)
	}
	if n := countRows(t, db, ctx, "serving_probe"); n != 0 {
		t.Errorf("a reshaped table has %d rows, want none", n)
	}
}

// probeMacro is a macro no derived file defines: it survives exactly
// when applyDerived does not run.
const probeMacro = `CREATE MACRO derived_probe() AS 1`

func probeSurvives(t *testing.T, ctx context.Context, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM duckdb_functions() WHERE function_name = 'derived_probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// Migrate applies the set to a database at this binary's version that
// carries another hash, and leaves one carrying this hash alone; a
// pending check reads the hash too.
func TestMigrateAppliesTheSetWhereTheHashDiffers(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	set := mustDerived(t)
	mustExec(t, db, probeMacro)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if !probeSurvives(t, ctx, db) {
		t.Fatal("Migrate re-applied a set the database already carries")
	}
	meta := schemaRows(t, ctx, db)
	mustExec(t, db, `UPDATE derived_state SET hash = 'another build'`)
	if pending, err := MigrationsPending(ctx, db); err != nil || !pending {
		t.Errorf("another hash: pending=%v err=%v, want pending", pending, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if probeSurvives(t, ctx, db) {
		t.Error("Migrate left the set of another hash in place")
	}
	if after := schemaRows(t, ctx, db); !slices.Equal(after, meta) || after[len(after)-1] != set.hash {
		t.Errorf("schema rows %v, want %v", after, meta)
	}
	if pending, err := MigrationsPending(ctx, db); err != nil || pending {
		t.Errorf("after Migrate: pending=%v err=%v", pending, err)
	}
}

// A database a later build has carried above this binary's version keeps
// its set, its version and its hash: nothing is downgraded.
func TestMigrateLeavesANewerDatabaseAlone(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, probeMacro)
	mustExec(t, db, `UPDATE derived_state SET hash = 'a later build'`)
	mustExec(t, db, `INSERT INTO schema_meta (gold_schema_version, applied_at) VALUES (?, 0)`, current+1)
	meta := schemaRows(t, ctx, db)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if !probeSurvives(t, ctx, db) {
		t.Error("Migrate replaced a newer database's set")
	}
	if after := schemaRows(t, ctx, db); !slices.Equal(after, meta) {
		t.Errorf("schema rows %v, want %v", after, meta)
	}
	if pending, err := MigrationsPending(ctx, db); err != nil || pending {
		t.Errorf("a newer database: pending=%v err=%v", pending, err)
	}
}

// A statement that fails rolls applyDerived back whole: the set the
// database had stays, and the error names the file and the object.
func TestAFailingDerivedFileLeavesTheSetWhole(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	set := mustDerived(t)
	before, meta := derivedCatalog(t, ctx, db), schemaRows(t, ctx, db)
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	broken := set
	broken.hash = "broken"
	broken.stmts = append(slices.Clone(set.stmts), derivedStmt{
		file: "derived/99_broken.sql", name: "broken",
		sql: "CREATE MACRO broken() AS TABLE SELECT * FROM no_such_table;\n",
	})
	err = applyDerived(ctx, db, broken, current, current+1)
	if err == nil || !strings.Contains(err.Error(), "derived/99_broken.sql: broken:") {
		t.Fatalf("applyDerived = %v, want an error naming the file and the object", err)
	}
	assertCatalogsEqual(t, "after the rollback", before, derivedCatalog(t, ctx, db))
	if after := schemaRows(t, ctx, db); !slices.Equal(after, meta) {
		t.Errorf("schema rows %v, want %v", after, meta)
	}
}
