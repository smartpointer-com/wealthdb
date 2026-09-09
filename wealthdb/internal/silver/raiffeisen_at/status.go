package raiffeisenat

import (
	"context"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the transaction ledger (posted_at), and
// the daily-balance series (daily_balances.balance_date — the closing cash
// marks, which reach back further than the ledger's own window). It bounds the
// snapshot range in Status and the re-emit window in ChangeWindow (gold's
// deleteWindow over [Start,End] must cover every record it re-applies).
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT balance_date FROM daily_balances)`

// Status and ChangeWindow are the shared load clock: this source has no
// change feed of its own, so a loaded dump is the trigger and a full
// re-emit is the response. See silver.LoadClockStatus.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.LoadClockStatus(ctx, c.db, "raiffeisen_at", spanExtrema)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, "raiffeisen_at", spanExtrema, since)
}
