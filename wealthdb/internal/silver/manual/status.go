package manual

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Silver dates are source ISO TEXT (calendar dates); strftime('%s', …)
// converts them to the canonical unix seconds the gold contract uses.

// snapshotExtrema is the MIN/MAX holding event date (eventDates), the
// snapshot range Status reports. The earliest can be an acquisition years
// before the latest load.
func (c *Connection) snapshotExtrema() string {
	return `SELECT MIN(t), MAX(t) FROM (` + c.eventDates() + `)`
}

// clock: the manual collector rebuilds silver from the CSVs on every
// load, so each load bumps load_at, and a load past the watermark
// re-emits the full history (silver.FullSpan): the per-event
// forward-filled snapshots all reach gold. The transaction range is the
// exits'.
func (c *Connection) clock() silver.Clock {
	k := silver.Clock{
		Kind:     "manual",
		Runs:     `SELECT MAX(load_at) FROM load_runs`,
		Snapshot: []string{c.snapshotExtrema()},
		Observed: silver.Observed{Runs: `SELECT MAX(CAST(load_at AS INTEGER)) FROM load_runs`},
		Mode:     silver.FullSpan{},
	}
	if c.schema.exits {
		k.Ledger = []string{`
SELECT MIN(CAST(strftime('%s', exit_date) AS INTEGER)), MAX(CAST(strftime('%s', exit_date) AS INTEGER)) FROM exits`}
	}
	return k
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
