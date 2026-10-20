package refdata

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The change clock is the silver load, not the fetch: dump_runs.loaded_at
// rises with every load that writes a run, so a run fetched earlier but
// loaded later (an import from another machine, a re-parse of kept
// bronze) still reaches gold. The observed watermarks stay the fetch.
const (
	latestLoad  = `SELECT MAX(loaded_at) FROM dump_runs`
	latestFetch = `SELECT MAX(snapshot_at) FROM dump_runs`
)

// runTables are the tables of every silver version whose rows carry
// run_at: the reference tables but lines, since a line belongs to its
// composition's run.
var runTables = []string{"compositions", "attributes", "counterparties", "securities", "security_facts"}

// windowExtrema is the MIN/MAX as_of over the rows of tables that the
// runs loaded after a change number wrote.
func windowExtrema(tables []string) string {
	parts := make([]string, len(tables))
	for i, t := range tables {
		parts[i] = "SELECT as_of FROM " + t + " WHERE run_at IN (SELECT snapshot_at FROM runs)"
	}
	return `
WITH runs AS (SELECT snapshot_at FROM dump_runs WHERE loaded_at > ?)
SELECT MIN(as_of), MAX(as_of) FROM (
    ` + strings.Join(parts, "\n    UNION ALL ") + `)`
}

// Status reports the as-of range of the compositions as the snapshot
// range and pins LatestChangeNumber to the latest load. Both observed
// watermarks are the latest fetch run (silver.ObserveByRuns): status shows
// when reference data was last fetched. How old each composition is
// belongs to the exposure report, not to freshness.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.UnknownStatus()
	var oldS, newS sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(as_of), MAX(as_of) FROM compositions`).Scan(&oldS, &newS); err != nil {
		return s, fmt.Errorf("refdata Status snapshot: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}
	latest, err := silver.LatestRun(ctx, c.db, kindName, latestLoad)
	if err != nil {
		return s, err
	}
	s.LatestChangeNumber = latest
	if err := silver.ObserveByRuns(ctx, c.db, kindName, latestFetch, &s); err != nil {
		return s, err
	}
	return s, nil
}

// ChangeWindow triggers on a load past `since` and spans the as-of days
// the rows of the runs loaded since describe. Snapshots then re-emits
// every row inside that span, whichever run wrote it, so gold's windowed
// delete over the span leaves it as silver holds it. A load that wrote
// nothing still advances the watermark.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	latest, err := silver.LatestRun(ctx, c.db, kindName, latestLoad)
	if err != nil {
		return w, err
	}
	if latest <= since {
		return w, nil
	}
	w.NewChangeNumber = latest
	tables := runTables
	if c.yield {
		tables = slices.Concat(runTables, silver.ReferenceYieldTables)
	}
	var start, end sql.NullInt64
	if err := c.db.QueryRowContext(ctx, windowExtrema(tables), since).Scan(&start, &end); err != nil {
		return w, fmt.Errorf("refdata ChangeWindow: %w", err)
	}
	if start.Valid && end.Valid {
		w.Start, w.End, w.HasChanges = start.Int64, end.Int64, true
	}
	return w, nil
}
