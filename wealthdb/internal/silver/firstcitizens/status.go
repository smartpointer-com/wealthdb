package firstcitizens

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at) and the transaction ledger (posted_at) —
// the latter carries both the per-day cash marks and the transaction events.
// It bounds the snapshot range in Status and the re-emit window in ChangeWindow
// (gold's deleteWindow over [Start,End] must cover every record it re-applies).
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions)`

// Status and ChangeWindow are the shared load clock: this source has no
// change feed of its own, so a loaded dump is the trigger and a full
// re-emit is the response. See silver.LoadClockStatus.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.LoadClockStatus(ctx, c.db, "firstcitizens", spanExtrema)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, "firstcitizens", spanExtrema, since)
}
