package fidelity

import (
	"context"
	"testing"
)

// A run observes the holdings when it read the positions page, and the
// ledger over its activity window; a statement corpus observes both
// through its latest statement.
func TestObservedFollowsRunsAndStatements(t *testing.T) {
	const d = 86400
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs VALUES
            (5*86400,  1, 'r1', 'all',        0,        5*86400, 1, 1),
            (10*86400, 1, 'r2', 'activity',   4*86400,  NULL,    0, 1),
            (20*86400, 1, 'r3', 'positions',  NULL,     NULL,    1, 0),
            (30*86400, 1, 'r4', 'activity',   25*86400, 30*86400, 0, 1);`); err != nil {
		t.Fatal(err)
	}
	s, err := openAdapter(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.ObservedSnapshotsThrough != 20*d || s.ObservedTransactionsThrough != 10*d {
		t.Errorf("runs: %d %d, want %d %d", s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough, 20*d, 10*d)
	}
}
