package carta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
	"github.com/shopspring/decimal"
)

type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

// Snapshots reconstructs the per-day portfolio from the silver's per-position
// change deltas (DESIGN.md §5.1 in the collector). The silver stores a row
// for a position only on a day its state changes; gold's as-of query, by
// contrast, takes the latest snapshot per source and reads ALL its positions.
// So for every event date we emit a COMPLETE forward-filled snapshot: each
// position's latest delta on/before that date, dropping the ones whose latest
// state is `exited`. An exited holding therefore drops out exactly at its
// disposition date, and a full portfolio is materialised at every date.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	meta, err := c.entityMeta(ctx)
	if err != nil {
		return nil, err
	}
	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		batch, err := c.buildBatch(ctx, t, meta)
		if err != nil {
			return nil, err
		}
		out.batches = append(out.batches, batch)
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

// accountID is the gold account_external_id for a Carta entity: the bare
// entity (corporation) id. instrumentID is the matching instruments-table
// key, prefixed so it reads unambiguously.
func accountID(entityID int64) string    { return strconv.FormatInt(entityID, 10) }
func instrumentID(entityID int64) string { return "entity:" + strconv.FormatInt(entityID, 10) }

// normCcy maps Carta's currency display to an ISO 4217 code: the cap-table
// side reports "$" rather than "USD". Empty / NULL also default to USD.
func normCcy(c string) string {
	if c == "" || c == "$" {
		return "USD"
	}
	return c
}

// snapshotTimesInWindow are the event dates on which any position changes —
// the union of the position-bearing content tables (dump_runs is the download
// time, not a holding event, so it is intentionally excluded).
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM entities     WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM securities   WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM fund_metrics WHERE snapshot_at BETWEEN ? AND ?
)
ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q,
		w.Start, w.End, w.Start, w.End, w.Start, w.End)
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

// entInfo is the stable per-entity metadata (name + fund flag + raw payload)
// used to stamp the account / instrument. Taken from the entity's latest row.
type entInfo struct {
	name    string
	isFund  bool
	payload string
}

func (c *Connection) entityMeta(ctx context.Context) (map[int64]entInfo, error) {
	const q = `
SELECT entity_external_id, is_fund_investment, COALESCE(legal_name, ''), payload
  FROM entities e
 WHERE snapshot_at = (SELECT MAX(snapshot_at) FROM entities e2
                       WHERE e2.entity_external_id = e.entity_external_id)`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("entityMeta: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]entInfo)
	for rows.Next() {
		var (
			id          int64
			isFundInt   int
			name, payld string
		)
		if err := rows.Scan(&id, &isFundInt, &name, &payld); err != nil {
			return nil, err
		}
		out[id] = entInfo{name: name, isFund: isFundInt != 0, payload: payld}
	}
	return out, rows.Err()
}

// buildBatch materialises the full portfolio as of date t: the forward-filled
// cap-table + fund positions, plus the account + instrument for every entity
// that holds something at t (so gold's FK from positions is satisfied and the
// account's seen-range is merged across batches).
func (c *Connection) buildBatch(ctx context.Context, t int64, meta map[int64]entInfo) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	active := make(map[int64]string) // entity id -> holdings currency

	if err := c.appendCapTableAt(ctx, t, &batch, active); err != nil {
		return batch, err
	}
	if err := c.appendFundAt(ctx, t, &batch, active); err != nil {
		return batch, err
	}

	ids := make([]int64, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	wrapper := canonical.TaxWrapperTaxablePersonal
	for _, eid := range ids {
		info := meta[eid]
		// GP-managed fund vs holder-controlled cap-table equity.
		style := canonical.ManagementStyleSelfDirected
		if info.isFund {
			style = canonical.ManagementStyleDiscretionary
		}
		wr := wrapper
		acct := canonical.AccountChange{
			AccountExternalID: accountID(eid),
			AccountKind:       canonical.AccountKindCustody,
			TaxWrapper:        &wr,
			ManagementStyle:   &style,
			FirstSeenAt:       t,
			LastSeenAt:        t,
			Payload:           json.RawMessage(info.payload),
		}
		if ccy := active[eid]; ccy != "" {
			bc := ccy
			acct.BaseCurrency = &bc
		}
		inst := canonical.InstrumentChange{
			InstrumentExternalID: instrumentID(eid),
			AssetClass:           assetClassFor(info.isFund),
			FirstSeenAt:          t,
			LastSeenAt:           t,
			Payload:              json.RawMessage(info.payload),
		}
		if info.name != "" {
			name := info.name
			acct.DisplayName = &name
			inst.Name = &name
		}
		batch.Accounts = append(batch.Accounts, acct)
		batch.Instruments = append(batch.Instruments, inst)
	}
	return batch, nil
}

// appendCapTableAt forward-fills the cap-table positions held as of t: each
// security's latest delta on/before t, keeping only those whose latest state
// is `held`. MarketValue is the collector's valuation (held shares ×
// FMV-at-last-exercise; 0 for unexercised options), BookValue the cost.
func (c *Connection) appendCapTableAt(ctx context.Context, t int64, batch *canonical.SnapshotBatch, active map[int64]string) error {
	const q = `
SELECT entity_external_id, security_type, security_external_id,
       COALESCE(currency, 'USD'), quantity, cost, market_value, payload
  FROM securities s
 WHERE position_status = 'held'
   AND snapshot_at = (SELECT MAX(snapshot_at) FROM securities s2
                       WHERE s2.entity_external_id   = s.entity_external_id
                         AND s2.security_type        = s.security_type
                         AND s2.security_external_id = s.security_external_id
                         AND s2.snapshot_at <= ?)`
	rows, err := c.db.QueryContext(ctx, q, t)
	if err != nil {
		return fmt.Errorf("appendCapTableAt: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			entityID, secID           int64
			secType, ccy, payload     string
			quantity, cost, marketVal sql.NullFloat64
		)
		if err := rows.Scan(&entityID, &secType, &secID, &ccy, &quantity, &cost, &marketVal, &payload); err != nil {
			return err
		}
		ccy = normCcy(ccy)
		instKey := instrumentID(entityID)
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    accountID(entityID),
			PositionKey:          secType + ":" + strconv.FormatInt(secID, 10),
			InstrumentExternalID: &instKey,
			AssetClass:           canonical.AssetClassPrivateEquity,
			Currency:             ccy,
			Payload:              json.RawMessage(payload),
		}
		if quantity.Valid {
			d := canonical.Decimal(decimal.NewFromFloat(quantity.Float64))
			change.Quantity = &d
		}
		if cost.Valid {
			d := canonical.Decimal(decimal.NewFromFloat(cost.Float64))
			change.BookValue = &d
		}
		if marketVal.Valid {
			d := canonical.Decimal(decimal.NewFromFloat(marketVal.Float64))
			change.MarketValue = &d
		}
		batch.Positions = append(batch.Positions, change)
		active[entityID] = ccy
	}
	return rows.Err()
}

// appendFundAt forward-fills the fund LP positions as of t: each fund's latest
// capital-account delta on/before t. MarketValue = net_asset_value (the NAV at
// that quarter), BookValue = capital_contributed. Money arrives as decimal
// strings, parsed exactly.
func (c *Connection) appendFundAt(ctx context.Context, t int64, batch *canonical.SnapshotBatch, active map[int64]string) error {
	const q = `
SELECT entity_external_id, COALESCE(currency, 'USD'),
       COALESCE(net_asset_value, ''), COALESCE(capital_contributed, ''), payload
  FROM fund_metrics fm
 WHERE snapshot_at = (SELECT MAX(snapshot_at) FROM fund_metrics fm2
                       WHERE fm2.entity_external_id = fm.entity_external_id
                         AND fm2.snapshot_at <= ?)`
	rows, err := c.db.QueryContext(ctx, q, t)
	if err != nil {
		return fmt.Errorf("appendFundAt: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			entityID              int64
			ccy, nav, contributed string
			payload               string
		)
		if err := rows.Scan(&entityID, &ccy, &nav, &contributed, &payload); err != nil {
			return err
		}
		ccy = normCcy(ccy)
		instKey := instrumentID(entityID)
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    accountID(entityID),
			PositionKey:          "fund",
			InstrumentExternalID: &instKey,
			AssetClass:           canonical.AssetClassPrivateFund,
			Currency:             ccy,
			Payload:              json.RawMessage(payload),
		}
		if mv, err := canonical.NewDecimalFromString(nav); err == nil && nav != "" {
			change.MarketValue = &mv
		}
		if bv, err := canonical.NewDecimalFromString(contributed); err == nil && contributed != "" {
			change.BookValue = &bv
		}
		batch.Positions = append(batch.Positions, change)
		active[entityID] = ccy
	}
	return rows.Err()
}
