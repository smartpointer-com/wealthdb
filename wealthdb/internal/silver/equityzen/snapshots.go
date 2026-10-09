package equityzen

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// accountKey is the single custody account: it holds the positions AND the
// double-entry transaction pairs (transactions.go), so the value spine and
// the cash flows meet on one entity at every returns grain. The silver has no
// buyer-id column and the holder has one relationship, so it is a constant.
const accountKey = "equityzen"

// Snapshots forward-fills the per-day portfolio from the silver's
// event-sourced `positions` table. The collector already replays each deal's
// timeline and computes its mark (silver migrations 0001/0002); this adapter
// does no valuation logic — for every event date it emits a COMPLETE snapshot
// (each deal's latest event on/before that date, keeping only the is_open
// ones), which is what gold's as-of query reads (the latest snapshot_at per
// source, all its positions). An exited deal drops out exactly at its exit
// date. When the LAST deal exits, the exit date gets the exit-day zero
// snapshot instead (silver.ClosureMarkerBatch) — dropping to an empty batch
// would leave the pre-exit marks carried forward as phantom value.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	// The held→empty transition is tracked within the walked window, so the
	// closure marker depends on ChangeWindow spanning the full content history
	// (it does — see status.go): a window starting between the last held date
	// and the exit date would miss the transition and re-emit nothing.
	batches := make([]canonical.SnapshotBatch, 0, len(times))
	var lastHeld canonical.SnapshotBatch
	prevHeld := false
	for _, t := range times {
		batch, err := c.buildBatch(ctx, t)
		if err != nil {
			return nil, err
		}
		held := len(batch.Positions) > 0
		switch {
		case held:
			lastHeld = batch
		case prevHeld:
			// The book just emptied: emit the exit-day zero snapshot so the
			// value spine and the as-of holdings register the closure ON the
			// exit date instead of carrying the last marks forward (an empty
			// batch is invisible to gold's queries).
			batch = silver.ClosureMarkerBatch(lastHeld, t, custodyAccount(t))
		}
		prevHeld = held
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
       COALESCE(o.payload, ''),
       o.shares_original,
       (SELECT SUM(cf.execution_fee) FROM cash_flows cf
         WHERE cf.deal_external_id = p.deal_external_id AND cf.kind = 'purchase')
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

	held := false
	for rows.Next() {
		var (
			deal, currency, kind, company, symbol, payl string
			shares, cost, market, bought, fee           sql.NullFloat64
			acqUnix                                     sql.NullInt64
		)
		if err := rows.Scan(&deal, &currency, &shares, &cost, &market,
			&kind, &company, &symbol, &acqUnix, &payl, &bought, &fee); err != nil {
			return batch, err
		}
		held = true
		ac := assetClassForKind(kind)
		acNew, vehicle := taxonomyForKind(kind)
		instKey := deal

		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    accountKey,
			PositionKey:          deal,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             currency,
			MarketValue:          silver.DecimalPtrFromNullFloat(market),
			AcquisitionDate:      silver.DatePtrFromNullUnix(acqUnix),
		}
		spv := ac == canonical.AssetClassSPV
		book, basis, feeShare := bookValue(spv, cost, shares, bought, fee)
		change.SetBookValue(book, basis)
		if feeShare != nil {
			change.Payload = silver.PayloadWith("{}", map[string]any{
				"execution_fee_in_basis": feeShare.String()})
		}
		// quantity is a share count only for SPVs; a multi-company fund's
		// LP interest has no meaningful unit count.
		if spv {
			change.Quantity = silver.DecimalPtrFromNullFloat(shares)
		}
		batch.Positions = append(batch.Positions, change)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
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
	if !held {
		return batch, nil // nothing held at t
	}
	batch.Accounts = append(batch.Accounts, custodyAccount(t))
	return batch, nil
}

// custodyAccount is the single EquityZen account's change record as of t.
// management_style is self_directed: the holder chooses which interests to
// buy/hold/sell — the GP management happening inside each vehicle is not
// modelled. The per-position asset_class (spv / private_fund) carries the
// vehicle distinction.
func custodyAccount(t int64) canonical.AccountChange {
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	name := "EquityZen"
	usd := "USD"
	return canonical.AccountChange{
		AccountExternalID: accountKey,
		AccountKind:       canonical.AccountKindCustody,
		DisplayName:       &name,
		BaseCurrency:      &usd,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	}
}
