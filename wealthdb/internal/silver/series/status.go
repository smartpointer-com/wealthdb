package series

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// fx_rates.snapshot_at and rates.as_of are the observation date (Unix s @
// 00:00 UTC). dump_runs.snapshot_at is the bronze fetch-run timestamp, and
// MAX(dump_runs.snapshot_at) is the logical change number: each new
// download + `load` of the collector bumps it and a subsequent `wealthdb
// load` re-emits, while an idle reload is a no-op.

// clock is the load clock (silver.LoadClock) over this file's fact
// tables: it reports their observation dates and no transactions. When a
// fetch run triggers it re-emits the full range, and the rows upsert by
// date in silver, so the latest fetch's revisions win. A file that holds
// neither table reports no snapshot range.
func (c *Connection) clock() silver.Clock {
	var ranges []string
	if c.schema.fx {
		ranges = append(ranges, silver.Range(silver.FxRatesTable, "snapshot_at"))
	}
	if c.schema.rates {
		ranges = append(ranges, silver.Range(silver.RatesTable, "as_of"))
	}
	return silver.Clock{Kind: c.kind, Runs: silver.LatestDump, Snapshot: ranges, Mode: silver.FullSpan{}}
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
