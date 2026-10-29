package firstcitizens

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// statementRatesTable holds the interest rate each statement states
// (collectors/firstcitizens/migrations/0003_statement_rates.sql): one row
// per account and cycle, the rate on the statement day in percent a year.
const statementRatesTable = "statement_rates"

// applyStatementRates puts the rate each statement states on a closing
// balance the batch already holds, as the payload's interest_rate,
// interest_side "credit" and interest_valid_from the statement's closing
// day, so the rate holds from that day even where its balance is an
// earlier one. The closing balances are the ledger's end-of-day
// running balances, one per day the balance moved, so a statement's closing
// day has one only when something posted that day. The rate goes on the
// account's closing balance on the statement's closing day, else on the
// latest one before it inside the statement's cycle. A cycle in which the
// balance never moved has no such balance, and its rate goes nowhere: no
// balance is added for it. A cycle whose statement states no rate has no
// row, so its balances state none. A silver older than the table states no
// rates.
func (c *Connection) applyStatementRates(ctx context.Context, batch *canonical.SnapshotBatch) error {
	has, err := silver.HasTables(ctx, c.db, statementRatesTable)
	if err != nil || !has {
		return err
	}
	// Each account's closing balances, in time order (appendRunningBalances
	// emits them ordered by account and day).
	closings := map[string][]int{}
	for i, cb := range batch.CashBalances {
		if cb.BalanceKind == canonical.BalanceKindClosing {
			closings[cb.AccountExternalID] = append(closings[cb.AccountExternalID], i)
		}
	}
	err = silver.ScanRows(ctx, c.db, `SELECT account_external_id, period_start, period_end, interest_rate
  FROM `+statementRatesTable+` ORDER BY account_external_id, period_end`, func(rows *sql.Rows) error {
		var (
			acct       string
			start, end int64
			rate       float64
		)
		if err := rows.Scan(&acct, &start, &end, &rate); err != nil {
			return err
		}
		at := -1
		for _, i := range closings[acct] {
			s := batch.CashBalances[i].SnapshotAt
			if s > end+silver.DayEndOffset {
				break
			}
			if s >= start {
				at = i
			}
		}
		if at < 0 {
			return nil
		}
		cb := &batch.CashBalances[at]
		day := time.Unix(end, 0).UTC().Format(time.DateOnly)
		payload, err := silver.WithCreditRate(cb.Payload, rate, day)
		cb.Payload = payload
		return err
	})
	if err != nil {
		return fmt.Errorf("firstcitizens statement rates: %w", err)
	}
	return nil
}
