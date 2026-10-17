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
// date; a deal the portal stopped listing drops out at the first dump that
// omitted it (removals), a snapshot time where the roster is emitted
// without it and every other deal keeps its own mark date (ValuedAt). When
// the LAST deal leaves, that date gets the exit-day zero snapshot instead
// (silver.ClosureMarkerBatch) — dropping to an empty batch would leave the
// pre-exit marks carried forward as phantom value.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	removals, err := c.removals(ctx)
	if err != nil {
		return nil, err
	}
	times, err := c.snapshotTimesInWindow(ctx, w, removals.Times())
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
		batch, err := c.buildBatch(ctx, t, removals)
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

// removals are the open deals a complete dump no longer lists, each with
// the time of the first such dump that omitted it (silver.Removals). A dump
// stamps every deal it lists (offerings.last_seen_at), and its manifest
// (dump_runs.payload) declares what it read: a dump is complete when its
// status is complete, its scope has the offerings, and the deals known to
// be listed around its time (bought by then, listed at or after it) number
// what the manifest declares. A dump without a manifest, or whose listing
// fell short, says nothing about the roster. A deal whose latest event
// already closes it (is_open = 0) leaves at that event and needs no
// removal; an offering with no last_seen_at predates the column and is
// never read as removed.
func (c *Connection) removals(ctx context.Context) (silver.Removals, error) {
	const q = `
WITH bought AS (
    SELECT deal_external_id, MIN(CAST(strftime('%s', as_of_date) AS INTEGER)) AS t
      FROM positions WHERE as_of_date IS NOT NULL GROUP BY 1),
complete AS (
    SELECT d.snapshot_at AS t
      FROM dump_runs d
     WHERE json_extract(d.payload, '$.status') = 'complete'
       AND instr(COALESCE(json_extract(d.payload, '$.scope'), ''), 'offerings') > 0
       AND json_extract(d.payload, '$.investments') = (
           SELECT COUNT(*) FROM offerings o
             LEFT JOIN bought b ON b.deal_external_id = o.deal_external_id
            WHERE o.last_seen_at >= d.snapshot_at AND COALESCE(b.t, 0) <= d.snapshot_at))
SELECT o.deal_external_id, (SELECT MIN(t) FROM complete WHERE t > o.last_seen_at)
  FROM offerings o
 WHERE o.last_seen_at IS NOT NULL
   AND EXISTS (SELECT 1 FROM complete WHERE t > o.last_seen_at)
   AND EXISTS (SELECT 1 FROM positions p
                WHERE p.deal_external_id = o.deal_external_id AND p.is_open = 1
                  AND p.event_seq = (SELECT MAX(p2.event_seq) FROM positions p2
                                      WHERE p2.deal_external_id = o.deal_external_id))`
	return silver.QueryRemovals(ctx, c.db, "equityzen", q)
}

// snapshotTimesInWindow are the distinct position event dates in the window
// (positions.as_of_date as unix seconds; the download time in dump_runs is
// provenance, not a holding event, so it is excluded) and the extra times
// in it on which the roster changes without an event: a dump that stopped
// listing a deal (removals).
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window, extra []int64) ([]int64, error) {
	return silver.SnapshotTimes(ctx, c.db, w, extra, `
SELECT DISTINCT CAST(strftime('%s', as_of_date) AS INTEGER) AS t
  FROM positions
 WHERE as_of_date IS NOT NULL
   AND CAST(strftime('%s', as_of_date) AS INTEGER) BETWEEN ? AND ?
 ORDER BY t`,
		w.Start, w.End)
}

// buildBatch materialises the full portfolio as of event date t: each deal's
// latest event on/before t (by event_seq, which increases with as_of_date),
// keeping only is_open=1 — a deal whose latest event ≤ t is its exit
// (is_open=0) is dropped, a deal with no event ≤ t (not yet invested) is
// absent, and so is a deal the portal had stopped listing by t (removals).
// Emits one position + one instrument per held deal, each position valued
// at its event's date, plus the single EquityZen account.
func (c *Connection) buildBatch(ctx context.Context, t int64, removals silver.Removals) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	const q = `
SELECT p.deal_external_id, CAST(strftime('%s', p.as_of_date) AS INTEGER),
       COALESCE(o.currency, 'USD'),
       p.shares_held, p.cost_basis_remaining, p.market_value,
       COALESCE(o.kind, ''), COALESCE(o.company_name, ''), COALESCE(o.ticker_symbol, ''),
       (SELECT MIN(CAST(strftime('%s', p2.as_of_date) AS INTEGER))
          FROM positions p2
         WHERE p2.deal_external_id = p.deal_external_id AND p2.as_of_date IS NOT NULL),
       COALESCE(o.payload, ''),
       o.basis, o.shares_original,
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
			asOf                                        int64
			shares, cost, market, paidIn, bought, fee   sql.NullFloat64
			acqUnix                                     sql.NullInt64
		)
		if err := rows.Scan(&deal, &asOf, &currency, &shares, &cost, &market,
			&kind, &company, &symbol, &acqUnix, &payl, &paidIn, &bought, &fee); err != nil {
			return batch, err
		}
		if removals.Removed(deal, t) {
			continue
		}
		held = true
		acNew, vehicle := taxonomyForKind(kind)
		instKey := deal

		change := canonical.PositionChange{
			SnapshotAt:           t,
			ValuedAt:             asOf,
			AccountExternalID:    accountKey,
			PositionKey:          deal,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             currency,
			MarketValue:          silver.DecimalPtrFromNullFloat(market),
			AcquisitionDate:      silver.DatePtrFromNullUnix(acqUnix),
		}
		spv := vehicle == canonical.VehicleSPV
		book, basis, feeShare := bookValue(stake{spv: spv, cost: cost, paidIn: paidIn,
			held: shares, bought: bought, fee: fee})
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
		// EZ-internal ticker, not a public listing); funds have none.
		// Surfacing it lets `wealthdb holdings positions` show a symbol
		// like public equities.
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
// modelled. The per-position vehicle (spv / fund) carries the packaging
// distinction.
func custodyAccount(t int64) canonical.AccountChange {
	a := silver.CustodyAccount(accountKey, t)
	name := "EquityZen"
	a.DisplayName = &name
	return a
}
