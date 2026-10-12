package manual

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions yields one `sell` per exit (exits.csv): what a position
// brought on the day it left the book, gross of any part an agent holds
// back. The lot engine realizes it against the position's book value.
// Nothing else is a transaction here: every wire that funds a purchase,
// pays a fee, or returns a distribution is a real movement in the bank
// accounts, already captured by the bank collectors, and the proceeds
// themselves arrive there too. The exit is the one datum the banks cannot
// tie to a position. See collectors/manual/DESIGN.md §6.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	batch := canonical.TransactionBatch{}
	if !w.HasChanges || !c.exits {
		return silver.NewTransactionStream(batch), nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT e.position_id, COALESCE(p.account_id, '`+defaultAccountKey+`'),
       CAST(strftime('%s', e.exit_date) AS INTEGER), e.proceeds, e.currency, COALESCE(e.notes, '')
  FROM exits e
  JOIN positions p ON p.id = e.position_id
 WHERE CAST(strftime('%s', e.exit_date) AS INTEGER) BETWEEN ?1 AND ?2
 ORDER BY e.exit_date, e.position_id`, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("manual Transactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, acct, proceeds, ccy, notes string
			at                             int64
		)
		if err := rows.Scan(&id, &acct, &at, &proceeds, &ccy, &notes); err != nil {
			return nil, fmt.Errorf("manual Transactions: %w", err)
		}
		amt, err := canonical.NewDecimalFromString(proceeds)
		if err != nil {
			return nil, fmt.Errorf("manual Transactions: exit of %s: proceeds %q: %w", id, proceeds, err)
		}
		inst := id
		tx := canonical.TransactionChange{
			TransactionExternalID: "exit:" + id,
			OccurredAt:            at,
			AccountExternalID:     acct,
			InstrumentExternalID:  &inst,
			Kind:                  canonical.TxKindSell,
			Currency:              ccy,
			GrossAmount:           &amt,
			NetAmount:             &amt,
			Payload:               json.RawMessage(`{"exit": true}`),
		}
		if notes != "" {
			tx.Description = &notes
		}
		batch.Transactions = append(batch.Transactions, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manual Transactions: %w", err)
	}
	return silver.NewTransactionStream(batch), nil
}
