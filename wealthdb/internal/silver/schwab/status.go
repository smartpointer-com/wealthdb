package schwab

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// clock is the api feed's: a dump or a transaction past the watermark
// opens a window over the new rows (docs/DESIGN.md §6.2 and §8.1), its
// start reaching back two dumps (reachBack). Each pull reads the
// holdings, and the ledger from its last pull, so the latest dump is the
// change number and both observed watermarks. Per docs/adapters/schwab.md
// §6, the change number is -1 when there are no dumps at all.
func (c *apiReader) clock() silver.Clock {
	return silver.Clock{
		Kind:     "schwab",
		Runs:     silver.LatestDump,
		Snapshot: []string{silver.Range("dump_runs", "snapshot_at")},
		Ledger:   []string{silver.Range("transactions", "timestamp")},
		Pin:      true,
		Mode: silver.Incremental{
			New: []string{
				silver.RangePast("dump_runs", "snapshot_at"),
				silver.RangePast("transactions", "timestamp"),
			},
			ReachBack: c.reachBack,
		},
	}
}

// reachBack is the second dump before start, or start where there are
// fewer: the mark guard reads a spike by the two observations after it,
// so a new snapshot can turn either of the two before it into a spike
// (marks.go), and the load re-emits them.
func (c *apiReader) reachBack(ctx context.Context, start int64) (int64, error) {
	var back sql.NullInt64
	if err := c.db.QueryRowContext(ctx, `
SELECT MIN(snapshot_at) FROM (
    SELECT snapshot_at FROM dump_runs WHERE snapshot_at < ? ORDER BY snapshot_at DESC LIMIT 2)`,
		start).Scan(&back); err != nil {
		return start, err
	}
	if back.Valid {
		return back.Int64, nil
	}
	return start, nil
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *apiReader) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *apiReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
