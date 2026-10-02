package plaid

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// spanExtrema is the MIN/MAX of every date the projection touches: the runs
// (every balance and holding carries a run's start), both ledgers' posting
// dates, the card statements' issue dates (their closing balances), and the
// start of every ledger window a run read. It bounds the snapshot range in
// Status and the re-emit window in ChangeWindow. Gold's deleteWindow over
// [Start,End] must cover every record it re-applies. A trade books no later
// than its posting date and no earlier than the oldest window start
// (investmentDate), so the span covers it too.
//
// The window starts belong here and would be easy to omit. Silver deletes a
// ledger's rows within each window a run read in full. It deletes the stale
// pending rows wherever they are dated. So a row Plaid stopped listing can be
// the oldest one. Every row silver ever held was stored by a run whose window
// reached its date. With every window in the span, a reload never leaves
// such a row standing in gold.
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT posted_at FROM investment_transactions
    UNION ALL SELECT window_start FROM run_products
    UNION ALL SELECT last_statement_issue_date FROM liabilities)`

// ledgerExtrema spans both ledgers. LoadClockStatus reads only the bank and
// card ledger, and an Item linked for investments alone has none.
const ledgerExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT posted_at AS t FROM transactions
    UNION ALL SELECT ` + investmentDate + ` FROM investment_transactions)`

// Status and ChangeWindow are the shared load clock: this source has no
// change feed of its own, so a loaded run is the trigger and a full re-emit
// is the response. See silver.LoadClockStatus.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return silver.LoadClockStatusOver(ctx, c.db, kindName, spanExtrema, ledgerExtrema)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return silver.LoadClockChangeWindow(ctx, c.db, kindName, spanExtrema, since)
}
