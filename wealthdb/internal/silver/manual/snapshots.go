package manual

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// defaultAccountKey is the account a position falls into when the book
// declares none — the one account every manual position was in before
// accounts.csv existed. The id is kept stable so a deployment that never adds
// accounts sees no change in gold.
const defaultAccountKey = "manual"

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
// valuation dated at acquired_at as its cost basis. Emits one position + one
// instrument per live position, plus one account per account those positions
// are held in.
//
// Only accounts holding something at t are emitted. Gold reads a snapshot as
// the complete state of the source at that date, so an account with nothing
// live in it has nothing to say — and emitting it would assert a container
// that held no value on a date it may not have existed.
func (c *Connection) buildBatch(ctx context.Context, t int64) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	const q = `
SELECT p.id, COALESCE(p.account_id, '` + defaultAccountKey + `') AS account_id,
       p.kind, COALESCE(p.vehicle, '') AS vehicle, p.currency, COALESCE(p.display_name, ''),
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
	held := map[string]bool{}
	for rows.Next() {
		var (
			id, acctID, kind, vehicle, currency, displayName, payload string
			acqUnix                                                   int64
			marketValue, bookValue                                    sql.NullString
		)
		if err := rows.Scan(&id, &acctID, &kind, &vehicle, &currency, &displayName,
			&acqUnix, &payload, &marketValue, &bookValue); err != nil {
			return batch, err
		}
		any = true
		held[acctID] = true
		ac := assetClassFor(kind)
		acNew, veh := taxonomyFor(kind, vehicle)
		instKey := id

		pos := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    acctID,
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

	accounts, err := c.accountsFor(ctx, held, t)
	if err != nil {
		return batch, err
	}
	batch.Accounts = accounts
	return batch, nil
}

// accountsFor is one AccountChange per account holding something at t.
//
// The declared taxonomy wins; the defaults are the ones the single hard-coded
// account always carried. No base_currency on any of them: a manual book's
// positions may span currencies, and the account is a sleeve rather than a
// denominated container.
func (c *Connection) accountsFor(ctx context.Context, held map[string]bool, t int64) ([]canonical.AccountChange, error) {
	if len(held) == 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT id, COALESCE(display_name, ''), COALESCE(account_kind, ''),
       COALESCE(tax_wrapper, ''), COALESCE(management_style, '')
  FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("accountsFor: %w", err)
	}
	defer rows.Close()

	declared := map[string]canonical.AccountChange{}
	for rows.Next() {
		var id, name, kind, wrapper, style string
		if err := rows.Scan(&id, &name, &kind, &wrapper, &style); err != nil {
			return nil, err
		}
		declared[id] = accountChange(id, name, kind, wrapper, style, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]canonical.AccountChange, 0, len(held))
	for id := range held {
		if ac, ok := declared[id]; ok {
			out = append(out, ac)
			continue
		}
		// A position naming an account the book does not declare. load.py
		// refuses that, so reaching here means silver predates accounts.csv
		// (or was written by hand): project the defaults rather than drop the
		// account and take its positions down with it.
		out = append(out, accountChange(id, "Manual", "", "", "", t))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AccountExternalID < out[j].AccountExternalID
	})
	return out, nil
}

// accountChange builds one account, filling each empty field with the value
// the single pre-accounts manual account carried. account_kind 'other' is the
// default because a directly-held asset has no institutional container;
// self_directed because the holder decides what to hold; taxable_personal
// because that is the common case and a sleeve is the exception worth
// declaring.
func accountChange(id, name, kind, wrapper, style string, t int64) canonical.AccountChange {
	if name == "" {
		name = "Manual"
	}
	ak := canonical.AccountKind(kind)
	if !ak.Valid() {
		ak = canonical.AccountKindOther
	}
	tw := canonical.TaxWrapper(wrapper)
	if !tw.Valid() {
		tw = canonical.TaxWrapperTaxablePersonal
	}
	ms := canonical.ManagementStyle(style)
	if !ms.Valid() {
		ms = canonical.ManagementStyleSelfDirected
	}
	return canonical.AccountChange{
		AccountExternalID: id,
		AccountKind:       ak,
		DisplayName:       &name,
		TaxWrapper:        &tw,
		ManagementStyle:   &ms,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	}
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
