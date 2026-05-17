package swissquote

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendCurrencyBalancesAndFxRates(ctx, w, byTime); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		out.batches = append(out.batches, *byTime[t])
	}
	return out, nil
}

func (s *snapshotStream) Next(context.Context) (canonical.SnapshotBatch, bool, error) {
	if s.idx >= len(s.batches) {
		return canonical.SnapshotBatch{}, false, nil
	}
	b := s.batches[s.idx]
	s.idx++
	return b, s.idx < len(s.batches), nil
}

func (s *snapshotStream) Close() error { return nil }

func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `SELECT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ? ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimesInWindow: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- accounts ------------------------------------------------------------

func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap    int64
			extID   string
			payload string
		)
		if err := rows.Scan(&snap, &extID, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// ---- positions -----------------------------------------------------------

// positionPayload mirrors the Swissquote XLS-derived position
// fields the silver loader extracts.
type positionPayload struct {
	AssetClass  string             `json:"asset_class"`
	Currency    string             `json:"currency"`
	Symbol      string             `json:"symbol"`
	Quantity    *canonical.Decimal `json:"quantity"`
	Price       *canonical.Decimal `json:"price"`
	UnitCost    *canonical.Decimal `json:"unit_cost"`
	TotalValue  *canonical.Decimal `json:"total_value"`
}

// appendPositions also emits an InstrumentChange per
// (snapshot_at, symbol+@+currency) so positions have a registered
// instrument to reference. Swissquote silver doesn't have a
// dedicated instruments table — we synthesize one from positions
// rows.
func (c *Connection) appendPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, symbol, currency, payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                       int64
			extID, symbol, currency    string
			payload                    string
		)
		if err := rows.Scan(&snap, &extID, &symbol, &currency, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p positionPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("appendPositions row (snap=%d, %s@%s): %w", snap, symbol, currency, err)
		}
		ac := assetClassFor(p.AssetClass)

		positionKey := symbol + "@" + currency
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: positionKey,
			AssetClass:           ac,
			Symbol:               strPtrIfNonEmpty(symbol),
			Currency:             strPtrIfNonEmpty(currency),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
			Payload:              json.RawMessage(payload),
		})

		instrIDCopy := positionKey
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    extID,
			PositionKey:          positionKey,
			InstrumentExternalID: &instrIDCopy,
			AssetClass:           ac,
			Currency:             currency,
			Quantity:             p.Quantity,
			MarketValue:          p.TotalValue,
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// ---- currency_balances → CashBalance + FxRate ----------------------------

type currencyBalancePayload struct {
	CashBalance    *canonical.Decimal `json:"cash_balance"`
	RateToCHF      *canonical.Decimal `json:"rate_to_chf"`
}

// appendCurrencyBalancesAndFxRates does double duty per
// docs/adapters/swissquote.md §5: each silver currency_balances
// row produces one CashBalanceChange (the cash component in the
// row's currency) AND — for non-CHF rows — one FxRateChange
// (base=CHF, quote=row's currency, mid_rate=rate_to_chf).
func (c *Connection) appendCurrencyBalancesAndFxRates(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, currency, payload
  FROM currency_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCurrencyBalancesAndFxRates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                  int64
			extID, currency       string
			payload               string
		)
		if err := rows.Scan(&snap, &extID, &currency, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p currencyBalancePayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("currency_balances payload (snap=%d, ccy=%s): %w", snap, currency, err)
		}

		if p.CashBalance != nil {
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: extID,
				Currency:          currency,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            *p.CashBalance,
				Payload:           json.RawMessage(payload),
			})
		}

		// FX rate, only for non-CHF rows (CHF→CHF is 1.0 trivially
		// and not worth a row). Convention: mid_rate = (1 quote in
		// base units), matching UBS. Swissquote's rate_to_chf is
		// "1 of this currency = N CHF", which is exactly that
		// mid_rate when base=CHF, quote=currency.
		if currency != "CHF" && p.RateToCHF != nil && !p.RateToCHF.IsZero() {
			batch.FxRates = append(batch.FxRates, canonical.FxRateChange{
				SnapshotAt:    snap,
				BaseCurrency:  "CHF",
				QuoteCurrency: currency,
				MidRate:       *p.RateToCHF,
				Payload:       json.RawMessage(payload),
			})
		}
	}
	return rows.Err()
}

// ---- helpers -------------------------------------------------------------

func strPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
