package relevate

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// clock: the snapshot range spans the dumps and the historical tables
// (silver migration 0002, from the Quartalsbericht PDFs). A dump or a
// transaction past the watermark opens a window over the new rows, its
// start reaching back to the earliest historical snapshot. The change
// number is the latest dump: the historical rows are outputs of the
// loader's PDF parse, not a change source of their own. The transaction
// range stays the -1 sentinel while the silver holds no transactions.
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind:     "relevate",
		Runs:     silver.LatestDump,
		Snapshot: []string{silver.Range("dump_runs", "snapshot_at")},
		Extra:    c.historicalRange,
		Ledger:   []string{silver.Range("transactions", "occurred_at")},
		Pin:      true,
		Mode: silver.Incremental{
			New: []string{
				silver.RangePast("dump_runs", "snapshot_at"),
				silver.RangePast("transactions", "occurred_at"),
			},
			Extra: c.historicalStart,
		},
	}
}

// historicalStart is the earliest historical snapshot, the start a
// window reaches back to: a rebuild from the same Quartalsbericht PDFs
// then replaces their rows in place rather than appending beside stale
// rows of the prior gold load.
func (c *Connection) historicalStart(ctx context.Context) (lo, hi int64, err error) {
	lo, _, err = c.historicalRange(ctx)
	return lo, -1, err
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
