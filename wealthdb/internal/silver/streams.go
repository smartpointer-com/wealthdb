package silver

import (
	"context"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// NewSnapshotStream returns a SnapshotStream that walks a
// precomputed slice of batches. A nil/empty slice yields nothing
// (the no-changes case). Every adapter builds its batches eagerly
// and hands them here rather than reimplementing the index-walk
// iterator.
func NewSnapshotStream(batches []canonical.SnapshotBatch) SnapshotStream {
	return &sliceSnapshotStream{batches: batches}
}

type sliceSnapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

func (s *sliceSnapshotStream) Next(context.Context) (canonical.SnapshotBatch, bool, error) {
	if s.idx >= len(s.batches) {
		return canonical.SnapshotBatch{}, false, nil
	}
	b := s.batches[s.idx]
	s.idx++
	return b, s.idx < len(s.batches), nil
}

func (s *sliceSnapshotStream) Close() error { return nil }

// NewTransactionStream returns a TransactionStream that yields the
// given batch exactly once. Every adapter assembles a single
// transaction batch per load window (none yields more than one),
// so the single-shot shape covers them all; the no-changes case
// passes a zero batch (yielded once, applied as a no-op).
func NewTransactionStream(batch canonical.TransactionBatch) TransactionStream {
	return &singleTxStream{batch: batch}
}

type singleTxStream struct {
	batch    canonical.TransactionBatch
	consumed bool
}

func (s *singleTxStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	if s.consumed {
		return canonical.TransactionBatch{}, false, nil
	}
	s.consumed = true
	return s.batch, false, nil
}

func (s *singleTxStream) Close() error { return nil }
