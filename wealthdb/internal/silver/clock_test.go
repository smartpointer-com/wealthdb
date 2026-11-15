package silver

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// loadClockSchema is the least a load-clock silver holds: its dump runs, a
// dated content table and a ledger.
const loadClockSchema = `CREATE TABLE dump_runs (snapshot_at INTEGER);
CREATE TABLE accounts (snapshot_at INTEGER);
CREATE TABLE transactions (posted_at INTEGER);`

const loadClockSpan = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions)`

// TestLoadClock pins the shared load clock: the status reads the span, the
// ledger and the newest dump; a dump past the watermark re-emits the whole
// span; an idle reload, a watermark past every dump and an empty silver
// change nothing.
func TestLoadClock(t *testing.T) {
	ctx := context.Background()
	k := LoadClock("test", loadClockSpan, Range("transactions", "posted_at"))

	t.Run("empty silver", func(t *testing.T) {
		db := openScratch(t, loadClockSchema)
		s, err := k.Status(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if s != canonical.UnknownStatus() {
			t.Errorf("Status = %+v, want the unknown status", s)
		}
		w, err := k.ChangeWindow(ctx, db, -1)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(w, canonical.Window{NewChangeNumber: -1}) {
			t.Errorf("ChangeWindow(-1) = %+v, want no changes", w)
		}
	})

	db := openScratch(t, loadClockSchema+`
INSERT INTO dump_runs VALUES (500), (900);
INSERT INTO accounts VALUES (500), (900);
INSERT INTO transactions VALUES (100), (300);`)

	s, err := k.Status(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	want := canonical.Status{
		OldestSnapshotAt: 100, LatestSnapshotAt: 900,
		OldestTransactionAt: 100, LatestTransactionAt: 300,
		LatestChangeNumber:       900,
		ObservedSnapshotsThrough: 900, ObservedTransactionsThrough: 900,
	}
	if s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
	checkWindows(t, db, k, map[int64]canonical.Window{
		0: {HasChanges: true, Start: 100, End: 900, NewChangeNumber: 900},
		// A dump past the watermark still re-emits the full span.
		500: {HasChanges: true, Start: 100, End: 900, NewChangeNumber: 900},
		// The watermark at the newest dump is an idle reload.
		900: {NewChangeNumber: 900},
		// A watermark past every dump stays where it is.
		1000: {NewChangeNumber: 1000},
	})

	k.Snapshot = []string{`SELECT nope FROM accounts`}
	if _, err := k.Status(ctx, db); err == nil || !strings.Contains(err.Error(), "test Status snapshot") {
		t.Errorf("a failing span query = %v, want it named", err)
	}
}

// checkWindows checks the window k opens past each since.
func checkWindows(t *testing.T, db *sql.DB, k Clock, want map[int64]canonical.Window) {
	t.Helper()
	for since, w := range want {
		got, err := k.ChangeWindow(context.Background(), db, since)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("ChangeWindow(%d) = %+v, want %+v", since, got, w)
		}
	}
}

// TestClockPinAndExtra pins a pinned change number, which follows the
// latest run whatever the watermark, and the dates read in Go: they widen
// the snapshot and the transaction range, and a full window that holds a
// date.
func TestClockPinAndExtra(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, loadClockSchema+`
INSERT INTO dump_runs VALUES (500);
INSERT INTO accounts VALUES (400);`)
	dates := func(lo, hi int64) Dates {
		return func(context.Context) (int64, int64, error) { return lo, hi, nil }
	}
	k := LoadClock("test", loadClockSpan)
	k.Pin = true
	k.Extra, k.LedgerExtra = dates(200, 600), dates(50, -1)
	k.Mode = FullSpan{Span: k.Snapshot, Extra: dates(-1, 700)}
	s, err := k.Status(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestSnapshotAt != 200 || s.LatestSnapshotAt != 600 ||
		s.OldestTransactionAt != 50 || s.LatestTransactionAt != -1 {
		t.Errorf("Status = %+v, want the snapshots over [200, 600] and the ledger from 50", s)
	}
	checkWindows(t, db, k, map[int64]canonical.Window{
		-1:   {HasChanges: true, Start: 400, End: 700, NewChangeNumber: 500},
		1000: {NewChangeNumber: 500},
	})
	k.Extra = func(context.Context) (int64, int64, error) { return 0, 0, errors.New("boom") }
	if _, err := k.Status(ctx, db); err == nil || err.Error() != "boom" {
		t.Errorf("a failing Extra = %v, want its error", err)
	}
}

// TestIncrementalClock pins the incremental window: the span of what lies
// past the watermark, widened to the Widen and Recent ranges and to Extra,
// then reached back; nothing past the watermark is no change.
func TestIncrementalClock(t *testing.T) {
	db := openScratch(t, `CREATE TABLE dump_runs (snapshot_at INTEGER);
CREATE TABLE events (t INTEGER); CREATE TABLE content (t INTEGER);
INSERT INTO dump_runs VALUES (1000), (2000);
INSERT INTO events VALUES (1500), (2500);
INSERT INTO content VALUES (300), (1300), (1900);`)
	base := Clock{Kind: "test", Runs: LatestDump, Pin: true}
	k := base
	k.Mode = Incremental{New: []string{RangePast("dump_runs", "snapshot_at"), RangePast("events", "t")}}
	checkWindows(t, db, k, map[int64]canonical.Window{
		-1:   {HasChanges: true, Start: 1000, End: 2500, NewChangeNumber: 2000},
		1000: {HasChanges: true, Start: 1500, End: 2500, NewChangeNumber: 2000},
		2000: {HasChanges: true, Start: 2500, End: 2500, NewChangeNumber: 2000},
		2500: {NewChangeNumber: 2000},
	})

	k = base
	k.Mode = Incremental{
		New:      []string{RangePast("dump_runs", "snapshot_at")},
		Recent:   []string{RangePast("content", "t")},
		Lookback: 200,
		Extra:    func(context.Context) (int64, int64, error) { return -1, 2100, nil },
	}
	checkWindows(t, db, k, map[int64]canonical.Window{
		// A watermark below zero binds as it is: every content row.
		-1:   {HasChanges: true, Start: 300, End: 2100, NewChangeNumber: 2000},
		1000: {HasChanges: true, Start: 1300, End: 2100, NewChangeNumber: 2000},
	})

	k = base
	k.Mode = Incremental{
		New:   []string{RangePast("dump_runs", "snapshot_at")},
		Widen: []string{Range("content", "t")},
		ReachBack: func(_ context.Context, start int64) (int64, error) {
			return start - 1, nil
		},
	}
	checkWindows(t, db, k, map[int64]canonical.Window{
		1000: {HasChanges: true, Start: 299, End: 2000, NewChangeNumber: 2000},
	})
	k.Pin = false
	checkWindows(t, db, k, map[int64]canonical.Window{math.MaxInt64: {NewChangeNumber: math.MaxInt64}})
}

// TestPerKindClock pins the window of a source whose kinds change over
// spans of their own: their union, each kind's span kept; a run past the
// watermark that wrote nothing advances it with no change.
func TestPerKindClock(t *testing.T) {
	db := openScratch(t, `CREATE TABLE dump_runs (snapshot_at INTEGER); INSERT INTO dump_runs VALUES (900);`)
	spans := map[string]canonical.Span{"a": {Start: 100, End: 200}, "b": {Start: 50, End: 150}}
	k := Clock{Kind: "test", Runs: LatestDump, Mode: PerKind(func(context.Context, int64) (map[string]canonical.Span, error) {
		return spans, nil
	})}
	checkWindows(t, db, k, map[int64]canonical.Window{
		-1:  {HasChanges: true, Start: 50, End: 200, NewChangeNumber: 900, Spans: spans},
		900: {NewChangeNumber: 900},
	})
	spans = map[string]canonical.Span{}
	checkWindows(t, db, k, map[int64]canonical.Window{-1: {NewChangeNumber: 900, Spans: spans}})
}

// TestClockObserved pins the observed watermarks: the latest run by
// default, another run query in its place, the first gapless stretch of
// the ledger windows, or an adapter's own rule.
func TestClockObserved(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, `CREATE TABLE dump_runs (snapshot_at INTEGER, since INTEGER, until INTEGER);
INSERT INTO dump_runs VALUES (100, 0, 100), (300000, 200000, 300000);`)
	k := Clock{Kind: "test", Runs: LatestDump, Mode: FullSpan{}}
	for _, c := range []struct {
		name      string
		observed  Observed
		snap, txs int64
	}{
		{"the latest run", Observed{}, 300000, 300000},
		{"a run query", Observed{Runs: `SELECT MIN(snapshot_at) FROM dump_runs`}, 100, 100},
		{"the ledger windows", Observed{Windows: `SELECT since, until FROM dump_runs`}, 300000, 100},
		{"the adapter's rule", Observed{Set: func(_ context.Context, s *canonical.Status) error {
			s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough = 7, 8
			return nil
		}}, 7, 8},
	} {
		k.Observed = c.observed
		s, err := k.Status(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if s.ObservedSnapshotsThrough != c.snap || s.ObservedTransactionsThrough != c.txs {
			t.Errorf("%s: observed %d, %d; want %d, %d", c.name,
				s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough, c.snap, c.txs)
		}
	}
}
