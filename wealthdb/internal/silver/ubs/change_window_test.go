package ubs

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
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

// A web row can be valued after the dump that read it: a payment order
// set for a later day. The change number is the dump, not the value
// date, so the watermark never runs ahead of the runs. A later dump past
// the watermark opens a window over the rows it wrote, a back-valued one
// among them, while the forward-valued row's day is still to come.
func TestWebChangeNumberIsTheDumpNotAValueDate(t *testing.T) {
	const day = int64(86400)
	t1 := 30*day + 3600 // the first dump
	forward := 33 * day // a value date three days after it
	t2 := t1 + day      // the next dump, before that day
	back := 29 * day    // a row the second dump reads, valued before the first

	webPath, seed := silvertest.NewDB(t, webSilverSchemaSQL)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := seed.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	addRow := func(id string, stamp, valued int64) {
		exec(`INSERT INTO transactions (transaction_external_id, account_external_id, snapshot_at,
              value_date, currency_iso, amount_debit, payload) VALUES (?, 'CH00WEB', ?, ?, 'CHF', 10, '{}')`,
			id, stamp, valued)
	}
	exec(`INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (?, 1, '/x/1')`, t1)
	addRow("FWD1", t1, forward)
	conn := openSubsources(t, "", webPath)
	ctx := context.Background()

	s, err := conn.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.LatestChangeNumber != t1 {
		t.Fatalf("change number = %d, want the dump's time %d, not the forward value date %d",
			s.LatestChangeNumber, t1, forward)
	}
	if idle, err := conn.ChangeWindow(ctx, t1); err != nil || idle.HasChanges {
		t.Fatalf("no dump past %d, but the window reports changes: %+v (%v)", t1, idle, err)
	}

	exec(`INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (?, 1, '/x/2')`, t2)
	addRow("BACK1", t2, back)
	w, err := conn.ChangeWindow(ctx, t1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.NewChangeNumber != t2 {
		t.Fatalf("a dump past the watermark must open a window and move it to %d: %+v", t2, w)
	}
	if w.Start > back || w.End < t2 {
		t.Errorf("window [%d, %d] must hold the back-valued row at %d and the dump at %d", w.Start, w.End, back, t2)
	}
}
