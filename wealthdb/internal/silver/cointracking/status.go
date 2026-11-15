package cointracking

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The DuckDB range queries: transactions.occurred_at is a TIMESTAMP, so
// EXTRACT(epoch FROM …) coerces it to the unix seconds of the canonical
// model.
const (
	dumpRange  = `SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs`
	tradeRange = `SELECT CAST(EXTRACT(epoch FROM MIN(occurred_at)) AS BIGINT),
       CAST(EXTRACT(epoch FROM MAX(occurred_at)) AS BIGINT) FROM transactions`
)

// clock: a dump past the watermark re-emits the whole history
// (silver.FullSpan). Silver rebuilds its transactions on each load by
// replaying the full trade history of each portfolio, so a transaction
// dated years back can appear with no other sign than the new dump. The
// change number is the latest dump.
var clock = silver.Clock{
	Kind:     "cointracking",
	Runs:     silver.LatestDump,
	Snapshot: []string{dumpRange},
	Ledger:   []string{tradeRange},
	Pin:      true,
	Mode:     silver.FullSpan{},
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
