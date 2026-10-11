package viac

import (
	"context"
	"testing"
)

// A dry run reads nothing: the observed watermarks stay at the latest
// real run.
func TestADryRunObservesNothing(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        ALTER TABLE dump_runs ADD COLUMN dry_run INTEGER NOT NULL DEFAULT 0;
        INSERT INTO dump_runs (snapshot_at, silver_schema_version, run_dir, dry_run) VALUES
            (864000, 1, 'r1', 0), (1728000, 1, 'r2', 1);`); err != nil {
		t.Fatal(err)
	}
	s, err := openAdapter(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.ObservedSnapshotsThrough != 864000 || s.ObservedTransactionsThrough != 864000 {
		t.Errorf("observed %d %d, want the real run", s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough)
	}
}
