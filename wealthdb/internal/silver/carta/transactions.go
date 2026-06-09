package carta

import (
	"context"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// Transactions is always empty for carta: the silver has no
// transaction table. Carta's internal API surfaces option exercises
// inside the grant payload (not as standalone events) and fund
// cash-flows (capital calls / distributions) only as document
// notices, so there's no event-grain data to project. The interface
// is satisfied with an empty stream; a future silver that captures
// structured exercises/distributions can land them without touching
// gold.
type txStream struct {
	consumed bool
}

func (c *Connection) Transactions(_ context.Context, w canonical.Window) (silver.TransactionStream, error) {
	return &txStream{consumed: !w.HasChanges}, nil
}

func (s *txStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	if s.consumed {
		return canonical.TransactionBatch{}, false, nil
	}
	s.consumed = true
	return canonical.TransactionBatch{}, false, nil
}

func (s *txStream) Close() error { return nil }
