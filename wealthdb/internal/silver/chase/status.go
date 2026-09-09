package chase

import (
	"context"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the transaction ledger (posted_at) — which
// carries both the per-day balance marks and the transaction events — and a
// card statement's period_end, where the statement-era closing balances land.
// It bounds the snapshot range in Status and the re-emit window in ChangeWindow
// (gold's deleteWindow over [Start,End] must cover every record it re-applies).
//
// period_end has to be in here for the window to be correct, not merely for
// Status to read well: a statement period with no ledger row of its own — a
// quiet month, or one whose transactions were refused — still emits a closing
// balance at its period_end, and a date outside the delete window is a record
// re-inserted without its predecessor being removed, so it would duplicate on
// every load. period_start is not included: nothing is emitted at it, and
// period_end already bounds every row this table produces.
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT period_end FROM statement_balances)`

// Status and ChangeWindow are the shared load clock: this source has no
// change feed of its own, so a loaded dump is the trigger and a full
// re-emit is the response. See silver.LoadClockStatus.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.LoadClockStatus(ctx, c.db, "chase", spanExtrema)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, "chase", spanExtrema, since)
}
