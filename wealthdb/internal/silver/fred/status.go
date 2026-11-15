package fred

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// fx_rates.snapshot_at and rates.as_of are the observation date (Unix s @
// 00:00 UTC). dump_runs.snapshot_at is the bronze fetch-run timestamp, and
// MAX(dump_runs.snapshot_at) is the logical change number: each new
// `fred download` + `load` bumps it and a subsequent `wealthdb load`
// re-emits, while an idle reload is a no-op.

// The MIN/MAX observation date over the fact tables: fx_rates alone, or
// fx_rates and rates where the file holds the latter.
const (
	fxExtrema   = `SELECT MIN(snapshot_at), MAX(snapshot_at) FROM fx_rates`
	bothExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM fx_rates
    UNION ALL SELECT as_of FROM ` + silver.RatesTable + `)`
)

// extrema is the observation-date range query of this file's fact tables.
func (c *Connection) extrema() string {
	if c.schema.rates {
		return bothExtrema
	}
	return fxExtrema
}

// clock is the load clock over this file's fact tables: it reports their
// observation dates and no transactions. When a fetch run triggers it
// re-emits the full range, and the rates upsert by date in silver, so the
// latest fetch's revisions win.
func (c *Connection) clock() silver.Clock { return silver.LoadClock(kindName, c.extrema()) }

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
