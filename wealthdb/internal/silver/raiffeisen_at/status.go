package raiffeisenat

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the loaded runs (dump_runs.snapshot_at,
// where the CURRENT balances are stamped), the transaction ledger
// (posted_at), and the daily-balance series (the closing cash marks, at each
// balance day's last second, which reach back further than the ledger's own
// window). It bounds the snapshot range in Status and the re-emit window in
// ChangeWindow (gold's deleteWindow over [Start,End] must cover every record
// it re-applies).
var spanExtrema = fmt.Sprintf(`
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT snapshot_at FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT balance_date + %d FROM daily_balances)`, silver.DayEndOffset)

// Status and ChangeWindow are the shared load clock: this source has no
// change feed of its own, so a loaded dump is the trigger and a full
// re-emit is the response. See silver.LoadClockStatus.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.LoadClockStatus(ctx, c.db, "raiffeisen_at", spanExtrema)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, "raiffeisen_at", spanExtrema, since)
}
