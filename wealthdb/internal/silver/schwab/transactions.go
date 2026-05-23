package schwab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// buildDescriptionToInstrumentKey scans silver positions and the
// instruments table for (description → preferredInstrumentKey)
// pairs. The key is what api positions register as the gold
// instrument_external_id (CUSIP when present, else symbol —
// matching preferredInstrumentKey). Used by the transactions
// builder as a fallback identity resolver for DIVIDEND_OR_INTEREST
// rows where the payload's transferItems leg is cash-only.
//
// Match is exact-after-normalisation only (uppercase, strip
// non-alphanumerics, drop trailing " ETF"/" INC"/" CO"/" LTD"/
// " FUND"/" TRUST"). Conservative: dividends whose description
// doesn't have a normalised-exact twin in positions land with
// no instrument link rather than risking a mismatch. Heavily
// truncated Schwab descriptions ("ISHARES TREASURY FLOATNGRATE
// BD ETF" vs "iShares Treasury Floating Rate Bond ETF") won't
// match — that's by design.
func (c *apiReader) buildDescriptionToInstrumentKey(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if err := c.scanPositionDescriptions(ctx, out); err != nil {
		return nil, err
	}
	if err := c.scanInstrumentDescriptions(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *apiReader) scanPositionDescriptions(ctx context.Context, out map[string]string) error {
	const q = `SELECT DISTINCT payload FROM positions`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("schwab descToInstrument positions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		var p struct {
			Instrument schwabInstrument `json:"instrument"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue
		}
		if p.Instrument.Description == "" {
			continue
		}
		key := preferredInstrumentKey(p.Instrument)
		if key == "" || isCashAssetType(p.Instrument.AssetType) {
			continue
		}
		norm := normalizeSchwabDescription(p.Instrument.Description)
		if norm == "" {
			continue
		}
		// First writer wins — keep the implementation simple. If
		// the same description maps to multiple keys (rare; only
		// possible across ticker reuse), neither resolves.
		if existing, ok := out[norm]; ok && existing != key {
			out[norm] = "" // ambiguous; sentinel
			continue
		}
		out[norm] = key
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Drop ambiguous sentinels so the caller sees a clean map.
	for k, v := range out {
		if v == "" {
			delete(out, k)
		}
	}
	return nil
}

func (c *apiReader) scanInstrumentDescriptions(ctx context.Context, out map[string]string) error {
	exists, err := c.hasTable(ctx, "instruments")
	if err != nil || !exists {
		return err
	}
	const q = `SELECT symbol, payload FROM instruments`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("schwab descToInstrument instruments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sym, payload string
		if err := rows.Scan(&sym, &payload); err != nil {
			return err
		}
		var p struct {
			Description string `json:"description"`
			CUSIP       string `json:"cusip"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		if p.Description == "" {
			continue
		}
		key := p.CUSIP
		if key == "" {
			key = sym
		}
		norm := normalizeSchwabDescription(p.Description)
		if norm == "" {
			continue
		}
		if _, present := out[norm]; !present {
			out[norm] = key
		}
	}
	return rows.Err()
}

// normalizeSchwabDescription folds the formatting differences
// between Schwab's various description surfaces (statements,
// transaction-history JSON, positions API) so an exact-equal
// comparison works between, say, "VANGUARD TOTAL STOCK MKT ETF"
// and "Vanguard Total Stock Market".
//
//	- Uppercase the entire string.
//	- Strip everything that's not an ASCII letter or digit
//	  (whitespace, ampersands, punctuation, parens).
//	- Strip common asset-class / type suffixes so they don't
//	  perturb the match: ETF, FUND, TRUST, INC, CO, LTD,
//	  CORP, COMPANY, CL[A-Z], CLASS[A-Z].
//
// Empty input returns "". The function is symmetric — the
// caller normalises both sides before comparing.
func normalizeSchwabDescription(s string) string {
	// Word-level cleanup before the squash. Schwab's positions
	// payload abbreviates "Class A" to "A" (no "CLASS" word), the
	// instruments table follows suit, but the dividend payload
	// description sometimes spells it out ("META PLATFORMS INC
	// CLASS A"). Dropping the word "CLASS" up front aligns both
	// surfaces. Same trick for "SERIES" (preferred-stock series).
	s = strings.ToUpper(s)
	s = strings.ReplaceAll(s, " CLASS ", " ")
	s = strings.ReplaceAll(s, " SERIES ", " ")
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c)
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		}
	}
	norm := b.String()
	// Iterate until no more suffixes peel — handles compound
	// trailers like "INCCLASSA" → strip CLASSA, then INC.
	for {
		before := norm
		for _, suffix := range []string{
			"COMPANY", "TRUST", "CORP", "FUND", "ETF", "INC",
			"LTD", "CO",
			"CLASSA", "CLASSB", "CLASSC", "CLASSD",
			"CLA", "CLB", "CLC", "CLD",
		} {
			norm = strings.TrimSuffix(norm, suffix)
		}
		if norm == before {
			break
		}
	}
	return norm
}

// txStream yields a single batch covering every transaction in
// the window. Personal-portfolio scale (≲a few thousand events
// per source) makes splitting unnecessary; we can revisit if a
// future window blows up memory.
type txStream struct {
	batch    canonical.TransactionBatch
	consumed bool
}

func (c *apiReader) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return &txStream{consumed: true}, nil
	}

	// Build a (normalized description → instrument key) lookup
	// from silver positions and instruments. Schwab API dividend
	// payloads only carry the security name as free text (no
	// CUSIP / symbol leg), so this is the only path for getting
	// a symbol on dividend rows where the same name has been
	// seen on a position the user has held.
	descToInstrument, err := c.buildDescriptionToInstrumentKey(ctx)
	if err != nil {
		return nil, err
	}

	const q = `
SELECT activity_id, timestamp, account_external_id, kind, payload
  FROM transactions
 WHERE timestamp BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("Transactions query: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			activityID, extID, kind, payload string
			occurredAt                       int64
		)
		if err := rows.Scan(&activityID, &occurredAt, &extID, &kind, &payload); err != nil {
			return nil, fmt.Errorf("Transactions scan: %w", err)
		}

		tx, err := buildTransaction(activityID, occurredAt, extID, kind, payload)
		if err != nil {
			return nil, fmt.Errorf("Transactions row (activity_id=%s): %w", activityID, err)
		}
		// If buildTransaction didn't land an instrument link (typical
		// for DIVIDEND_OR_INTEREST where transferItems is cash-only),
		// fall back to a description match against silver positions.
		if tx.InstrumentExternalID == nil && tx.Description != nil {
			if key, ok := descToInstrument[normalizeSchwabDescription(*tx.Description)]; ok {
				k := key
				tx.InstrumentExternalID = &k
			}
		}
		out.Transactions = append(out.Transactions, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &txStream{batch: out}, nil
}

func (s *txStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	if s.consumed {
		return canonical.TransactionBatch{}, false, nil
	}
	s.consumed = true
	return s.batch, false, nil
}

func (s *txStream) Close() error { return nil }

// schwabTransferItem is one leg of a transaction's transferItems
// array.
type schwabTransferItem struct {
	Instrument     schwabInstrument   `json:"instrument"`
	Amount         canonical.Decimal  `json:"amount"`
	Cost           *canonical.Decimal `json:"cost"`
	Price          *canonical.Decimal `json:"price"`
	PositionEffect string             `json:"positionEffect"`
}

// schwabTxPayload covers what we extract from each transactions
// row's payload.
type schwabTxPayload struct {
	NetAmount     *canonical.Decimal   `json:"netAmount"`
	Description   string               `json:"description"`
	TransferItems []schwabTransferItem `json:"transferItems"`
}

func buildTransaction(activityID string, occurredAt int64, extID, silverKind, payload string) (canonical.TransactionChange, error) {
	var tp schwabTxPayload
	if err := json.Unmarshal([]byte(payload), &tp); err != nil {
		return canonical.TransactionChange{}, fmt.Errorf("payload unmarshal: %w", err)
	}

	netAmount := canonical.Decimal{}
	if tp.NetAmount != nil {
		netAmount = *tp.NetAmount
	}

	kind := kindFor(silverKind, netAmount, tp.Description)

	tx := canonical.TransactionChange{
		TransactionExternalID: activityID,
		OccurredAt:            occurredAt,
		AccountExternalID:     extID,
		Kind:                  kind,
		Currency:              "USD", // Schwab retail is USD-only.
		NetAmount:             canonical.ApplyCanonicalSign(kind, tp.NetAmount),
		Description:           strPtrIfNonEmpty(tp.Description),
		Payload:               json.RawMessage(payload),
	}

	// For trades, surface the instrument leg's quantity, price,
	// and instrument identity. Pick the leg whose instrument is
	// not a cash placeholder (CURRENCY/CASH_EQUIVALENT).
	if leg, ok := pickInstrumentLeg(tp.TransferItems); ok {
		key := preferredInstrumentKey(leg.Instrument)
		if key != "" {
			tx.InstrumentExternalID = &key
		}
		qty := leg.Amount
		tx.Quantity = &qty
		if leg.Price != nil {
			tx.Price = leg.Price
		}
		if leg.Cost != nil {
			tx.GrossAmount = canonical.ApplyCanonicalSign(kind, leg.Cost)
		}
	}
	return tx, nil
}

// pickInstrumentLeg returns the first non-cash transfer item, if
// any. Schwab puts the cash leg alongside the instrument leg for
// trades; the instrument leg is the one carrying the security.
func pickInstrumentLeg(items []schwabTransferItem) (schwabTransferItem, bool) {
	for _, it := range items {
		if isCashAssetType(it.Instrument.AssetType) {
			continue
		}
		if it.Instrument.AssetType == "" && it.Instrument.CUSIP == "" && it.Instrument.Symbol == "" {
			continue
		}
		return it, true
	}
	return schwabTransferItem{}, false
}

// preferredInstrumentKey returns CUSIP if present, else symbol.
// Mirrors silver-schwab's `instrument_key` PK choice.
func preferredInstrumentKey(i schwabInstrument) string {
	if i.CUSIP != "" {
		return i.CUSIP
	}
	return i.Symbol
}
