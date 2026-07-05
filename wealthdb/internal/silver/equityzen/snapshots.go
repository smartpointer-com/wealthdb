package equityzen

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// accountKey is the custody account holding the positions; fundingAccountKey
// is the sentinel cash account carrying the double-entry transaction pairs
// (transactions.go). The silver has no buyer-id column and the holder has one
// relationship, so both are constants.
const (
	accountKey        = "equityzen"
	fundingAccountKey = "equityzen-funding"
)

// Snapshots forward-fills the per-day portfolio from the silver's
// event-sourced `positions` table. The collector already replays each deal's
// timeline and computes its mark (silver migrations 0001/0002); this adapter
// does no valuation logic — for every event date it emits a COMPLETE snapshot
// (each deal's latest event on/before that date, keeping only the is_open
// ones), which is what gold's as-of query reads (the latest snapshot_at per
// source, all its positions). An exited deal drops out exactly at its exit
// date.
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

// snapshotTimesInWindow are the distinct position event dates in the window
// (positions.as_of_date as unix seconds; the download time in dump_runs is
// provenance, not a holding event, so it is excluded).
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT CAST(strftime('%s', as_of_date) AS INTEGER) AS t
  FROM positions
 WHERE as_of_date IS NOT NULL
   AND CAST(strftime('%s', as_of_date) AS INTEGER) BETWEEN ? AND ?
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

// buildBatch materialises the full portfolio as of event date t: each deal's
// latest event on/before t (by event_seq, which increases with as_of_date),
// keeping only is_open=1 — a deal whose latest event ≤ t is its exit
// (is_open=0) is dropped, and a deal with no event ≤ t (not yet invested) is
// absent. Emits one position + one instrument per held deal, plus the single
// EquityZen account.
func (c *Connection) buildBatch(ctx context.Context, t int64) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	const q = `
SELECT p.deal_external_id,
       COALESCE(o.currency, 'USD'),
       p.shares_held, p.cost_basis_remaining, p.market_value,
       COALESCE(o.kind, ''), COALESCE(o.company_name, ''), COALESCE(o.ticker_symbol, ''),
       (SELECT MIN(CAST(strftime('%s', p2.as_of_date) AS INTEGER))
          FROM positions p2
         WHERE p2.deal_external_id = p.deal_external_id AND p2.as_of_date IS NOT NULL),
       COALESCE(o.payload, '')
  FROM positions p
  JOIN offerings o ON o.deal_external_id = p.deal_external_id
 WHERE p.is_open = 1
   AND p.event_seq = (
       SELECT MAX(s2.event_seq) FROM positions s2
        WHERE s2.deal_external_id = p.deal_external_id
          AND s2.as_of_date IS NOT NULL
          AND CAST(strftime('%s', s2.as_of_date) AS INTEGER) <= ?)
 ORDER BY p.deal_external_id`
	rows, err := c.db.QueryContext(ctx, q, t)
	if err != nil {
		return batch, fmt.Errorf("buildBatch: %w", err)
	}
	defer rows.Close()

	any := false
	for rows.Next() {
		var (
			deal, currency, kind, company, symbol, payl string
			shares, cost, market                        sql.NullFloat64
			acqUnix                                     sql.NullInt64
		)
		if err := rows.Scan(&deal, &currency, &shares, &cost, &market,
			&kind, &company, &symbol, &acqUnix, &payl); err != nil {
			return batch, err
		}
		any = true
		ac := assetClassForKind(kind)
		instKey := deal

		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    accountKey,
			PositionKey:          deal,
			InstrumentExternalID: &instKey,
			AssetClass:           ac,
			Currency:             currency,
			MarketValue:          realPtr(market),
			BookValue:            realPtr(cost),
			AcquisitionDate:      acqDate(acqUnix),
		}
		// quantity is a share count only for SPVs; a multi-company fund's
		// LP interest has no meaningful unit count.
		if ac == canonical.AssetClassSPV {
			change.Quantity = realPtr(shares)
		}
		batch.Positions = append(batch.Positions, change)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           ac,
			FirstSeenAt:          t,
			LastSeenAt:           t,
		}
		if company != "" {
			inst.Name = &company
		}
		// EquityZen assigns single-company SPVs a per-company symbol (an
		// EZ-internal ticker, not a public listing); funds have none. Surfacing
		// it lets `wealthdb positions` show a symbol like public equities.
		if symbol != "" {
			inst.Symbol = &symbol
		}
		if payl != "" {
			inst.Payload = json.RawMessage(payl)
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	if err := rows.Err(); err != nil {
		return batch, err
	}
	if !any {
		return batch, nil // nothing held at t
	}

	// One account for the whole EquityZen book. management_style is
	// self_directed: the holder chooses which interests to buy/hold/sell — we
	// do not model the GP management happening inside each vehicle. The
	// per-position asset_class (spv / private_fund) carries the vehicle
	// distinction.
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	name := "EquityZen"
	usd := "USD"
	batch.Accounts = append(batch.Accounts, canonical.AccountChange{
		AccountExternalID: accountKey,
		AccountKind:       canonical.AccountKindCustody,
		DisplayName:       &name,
		BaseCurrency:      &usd,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	})

	// The sentinel funding account: a synthetic cash conduit carrying the
	// double-entry transaction pairs (transactions.go). Every investment leg is
	// offset by a deposit/withdrawal leg, so its derived balance is always
	// exactly 0 — EquityZen's real external funding account is unobserved. No
	// positions and no cash_balance row: the 0 is implicit in the paired ledger.
	fundingName := "EquityZen (funding)"
	batch.Accounts = append(batch.Accounts, canonical.AccountChange{
		AccountExternalID: fundingAccountKey,
		AccountKind:       canonical.AccountKindCash,
		DisplayName:       &fundingName,
		BaseCurrency:      &usd,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	})
	return batch, nil
}

// ---- helpers ------------------------------------------------------

// realPtr converts a nullable SQLite REAL (dollars) to a canonical Decimal
// pointer. Returns nil for SQL NULL. The collector already rounds money to
// cents, so the float→decimal reconstruction is exact at display precision.
func realPtr(n sql.NullFloat64) *canonical.Decimal {
	if !n.Valid {
		return nil
	}
	d := canonical.NewDecimalFromFloat(n.Float64)
	return &d
}

// acqDate converts a nullable unix-seconds timestamp to a UTC-midnight
// calendar date (gold stores AcquisitionDate as DATE).
func acqDate(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}
