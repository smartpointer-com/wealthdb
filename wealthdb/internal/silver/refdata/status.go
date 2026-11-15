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

// clock: the change number is the latest load, and a load past the
// watermark re-reads each kind of record on a span of its own (spans): a
// filing's period facts reach back years where the day's compositions do
// not, and one span would re-emit every composition since. Snapshots then
// re-emits every row of a kind inside its span, whichever run wrote it,
// so gold's windowed delete over that span leaves it as silver holds it.
// A load that wrote nothing still advances the watermark. The snapshot
// range is the as-of range of the compositions; both observed watermarks
// are the latest fetch run, so status shows when reference data was last
// fetched. How old each composition is belongs to the exposure report,
// not to freshness.
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind:     kindName,
		Runs:     latestLoad,
		Snapshot: []string{silver.Range("compositions", "as_of")},
		Observed: silver.Observed{Runs: latestFetch},
		Mode:     silver.PerKind(c.spans),
	}
}

// spans are the as-of spans of each kind of record (canonical.Window.Spans)
// over the tables whose rows carry run_at, for the runs loaded past since;
// lines follow their compositions.
func (c *Connection) spans(ctx context.Context, since int64) (map[string]canonical.Span, error) {
	spans := map[string]canonical.Span{}
	if err := silver.ScanRows(ctx, c.db, "refdata ChangeWindow", windowExtrema(silver.ReferenceRunTables(c.schema)), func(rows *sql.Rows) error {
		var kind string
		var span canonical.Span
		if err := rows.Scan(&kind, &span.Start, &span.End); err != nil {
			return fmt.Errorf("refdata ChangeWindow scan: %w", err)
		}
		spans[kind] = span
		return nil
	}, since); err != nil {
		return nil, err
	}
	return spans, nil
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
