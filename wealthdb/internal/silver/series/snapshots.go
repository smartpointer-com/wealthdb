package series

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits one FxRateChange per fx_rates row in the window, then
// one RateObservationChange per rates row in it, each in batches of their
// own, for the tables the file holds. SilverSourceID is filled by the
// loader from config.
func (c *Connection) Snapshots(_ context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	var streams []silver.SnapshotStream
	if c.schema.fx {
		streams = append(streams, silver.NewFxRateStream(c.db, w, c.kind))
	}
	if c.schema.rates {
		streams = append(streams, silver.NewRateStream(c.db, w))
	}
	if len(streams) == 1 {
		return streams[0], nil
	}
	return silver.NewConcatSnapshotStream(streams), nil
}

// Transactions: a series source has no event-grain facts.
func (c *Connection) Transactions(_ context.Context, _ canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
