package ubs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// This file implements silver.Connection on the orchestrator
// *Connection by delegating to the configured subsources.
//
// Single-subsource case: thin passthrough. Both-subsources case:
// the streams compose as follows.
//
//   Snapshots
//     - Historical PDF stream (web.snapshotsHistorical) for dates
//       pre-dating live coverage.
//     - Live web stream (web.snapshotsForOverlap) emits dimensions
//       only — filtered to snapshot_at < PSN-start per banking
//       relationship.
//     - PSN stream (psn.Snapshots) emits all dimensions plus the
//       positions / cash facts. The orchestrator wraps it in a
//       fold stream that injects web's per-(date, key) payload
//       under `payload.web` so web-only fields (cost_price,
//       lending_value, market_value_base) stay queryable through
//       PSN's faithful safekeeping + portfolio identity.
//
//   Transactions
//     - Hard cut at PSN-start per relationship — see
//       transactionsBeforePSNStart for why an overlap merge isn't
//       safe here.

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

// Snapshots emits the merged stream described in the file header.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	var (
		webPosPayloads         map[webPosKey]string
		webCashPayloads        map[webCashKey]string
		webMortgages           []canonical.PositionChange
		cutoff                 map[string]int64
		psnAssetClass          map[string]canonical.AssetClass
		safekeepingByPortfolio map[string]string
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
		webMortgages, err = c.web.latestMortgagePositions(ctx, w.End)
		if err != nil {
			return nil, err
		}
		// Map web historical securities onto real PSN safekeeping
		// accounts (1:1 portfolios only) for cross-cutover account
		// continuity. nil when PSN absent → overlay fallback.
		safekeepingByPortfolio, err = c.psn.safekeepingByPortfolio(ctx)
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
		hist, err := c.web.snapshotsHistorical(ctx, w, safekeepingByPortfolio)
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
		// Drop PSN position rows dated outside the [first, last] window
		// of PSN's securities-holdings batches. PSN's cash + forward
		// feeds can bracket those batches — they begin a day or two
		// before the first MT535 snapshot, and after a nightly run they
		// can arrive before that day's holdings land. On such a day PSN
		// emits a holdings-less positions snapshot (forwards / mortgage
		// only) that would otherwise win gold's "latest snapshot per
		// source" and blank out securities: a one-day dip to ~0 at the
		// leading edge, or a collapse to forwards-only at the trailing
		// edge (a same-day nightly captured before that day's holdings).
		// Cash/dimensions still flow, so the nearest complete securities
		// snapshot stays authoritative. The leading bound only bites
		// when web carries securities across the gap; the trailing bound
		// guards every configuration.
		first, last, ok, err := c.psn.holdingsSnapshotRange(ctx)
		if err != nil {
			return nil, fmt.Errorf("ubs psn holdings range: %w", err)
		}
		if ok {
			lower := int64(0)
			if c.web != nil {
				lower = first
			}
			s = &psnHoldingsGapFilter{inner: s, firstHoldingsAt: lower, lastHoldingsAt: last}
		}
		if len(webPosPayloads)+len(webCashPayloads)+len(webMortgages) > 0 {
			s = &psnWebFoldStream{
				inner:     s,
				webPos:    webPosPayloads,
				webCash:   webCashPayloads,
				mortgages: webMortgages,
			}
		}
		streams = append(streams, s)
	}
	return &concatSnapshotStream{streams: streams}, nil
}

// Transactions applies a hard cut at PSN_start per relationship.
// Web emits transactions whose value_date is strictly before the
// cutover; PSN emits its events unfiltered for the remainder.
// See transactionsBeforePSNStart for why a hard cut and not an
// overlap merge.
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
//
// The stream also carries forward web-only positions that PSN
// doesn't surface — currently just mortgages. For each PSN batch
// in the window, it injects one PositionChange per known mortgage
// at the batch's snapshot_at, so the gold-side
// "latest snapshot per silver source" query still surfaces them
// when PSN snapshots have progressed past the last web dump.
type psnWebFoldStream struct {
	inner     silver.SnapshotStream
	webPos    map[webPosKey]string
	webCash   map[webCashKey]string
	mortgages []canonical.PositionChange // template rows, snapshot_at set per batch
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
	// Only inject mortgages into batches that already carry
	// Positions. Otherwise gold's "latest snapshot per silver
	// source" query (which is MAX over positions.snapshot_at)
	// would land on a snapshot whose only row is the injected
	// mortgage — every other position would disappear from the
	// "today" view. Cash-only / fx-only PSN batches are skipped
	// for the same reason.
	if len(s.mortgages) > 0 && len(batch.Positions) > 0 {
		snap := batch.Positions[0].SnapshotAt
		for _, m := range s.mortgages {
			m.SnapshotAt = snap
			batch.Positions = append(batch.Positions, m)
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

// psnHoldingsGapFilter drops PSN PositionChanges whose SnapshotAt falls
// outside the [firstHoldingsAt, lastHoldingsAt] window of PSN's MT535
// holdings batches. PSN's cash and forward-contract feeds can bracket
// holdings: they go live a day or two before the first batch, and after
// a nightly run they can arrive before that day's holdings land. On
// those bracket days PSN emits a positions snapshot (forwards only, no
// securities) that would win gold's "latest snapshot per source" and
// replace the real portfolio with a near-empty view — a one-day dip to
// ~0 before holdings begin, or a collapse to forwards/mortgage-only
// when a same-day nightly is captured before that day's holdings. Both
// edges are suppressed here so the nearest complete securities snapshot
// stays authoritative. Cash balances and dimensions in the same batches
// pass through untouched, so PSN cash still flows. Running BEFORE
// psnWebFoldStream means an emptied bracket-day batch carries no
// positions, so the fold won't anchor a mortgage onto it either
// (matching the rule that a position-less snapshot must not hijack the
// today view). lastHoldingsAt == 0 means no upper bound.
type psnHoldingsGapFilter struct {
	inner           silver.SnapshotStream
	firstHoldingsAt int64
	lastHoldingsAt  int64
}

func (s *psnHoldingsGapFilter) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	batch, more, err := s.inner.Next(ctx)
	if err != nil {
		return batch, more, err
	}
	if len(batch.Positions) > 0 {
		kept := batch.Positions[:0]
		for _, p := range batch.Positions {
			if p.SnapshotAt >= s.firstHoldingsAt &&
				(s.lastHoldingsAt == 0 || p.SnapshotAt <= s.lastHoldingsAt) {
				kept = append(kept, p)
			}
		}
		batch.Positions = kept
	}
	return batch, more, nil
}

func (s *psnHoldingsGapFilter) Close() error { return s.inner.Close() }

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
