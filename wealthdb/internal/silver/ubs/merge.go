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
//     - An era fold (buildEraFold) collapses a booking that two eras
//       both recorded: a statement reconstruction whose account,
//       value day, signed amount and currency match an export or
//       MT940 row is dropped, and its narrative is carried onto the
//       row that kept the booking. One booking, one row.
//     - The PSN stream is wrapped in a text fold
//       (psnWebTextFoldStream) that fills a narrative column the
//       MT940 feed left as a bare code from the account-statement
//       export's record of the same entry, matched on the bank's own
//       transaction number. It moves no row and no number: the hard
//       cut still decides which side emits, and amounts, dates, kinds
//       and ids are untouched.

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
		psnTaxPair             map[string]taxPair
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
		psnTaxPair, err = c.psn.taxPairByISIN(ctx)
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
	var portfolioCutoff, accountCutoff map[string]int64
	if c.web != nil {
		var err error
		cutoff, err = buildPSNStartByWebRel(ctx, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web cutoff: %w", err)
		}
		portfolioCutoff, accountCutoff, err = c.web.buildHistoricalCutoffs(ctx, cutoff, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web historical cutoff: %w", err)
		}
	}

	streams := make([]silver.SnapshotStream, 0, 3)
	if c.web != nil {
		hist, err := c.web.snapshotsHistorical(ctx, w, safekeepingByPortfolio, portfolioCutoff, accountCutoff)
		if err != nil {
			return nil, fmt.Errorf("ubs web Snapshots (historical): %w", err)
		}
		streams = append(streams, hist)
		s, err := c.web.snapshotsForOverlap(ctx, w, cutoff, psnTaxPair)
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
	return silver.NewConcatSnapshotStream(streams), nil
}

// Transactions applies a hard cut at PSN_start per relationship.
// Web emits transactions whose value_date is strictly before the
// cutover; PSN emits its events unfiltered for the remainder.
// See transactionsBeforePSNStart for why a hard cut and not an
// overlap merge.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	streams := make([]silver.TransactionStream, 0, 2)
	// The web side builds the same-day offset veto over both feeds and hands
	// back the PSN half, so a vetoed pair drops on both sides of the seam.
	var hints psnHints
	if c.web != nil {
		s, h, err := c.web.transactionsBeforePSNStart(ctx, w, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web Transactions: %w", err)
		}
		hints = h
		streams = append(streams, s)
		// Cards are web-only and the PSN cut does not touch them: there
		// is no PSN row for the seam to arbitrate against. They ride as
		// their own stream so the cash path's cutoff, offset veto and
		// text fold — all of which are about reconciling two feeds —
		// stay off rows only one feed has.
		cards, err := c.web.cardTransactions(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("ubs web card Transactions: %w", err)
		}
		if len(cards.Transactions) > 0 {
			streams = append(streams, silver.NewTransactionStream(cards))
		}
	}
	if c.psn != nil {
		s, err := c.psn.Transactions(ctx, w, hints.veto)
		if err != nil {
			return nil, fmt.Errorf("ubs psn Transactions: %w", err)
		}
		if c.web != nil {
			texts, err := c.web.transactionTextByKey(ctx)
			if err != nil {
				return nil, fmt.Errorf("ubs web transaction text: %w", err)
			}
			if len(texts) > 0 {
				s = &psnWebTextFoldStream{inner: s, texts: texts}
			}
		}
		// Outermost, so the export's own record of the entry — found by the
		// bank's number for it — fills a bare column first, and the dropped
		// statement copy only fills what is still bare after that.
		if len(hints.carry) > 0 {
			s = &psnStatementCarryStream{inner: s, texts: hints.carry}
		}
		streams = append(streams, s)
	}
	return silver.NewConcatTransactionStream(streams), nil
}

// psnWebTextFoldStream fills the narrative columns the MT940 feed left
// as a bare code from the account-statement export's record of the
// same entry.
//
// Both feeds carry the same bookings across the seam, and the hard cut
// gives the MT940 row to gold. What that row says is often the bank's
// code and nothing more: the :86: narrative reduces to a
// cash-withdrawal or dividend code, the :61: type code is the provider
// category, and MT940 carries no structured payee — the bank's own name
// on a charge code is the one the adapter asserts. The export's
// row for the same entry names the payee, the printed booking type and
// the instrument. Keyed on the bank's own number for the entry — the
// export's "Transaction no.", which the :61: line repeats as its bank
// reference, paired with the account so the two legs of an
// inter-account transfer stay apart — the text moves onto the row that
// is missing it and nothing else moves: the amount, the value date,
// the kind and the id are the MT940 row's, byte for byte.
//
// The payer's message does not travel with it. It is the payer's own
// words about the entry rather than the bank's record of whom it paid,
// and the columns this fills are the ones a narrative is read from.
//
// A row whose payload carries no bank reference — every feed but the
// cash movements — is passed through untouched, as is one no export
// row matches.
type psnWebTextFoldStream struct {
	inner silver.TransactionStream
	texts map[webTxTextKey]webTxText
}

func (s *psnWebTextFoldStream) Next(ctx context.Context) (canonical.TransactionBatch, bool, error) {
	batch, more, err := s.inner.Next(ctx)
	if err != nil {
		return batch, more, err
	}
	for i := range batch.Transactions {
		t := &batch.Transactions[i]
		var p cashMovementPayload
		if err := json.Unmarshal(t.Payload, &p); err != nil || p.BankRef == "" {
			continue
		}
		web, ok := s.texts[webTxTextKey{account: t.AccountExternalID, txnNo: p.BankRef}]
		if !ok {
			continue
		}
		t.Counterparty = richerText(t.Counterparty, web.counterparty)
		t.Description = richerText(t.Description, web.description)
		t.ProviderCategory = richerText(t.ProviderCategory, web.providerCategory)
	}
	return batch, more, nil
}

func (s *psnWebTextFoldStream) Close() error { return s.inner.Close() }

// psnStatementCarryStream is the PSN half of the era fold: it carries the
// narrative of a statement reconstruction the web side dropped onto the
// MT940 row that kept the booking.
//
// The three eras of the cash ledger overlap in time and share no id, so an
// entry printed on a statement and also carried by the feed reaches gold
// twice unless something matches them on the booking itself — account,
// value day, signed amount, currency (buildEraFold). The machine-readable
// record survives; this is what stops the fold from also losing what the
// printed one said. Per column and only downward (richerText), exactly as
// the export-side fold above: a column that is empty or a bare code takes
// the statement's value, a column that already says something keeps it.
//
// Keyed on the surviving row's event id, decided on the web side where
// both eras are in hand, so nothing is re-matched here. No row is added or
// dropped by this stream, and the amount, the value date, the kind and the
// id are the MT940 row's, byte for byte.
type psnStatementCarryStream struct {
	inner silver.TransactionStream
	texts map[string]webTxText
}

func (s *psnStatementCarryStream) Next(ctx context.Context) (canonical.TransactionBatch, bool, error) {
	batch, more, err := s.inner.Next(ctx)
	if err != nil {
		return batch, more, err
	}
	for i := range batch.Transactions {
		t := &batch.Transactions[i]
		alt, ok := s.texts[t.TransactionExternalID]
		if !ok {
			continue
		}
		t.Counterparty = richerText(t.Counterparty, alt.counterparty)
		t.Description = richerText(t.Description, alt.description)
		t.ProviderCategory = richerText(t.ProviderCategory, alt.providerCategory)
	}
	return batch, more, nil
}

func (s *psnStatementCarryStream) Close() error { return s.inner.Close() }

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

// foldWebPayloadAsWebKey injects the web JSON under the "web" key
// of a PSN-owned canonical row (the base row here is owned by PSN).
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
