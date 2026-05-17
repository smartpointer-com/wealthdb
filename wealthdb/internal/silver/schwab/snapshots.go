package schwab

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// snapshotStream yields one canonical.SnapshotBatch per
// silver snapshot_at in the change window. Loads the whole window
// up front; at personal-portfolio scale (≲50 positions × ≲10
// new snapshots per load), this stays trivially in memory.
type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

// Snapshots collects every snapshot-grain row in the change window
// from accounts, account_balances, and positions, splits each
// silver row into the right canonical record type, and groups
// them by snapshot_at so the caller can apply one batch per
// snapshot.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}

	// Collect distinct snapshot_at values in the window so we can
	// build batches in chronological order. We use dump_runs as
	// the authoritative list — every silver row's snapshot_at must
	// correspond to a dump_runs entry.
	snapshotTimes, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}

	// One batch per snapshot_at. The map gives O(1) routing as we
	// walk each table's rows.
	byTime := make(map[int64]*canonical.SnapshotBatch, len(snapshotTimes))
	for _, t := range snapshotTimes {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendAccountBalances(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, w, byTime); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(snapshotTimes))}
	for _, t := range snapshotTimes {
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

// snapshotTimesInWindow returns the distinct dump_runs.snapshot_at
// values in [w.Start, w.End], in chronological order.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT snapshot_at FROM dump_runs
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimesInWindow: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("snapshotTimesInWindow scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// schwabAccountPayload covers the {accountNumber, hashValue}
// shape Schwab silver writes. account_external_id is the hash;
// the human-readable account number lives in payload only.
type schwabAccountPayload struct {
	AccountNumber string `json:"accountNumber"`
}

// appendAccounts emits one AccountChange per accounts row in the
// window. Schwab accounts are brokerage-kind; the silver
// account_external_id is the Schwab hashValue. DisplayName is set
// to the plaintext accountNumber from the payload so user-facing
// output can show something more recognisable than the hash.
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
			return fmt.Errorf("appendAccounts scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var p schwabAccountPayload
		_ = json.Unmarshal([]byte(payload), &p) // best-effort

		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			DisplayName:       strPtrIfNonEmpty(p.AccountNumber),
			BaseCurrency:      strPtrIfNonEmpty("USD"),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// schwabBalancePayload covers the fields we currently extract
// from account_balances rows. Schwab's full balance object has
// many fields; we only need the cash component for the gold
// cash_balances table.
type schwabBalancePayload struct {
	CashBalance *canonical.Decimal `json:"cashBalance"`
	// Some balance subtypes nest the value differently; we expand
	// this struct as new shapes appear.
}

// appendAccountBalances emits CashBalanceChange rows from
// account_balances. Each silver row maps to at most one canonical
// row: a balance payload without a cashBalance field is skipped
// (no useful cash quantity to project).
func (c *Connection) appendAccountBalances(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, balance_kind, payload
  FROM account_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccountBalances: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap        int64
			extID, kind string
			payload     string
		)
		if err := rows.Scan(&snap, &extID, &kind, &payload); err != nil {
			return fmt.Errorf("appendAccountBalances scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var bp schwabBalancePayload
		if err := json.Unmarshal([]byte(payload), &bp); err != nil {
			return fmt.Errorf("appendAccountBalances row (snap=%d, kind=%s): %w", snap, kind, err)
		}
		if bp.CashBalance == nil {
			continue
		}

		bk := canonicalBalanceKind(kind)
		if !bk.Valid() {
			// Unknown silver balance_kind; skip rather than emit a
			// row that would fail the gold-side enum guard.
			continue
		}
		batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        snap,
			AccountExternalID: extID,
			Currency:          "USD", // Schwab retail is USD-only.
			BalanceKind:       bk,
			Amount:            *bp.CashBalance,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

func canonicalBalanceKind(silverKind string) canonical.BalanceKind {
	switch silverKind {
	case "initial":
		return canonical.BalanceKindInitial
	case "current":
		return canonical.BalanceKindCurrent
	case "projected":
		return canonical.BalanceKindProjected
	case "aggregated":
		return canonical.BalanceKindAggregated
	default:
		return ""
	}
}

// schwabInstrument is the embedded instrument descriptor that
// appears under both positions and transactions[].transferItems[].
type schwabInstrument struct {
	AssetType   string `json:"assetType"`
	CUSIP       string `json:"cusip"`
	Symbol      string `json:"symbol"`
	Description string `json:"description"`
}

// schwabPositionPayload covers the fields we extract from each
// positions row.
type schwabPositionPayload struct {
	LongQuantity    canonical.Decimal  `json:"longQuantity"`
	ShortQuantity   canonical.Decimal  `json:"shortQuantity"`
	AveragePrice    *canonical.Decimal `json:"averagePrice"`
	MarketValue     *canonical.Decimal `json:"marketValue"`
	Instrument      schwabInstrument   `json:"instrument"`
}

// appendPositions emits InstrumentChange + PositionChange for each
// non-cash position, or CashBalanceChange for CASH_EQUIVALENT and
// CURRENCY positions. See docs/adapters/schwab.md §4.
func (c *Connection) appendPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, instrument_key, payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap          int64
			extID, posKey string
			payload       string
		)
		if err := rows.Scan(&snap, &extID, &posKey, &payload); err != nil {
			return fmt.Errorf("appendPositions scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var pp schwabPositionPayload
		if err := json.Unmarshal([]byte(payload), &pp); err != nil {
			return fmt.Errorf("appendPositions row (snap=%d, key=%s): %w", snap, posKey, err)
		}

		// Cash-like assetType → cash_balances, not positions.
		if isCashAssetType(pp.Instrument.AssetType) {
			amount := canonical.Decimal{}
			if pp.MarketValue != nil {
				amount = *pp.MarketValue
			}
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: extID,
				Currency:          "USD",
				BalanceKind:       canonical.BalanceKindCurrent,
				Amount:            amount,
				Payload:           json.RawMessage(payload),
			})
			continue
		}

		// Real security position.
		instrExtID := posKey
		ac := assetClassFor(pp.Instrument.AssetType)

		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: instrExtID,
			AssetClass:           ac,
			CUSIP:                strPtrIfNonEmpty(pp.Instrument.CUSIP),
			Symbol:               strPtrIfNonEmpty(pp.Instrument.Symbol),
			Name:                 strPtrIfNonEmpty(pp.Instrument.Description),
			Currency:             strPtrIfNonEmpty("USD"),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})

		quantity := pp.LongQuantity.Sub(pp.ShortQuantity)
		instrExtIDPtr := &instrExtID
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    extID,
			PositionKey:          posKey,
			InstrumentExternalID: instrExtIDPtr,
			AssetClass:           ac,
			Currency:             "USD",
			Quantity:             &quantity,
			MarketValue:          pp.MarketValue,
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// strPtrIfNonEmpty returns a *string to s, or nil when s is empty.
// Convenient for converting JSON-decoded strings (which default to
// "") into the nullable shape canonical types use.
func strPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

