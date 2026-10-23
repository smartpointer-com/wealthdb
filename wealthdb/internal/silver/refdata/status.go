package refdata

import (
	"context"
	"database/sql"
	"fmt"
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

// windowExtrema is the MIN/MAX as_of per kind of record
// (silver.ReferenceTableKinds) over the rows of tables that the runs
// loaded after a change number wrote.
func windowExtrema(tables []string) string {
	parts := make([]string, len(tables))
	for i, t := range tables {
		parts[i] = "SELECT '" + silver.ReferenceTableKinds[t] + "' AS kind, as_of FROM " + t +
			" WHERE run_at IN (SELECT snapshot_at FROM runs)"
	}
	return `
WITH runs AS (SELECT snapshot_at FROM dump_runs WHERE loaded_at > ?)
SELECT kind, MIN(as_of), MAX(as_of) FROM (
    ` + strings.Join(parts, "\n    UNION ALL ") + `)
 GROUP BY kind`
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
// the rows of the runs loaded since describe, each kind of record on a
// span of its own: a filing's period facts reach back years where the
// day's compositions do not, and one span would re-emit every
// composition since. Snapshots then re-emits every row of a kind inside
// its span, whichever run wrote it, so gold's windowed delete over that
// span leaves it as silver holds it. A load that wrote nothing still
// advances the watermark.
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
	// Each kind on a span of its own (canonical.Window.Spans), over the
	// tables whose rows carry run_at; lines follow their compositions.
	rows, err := c.db.QueryContext(ctx, windowExtrema(silver.ReferenceRunTables(c.yield)), since)
	if err != nil {
		return w, fmt.Errorf("refdata ChangeWindow: %w", err)
	}
	defer rows.Close()
	w.Spans = map[string]canonical.Span{}
	for rows.Next() {
		var kind string
		var span canonical.Span
		if err := rows.Scan(&kind, &span.Start, &span.End); err != nil {
			return w, fmt.Errorf("refdata ChangeWindow scan: %w", err)
		}
		w.Spans[kind] = span
		if !w.HasChanges {
			w.Start, w.End = span.Start, span.End
		}
		w.Start, w.End = min(w.Start, span.Start), max(w.End, span.End)
		w.HasChanges = true
	}
	return w, rows.Err()
}
