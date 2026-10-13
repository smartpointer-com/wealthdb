package loader_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"

	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/refdata"
)

// A refdata silver loads its reference records into gold, a later run
// replaces only the days it re-served, and a reset clears them.
func TestLoadRefdata(t *testing.T) {
	ctx := context.Background()
	schema, err := os.ReadFile("../silver/refdata/testdata/silver_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	silverPath := filepath.Join(dir, "refdata.db")
	s, err := sql.Open("sqlite", "file:"+silverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustExec(t, s, string(schema))
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_767_139_200, 1_769_817_600 // 2025-12-31, 2026-01-31
	)
	mustExec(t, s, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, run1+5)
	for _, d := range []int64{d1, d2} {
		mustExec(t, s, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, weight_sum, run_at)
		    VALUES ('symbol:IDX1', ?, 'holdings', 'inbox', 'inbox', 2, '100', ?)`, d, run1)
		mustExec(t, s, `INSERT INTO lines (fund_ref, as_of, basis, line_no, ticker, weight) VALUES
		    ('symbol:IDX1', ?, 'holdings', 1, 'AAA', '70'), ('symbol:IDX1', ?, 'holdings', 2, 'BBB', '30')`, d, d)
	}
	mustExec(t, s, `INSERT INTO security_facts (security_key, field, as_of, provider, value, run_at)
	    VALUES ('symbol:AAA', 'sic', ?, 'sec-edgar', '3571', ?)`, d2, run1)

	g, err := gold.OpenFresh(filepath.Join(dir, "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "ref", Kind: "refdata", Path: silverPath}
	res, err := ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.SnapshotsLoaded != 7 {
		t.Errorf("loaded %d reference rows, want 2 compositions + 4 lines + 1 fact", res.SnapshotsLoaded)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := g.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM fund_lines WHERE security_key = 'symbol:AAA' AND weight = 70`); n != 2 {
		t.Errorf("AAA lines = %d, want 2", n)
	}

	// Run 2 re-serves 2026-01-31 with one line; 2025-12-31 is untouched.
	mustExec(t, s, `INSERT INTO dump_runs VALUES (?, 1, '/x/2', ?)`, run2, run2+5)
	mustExec(t, s, `DELETE FROM lines WHERE as_of = ?`, d2)
	mustExec(t, s, `UPDATE compositions SET line_count = 1, run_at = ? WHERE as_of = ?`, run2, d2)
	mustExec(t, s, `INSERT INTO lines (fund_ref, as_of, basis, line_no, ticker, weight) VALUES ('symbol:IDX1', ?, 'holdings', 1, 'CCC', '100')`, d2)
	res, err = ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if res.Window.Start != d2 || res.Window.End != d2 {
		t.Errorf("second window = %+v, want the re-served day alone", res.Window)
	}
	if n := count(`SELECT COUNT(*) FROM fund_lines`); n != 3 {
		t.Errorf("lines after the re-serve = %d, want 2 + 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM security_facts`); n != 1 {
		t.Errorf("facts = %d, want the one inside the window re-emitted", n)
	}

	if err := ld.Reset(ctx, "ref"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, tbl := range gold.ReferenceTables {
		if n := count(`SELECT COUNT(*) FROM ` + tbl); n != 0 {
			t.Errorf("%s keeps %d rows after reset", tbl, n)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
