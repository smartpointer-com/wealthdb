package angellist

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// clock: a new download, any dump past the watermark, re-emits the full
// event history (silver.FullSpan). A download can revise any position's
// marks at any past event date (a K-1 finalises, a statement restates),
// and the adapter forward-fills complete per-event portfolios, so
// re-emit-all is the natural unit. The snapshot range runs from the
// earliest content date to the latest: the position event dates
// (position_snapshots.as_of_date, an inception, an early K-1 year) and
// the funding-ledger dates (funding_transactions.occurred_at), since a
// funding cash flow can predate the first investment (an opening deposit)
// or postdate the last valuation. It reaches the latest removal or K-1
// period end where that is later (reachDates). The transaction range is
// the funding-account cash ledger. A silver without the ledger has
// neither ledger range.
func (c *Connection) clock() silver.Clock {
	k := silver.Clock{
		Kind:     "angellist",
		Runs:     silver.LatestDump,
		Snapshot: []string{silver.Range("position_snapshots", "as_of_date")},
		Extra:    c.reachDates,
		Mode:     silver.FullSpan{},
	}
	if c.schema.fundingTransactions {
		ledger := silver.Range("funding_transactions", "occurred_at")
		k.Snapshot = append(k.Snapshot, ledger)
		k.Ledger = []string{ledger}
	}
	return k
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
