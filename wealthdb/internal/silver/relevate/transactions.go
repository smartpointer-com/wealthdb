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
