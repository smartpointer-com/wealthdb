package equityzen

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Silver dates are source ISO TEXT (calendar dates); strftime('%s', …)
// converts them to the canonical unix seconds the gold contract uses.

// txExtrema yields the MIN/MAX cash-flow date — the buyer's purchases and
// distributions (see transactions.go).
const txExtrema = `
SELECT MIN(CAST(strftime('%s', flow_date) AS INTEGER)),
       MAX(CAST(strftime('%s', flow_date) AS INTEGER))
  FROM cash_flows WHERE flow_date IS NOT NULL`

// windowExtrema spans both fact streams (positions + cash_flows) so the
// re-emit window covers every snapshot AND every transaction. Cash-flow dates
// are a subset of position event dates today, but the union keeps the window
// correct regardless.
const windowExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT CAST(strftime('%s', as_of_date) AS INTEGER) AS t FROM positions WHERE as_of_date IS NOT NULL
    UNION ALL
    SELECT CAST(strftime('%s', flow_date) AS INTEGER) FROM cash_flows WHERE flow_date IS NOT NULL)`

// positionRange is the MIN/MAX position event date. The positions timeline
// runs from each deal's original investment (well before the first
// download) to the latest event date.
const positionRange = `
SELECT (SELECT MIN(CAST(strftime('%s', as_of_date) AS INTEGER)) FROM positions WHERE as_of_date IS NOT NULL),
       (SELECT MAX(CAST(strftime('%s', as_of_date) AS INTEGER)) FROM positions WHERE as_of_date IS NOT NULL)`

// clock: a new download, any dump past the watermark, re-emits the full
// event history. A download can revise any deal's marks at any past event
// date (a tender records, a statement NAV finalises), and the adapter
// forward-fills complete per-event portfolios, so re-emit-all is the
// natural unit. A dump that stopped listing a deal is a snapshot at the
// dump time (snapshots.go), which can lie after every mark: the snapshot
// range and the window reach it.
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind:     "equityzen",
		Runs:     silver.LatestDump,
		Snapshot: []string{positionRange},
		Extra:    c.removalDates,
		Ledger:   []string{txExtrema},
		Mode:     silver.FullSpan{Span: []string{windowExtrema}, Extra: c.removalDates},
	}
}

// removalDates reach the latest dump that stopped listing a deal.
func (c *Connection) removalDates(ctx context.Context) (lo, hi int64, err error) {
	removals, err := c.removals(ctx)
	return -1, removals.Reach(-1), err
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
