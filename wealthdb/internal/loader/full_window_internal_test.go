package loader

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// A full window deletes the source's rows whole, whatever their time, and
// leaves every other source's alone; a span deletes only inside itself.
func TestDeleteWindowFullDeletesTheSourceWhole(t *testing.T) {
	ctx := context.Background()
	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("gold.OpenFresh: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	write := func(source string, at int64) {
		t.Helper()
		tx, err := g.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		w := canonical.Window{Start: at, End: at, HasChanges: true}
		if _, err := applySnapshots(ctx, tx, stubSilverAt("ACCT0001", at), w, SourceSpec{ID: source}); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []int64{-100000, 1600000000, 1700000000} {
		write("whole", at)
		write("other", at)
	}
	del := func(source string, w canonical.Window) {
		t.Helper()
		tx, err := g.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := deleteWindow(ctx, tx, source, w); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	count := func(source string) int {
		t.Helper()
		var n int
		if err := g.QueryRowContext(ctx, `SELECT count(*) FROM positions WHERE silver_source_id = ?`, source).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	del("other", canonical.Window{Start: 1650000000, End: 1750000000, HasChanges: true})
	if n := count("other"); n != 2 {
		t.Errorf("a span left %d positions of other, want the 2 outside it", n)
	}
	del("whole", canonical.FullWindow(1))
	if n, m := count("whole"), count("other"); n != 0 || m != 2 {
		t.Errorf("a full window left %d positions of whole and %d of other, want 0 and 2", n, m)
	}
}
