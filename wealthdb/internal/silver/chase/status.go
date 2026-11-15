package chase

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the transaction ledger (posted_at) — which
// carries both the per-day balance marks and the transaction events — and a
// card statement's period_end, where the statement-era closing balances land.
// It is the snapshot range Status reports.
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT period_end FROM statement_balances)`

// clock is the shared load clock (silver.LoadClock): this source has no
// change feed of its own, so a loaded dump is the trigger and a full
// re-emit is the response.
var clock = silver.LoadClock("chase", spanExtrema, silver.Range("transactions", "posted_at"))

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
