package firstcitizens

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the loaded runs (dump_runs.snapshot_at,
// where the CURRENT balances are stamped) and the transaction ledger: the
// transaction events at posted_at, and the per-day cash marks at the day's
// last second. It bounds the snapshot range in Status and the re-emit window
// in ChangeWindow (gold's deleteWindow over [Start,End] must cover every
// record it re-applies).
var spanExtrema = fmt.Sprintf(`
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT snapshot_at FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT posted_at + %d FROM transactions WHERE balance IS NOT NULL)`, silver.DayEndOffset)

// clock is the shared load clock (silver.LoadClock): this source has no
// change feed of its own, so a loaded dump is the trigger and a full
// re-emit is the response.
var clock = silver.LoadClock("firstcitizens", spanExtrema, silver.Range("transactions", "posted_at"))

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
