package plaid

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: the runs
// (every balance and holding carries a run's start), both ledgers' posting
// dates, the card statements' issue dates (their closing balances), and the
// start of every ledger window a run read. It is the snapshot range Status
// reports.
//
// A load re-emits the whole history and deletes the source's rows whole
// first (silver.FullSpan). Silver deletes a ledger's rows within each window
// a run read in full, and the stale pending rows wherever they are dated, so
// a row Plaid stopped listing leaves gold on the next load too.
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT posted_at FROM investment_transactions
    UNION ALL SELECT window_start FROM run_products
    UNION ALL SELECT last_statement_issue_date FROM liabilities)`

// ledgerExtrema is the transaction range over both ledgers: an Item linked
// for investments alone has no bank or card ledger.
const ledgerExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT posted_at AS t FROM transactions
    UNION ALL SELECT ` + investmentDate + ` FROM investment_transactions)`

// clock is the shared load clock (silver.LoadClock): this source has no
// change feed of its own, so a loaded run is the trigger and a full
// re-emit is the response.
var clock = silver.LoadClock(kindName, spanExtrema, ledgerExtrema)

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
