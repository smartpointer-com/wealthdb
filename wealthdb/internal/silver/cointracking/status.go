package cointracking

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The DuckDB range queries: transactions.occurred_at is a TIMESTAMP and
// coin_prices.as_of_date a DATE, so EXTRACT(epoch FROM …) coerces each to
// the unix seconds of the canonical model.
const (
	dumpRange  = `SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs`
	tradeRange = `SELECT CAST(EXTRACT(epoch FROM MIN(occurred_at)) AS BIGINT),
       CAST(EXTRACT(epoch FROM MAX(occurred_at)) AS BIGINT) FROM transactions`
	priceRange = `SELECT CAST(EXTRACT(epoch FROM CAST(MIN(as_of_date) AS TIMESTAMP)) AS BIGINT),
       CAST(EXTRACT(epoch FROM CAST(MAX(as_of_date) AS TIMESTAMP)) AS BIGINT) FROM coin_prices`
)

// clock: a dump past the watermark re-emits every transaction, every dump
// and every coin price. Silver rebuilds its transactions on each load by
// replaying the full trade history of each portfolio, so a transaction
// dated years back can appear with no other sign than the new dump; the
// coin prices are in the window because the adapter emits FX rates from
// them. The change number is the latest dump.
var clock = silver.Clock{
	Kind:     "cointracking",
	Runs:     silver.LatestDump,
	Snapshot: []string{dumpRange},
	Ledger:   []string{tradeRange},
	Pin:      true,
	Mode: silver.Incremental{
		New:   []string{`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs WHERE snapshot_at > ?`},
		Widen: []string{tradeRange, dumpRange, priceRange},
	},
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
