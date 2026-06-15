package fred

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// fxBatchSize bounds how many FxRateChange rows ride in one SnapshotBatch,
// so a full-history re-emit (decades × ~8 pairs) doesn't build one giant
// batch. Each FxRateChange carries its own SnapshotAt, so a batch may span
// multiple dates.
const fxBatchSize = 2000

// Snapshots emits one FxRateChange per fx_rates row in the window. fred has
// no other snapshot-grain facts. The fx_rates direction already matches the
// canonical convention ((base, quote, mid) = "1 quote = mid base"), so the
// columns map straight across; SilverSourceID is filled by the loader from
// config.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	const q = `
SELECT snapshot_at, base_currency_iso, quote_currency_iso, mid, payload
  FROM fx_rates
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("fred Snapshots: %w", err)
	}
	defer rows.Close()

	var (
		batches []canonical.SnapshotBatch
		cur     canonical.SnapshotBatch
	)
	for rows.Next() {
		var (
			snap                            int64
			base, quote, mid, payload       string
		)
		if err := rows.Scan(&snap, &base, &quote, &mid, &payload); err != nil {
			return nil, fmt.Errorf("fred Snapshots scan: %w", err)
		}
		rate, err := canonical.NewDecimalFromString(mid)
		if err != nil {
			return nil, fmt.Errorf("fred fx_rates mid %q (snap=%d): %w", mid, snap, err)
		}
		cur.FxRates = append(cur.FxRates, canonical.FxRateChange{
			SnapshotAt:    snap,
			BaseCurrency:  base,
			QuoteCurrency: quote,
			MidRate:       rate,
			Payload:       json.RawMessage(payload),
		})
		if len(cur.FxRates) >= fxBatchSize {
			batches = append(batches, cur)
			cur = canonical.SnapshotBatch{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fred Snapshots rows: %w", err)
	}
	if len(cur.FxRates) > 0 {
		batches = append(batches, cur)
	}
	return silver.NewSnapshotStream(batches), nil
}

// Transactions: fred is a reference-data source with no event-grain facts.
func (c *Connection) Transactions(_ context.Context, _ canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
