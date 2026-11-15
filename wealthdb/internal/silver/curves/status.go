package curves

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// rates.as_of is the observation date (Unix s @ 00:00 UTC).
// dump_runs.snapshot_at is the bronze fetch-run timestamp, and
// MAX(dump_runs.snapshot_at) is the logical change number: each new
// `curves download` + `load` bumps it and a subsequent `wealthdb load`
// re-emits, while an idle reload is a no-op.

// extrema is the observation-date range of the rates.
const extrema = `SELECT MIN(as_of), MAX(as_of) FROM ` + silver.RatesTable

// clock is the load clock over the rates: it reports their observation
// dates and no transactions. When a fetch run triggers it re-emits the
// full range, and the rates upsert by date in silver, so the latest
// fetch's revisions win.
var clock = silver.LoadClock(kindName, extrema)

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
