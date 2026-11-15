package equityzen

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// accountKey is the single custody account: it holds the positions AND the
// double-entry transaction pairs (transactions.go), so the value spine and
// the cash flows meet on one entity at every returns grain. The silver has no
// buyer-id column and the holder has one relationship, so it is a constant.
const accountKey = "equityzen"

// Snapshots forward-fills the per-day portfolio from the silver's
// event-sourced `positions` table (silver.Book). The collector already
// replays each deal's timeline and computes its mark (silver migrations
// 0001/0002); this adapter does no valuation logic. Every event date
// gets a COMPLETE snapshot: each deal's latest event on or before that
// date, keeping only the is_open ones, which is what gold's as-of query
// reads (each account's latest snapshot, all its positions). An exited
// deal drops out exactly at its exit date; a deal the portal stopped
// listing drops out at the first dump that omitted it (removals), a
// snapshot time where the roster is emitted without it and every other
// deal keeps its own mark date (ValuedAt). When the LAST deal leaves,
// that date gets the exit-day zero snapshot instead
// (silver.ClosureMarkerBatch).
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	removals, err := c.removals(ctx)
	if err != nil {
		return nil, err
	}
	states, err := c.states(ctx)
	if err != nil {
		return nil, err
	}
	// The closure marker depends on ChangeWindow spanning the full content
	// history (it does — see status.go): a window starting between the last
	// held date and the exit date would miss the transition and re-emit
	// nothing.
	batches, err := silver.Book{
		States:   states,
		Removals: removals,
		Account:  custodyAccount,
		Emit: func(t int64, held []silver.Holding) (canonical.SnapshotBatch, error) {
			return emit(t, held), nil
		},
		StampValuedAt: true,
		Closure:       true,
	}.Batches(w)
	if err != nil {
		return nil, err
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

// state is one positions row with what the deal's batch reads from it
// (silver.Holding.Row).
type state struct {
	currency, kind, company, symbol, payload string
	shares, cost, paidIn, bought, fee        sql.NullFloat64
	market                                   *canonical.Decimal
	acquired                                 sql.NullInt64
}

// states are every deal's dated events in event_seq order (silver.Book):
// a deal's latest event on or before a date is the one with the highest
// event_seq. An event is held while it is is_open=1 and its offering is
// known; an exit is is_open=0, and a deal with no event by a date is not
// yet invested. An event with no date is no state.
func (c *Connection) states(ctx context.Context) ([]silver.Holding, error) {
	const q = `
SELECT p.deal_external_id, CAST(strftime('%s', p.as_of_date) AS INTEGER),
       COALESCE(p.is_open = 1 AND o.deal_external_id IS NOT NULL, 0),
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
  LEFT JOIN offerings o ON o.deal_external_id = p.deal_external_id
 WHERE CAST(strftime('%s', p.as_of_date) AS INTEGER) IS NOT NULL
 ORDER BY p.deal_external_id, p.event_seq`
	return silver.Query(ctx, c.db, "states", q, func(rows *sql.Rows) (silver.Holding, error) {
		var (
			h silver.Holding
			s state
		)
		err := rows.Scan(&h.Key, &h.At, &h.Open, &s.currency, &s.shares, &s.cost,
			silver.Dec(&s.market, silver.DecFloat), &s.kind, &s.company, &s.symbol, &s.acquired,
			&s.payload, &s.paidIn, &s.bought, &s.fee)
		h.Row = s
		return h, err
	})
}

// emit is the batch at t of the deals held then: one position and one
// instrument per held deal, each position valued at its event's date.
// The book carries the single EquityZen account.
func emit(t int64, held []silver.Holding) canonical.SnapshotBatch {
	var batch canonical.SnapshotBatch
	for _, h := range held {
		s := h.Row.(state)
		acNew, vehicle := taxonomyForKind(s.kind)
		instKey := h.Key

		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    accountKey,
			PositionKey:          h.Key,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             s.currency,
			AcquisitionDate:      silver.DatePtrFromNullUnix(s.acquired),
		}
		if s.market != nil {
			market := *s.market // each batch holds its own value
			change.MarketValue = &market
		}
		spv := vehicle == canonical.VehicleSPV
		book, basis, feeShare := bookValue(stake{spv: spv, cost: s.cost, paidIn: s.paidIn,
			held: s.shares, bought: s.bought, fee: s.fee})
		change.SetBookValue(book, basis)
		if feeShare != nil {
			change.Payload = silver.PayloadWith("{}", map[string]any{
				"execution_fee_in_basis": feeShare.String()})
		}
		// quantity is a share count only for SPVs; a multi-company fund's
		// LP interest has no meaningful unit count.
		if spv {
			change.Quantity = silver.DecimalPtrFromNullFloat(s.shares)
		}
		batch.Positions = append(batch.Positions, change)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			FirstSeenAt:          t,
			LastSeenAt:           t,
		}
		if s.company != "" {
			company := s.company
			inst.Name = &company
		}
		// EquityZen assigns single-company SPVs a per-company symbol (an
		// EZ-internal ticker, not a public listing); funds have none.
		// Surfacing it lets `wealthdb holdings positions` show a symbol
		// like public equities.
		if s.symbol != "" {
			symbol := s.symbol
			inst.Symbol = &symbol
		}
		if s.payload != "" {
			inst.Payload = json.RawMessage(s.payload)
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	return batch
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
