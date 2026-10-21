package curves

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits one RateObservationChange per rates row in the window
// (silver.NewRateStream). SilverSourceID is filled by the loader from
// config.
func (c *Connection) Snapshots(_ context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	return silver.NewRateStream(c.db, w), nil
}

// Transactions: curves is a reference-data source with no event-grain
// facts.
func (c *Connection) Transactions(_ context.Context, _ canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
