package angellist

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// contentMin and contentMax are the earliest and latest content date: the
// position event dates (position_snapshots.as_of_date) unioned with the
// funding-ledger dates (funding_transactions.occurred_at): a funding cash
// flow can predate the first investment (an opening deposit) or postdate
// the last valuation.
const (
	contentMin = `SELECT MIN(t) FROM (SELECT MIN(as_of_date) t FROM position_snapshots
                    UNION ALL SELECT MIN(occurred_at) FROM funding_transactions)`
	contentMax = `SELECT MAX(t) FROM (SELECT MAX(as_of_date) t FROM position_snapshots
                    UNION ALL SELECT MAX(occurred_at) FROM funding_transactions)`
)

// clock: a new download, any dump past the watermark, re-emits the full
// event history (silver.FullSpan). A download can revise any position's
// marks at any past event date (a K-1 finalises, a statement restates),
// and the adapter forward-fills complete per-event portfolios, so
// re-emit-all is the natural unit. The snapshot range runs from the
// earliest content date (an inception, an early K-1 year) to the latest,
// or to the latest removal or K-1 period end where that is later
// (reachDates); the transaction range is the funding-account cash
// ledger.
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind:     "angellist",
		Runs:     silver.LatestDump,
		Snapshot: []string{`SELECT (` + contentMin + `), (` + contentMax + `)`},
		Extra:    c.reachDates,
		Ledger:   []string{silver.Range("funding_transactions", "occurred_at")},
		Mode:     silver.FullSpan{},
	}
}

// reachDates reach the latest dump that stopped listing a position and the
// latest K-1 period end: snapshot times (snapshots.go) that can lie after
// every mark.
func (c *Connection) reachDates(ctx context.Context) (lo, hi int64, err error) {
	removals, err := c.removals(ctx)
	if err != nil {
		return -1, -1, err
	}
	k, _, err := c.readInKind(ctx)
	if err != nil {
		return -1, -1, err
	}
	return -1, k.Reach(removals.Reach(-1)), nil
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
