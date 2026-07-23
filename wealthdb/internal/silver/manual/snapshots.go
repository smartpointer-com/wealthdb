package manual

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// accountKey is the single account holding every manual position. The manual
// source is one logical holder, so it is a constant.
const accountKey = "manual"

// Snapshots reconstructs the per-date portfolio from the silver's positions +
// valuations. The silver stores a position once (with acquired_at / closed_at)
// and a valuation per (position, as_of_date); gold's as-of query, by contrast,
// takes the latest snapshot_at per source and reads ALL its positions. So for
// every event date — any date a position is acquired, re-valued, or closed —
// we emit a COMPLETE forward-filled snapshot: every position live at that date
// (acquired_at ≤ date < closed_at), each marked at its latest valuation
// on/before the date. A closed position drops out exactly at its closed_at;
// a complete portfolio exists at every date so historical as-of queries work.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		batch, err := c.buildBatch(ctx, t)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return silver.NewSnapshotStream(batches), nil
}

// snapshotTimesInWindow are the distinct event dates in the window: the union
// of every position's acquired_at + closed_at and every valuation's as_of_date
// (silver ISO TEXT → unix seconds). load_runs.load_at is the load time, not a
// holding event, so it is excluded.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT t FROM (
    SELECT CAST(strftime('%s', acquired_at) AS INTEGER) AS t FROM positions
    UNION SELECT CAST(strftime('%s', closed_at)  AS INTEGER) FROM positions WHERE closed_at IS NOT NULL
    UNION SELECT CAST(strftime('%s', as_of_date) AS INTEGER) FROM valuations
)
WHERE t BETWEEN ? AND ?
ORDER BY t`
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

// buildBatch materialises the full portfolio as of event date t: every
// position live at t, marked at its latest valuation on/before t, with the
// valuation dated at acquired_at as its cost basis. Emits one position +
// one instrument per live position, plus the single manual account.
func (c *Connection) buildBatch(ctx context.Context, t int64) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	const q = `
SELECT p.id, p.kind, COALESCE(p.vehicle, '') AS vehicle, p.currency, COALESCE(p.display_name, ''),
       CAST(strftime('%s', p.acquired_at) AS INTEGER) AS acq_unix,
       p.payload,
       (SELECT v.value FROM valuations v
         WHERE v.position_id = p.id
           AND CAST(strftime('%s', v.as_of_date) AS INTEGER) <= ?
         ORDER BY CAST(strftime('%s', v.as_of_date) AS INTEGER) DESC
         LIMIT 1) AS market_value,
       (SELECT v.value FROM valuations v
         WHERE v.position_id = p.id
           AND CAST(strftime('%s', v.as_of_date) AS INTEGER)
               = CAST(strftime('%s', p.acquired_at) AS INTEGER)
         LIMIT 1) AS book_value
  FROM positions p
 WHERE CAST(strftime('%s', p.acquired_at) AS INTEGER) <= ?
   AND (p.closed_at IS NULL
        OR CAST(strftime('%s', p.closed_at) AS INTEGER) > ?)
 ORDER BY p.id`
	rows, err := c.db.QueryContext(ctx, q, t, t, t)
	if err != nil {
		return batch, fmt.Errorf("buildBatch: %w", err)
	}
	defer rows.Close()

	any := false
	for rows.Next() {
		var (
			id, kind, vehicle, currency, displayName, payload string
			acqUnix                                           int64
			marketValue, bookValue                            sql.NullString
		)
		if err := rows.Scan(&id, &kind, &vehicle, &currency, &displayName,
			&acqUnix, &payload, &marketValue, &bookValue); err != nil {
			return batch, err
		}
		any = true
		ac := assetClassFor(kind)
		acNew, veh := taxonomyFor(kind, vehicle)
		instKey := id

		pos := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    accountKey,
			PositionKey:          id,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              veh,
			Currency:             currency,
			AcquisitionDate:      acqDate(acqUnix),
			Payload:              json.RawMessage(payload),
			// Quantity stays nil: manual holdings are valued by amount, not a
			// unit count (real estate, a loan, a whole-company stake, an LP
			// interest — none is unit-denominated).
		}
		// market_value = latest valuation ≤ t (forward-filled). book_value =
		// the valuation dated at acquired_at (the cost basis); held constant
		// while market moves. A liability kind (mortgage) is entered as a
		// positive outstanding balance — "direction comes from kind" — so we
		// negate it here, matching the gold convention that liability positions
		// carry a negative market_value and net against assets in rollups.
		neg := ac == canonical.AssetClassMortgage
		if marketValue.Valid {
			if mv, err := canonical.NewDecimalFromString(signed(marketValue.String, neg)); err == nil {
				pos.MarketValue = &mv
			}
		}
		if bookValue.Valid {
			if bv, err := canonical.NewDecimalFromString(signed(bookValue.String, neg)); err == nil {
				pos.BookValue = &bv
			}
		}
		batch.Positions = append(batch.Positions, pos)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           acNew,
			Vehicle:              veh,
			FirstSeenAt:          t,
			LastSeenAt:           t,
			Payload:              json.RawMessage(payload),
		}
		if displayName != "" {
			n := displayName
			inst.Name = &n
		}
		if currency != "" {
			ccy := currency
			inst.Currency = &ccy
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	if err := rows.Err(); err != nil {
		return batch, err
	}
	if !any {
		return batch, nil // nothing held at t
	}

	// One account for the whole manual book. account_kind 'other': these are
	// directly-held assets with no institutional container. management_style
	// self_directed (the holder decides what to hold); the asset-family split
	// rides on each position's asset_class, not the account. No base_currency:
	// positions may span multiple currencies.
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	name := "Manual"
	batch.Accounts = append(batch.Accounts, canonical.AccountChange{
		AccountExternalID: accountKey,
		AccountKind:       canonical.AccountKindOther,
		DisplayName:       &name,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	})
	return batch, nil
}

// signed flips a positive-magnitude decimal string to negative for liability
// positions (mortgage); asset values pass through unchanged. The collector
// validates valuations as non-negative, so the input is always a magnitude.
func signed(magnitude string, neg bool) string {
	if neg {
		return "-" + magnitude
	}
	return magnitude
}

// acqDate converts a unix-seconds timestamp to a UTC-midnight calendar date
// (gold stores AcquisitionDate as DATE).
func acqDate(unix int64) *time.Time {
	t := time.Unix(unix, 0).UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}
