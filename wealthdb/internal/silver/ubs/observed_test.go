package ubs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A ubs-web run reads the holdings, and the ledger over its window: the
// ledger is observed through the first gap in the windows, the holdings
// through the latest run. A silver without the window columns counts
// every run as reading both.
func TestWebObservedFollowsTheWindows(t *testing.T) {
	d := func(n int64) int64 { return n * 86400 }
	for _, c := range []struct {
		name, schema         string
		wantSnaps, wantTrans int64
	}{
		{"windows", `CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, silver_schema_version INTEGER,
            run_dir TEXT, window_since INTEGER, window_until INTEGER);
            INSERT INTO dump_runs VALUES (864000, 1, 'r1', 0, 864000), (1728000, 1, 'r2', 777600, NULL),
                                         (3456000, 1, 'r3', 2592000, 3456000);`, d(40), d(20)},
		{"no window columns", `CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, silver_schema_version INTEGER, run_dir TEXT);
            INSERT INTO dump_runs VALUES (864000, 1, 'r1'), (3456000, 1, 'r3');`, d(40), d(40)},
	} {
		db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ubs-web.db")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(c.schema); err != nil {
			t.Fatal(err)
		}
		st := canonical.UnknownStatus()
		if err := (&webReader{db: db}).observed(context.Background(), &st); err != nil {
			t.Fatal(err)
		}
		if st.ObservedSnapshotsThrough != c.wantSnaps || st.ObservedTransactionsThrough != c.wantTrans {
			t.Errorf("%s: %d %d, want %d %d", c.name, st.ObservedSnapshotsThrough, st.ObservedTransactionsThrough, c.wantSnaps, c.wantTrans)
		}
		db.Close()
	}
}
