package manual

import (
	"context"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Transactions yields nothing. The manual collector tracks positions +
// valuations only and records no cash-flow ledger: every wire that funds a
// purchase, pays a fee, or returns a distribution is a real movement in the
// bank accounts, already captured by the bank collectors. Projecting
// them here (e.g. on a funding sentinel, as carta/equityzen do for their
// invisible cash) would only duplicate the banks. See
// collectors/manual/DESIGN.md §6. The interface still requires the method, so
// it returns an empty stream.
func (c *Connection) Transactions(_ context.Context, _ canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
