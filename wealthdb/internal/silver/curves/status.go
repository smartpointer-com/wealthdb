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

// Status reports the observation-date range of the rates and pins
// LatestChangeNumber and both observed watermarks to the latest loaded
// fetch run (silver.FetchRunStatus).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.FetchRunStatus(ctx, c.db, kindName, extrema)
}

// ChangeWindow triggers on a new fetch run (any dump_runs row past
// `since`). When triggered it re-emits the FULL range of the rates:
// gold's deleteWindow over [Start,End] makes the re-emit idempotent
// (rates upsert by date in silver, so the latest fetch's revisions win).
// NewChangeNumber advances to the latest fetch so an idle reload doesn't
// re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, kindName, extrema, since)
}
