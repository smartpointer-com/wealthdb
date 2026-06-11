package relevate

import (
	"context"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// Transactions emits whatever rows silver.transactions carries.
// In current Relevate silvers this is empty — Relevate's web
// app exposes no transaction-history endpoint, so the silver
// loader's deposits_endpoint source produces no rows. The
// schema is plumbed so a future Relevate API change (or PDF
// credit-note parsing) can land transactions without touching
// gold.
func (c *Connection) Transactions(_ context.Context, w canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
