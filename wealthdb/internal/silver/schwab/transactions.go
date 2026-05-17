package schwab

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// txStream yields a single batch covering every transaction in
// the window. Personal-portfolio scale (≲a few thousand events
// per source) makes splitting unnecessary; we can revisit if a
// future window blows up memory.
type txStream struct {
	batch    canonical.TransactionBatch
	consumed bool
}

func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return &txStream{consumed: true}, nil
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

	kind := kindFor(silverKind, netAmount)

	tx := canonical.TransactionChange{
		TransactionExternalID: activityID,
		OccurredAt:            occurredAt,
		AccountExternalID:     extID,
		Kind:                  kind,
		Currency:              "USD", // Schwab retail is USD-only.
		NetAmount:             tp.NetAmount,
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
			tx.GrossAmount = leg.Cost
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
