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

// The MIN/MAX observation date over the fact tables a file holds.
const (
	fxExtrema    = `SELECT MIN(snapshot_at), MAX(snapshot_at) FROM ` + silver.FxRatesTable
	ratesExtrema = `SELECT MIN(as_of), MAX(as_of) FROM ` + silver.RatesTable
	bothExtrema  = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM ` + silver.FxRatesTable + `
    UNION ALL SELECT as_of FROM ` + silver.RatesTable + `)`
)

// extrema is the observation-date range query of this file's fact
// tables. A file that holds neither reads the rates, and its Status
// fails on the missing table.
func (c *Connection) extrema() string {
	switch {
	case c.schema.fx && c.schema.rates:
		return bothExtrema
	case c.schema.fx:
		return fxExtrema
	}
	return ratesExtrema
}

// clock is the load clock over this file's fact tables: it reports their
// observation dates and no transactions. When a fetch run triggers it
// re-emits the full range, and the rows upsert by date in silver, so the
// latest fetch's revisions win.
func (c *Connection) clock() silver.Clock { return silver.LoadClock(c.kind, c.extrema()) }

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
