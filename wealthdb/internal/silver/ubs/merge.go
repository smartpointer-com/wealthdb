package ubs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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
//     - A paid-in fold (paidInStream) over the merged stream sets the
//       basis of a private-markets fund's units, which neither feed
//       states, from the capital calls web reads.
//
//   Realized lots (RealizedLots, realized.go) come from web alone.
//
//   Transactions
//     - Hard cut at PSN-start per relationship, on the accounts the
//       MT940 feed speaks for (psnCut) — see transactionsBeforePSNStart
//       for why an overlap merge isn't safe here. The cut sits at the
//       first PSN dump, whose MT940 statements reach back over the days
//       before it, so a seam fold (buildSeamBankRefs) drops the web
//       copy of a booking the feed already holds, found by the bank's
//       own number for the entry. An account the feed is not delivered
//       for is the web side's on every day; what the feed says about
//       such an account from the other side of a movement is booked on
//       it by the PSN stream (conversionMirrors,
//       corporateActionCashLegs).
//     - An era fold (buildEraFold) collapses a booking that two eras
//       both recorded: a statement reconstruction whose account,
//       value day, signed amount and currency match an export or
//       MT940 row is dropped, and its narrative is carried onto the
//       row that kept the booking. So is an export row a
//       confirmation's cash leg records. One booking, one row.
//     - Inside PSN, a settlement fold (buildSettlementFold) does the
//       same for the two rails that record one trade: the MT515
//       confirmation keeps it, the MT940 cash leg settling it drops.
//     - The PSN stream is wrapped in a text fold
//       (psnWebTextFoldStream) that fills a narrative column the
//       MT940 feed left as a bare code from the account-statement
//       export's record of the same entry, matched on the bank's own
//       transaction number. It moves no row and no number: the hard
//       cut still decides which side emits, and amounts, dates, kinds
//       and ids are untouched.

// Status merges the per-subsource Status (silver.MergeStatuses).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	var feeds []canonical.Status
	for _, sub := range c.subsources() {
		s, err := sub.status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("ubs %s status: %w", sub.name, err)
		}
		feeds = append(feeds, s)
	}
	// The report feed states the securities and the web feed the cash
	// accounts: the source is observed only as far as both are.
	return silver.MergeStatuses(feeds, false), nil
}

// subsource is one configured reader's Status and ChangeWindow, named
// for error messages.
type subsource struct {
	name         string
	status       func(context.Context) (canonical.Status, error)
	changeWindow func(context.Context, int64) (canonical.Window, error)
}

// subsources lists the configured readers, PSN first.
func (c *Connection) subsources() []subsource {
	var out []subsource
	if c.psn != nil {
		out = append(out, subsource{"psn", c.psn.Status, c.psn.ChangeWindow})
	}
	if c.web != nil {
		out = append(out, subsource{"web", c.web.Status, c.web.ChangeWindow})
	}
	return out
}

// ChangeWindow merges the per-subsource windows past sinceN
// (silver.MergeWindows).
func (c *Connection) ChangeWindow(ctx context.Context, sinceN int64) (canonical.Window, error) {
	var windows []canonical.Window
	for _, sub := range c.subsources() {
		w, err := sub.changeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("ubs %s ChangeWindow: %w", sub.name, err)
		}
		windows = append(windows, w)
	}
	return silver.MergeWindows(sinceN, windows), nil
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
		reports, err := c.web.exposureReports(ctx, w)
		if err != nil {
			return nil, err
		}
		if len(reports) > 0 {
			streams = append(streams, silver.NewSnapshotStream(
				[]canonical.SnapshotBatch{{ExposureReports: reports}}))
		}
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
		// only). Gold reads each account at its own active snapshot, and
		// a later snapshot that reports every account still reporting
		// beside an account ends it (DESIGN.md §10.7). Such a snapshot
		// reports the overlay and mortgage accounts, the only ones still
		// reporting at its instant, so it would end every securities
		// account observed beside them: a one-day dip to ~0 at the
		// leading edge, or a collapse to forwards-only at the trailing
		// edge (a same-day nightly captured before that day's holdings).
		// Cash/dimensions still flow, so each securities account keeps
		// its nearest complete snapshot. The leading bound only bites
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
	merged := silver.NewConcatSnapshotStream(streams)
	if c.web == nil {
		return merged, nil
	}
	// A private-markets fund's units carry no stated cost on either
	// feed; the capital calls web reads supply their paid-in basis, on
	// whichever stream's era the position falls in (paid_in.go).
	paidIn, err := c.web.paidInByISIN(ctx, c.psn, safekeepingByPortfolio)
	if err != nil {
		_ = merged.Close()
		return nil, fmt.Errorf("ubs web paid-in basis: %w", err)
	}
	var out silver.SnapshotStream = merged
	if len(paidIn) > 0 {
		out = &paidInStream{inner: merged, series: paidIn}
	}
	// The netbanking's credit rates go on the balances already emitted
	// (rates.go), whichever stream emitted them.
	rates, err := c.web.creditRates(ctx)
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	if len(rates) > 0 {
		out = &rateStampStream{inner: out, rates: rates}
	}
	return out, nil
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
		s, outcome, err := c.web.transactionsBeforePSNStart(ctx, w, c.psn, c.relationships)
		if err != nil {
			return nil, fmt.Errorf("ubs web Transactions: %w", err)
		}
		hints = outcome.hints
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
		// The managed portfolios' own trades, on the cash accounts that
		// settled them. Also its own stream, and for the stronger
		// reason: the cut decides which of two records of one booking
		// gold keeps, and this pass emits only where neither of the
		// other rails recorded the booking at all. What it folds
		// against is what the cash pass just emitted, which is why it
		// runs after it.
		portfolio, err := c.web.portfolioTransactions(ctx, w, c.psn, outcome.settled)
		if err != nil {
			return nil, fmt.Errorf("ubs web portfolio Transactions: %w", err)
		}
		if len(portfolio.Transactions) > 0 {
			streams = append(streams, silver.NewTransactionStream(portfolio))
		}
	}
	if c.psn != nil {
		s, err := c.psn.Transactions(ctx, w, hints)
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
		if web, ok := s.texts[webTxTextKey{account: t.AccountExternalID, txnNo: p.BankRef}]; ok {
			web.fill(t)
		}
	}
	return batch, more, nil
}

func (s *psnWebTextFoldStream) Close() error { return s.inner.Close() }

// psnStatementCarryStream is the PSN half of the era fold: it carries the
// narrative of a statement reconstruction the web side dropped onto the
// MT940 row that kept the booking, and that of an export row onto the
// confirmation's cash leg that kept it.
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
// Keyed on the surviving row's id, decided on the web side where both eras
// are in hand, so nothing is re-matched here. No row is added or dropped
// by this stream, and the amount, the value date, the kind and the id are
// the surviving row's, byte for byte.
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
		if alt, ok := s.texts[t.TransactionExternalID]; ok {
			alt.fill(t)
		}
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
// at the batch's snapshot_at, so a mortgage reports in every PSN
// snapshot. A PSN snapshot without it would report every account
// the mortgage was last observed beside, and gold's per-account
// reading would end the mortgage there (DESIGN.md §10.7).
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
	// Positions. A mortgage-only snapshot would report the
	// mortgage alone, the one account still reporting at its
	// instant, so gold's per-account reading would end every
	// account last observed beside the mortgage (DESIGN.md
	// §10.7). Cash-only / fx-only PSN batches are skipped for the
	// same reason.
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
// securities). It reports the overlay accounts, the only ones still
// reporting at its instant, so gold's per-account reading would end
// every securities account observed beside them (DESIGN.md §10.7) — a
// one-day dip to ~0 before holdings begin, or a collapse to
// forwards/mortgage-only when a same-day nightly is captured before
// that day's holdings. Both edges are suppressed here, so each
// securities account keeps its nearest complete snapshot. Cash
// balances and dimensions in the same batches pass through untouched,
// so PSN cash still flows. Running BEFORE psnWebFoldStream means an
// emptied bracket-day batch carries no positions, so the fold won't
// anchor a mortgage onto it either (matching the rule that no snapshot
// holds a mortgage alone). lastHoldingsAt == 0 means no upper bound.
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
