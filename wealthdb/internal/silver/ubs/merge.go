package ubs

import (
	"context"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// This file implements silver.Connection on the orchestrator
// *Connection by delegating to the configured subsources.
// Iteration 1 (current): when only one subsource is configured the
// orchestrator is a thin passthrough; when both are configured the
// merge logic in this file splices web (pre-PSN-start) and PSN
// (>= PSN-start) into a single stream, per banking relationship.
// Iteration 2 (planned): replace the hard cut with an overlap
// merge — web is the source of truth for IDs and structured
// fields, PSN payload extends.

// Status aggregates the per-subsource Status. The change number
// is max across subsources (so the gold watermark covers
// whichever advanced further); snapshot/transaction extrema use
// (min, max) across the union.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	// Sentinel-aware combiner. -1 means "no observable state" per
	// canonical.Status; treat as absent during the combine.
	combine := func(a, b int64, op func(int64, int64) int64) int64 {
		switch {
		case a == -1:
			return b
		case b == -1:
			return a
		default:
			return op(a, b)
		}
	}
	min := func(a, b int64) int64 {
		if a < b {
			return a
		}
		return b
	}
	max := func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	}

	out := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	if c.psn != nil {
		s, err := c.psn.Status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("ubs psn status: %w", err)
		}
		out.OldestSnapshotAt = combine(out.OldestSnapshotAt, s.OldestSnapshotAt, min)
		out.LatestSnapshotAt = combine(out.LatestSnapshotAt, s.LatestSnapshotAt, max)
		out.OldestTransactionAt = combine(out.OldestTransactionAt, s.OldestTransactionAt, min)
		out.LatestTransactionAt = combine(out.LatestTransactionAt, s.LatestTransactionAt, max)
		out.LatestChangeNumber = combine(out.LatestChangeNumber, s.LatestChangeNumber, max)
	}
	if c.web != nil {
		s, err := c.web.Status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("ubs web status: %w", err)
		}
		out.OldestSnapshotAt = combine(out.OldestSnapshotAt, s.OldestSnapshotAt, min)
		out.LatestSnapshotAt = combine(out.LatestSnapshotAt, s.LatestSnapshotAt, max)
		out.OldestTransactionAt = combine(out.OldestTransactionAt, s.OldestTransactionAt, min)
		out.LatestTransactionAt = combine(out.LatestTransactionAt, s.LatestTransactionAt, max)
		out.LatestChangeNumber = combine(out.LatestChangeNumber, s.LatestChangeNumber, max)
	}
	return out, nil
}

// ChangeWindow produces a window covering any subsource that has
// advanced past sinceN. NewChangeNumber is max across subsources
// (so the watermark moves forward by whatever progress either
// side made). HasChanges is true when at least one subsource
// reports changes.
func (c *Connection) ChangeWindow(ctx context.Context, sinceN int64) (canonical.Window, error) {
	out := canonical.Window{NewChangeNumber: sinceN}
	min := func(a, b int64) int64 {
		if a < b {
			return a
		}
		return b
	}
	max := func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	}
	if c.psn != nil {
		w, err := c.psn.ChangeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("ubs psn ChangeWindow: %w", err)
		}
		if w.HasChanges {
			if !out.HasChanges {
				out.Start, out.End = w.Start, w.End
			} else {
				out.Start = min(out.Start, w.Start)
				out.End = max(out.End, w.End)
			}
			out.HasChanges = true
		}
		out.NewChangeNumber = max(out.NewChangeNumber, w.NewChangeNumber)
	}
	if c.web != nil {
		w, err := c.web.ChangeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("ubs web ChangeWindow: %w", err)
		}
		if w.HasChanges {
			if !out.HasChanges {
				out.Start, out.End = w.Start, w.End
			} else {
				out.Start = min(out.Start, w.Start)
				out.End = max(out.End, w.End)
			}
			out.HasChanges = true
		}
		out.NewChangeNumber = max(out.NewChangeNumber, w.NewChangeNumber)
	}
	return out, nil
}

// Snapshots emits a single merged stream. PSN data passes through
// unfiltered (PSN is authoritative within its window); web data
// is filtered through a per-banking-relationship cutoff so any
// snapshot_at >= the PSN-start for that relationship is dropped
// — PSN covers those dates. Web's authority is the dates BEFORE
// PSN-start (the historical backfill PSN structurally can't
// deliver).
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	streams := make([]silver.SnapshotStream, 0, 2)
	if c.psn != nil {
		s, err := c.psn.Snapshots(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("ubs psn Snapshots: %w", err)
		}
		streams = append(streams, s)
	}
	if c.web != nil {
		cutoff, err := buildPSNStartByWebRel(ctx, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web cutoff: %w", err)
		}
		accountToRel, err := c.web.buildAccountToRelMap(ctx)
		if err != nil {
			return nil, fmt.Errorf("ubs web account→rel: %w", err)
		}
		s, err := c.web.snapshotsWith(ctx, w, cutoff, accountToRel)
		if err != nil {
			return nil, fmt.Errorf("ubs web Snapshots: %w", err)
		}
		streams = append(streams, s)
	}
	return &concatSnapshotStream{streams: streams}, nil
}

// Transactions emits a single merged stream. Iteration 1: PSN and
// web each produce their own already-spliced subset (PSN omits
// pre-PSN-start, web omits >= PSN-start), and we concatenate.
// The PSN-start cutover is computed once per banking relationship
// inside web_transactions.go's reader, using the relationships
// pairing from OpenSpec to know which PSN side to ask.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	streams := make([]silver.TransactionStream, 0, 2)
	if c.psn != nil {
		s, err := c.psn.Transactions(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("ubs psn Transactions: %w", err)
		}
		streams = append(streams, s)
	}
	if c.web != nil {
		s, err := c.web.Transactions(ctx, w, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web Transactions: %w", err)
		}
		streams = append(streams, s)
	}
	return &concatTransactionStream{streams: streams}, nil
}

// concatSnapshotStream drains each underlying stream in order.
type concatSnapshotStream struct {
	streams []silver.SnapshotStream
	idx     int
}

func (s *concatSnapshotStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	for s.idx < len(s.streams) {
		batch, more, err := s.streams[s.idx].Next(ctx)
		if err != nil {
			return canonical.SnapshotBatch{}, false, err
		}
		if more {
			return batch, true, nil
		}
		// `more=false` delivers a final possibly-empty batch from
		// this stream; move on to the next stream and report
		// `more` based on whether anything is left.
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

// concatTransactionStream mirrors concatSnapshotStream for
// transactions.
type concatTransactionStream struct {
	streams []silver.TransactionStream
	idx     int
}

func (s *concatTransactionStream) Next(ctx context.Context) (canonical.TransactionBatch, bool, error) {
	for s.idx < len(s.streams) {
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
