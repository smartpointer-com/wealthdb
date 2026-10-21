package curves_test

import (
	"context"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/curves"
)

//go:embed testdata/silver_schema.sql
var schemaSQL string

// A curves silver loads into gold's rate_observations: gold admits the
// kind, a value with more decimals than the column keeps is rounded to
// it, the payload is kept, and a later fetch run that revises a day
// replaces it. The values are invented.
func TestLoadIntoGold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path, s := silvertest.NewDB(t, schemaSQL)
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_767_312_000, 1_767_571_200 // 2026-01-02, 2026-01-05
		means      = `{"currency":"EUR","tenor":"10Y","basis":"spot","compounding":"continuous"}`
	)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 1, '/x/1')`, run1)
	silvertest.Exec(t, s, `INSERT INTO rates (as_of, series_id, value, payload) VALUES
	    (?, 'EUR-10Y', '2.512345678912', ?), (?, 'EUR-10Y', '2.6', ?)`, d1, means, d2, means)

	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "curves", Kind: "curves", Path: path}
	if _, err := ld.Load(ctx, spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := g.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM rate_observations WHERE silver_source_id = 'curves' AND series_id = 'EUR-10Y'
	    AND as_of = 1767312000 AND value = 2.51234568 AND json_extract_string(payload, '$.basis') = 'spot'
	    AND json_extract_string(payload, '$.compounding') = 'continuous'`); n != 1 {
		t.Errorf("EUR-10Y on 2026-01-02 = %d rows, want 1", n)
	}

	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 1, '/x/2')`, run2)
	silvertest.Exec(t, s, `UPDATE rates SET value = '2.65' WHERE as_of = ?`, d2)
	if _, err := ld.Load(ctx, spec); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM rate_observations`); n != 2 {
		t.Errorf("rates after the reload = %d, want 2", n)
	}
	if n := count(`SELECT COUNT(*) FROM rate_observations WHERE as_of = 1767571200 AND value = 2.65`); n != 1 {
		t.Errorf("the revised day = %d rows, want 1 at the revised value", n)
	}
}
