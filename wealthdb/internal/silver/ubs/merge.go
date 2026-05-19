package ubs

import (
	"bytes"
	"context"
	"encoding/json"
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

// Snapshots emits the merged stream. Dimensions (portfolios,
// accounts) follow iteration 1 — web emits dimensions strictly
// before PSN_start, PSN emits all dimensions, the per-column
// upsert guard reconciles. Facts (positions, cash) come solely
// from PSN; web's per-(date, key) payload is folded into PSN's
// rows under `payload.web` so PSN-missing fields like
// cost_price stay visible.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	var (
		webPosPayloads  map[webPosKey]string
		webCashPayloads map[webCashKey]string
		cutoff          map[string]int64
		psnAssetClass   map[string]canonical.AssetClass
	)

	if c.web != nil && c.psn != nil {
		var err error
		webPosPayloads, err = c.web.positionPayloadByKey(ctx, w.Start, w.End)
		if err != nil {
			return nil, err
		}
		webCashPayloads, err = c.web.cashPayloadByKey(ctx, w.Start, w.End)
		if err != nil {
			return nil, err
		}
		psnAssetClass, err = c.psn.assetClassByISIN(ctx)
		if err != nil {
			return nil, err
		}
	}
	if c.web != nil {
		var err error
		cutoff, err = buildPSNStartByWebRel(ctx, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web cutoff: %w", err)
		}
	}

	streams := make([]silver.SnapshotStream, 0, 3)
	if c.web != nil {
		hist, err := c.web.snapshotsHistorical(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("ubs web Snapshots (historical): %w", err)
		}
		streams = append(streams, hist)
		s, err := c.web.snapshotsForOverlap(ctx, w, cutoff, psnAssetClass)
		if err != nil {
			return nil, fmt.Errorf("ubs web Snapshots: %w", err)
		}
		streams = append(streams, s)
	}
	if c.psn != nil {
		s, err := c.psn.Snapshots(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("ubs psn Snapshots: %w", err)
		}
		if len(webPosPayloads)+len(webCashPayloads) > 0 {
			s = &psnWebFoldStream{
				inner:     s,
				webPos:    webPosPayloads,
				webCash:   webCashPayloads,
				lookupSK:  c.psn,
				ctx:       ctx,
			}
		}
		streams = append(streams, s)
	}
	return &concatSnapshotStream{streams: streams}, nil
}

// Transactions keeps iteration 1's hard cut at PSN_start. Web
// emits transactions whose value_date is strictly before the
// per-relationship cutover; PSN emits its events unfiltered for
// the remainder. We tried iteration-2-style identity merge but
// the silvers' transaction_external_id schemes don't actually
// align in practice (web uses UBS Transaction No. like
// "0104030TJ0060041"; PSN events use prefixed strings like
// "mt515:..."), so any cross-source match would be heuristic and
// risk double-counting. Hard cut is the safe choice.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	streams := make([]silver.TransactionStream, 0, 2)
	if c.web != nil {
		s, err := c.web.transactionsBeforePSNStart(ctx, w, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web Transactions: %w", err)
		}
		streams = append(streams, s)
	}
	if c.psn != nil {
		s, err := c.psn.Transactions(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("ubs psn Transactions: %w", err)
		}
		streams = append(streams, s)
	}
	return &concatTransactionStream{streams: streams}, nil
}

// psnWebFoldStream wraps a SnapshotStream and folds web's
// per-(UTC date, key) payload into PSN's position and cash rows.
// PSN keeps identity (safekeeping accounts, faithful portfolios,
// parsed MT535 values); web's CSV row is preserved under the
// "web" key inside the row's payload so cost_price / lending_
// value / market_value_base remain queryable.
//
// Position match key is (utc_day(snapshot_at), ISIN). Cash match
// key is (utc_day(snapshot_at), account, currency).
type psnWebFoldStream struct {
	inner   silver.SnapshotStream
	webPos  map[webPosKey]string
	webCash map[webCashKey]string
	// ctx is captured here only to keep the Stream interface
	// signature (Next takes ctx); we don't fan out any work.
	ctx      context.Context
	lookupSK *psnReader // reserved for future (cross-source acct lookup); currently unused
}

func (s *psnWebFoldStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	batch, more, err := s.inner.Next(ctx)
	if err != nil {
		return batch, more, err
	}
	for i := range batch.Positions {
		p := &batch.Positions[i]
		key := webPosKey{utcDate: utcDay(p.SnapshotAt), isin: p.PositionKey}
		if webPayload, ok := s.webPos[key]; ok {
			p.Payload = foldWebPayloadAsWebKey(p.Payload, webPayload)
		}
	}
	for i := range batch.CashBalances {
		cb := &batch.CashBalances[i]
		key := webCashKey{utcDate: utcDay(cb.SnapshotAt), account: cb.AccountExternalID, currency: cb.Currency}
		if webPayload, ok := s.webCash[key]; ok {
			cb.Payload = foldWebPayloadAsWebKey(cb.Payload, webPayload)
		}
	}
	return batch, more, nil
}

func (s *psnWebFoldStream) Close() error { return s.inner.Close() }

// foldWebPayloadAsWebKey mirrors foldPSNPayload but injects the
// web JSON under the "web" key rather than "psn". The base
// (canonical) row is owned by PSN here.
func foldWebPayloadAsWebKey(base json.RawMessage, web string) json.RawMessage {
	if web == "" {
		return base
	}
	baseTrim := bytes.TrimSpace(base)
	if len(baseTrim) == 0 || baseTrim[0] != '{' {
		return json.RawMessage(`{"web":` + web + `}`)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(baseTrim, &m); err != nil {
		return json.RawMessage(`{"web":` + web + `}`)
	}
	m["web"] = json.RawMessage(web)
	out, err := json.Marshal(m)
	if err != nil {
		return baseTrim
	}
	return out
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
