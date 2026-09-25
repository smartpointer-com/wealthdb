package gold

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Every copy OpenFresh hands out is at the current schema, is a database
// of its own, and never lands on a file that already exists.
func TestOpenFreshCopiesAreCurrentAndIndependent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	open := func(name string) *sql.DB {
		t.Helper()
		db, err := OpenFresh(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("OpenFresh %s: %v", name, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	a, b := open("a.db"), open("b.db")
	t.Logf("fresh image: %d bytes", len(fresh.data))

	migrations, err := listMigrations()
	if err != nil {
		t.Fatal(err)
	}
	latest := 0
	for _, m := range migrations {
		if m.version > latest {
			latest = m.version
		}
	}
	for name, db := range map[string]*sql.DB{"a": a, "b": b} {
		v, err := currentSchemaVersion(ctx, db)
		if err != nil {
			t.Fatalf("%s: schema version: %v", name, err)
		}
		if v != latest {
			t.Errorf("%s: schema version %d, want %d", name, v, latest)
		}
	}

	if _, err := a.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
                                   high_watermark, first_loaded_at, last_loaded_at)
        VALUES ('only-a', 'schwab', '/tmp/a.db', -1, 0, 0)`); err != nil {
		t.Fatalf("insert into a: %v", err)
	}
	var n int
	if err := b.QueryRowContext(ctx, `SELECT count(*) FROM silver_sources`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("b sees %d silver_sources rows written to a", n)
	}

	if db, err := OpenFresh(filepath.Join(dir, "a.db")); err == nil {
		db.Close()
		t.Error("OpenFresh over an existing file succeeded, want an error")
	}
}
