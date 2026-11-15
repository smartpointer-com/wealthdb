package silver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestJoinText pins the transaction-text composition rule the adapters
// share: trim each part, drop the empty ones, join the rest with "; ",
// and yield "" — never a stray separator — when nothing is left.
func TestJoinText(t *testing.T) {
	for _, c := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"no parts", nil, ""},
		{"every part blank", []string{"", "   ", "\t"}, ""},
		{"one part is trimmed", []string{"  PAYMENT ORDER  "}, "PAYMENT ORDER"},
		{"parts are joined", []string{"PAYMENT ORDER", "EXAMPLE SHOP"},
			"PAYMENT ORDER; EXAMPLE SHOP"},
		{"blank parts drop out", []string{"", "PAYMENT ORDER", "  ", "EXAMPLE SHOP"},
			"PAYMENT ORDER; EXAMPLE SHOP"},
		{"an already-separated part is not re-split", []string{"A; B", "C"}, "A; B; C"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := JoinText(c.parts...); got != c.want {
				t.Errorf("JoinText(%q) = %q, want %q", c.parts, got, c.want)
			}
		})
	}
}

// TestPayloadWith pins the payload-merge rule the card adapters share: the
// collector's own JSON survives, the adapter's annotations are laid over it,
// and a payload that is not a JSON object never costs the annotations.
func TestPayloadWith(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		extra   map[string]any
		want    string
	}{
		{"no annotations passes the payload through untouched",
			`{"a":1}`, nil, `{"a":1}`},
		{"no annotations does not even reformat",
			`not json at all`, nil, `not json at all`},
		{"annotations are merged in",
			`{"a":1}`, map[string]any{"b": "x"}, `{"a":1,"b":"x"}`},
		{"an annotation overrides the collector's key",
			`{"a":1}`, map[string]any{"a": 2}, `{"a":2}`},
		{"an undecodable payload yields the annotations alone",
			`not json at all`, map[string]any{"b": "x"}, `{"b":"x"}`},
		{"a JSON non-object yields the annotations alone",
			`[1,2]`, map[string]any{"b": "x"}, `{"b":"x"}`},
		{"a null payload yields the annotations alone",
			`null`, map[string]any{"b": "x"}, `{"b":"x"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(PayloadWith(c.payload, c.extra)); got != c.want {
				t.Errorf("PayloadWith(%q, %v) = %s, want %s",
					c.payload, c.extra, got, c.want)
			}
		})
	}
}

// TestHasTables pins the probe the adapters share for tables a newer
// silver migration adds: true only when every named table exists, and a
// view or an index of that name does not count.
func TestHasTables(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "silver.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
        CREATE TABLE a (x INTEGER);
        CREATE TABLE b (x INTEGER);
        CREATE VIEW v AS SELECT x FROM a;
        CREATE INDEX i ON a (x);`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		names []string
		want  bool
	}{
		{[]string{"a"}, true},
		{[]string{"a", "b"}, true},
		{[]string{"a", "c"}, false},
		{[]string{"c"}, false},
		{[]string{"v"}, false},
		{[]string{"i"}, false},
		{nil, true},
	} {
		got, err := HasTables(context.Background(), db, c.names...)
		if err != nil {
			t.Fatalf("HasTables(%q): %v", c.names, err)
		}
		if got != c.want {
			t.Errorf("HasTables(%q) = %v, want %v", c.names, got, c.want)
		}
	}
}

// TestHasColumns pins the probe for columns a newer silver migration adds:
// true only when the table has every named column, and false for a table
// the file does not hold.
func TestHasColumns(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "silver.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE a (x INTEGER, y TEXT, z TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		table   string
		columns []string
		want    bool
	}{
		{"a", []string{"x"}, true},
		{"a", []string{"x", "z"}, true},
		{"a", []string{"x", "w"}, false},
		{"a", []string{"w"}, false},
		{"b", []string{"x"}, false},
		{"a", nil, true},
	} {
		got, err := HasColumns(context.Background(), db, c.table, c.columns...)
		if err != nil {
			t.Fatalf("HasColumns(%s, %q): %v", c.table, c.columns, err)
		}
		if got != c.want {
			t.Errorf("HasColumns(%s, %q) = %v, want %v", c.table, c.columns, got, c.want)
		}
	}
}

func TestLotHelpers(t *testing.T) {
	if d := ISODate(" 2024-03-05 "); d == nil || d.Format(time.DateOnly) != "2024-03-05" {
		t.Errorf("ISODate = %v", d)
	}
	for _, s := range []string{"", "Various", "03/05/2024"} {
		if ISODate(s) != nil {
			t.Errorf("ISODate(%q) parsed", s)
		}
	}
	neg := canonical.NewDecimalFromInt(-7)
	if a := AbsPtr(&neg); a == nil || a.String() != "7" || neg.String() != "-7" {
		t.Errorf("AbsPtr = %v (input %v)", a, neg)
	}
	if AbsPtr(nil) != nil {
		t.Error("AbsPtr(nil) not nil")
	}
	d := time.Date(2023, 12, 29, 0, 0, 0, 0, time.UTC)
	if TaxYearOf(nil, &d) != 2023 || TaxYearOf() != 0 {
		t.Error("TaxYearOf")
	}
}

func TestWithAccrued(t *testing.T) {
	clean, accrued := canonical.NewDecimalFromInt(1000), canonical.NewDecimalFromFloat(12.5)
	if v := WithAccrued(&clean, &accrued); v == nil || v.String() != "1012.5" || clean.String() != "1000" {
		t.Errorf("WithAccrued = %v (clean %v)", v, clean)
	}
	if v := WithAccrued(&clean, nil); v != &clean {
		t.Error("no accrued figure changed the value")
	}
	if WithAccrued(nil, &accrued) != nil {
		t.Error("a nil value gained one")
	}
}

// TestCloseDB pins the shared Close: it closes and clears the handle, and a
// second call, like a nil handle, is a no-op.
func TestCloseDB(t *testing.T) {
	db := openScratch(t, `CREATE TABLE x (a INTEGER);`)
	if err := CloseDB(&db); err != nil || db != nil {
		t.Fatalf("CloseDB = %v, handle %v; want closed and cleared", err, db)
	}
	if err := CloseDB(&db); err != nil {
		t.Errorf("second CloseDB = %v, want a no-op", err)
	}
	if err := CloseDB(nil); err != nil {
		t.Errorf("CloseDB(nil) = %v, want a no-op", err)
	}
}

// TestExtrema pins the range fold: the least MIN and the greatest MAX over
// every query, a NULL counting for nothing, and (-1, -1) when all are NULL.
func TestExtrema(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, `CREATE TABLE a (t INTEGER); CREATE TABLE b (t INTEGER);
CREATE TABLE e (t INTEGER);
INSERT INTO a VALUES (300), (700); INSERT INTO b VALUES (200), (500);`)
	for _, c := range []struct {
		name    string
		queries []string
		args    []any
		lo, hi  int64
	}{
		{"widest of both", []string{`SELECT MIN(t), MAX(t) FROM a`, `SELECT MIN(t), MAX(t) FROM b`}, nil, 200, 700},
		{"an empty table adds nothing", []string{`SELECT MIN(t), MAX(t) FROM e`, `SELECT MIN(t), MAX(t) FROM b`}, nil, 200, 500},
		{"bound by args", []string{`SELECT MIN(t), MAX(t) FROM a WHERE t > ?`}, []any{400}, 700, 700},
		{"all empty", []string{`SELECT MIN(t), MAX(t) FROM e`}, nil, -1, -1},
		{"no queries", nil, nil, -1, -1},
	} {
		lo, hi, err := Extrema(ctx, db, "test", c.queries, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if lo != c.lo || hi != c.hi {
			t.Errorf("%s: Extrema = [%d,%d], want [%d,%d]", c.name, lo, hi, c.lo, c.hi)
		}
	}
	if _, _, err := Extrema(ctx, db, "the range", []string{`SELECT nope FROM a`}); err == nil ||
		!strings.HasPrefix(err.Error(), "the range: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestSnapshotTimesOfUnionsTheTables pins the generated union: every
// snapshot_at of each table inside the window, with the extra times, once
// each and ascending.
func TestSnapshotTimesOfUnionsTheTables(t *testing.T) {
	db := openScratch(t, `CREATE TABLE a (snapshot_at INTEGER); CREATE TABLE b (snapshot_at INTEGER);
INSERT INTO a VALUES (10), (30), (90); INSERT INTO b VALUES (30), (20), (5);`)
	w := canonical.Window{Start: 10, End: 40, HasChanges: true}
	got, err := SnapshotTimesOf(context.Background(), db, w, []int64{25, 50}, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{10, 20, 25, 30}; !slices.Equal(got, want) {
		t.Errorf("SnapshotTimesOf = %v, want %v", got, want)
	}
}

// TestScanRows pins the row loop the silver readers share: rows reach fn
// in the query's order, the first error fn returns ends the loop and is
// returned, a query that does not run returns its error, and the result
// is closed on every return, so the one connection serves the next query.
func TestScanRows(t *testing.T) {
	// A result left open holds the one connection: the next query then
	// waits out this deadline and fails, never hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errStop := errors.New("stop")
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "silver.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE a (x INTEGER); INSERT INTO a VALUES (3), (1), (2)`); err != nil {
		t.Fatal(err)
	}
	collect := func(stopAt int64) ([]int64, error) {
		var seen []int64
		err := ScanRows(ctx, db, "", `SELECT x FROM a WHERE x >= ? ORDER BY x`, func(rows *sql.Rows) error {
			var x int64
			if err := rows.Scan(&x); err != nil {
				return err
			}
			seen = append(seen, x)
			if x == stopAt {
				return errStop
			}
			return nil
		}, 1)
		return seen, err
	}
	if seen, err := collect(0); err != nil || !slices.Equal(seen, []int64{1, 2, 3}) {
		t.Errorf("every row: %v (%v), want [1 2 3] in order", seen, err)
	}
	if seen, err := collect(2); err != errStop || !slices.Equal(seen, []int64{1, 2}) {
		t.Errorf("stop at 2: %v (%v), want [1 2] and the scan's error", seen, err)
	}
	if err := ScanRows(ctx, db, "", `SELECT nope FROM a`, func(*sql.Rows) error { return nil }); err == nil {
		t.Error("a query that does not run returned no error")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM a`).Scan(&n); err != nil || n != 3 {
		t.Errorf("after the scans: %d rows (%v), want the connection free", n, err)
	}
}

// TestQueryCollectsAndLabels pins Query: the records in the query's order,
// a label on the errors of the query but not on a scan's own error.
func TestQueryCollectsAndLabels(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, `CREATE TABLE a (x INTEGER); INSERT INTO a VALUES (3), (1), (2);`)
	scan := func(rows *sql.Rows) (int64, error) {
		var x int64
		err := rows.Scan(&x)
		return x, err
	}
	got, err := Query(ctx, db, "a read", `SELECT x FROM a ORDER BY x`, scan)
	if err != nil || !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("Query = %v (%v), want [1 2 3]", got, err)
	}
	if _, err := Query(ctx, db, "a read", `SELECT nope FROM a`, scan); err == nil ||
		!strings.HasPrefix(err.Error(), "a read: ") {
		t.Errorf("a failing query = %v, want it labelled", err)
	}
	errScan := errors.New("bad row")
	_, err = Query(ctx, db, "a read", `SELECT x FROM a`, func(*sql.Rows) (int64, error) { return 0, errScan })
	if err != errScan {
		t.Errorf("a failing scan = %v, want its error as it is", err)
	}
}

// TestDecKeepsEachPath pins the decimal scan target: NULL is nil on both
// paths, the text path parses the text and reads blank text as nil, and
// the float path converts the float.
func TestDecKeepsEachPath(t *testing.T) {
	db := openScratch(t, `CREATE TABLE m (v REAL, s TEXT); INSERT INTO m VALUES (0.1, '0.10'), (NULL, ''), (NULL, NULL);`)
	rows, err := db.Query(`SELECT v, CAST(v AS VARCHAR), s FROM m ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var f, cast, text *canonical.Decimal
		if err := rows.Scan(Dec(&f, DecFloat), Dec(&cast, DecText), Dec(&text, DecText)); err != nil {
			t.Fatal(err)
		}
		show := func(d *canonical.Decimal) string {
			if d == nil {
				return "nil"
			}
			return d.String()
		}
		got = append(got, show(f)+" "+show(cast)+" "+show(text))
	}
	if want := []string{"0.1 0.1 0.1", "nil nil nil", "nil nil nil"}; !slices.Equal(got, want) {
		t.Errorf("Dec = %q, want %q", got, want)
	}
}

// TestProbeOptional pins the schema probe: a table, a column set present
// whole, and a column set one of whose columns is missing.
func TestProbeOptional(t *testing.T) {
	db := openScratch(t, `CREATE TABLE a (x INTEGER, y INTEGER);`)
	var table, missing, cols, partial bool
	if _, err := ProbeOptional(context.Background(), db, "test",
		Optional{Table: "a", Has: &table},
		Optional{Table: "b", Has: &missing},
		Optional{Table: "a", Columns: []string{"x", "y"}, Has: &cols},
		Optional{Table: "a", Columns: []string{"x", "z"}, Has: &partial},
	); err != nil {
		t.Fatal(err)
	}
	if !table || missing || !cols || partial {
		t.Errorf("probed %v %v %v %v, want true false true false", table, missing, cols, partial)
	}
}

// TestProbeOptionalNotesAnOlderSilver pins the advice: a part absent from
// a silver whose schema version predates it is noted, by name, with both
// versions; a part the version already holds, or a part of version 0,
// is not; a current silver notes nothing.
func TestProbeOptionalNotesAnOlderSilver(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, `CREATE TABLE schema_meta (silver_schema_version INTEGER, applied_at INTEGER);
INSERT INTO schema_meta VALUES (1, 0), (3, 0);
CREATE TABLE a (x INTEGER);`)
	var has [4]bool
	parts := []Optional{
		{Table: "a", Columns: []string{"y"}, Version: 4, Has: &has[0]},
		{Table: "b", Version: 5, Has: &has[1]},
		{Table: "c", Version: 2, Has: &has[2]},
		{Table: "d", Has: &has[3]},
	}
	n, err := ProbeOptional(ctx, db, "kind", parts...)
	if err != nil {
		t.Fatal(err)
	}
	want := SchemaNote{Silver: "kind", Version: 3, Reads: 5, Missing: []string{"a.y", "b"}}
	if !reflect.DeepEqual(n, want) {
		t.Errorf("note = %+v, want %+v", n, want)
	}
	if got := n.String(); got != "kind is at silver schema 3 and the adapter reads 5; a.y and b load empty until its collector's load applies the migrations" {
		t.Errorf("advice = %q", got)
	}
	var a Advice
	a.Note(n)
	a.Note(SchemaNote{})
	a.Note(SchemaNote{Silver: "kind", Version: 3, Reads: 6, Missing: []string{"e"}})
	want.Reads, want.Missing = 6, []string{"a.y", "b", "e"}
	if got := a.SchemaAdvice(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("advice kept %+v, want the one joined note %+v", got, want)
	}
	current := openScratch(t, `CREATE TABLE schema_meta (silver_schema_version INTEGER, applied_at INTEGER);
INSERT INTO schema_meta VALUES (5, 0); CREATE TABLE b (x INTEGER);`)
	if n, err := ProbeOptional(ctx, current, "kind", parts[1]); err != nil || len(n.Missing) != 0 {
		t.Errorf("a current silver noted %+v (%v)", n, err)
	}
	if got := ColumnOr(false, "p", "q"); got != "NULL, NULL" {
		t.Errorf("ColumnOr(false) = %q", got)
	}
	if got := ColumnOr(true, "p", "q"); got != "p, q" {
		t.Errorf("ColumnOr(true) = %q", got)
	}
}
