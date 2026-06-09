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

// Snapshots reconstructs the per-day portfolio from the silver's per-lot change
// deltas (DESIGN.md §5.1 in the collector). The silver stores a row for a
// security lot only on a day its state changes; gold's as-of query, by
// contrast, takes the latest snapshot per source and reads ALL its positions.
// So for every event date we emit a COMPLETE forward-filled snapshot: each
// lot's latest delta on/before that date, keep only the `held` ones, then
// aggregate them per company into a single position. An exited holding drops
// out exactly at its disposition date; a full portfolio exists at every date.
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
	acct, err := c.accountKey(ctx)
	if err != nil {
		return nil, err
	}
	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		batch, err := c.buildBatch(ctx, t, meta, acct)
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

// The whole Carta individual portfolio is ONE gold account; each held company
// is one position under it, and that company's share certificates / option
// grants are the lots of the position (aggregated). This mirrors how a public
// brokerage account holds one position per security with tax lots underneath.
const accountKeyFallback = "carta"

// fundingAccountKey is the sentinel cash account carrying the double-entry
// transaction pairs (transactions.go). Carta exposes no real funding balance,
// so every event is a balanced pair and this account's derived balance is
// always exactly 0 — a pure pass-through clearing account.
const fundingAccountKey = "carta-funding"

// instrumentID / positionKey key the per-company instrument + position on the
// Carta entity (corporation / fund) id, prefixed so it reads unambiguously.
// One instrument and one position per company; the position keys on its
// instrument (one position per security, brokerage-style).
func instrumentID(entityID int64) string { return "entity:" + strconv.FormatInt(entityID, 10) }
func positionKey(entityID int64) string  { return instrumentID(entityID) }

// accountKey is the gold account_external_id for the single Carta account: the
// individual-portfolio id (falling back to the firm id, then a constant). All
// of the holder's companies hang off this one account.
func (c *Connection) accountKey(ctx context.Context) (string, error) {
	var ind, firm sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT individual_id, firm_id FROM dump_runs ORDER BY snapshot_at DESC LIMIT 1`).
		Scan(&ind, &firm)
	if err == sql.ErrNoRows {
		return accountKeyFallback, nil
	}
	if err != nil {
		return "", fmt.Errorf("accountKey: %w", err)
	}
	if ind.Valid && ind.String != "" {
		return ind.String, nil
	}
	if firm.Valid && firm.String != "" {
		return firm.String, nil
	}
	return accountKeyFallback, nil
}

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
// used to stamp the company instrument. Taken from the entity's latest row.
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

// buildBatch materialises the full portfolio as of date t: one position per
// held company (cap-table lots aggregated, or the fund's capital account),
// plus the single Carta account and one instrument per held company (so gold's
// FK from positions is satisfied and the account's seen-range merges across
// batches).
func (c *Connection) buildBatch(ctx context.Context, t int64, meta map[int64]entInfo, acct string) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	active := make(map[int64]string) // entity id -> holdings currency

	if err := c.appendCapTableAt(ctx, t, acct, &batch, active); err != nil {
		return batch, err
	}
	if err := c.appendFundAt(ctx, t, acct, &batch, active); err != nil {
		return batch, err
	}
	if len(active) == 0 {
		return batch, nil // nothing held at t
	}

	ids := make([]int64, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// One account for the whole Carta portfolio. management_style is an
	// account-level field (the canonical position carries none), so the
	// GP-managed fund vs holder-controlled equity distinction rides on each
	// position's asset_class (private_fund vs private_equity), not here; the
	// account is self-directed — the holder controls what the portfolio holds.
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	usd := "USD"
	name := "Carta"
	batch.Accounts = append(batch.Accounts, canonical.AccountChange{
		AccountExternalID: acct,
		AccountKind:       canonical.AccountKindCustody,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		BaseCurrency:      &usd,
		DisplayName:       &name,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	})

	// The sentinel funding account carrying the double-entry transaction pairs
	// (transactions.go): a synthetic cash conduit whose derived balance is
	// always 0 (Carta's real external funding account is unobserved). No
	// positions and no cash_balance row — the 0 is implicit in the paired
	// ledger.
	fundingName := "Carta (funding)"
	batch.Accounts = append(batch.Accounts, canonical.AccountChange{
		AccountExternalID: fundingAccountKey,
		AccountKind:       canonical.AccountKindCash,
		BaseCurrency:      &usd,
		DisplayName:       &fundingName,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	})

	for _, eid := range ids {
		info := meta[eid]
		inst := canonical.InstrumentChange{
			InstrumentExternalID: instrumentID(eid),
			AssetClass:           assetClassFor(info.isFund),
			FirstSeenAt:          t,
			LastSeenAt:           t,
			Payload:              json.RawMessage(info.payload),
		}
		if info.name != "" {
			n := info.name
			inst.Name = &n
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	return batch, nil
}

// lot is one cap-table security line inside a company position's payload — a
// tax lot (a share certificate or option grant) of the aggregated holding.
type lot struct {
	SecurityType  string   `json:"security_type"`
	SecurityID    int64    `json:"security_id"`
	Label         string   `json:"label,omitempty"`
	Quantity      *float64 `json:"quantity,omitempty"`
	Cost          *float64 `json:"cost,omitempty"`
	MarketValue   *float64 `json:"market_value,omitempty"`
	IssueDate     string   `json:"issue_date,omitempty"`
	ExercisePrice *float64 `json:"exercise_price,omitempty"`
}

// appendCapTableAt forward-fills the cap-table holdings as of t and aggregates
// them into ONE position per company: each security lot's latest delta
// on/before t, keeping only `held` lots, summed. Quantity is the share count
// (share-type lots only — an unexercised option is a different unit and would
// also double-count the certificates it became, so it stays a 0-value lot in
// the payload). MarketValue / BookValue sum every held lot — the collector's
// per-date FMV valuation (collector DESIGN.md §5.1) and the cost basis. The
// per-lot detail rides in the position payload.
func (c *Connection) appendCapTableAt(ctx context.Context, t int64, acct string, batch *canonical.SnapshotBatch, active map[int64]string) error {
	const q = `
SELECT entity_external_id, security_type, security_external_id,
       COALESCE(currency, 'USD'), quantity, cost, market_value,
       COALESCE(label, ''), COALESCE(issue_date, ''), exercise_price
  FROM securities s
 WHERE position_status = 'held'
   AND snapshot_at = (SELECT MAX(snapshot_at) FROM securities s2
                       WHERE s2.entity_external_id   = s.entity_external_id
                         AND s2.security_type        = s.security_type
                         AND s2.security_external_id = s.security_external_id
                         AND s2.snapshot_at <= ?)
 ORDER BY entity_external_id, security_external_id`
	rows, err := c.db.QueryContext(ctx, q, t)
	if err != nil {
		return fmt.Errorf("appendCapTableAt: %w", err)
	}
	defer rows.Close()

	type agg struct {
		ccy                         string
		shareQty, mv, cost          float64
		hasShareQty, hasMV, hasCost bool
		lots                        []lot
	}
	aggs := make(map[int64]*agg)
	var order []int64
	for rows.Next() {
		var (
			entityID, secID            int64
			secType, ccy, label, isDt  string
			quantity, cost, mv, strike sql.NullFloat64
		)
		if err := rows.Scan(&entityID, &secType, &secID, &ccy,
			&quantity, &cost, &mv, &label, &isDt, &strike); err != nil {
			return err
		}
		a := aggs[entityID]
		if a == nil {
			a = &agg{ccy: normCcy(ccy)}
			aggs[entityID] = a
			order = append(order, entityID)
		}
		if secType == "share" && quantity.Valid {
			a.shareQty += quantity.Float64
			a.hasShareQty = true
		}
		if mv.Valid {
			a.mv += mv.Float64
			a.hasMV = true
		}
		if cost.Valid {
			a.cost += cost.Float64
			a.hasCost = true
		}
		l := lot{SecurityType: secType, SecurityID: secID, Label: label, IssueDate: isDt}
		if quantity.Valid {
			v := quantity.Float64
			l.Quantity = &v
		}
		if cost.Valid {
			v := cost.Float64
			l.Cost = &v
		}
		if mv.Valid {
			v := mv.Float64
			l.MarketValue = &v
		}
		if strike.Valid {
			v := strike.Float64
			l.ExercisePrice = &v
		}
		a.lots = append(a.lots, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, eid := range order {
		a := aggs[eid]
		payload, err := json.Marshal(struct {
			Lots []lot `json:"lots"`
		}{a.lots})
		if err != nil {
			return fmt.Errorf("appendCapTableAt payload: %w", err)
		}
		instKey := instrumentID(eid)
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    acct,
			PositionKey:          positionKey(eid),
			InstrumentExternalID: &instKey,
			AssetClass:           canonical.AssetClassPrivateEquity,
			Currency:             a.ccy,
			Payload:              json.RawMessage(payload),
		}
		if a.hasShareQty {
			d := canonical.Decimal(decimal.NewFromFloat(a.shareQty))
			change.Quantity = &d
		}
		if a.hasMV {
			d := canonical.Decimal(decimal.NewFromFloat(a.mv))
			change.MarketValue = &d
		}
		if a.hasCost {
			d := canonical.Decimal(decimal.NewFromFloat(a.cost))
			change.BookValue = &d
		}
		batch.Positions = append(batch.Positions, change)
		active[eid] = a.ccy
	}
	return nil
}

// appendFundAt forward-fills the fund LP positions as of t: each fund's latest
// capital-account delta on/before t (one position per fund). MarketValue =
// net_asset_value (the NAV at that quarter), BookValue = capital_contributed.
// Money arrives as decimal strings, parsed exactly.
func (c *Connection) appendFundAt(ctx context.Context, t int64, acct string, batch *canonical.SnapshotBatch, active map[int64]string) error {
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
			AccountExternalID:    acct,
			PositionKey:          positionKey(entityID),
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
