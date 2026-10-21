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
	if c.rates {
		return bothExtrema
	}
	return fxExtrema
}

// Status reports the observation-date range of the FX and interest rates
// and pins LatestChangeNumber and both observed watermarks to the latest
// loaded fetch run (silver.FetchRunStatus).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.FetchRunStatus(ctx, c.db, kindName, c.extrema())
}

// ChangeWindow triggers on a new fetch run (any dump_runs row past `since`).
// When triggered it re-emits the FULL range of the fact tables: gold's
// deleteWindow over [Start,End] makes the re-emit idempotent (rates upsert
// by date in silver, so the latest fetch's revisions win). NewChangeNumber
// advances to the latest fetch so an idle reload doesn't re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, kindName, c.extrema(), since)
}
