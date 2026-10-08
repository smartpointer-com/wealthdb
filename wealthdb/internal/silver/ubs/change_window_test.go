package ubs

import (
	"context"
	"testing"
)

// PSN delivers each night's report after midnight and dates its content by
// the business day before, so a dump's holdings and events always predate the
// dump itself, and the previous dump's time too. The window an incremental
// load gets must still reach them.
func TestChangeWindowReachesTheBusinessDayOfANewDump(t *testing.T) {
	const day = int64(86400)
	d1 := 20 * day        // business day of the first report, midnight
	t1 := d1 + day + 5400 // its delivery, 01:30 the next day
	d2 := d1 + day        // business day of the second report
	t2 := d2 + day + 5400 // its delivery
	d3 := d2 - 2*day      // a report for an earlier business day…
	t3 := t2 + day        // …delivered late, after the second

	path, seed := newFixtureSilver(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := seed.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	addDump := func(run, business int64, event string) {
		exec(`INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (?, 1, '/x')`, run)
		exec(`INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload)
              VALUES (?, 'SFTPCHxx', 'CH00SAFE', 'CH0000000001', '{}')`, business)
		exec(`INSERT INTO events(event_external_id, timestamp, relationship_id, account_external_id, kind, payload)
              VALUES (?, ?, 'SFTPCHxx', 'CH00CASH', 'trade_confirmation', '{}')`, event, business)
	}
	addDump(t1, d1, "e1")
	addDump(t2, d2, "e2")
	conn := openAdapter(t, path)
	ctx := context.Background()

	w, err := conn.ChangeWindow(ctx, t1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges {
		t.Fatal("a new dump must report changes")
	}
	if w.Start > d2 {
		t.Errorf("window starts at %d, after the new report's business day %d", w.Start, d2)
	}
	if w.End < t2 {
		t.Errorf("window ends at %d, before the new dump at %d", w.End, t2)
	}
	if w.NewChangeNumber != t2 {
		t.Errorf("NewChangeNumber = %d, want the new dump's time %d", w.NewChangeNumber, t2)
	}

	idle, err := conn.ChangeWindow(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if idle.HasChanges {
		t.Errorf("no new dump since %d, but the window reports changes: %+v", t2, idle)
	}

	addDump(t3, d3, "e3")
	w, err = conn.ChangeWindow(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start > d3 {
		t.Errorf("a report delivered late must still be inside the window: %+v, business day %d", w, d3)
	}
}
