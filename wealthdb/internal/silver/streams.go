package silver

import (
	"context"
	"maps"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// Batches collects a snapshot stream's batches by snapshot time: an adapter
// seeds the times it reads, routes each row to its time's batch, and streams
// the result.
type Batches map[int64]*canonical.SnapshotBatch

// NewBatches returns an empty batch at each of times.
func NewBatches(times []int64) Batches {
	b := make(Batches, len(times))
	for _, t := range times {
		b[t] = &canonical.SnapshotBatch{}
	}
	return b
}

// At returns the batch at t, adding an empty one when there is none.
func (b Batches) At(t int64) *canonical.SnapshotBatch {
	if batch, ok := b[t]; ok {
		return batch
	}
	batch := &canonical.SnapshotBatch{}
	b[t] = batch
	return batch
}

// Sorted returns the batches in ascending time order.
func (b Batches) Sorted() []canonical.SnapshotBatch {
	out := make([]canonical.SnapshotBatch, 0, len(b))
	for _, t := range slices.Sorted(maps.Keys(b)) {
		out = append(out, *b[t])
	}
	return out
}

// Stream yields the batches in ascending time order, which gold's
// earlier-wins FirstSeenAt accounting on instruments and accounts relies on.
func (b Batches) Stream() SnapshotStream { return NewSnapshotStream(b.Sorted()) }

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

// NewConcatSnapshotStream returns a SnapshotStream that drains each
// underlying stream in order, then reports exhaustion. Adapters that
// merge two source streams (api+web, psn+web) hand their ordered
// slice here rather than each carrying its own concat type.
func NewConcatSnapshotStream(streams []SnapshotStream) SnapshotStream {
	return &concatSnapshotStream{streams: streams}
}

type concatSnapshotStream struct {
	streams []SnapshotStream
	idx     int
}

func (s *concatSnapshotStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	if s.idx < len(s.streams) {
		batch, more, err := s.streams[s.idx].Next(ctx)
		if err != nil {
			return canonical.SnapshotBatch{}, false, err
		}
		if more {
			return batch, true, nil
		}
		// A final possibly-empty batch with more=false ends this
		// stream; advance and report more based on what's left.
		s.idx++
		anyLeft := s.idx < len(s.streams)
		return batch, anyLeft, nil
	}
	return canonical.SnapshotBatch{}, false, nil
}

func (s *concatSnapshotStream) Close() error {
	var firstErr error
	for _, st := range s.streams {
		if err := st.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NewConcatTransactionStream mirrors NewConcatSnapshotStream for
// transaction streams.
func NewConcatTransactionStream(streams []TransactionStream) TransactionStream {
	return &concatTransactionStream{streams: streams}
}

type concatTransactionStream struct {
	streams []TransactionStream
	idx     int
}

func (s *concatTransactionStream) Next(ctx context.Context) (canonical.TransactionBatch, bool, error) {
	if s.idx < len(s.streams) {
		batch, more, err := s.streams[s.idx].Next(ctx)
		if err != nil {
			return canonical.TransactionBatch{}, false, err
		}
		if more {
			return batch, true, nil
		}
		s.idx++
		anyLeft := s.idx < len(s.streams)
		return batch, anyLeft, nil
	}
	return canonical.TransactionBatch{}, false, nil
}

func (s *concatTransactionStream) Close() error {
	var firstErr error
	for _, st := range s.streams {
		if err := st.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
