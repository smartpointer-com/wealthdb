package loader_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/fred"
)

// A fred silver's rate series load into rate_observations beside its FX
// rates, a later fetch run that revises a day replaces it rather than
// adding a second row, and a reset clears them.
func TestLoadFredRates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	silverPath, s := newSilver(t, "../silver/fred/testdata/silver_schema.sql")
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_767_312_000, 1_767_571_200 // 2026-01-02, 2026-01-05
	)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/x/1')`, run1)
	silvertest.Exec(t, s, `INSERT INTO fx_rates VALUES (?, 'CHF', 'USD', '0.8', '{}')`, d1)
	silvertest.Exec(t, s, `INSERT INTO rates (as_of, series_id, value, payload) VALUES
	    (?, 'DGS10', '4.18', '{"currency":"USD","tenor":"10Y","basis":"constant_maturity"}'),
	    (?, 'DTB3', '3.62', '{}'), (?, 'DGS10', '4.2', '{}')`, d1, d1, d2)

	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "fred", Kind: "fred", Path: silverPath}
	res, err := ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.SnapshotsLoaded != 4 {
		t.Errorf("loaded %d rows, want 1 FX rate + 3 rates", res.SnapshotsLoaded)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM rate_observations WHERE silver_source_id = 'fred' AND series_id = 'DGS10'
	    AND as_of = 1767312000 AND value = 4.18 AND json_extract_string(payload, '$.tenor') = '10Y'`); n != 1 {
		t.Errorf("DGS10 on 2026-01-02 = %d rows, want 1", n)
	}

	// Run 2 revises 2026-01-05; the reload re-emits the whole range.
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/x/2')`, run2)
	silvertest.Exec(t, s, `UPDATE rates SET value = '4.21' WHERE as_of = ?`, d2)
	if _, err := ld.Load(ctx, spec); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM rate_observations`); n != 3 {
		t.Errorf("rates after the reload = %d, want 3", n)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM rate_observations WHERE as_of = 1767571200 AND value = 4.21`); n != 1 {
		t.Errorf("the revised day = %d rows, want 1 at the revised value", n)
	}

	if err := ld.Reset(ctx, "fred"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, tbl := range []string{"rate_observations", "fx_rates"} {
		if n := goldCount(t, g, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s keeps %d rows after reset", tbl, n)
		}
	}
}
