package carta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/shopspring/decimal"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots reconstructs the per-day portfolio from the silver's per-lot change
// deltas (DESIGN.md §5.1 in the collector). The silver stores a row for a
// security lot only on a day its state changes; gold's as-of query, by
// contrast, takes the latest snapshot per source and reads ALL its positions.
// So for every event date we emit a COMPLETE forward-filled snapshot: each
// lot's latest delta on/before that date, keep only the `held` ones, then
// aggregate them per company into a single position. An exited holding drops
// out exactly at its disposition date; a full portfolio exists at every date.
// When the LAST holding exits, the disposition date gets the exit-day zero
// snapshot instead (silver.ClosureMarkerBatch) — dropping to an empty batch
// would leave the pre-exit marks carried forward as phantom value.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ledger, err := c.loadFundLedger(ctx)
	if err != nil {
		return nil, err
	}
	inKind, err := c.readInKind(ctx)
	if err != nil {
		return nil, err
	}
	times, err := c.snapshotTimesInWindow(ctx, w, append(ledger.carryDates(), inKind.PeriodEnds()...))
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
	// The held→empty transition is tracked within the walked window, so the
	// closure marker depends on ChangeWindow spanning the full content history
	// (it does — see status.go): a window starting between the last held date
	// and the exit date would miss the transition and re-emit nothing.
	batches := make([]canonical.SnapshotBatch, 0, len(times))
	var lastHeld canonical.SnapshotBatch
	prevHeld := false
	for _, t := range times {
		batch, err := c.buildBatch(ctx, t, meta, acct, ledger, inKind)
		if err != nil {
			return nil, err
		}
		held := len(batch.Positions) > 0
		switch {
		case held:
			lastHeld = batch
		case prevHeld:
			// The portfolio just emptied: emit the exit-day zero snapshot so
			// the value spine and the as-of holdings register the closure ON
			// the disposition date instead of carrying the last marks forward
			// (an empty batch is invisible to gold's queries).
			batch = silver.ClosureMarkerBatch(lastHeld, t, custodyAccount(acct, t))
		}
		prevHeld = held
		batches = append(batches, batch)
	}
	inKind.LogClamped(kindName)
	return silver.NewSnapshotStream(batches), nil
}

// The whole Carta individual portfolio is ONE gold account; each held company
// is one position under it, and that company's share certificates / option
// grants are the lots of the position (aggregated). This mirrors how a public
// brokerage account holds one position per security with tax lots underneath.
// The double-entry transaction pairs (transactions.go) sit on this same
// account, so the value spine and the cash flows meet on one entity at every
// returns grain.
const accountKeyFallback = "carta"

// instrumentID / positionKey key the per-company instrument + position on the
// Carta entity (corporation / fund) id, prefixed so it reads unambiguously.
// One instrument and one position per company; the position keys on its
// instrument (one position per security, brokerage-style).
func instrumentID(entityID int64) string { return "entity:" + strconv.FormatInt(entityID, 10) }
func positionKey(entityID int64) string  { return instrumentID(entityID) }

// custodyAccount is the Carta login's one account's change record as of t.
// management_style is an account-level field (the canonical position carries
// none), so the GP-managed fund vs equity vs pre-conversion SAFE distinction
// rides on each position's (asset_class, vehicle) pair ((private_equity, fund)
// vs (private_equity, stock) vs (private_debt, convertible_note)), not here;
// the account is self-directed — the holder controls what the portfolio holds.
func custodyAccount(acct string, t int64) canonical.AccountChange {
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	usd := "USD"
	name := "Carta"
	return canonical.AccountChange{
		AccountExternalID: acct,
		AccountKind:       canonical.AccountKindCustody,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		BaseCurrency:      &usd,
		DisplayName:       &name,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	}
}

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
// the union of the position-bearing content tables and the extra days a
// position's value or book value changes: a fund's carried value, a K-1's
// in-kind distribution (dump_runs is the download time, not a holding event,
// so it is intentionally excluded).
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window, extra []int64) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM entities     WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM securities   WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM fund_metrics WHERE snapshot_at BETWEEN ? AND ?
)`
	rows, err := c.db.QueryContext(ctx, q,
		w.Start, w.End, w.Start, w.End, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimesInWindow: %w", err)
	}
	defer rows.Close()
	var out []int64
	seen := make(map[int64]bool)
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		seen[t] = true
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range extra {
		if t >= w.Start && t <= w.End && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
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
// held company (cap-table lots aggregated, or the fund's capital account, or
// before the fund's first NAV its called capital),
// plus the single Carta account and one instrument per held company (so gold's
// FK from positions is satisfied and the account's seen-range merges across
// batches). A fund's book value is its capital paid in less the basis its
// K-1s say left in kind by t (inKind).
func (c *Connection) buildBatch(ctx context.Context, t int64, meta map[int64]entInfo, acct string, ledger fundLedger, inKind *silver.InKind) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	active := make(map[int64]string)                   // entity id -> holdings currency
	classesNew := make(map[int64]canonical.AssetClass) // entity id -> exposure (asset_class)
	vehicles := make(map[int64]canonical.Vehicle)      // entity id -> vehicle

	if err := c.appendCapTableAt(ctx, t, acct, &batch, active, classesNew, vehicles); err != nil {
		return batch, err
	}
	if err := c.appendFundAt(ctx, t, acct, ledger, inKind, &batch, active, classesNew, vehicles); err != nil {
		return batch, err
	}
	if err := appendFundCarryAt(t, acct, ledger, inKind, &batch, active, classesNew, vehicles); err != nil {
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

	batch.Accounts = append(batch.Accounts, custodyAccount(acct, t))

	for _, eid := range ids {
		info := meta[eid]
		inst := canonical.InstrumentChange{
			InstrumentExternalID: instrumentID(eid),
			AssetClass:           classesNew[eid], // 2-D taxonomy: same pair its position carries
			Vehicle:              vehicles[eid],
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
	SecurityType string   `json:"security_type"`
	SecurityID   int64    `json:"security_id"`
	Label        string   `json:"label,omitempty"`
	Quantity     *float64 `json:"quantity,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
	MarketValue  *float64 `json:"market_value,omitempty"`
	IssueDate    string   `json:"issue_date,omitempty"`
	// AcquiredOn is the date the holder ACQUIRED the lot, which is not
	// the date the certificate carries. A certificate is re-issued
	// whenever the holding is restructured — a transfer, a stock
	// split, a conversion — and the new one is dated to the re-issue
	// while the shares behind it are the same shares. Carta states
	// the acquisition date separately, and it can precede the
	// platform's own coverage.
	AcquiredOn    string   `json:"acquired_on,omitempty"`
	ExercisePrice *float64 `json:"exercise_price,omitempty"`
}

// shareLot is a held share certificate as an open lot of its company's
// position, keyed by the certificate's id: its shares, the cash paid for
// them as its book value, its per-date value and the date the holder
// acquired it (not the certificate's issue date — see lot.AcquiredOn). A
// certificate born from an option exercise carries the exercise's type,
// date and fair-market-value per share in the payload, as silver states
// them; the book value stays the cash paid. An option or a convertible is
// not a lot: neither holds shares until it is exercised or converts, and
// then the certificate it becomes is one. The caller sets the position's
// keys and currency.
func shareLot(secID int64, quantity, cost, mv sql.NullFloat64, acquired string,
	exType, exDate sql.NullString, exFMV sql.NullFloat64) canonical.PositionLotChange {
	l := canonical.PositionLotChange{
		LotKey:      strconv.FormatInt(secID, 10),
		Quantity:    silver.DecimalPtrFromNullFloat(quantity),
		BookValue:   silver.DecimalPtrFromNullFloat(cost),
		MarketValue: silver.DecimalPtrFromNullFloat(mv),
	}
	if l.BookValue != nil {
		l.BasisOrigin = canonical.BasisStated
	}
	if acq, ok := flowDateUnix(acquired); ok {
		l.AcquisitionDate = silver.DatePtrFromNullUnix(sql.NullInt64{Int64: acq, Valid: true})
	}
	ex := map[string]any{}
	if exType.Valid && exType.String != "" {
		ex["exercise_type"] = exType.String
	}
	if exDate.Valid && exDate.String != "" {
		ex["exercise_date"] = exDate.String
	}
	if exFMV.Valid {
		ex["exercise_fmv"] = exFMV.Float64
	}
	if len(ex) > 0 {
		l.Payload = silver.PayloadWith("{}", ex)
	}
	return l
}

// appendCapTableAt forward-fills the cap-table holdings as of t and aggregates
// them into ONE position per company: each security lot's latest delta
// on/before t, keeping only `held` lots, summed. Quantity is the share count
// (share-type lots only — an unexercised option is a different unit and would
// also double-count the certificates it became, so it stays a 0-value lot in
// the payload). MarketValue / BookValue sum every held lot — the collector's
// per-date FMV valuation (collector DESIGN.md §5.1) and the cash paid. The
// per-lot detail rides in the position payload, and each held share
// certificate is one open lot of the position (shareLot). The book value is
// stamped by what it sums: its lots' cost where every costed line is a share
// certificate (shareBasis), else the cash paid (cashPaidBasis).
func (c *Connection) appendCapTableAt(ctx context.Context, t int64, acct string, batch *canonical.SnapshotBatch, active map[int64]string, classesNew map[int64]canonical.AssetClass, vehicles map[int64]canonical.Vehicle) error {
	exercise := `NULL, NULL, NULL`
	if c.exercise {
		exercise = `exercise_type, exercise_date, exercise_fmv`
	}
	q := `
SELECT entity_external_id, security_type, security_external_id,
       COALESCE(currency, 'USD'), quantity, cost, market_value,
       COALESCE(label, ''), COALESCE(issue_date, ''), exercise_price,
       COALESCE(json_extract(payload, '$.original_acquisition_date'), ''),
       ` + exercise + `
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

	// The sums are decimal so that a position's quantity and book value are
	// exactly the sums of its share lots'.
	type agg struct {
		ccy                         string
		shareQty, mv, cost          decimal.Decimal
		hasShareQty, hasMV, hasCost bool
		hasEquity                   bool  // any non-convertible lot (share/option/…)
		hasStockLike                bool  // any real share-settled lot (share/rsu/rsa/piu/equity_grant)
		costsOther                  bool  // a costed line that is no share certificate
		acquiredUnix                int64 // earliest date among the lines that are no share lot
		hasAcquired                 bool
		lots                        []lot
		shareLots                   []canonical.PositionLotChange
	}
	aggs := make(map[int64]*agg)
	var order []int64
	for rows.Next() {
		var (
			entityID, secID                   int64
			secType, ccy, label, isDt, acqStr string
			quantity, cost, mv, strike        sql.NullFloat64
			exType, exDate                    sql.NullString
			exFMV                             sql.NullFloat64
		)
		if err := rows.Scan(&entityID, &secType, &secID, &ccy,
			&quantity, &cost, &mv, &label, &isDt, &strike, &acqStr,
			&exType, &exDate, &exFMV); err != nil {
			return err
		}
		a := aggs[entityID]
		if a == nil {
			a = &agg{ccy: normCcy(ccy)}
			aggs[entityID] = a
			order = append(order, entityID)
		}
		if secType != "convertible" {
			a.hasEquity = true // a SAFE/note is a convertible_note only until real equity appears
		}
		if isStockVehicleType(secType) {
			a.hasStockLike = true // share-settled ownership → stock vehicle (vs option/warrant/sar)
		}
		if secType == "share" {
			if quantity.Valid {
				a.shareQty = a.shareQty.Add(decimal.NewFromFloat(quantity.Float64))
				a.hasShareQty = true
			}
			a.shareLots = append(a.shareLots, shareLot(secID, quantity, cost, mv, acqStr, exType, exDate, exFMV))
		}
		if mv.Valid {
			a.mv = a.mv.Add(decimal.NewFromFloat(mv.Float64))
			a.hasMV = true
		}
		if cost.Valid {
			a.cost = a.cost.Add(decimal.NewFromFloat(cost.Float64))
			a.hasCost = true
			a.costsOther = a.costsOther || secType != "share"
		}
		l := lot{SecurityType: secType, SecurityID: secID, Label: label,
			IssueDate: isDt, AcquiredOn: acqStr}
		// A holding without a dated share lot is acquired on the EARLIEST
		// date its other held lines carry. A convertible is not re-issued
		// on a split or transfer the way a certificate is, so where Carta
		// states no acquisition date its issue date is the day it was
		// bought.
		acquired := acqStr
		if acquired == "" && secType == "convertible" {
			acquired = isDt
		}
		if acq, ok := flowDateUnix(acquired); ok && secType != "share" && (!a.hasAcquired || acq < a.acquiredUnix) {
			a.acquiredUnix, a.hasAcquired = acq, true
		}
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
		classNew, vehicle := capTableTaxonomy(a.hasEquity, a.hasStockLike)
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    acct,
			PositionKey:          positionKey(eid),
			InstrumentExternalID: &instKey,
			AssetClass:           classNew,
			Vehicle:              vehicle,
			Currency:             a.ccy,
			Payload:              json.RawMessage(payload),
		}
		if a.hasShareQty {
			change.Quantity = &a.shareQty
		}
		if a.hasMV {
			change.MarketValue = &a.mv
		}
		if a.hasCost {
			basis := shareBasis
			if a.costsOther {
				basis = cashPaidBasis
			}
			change.SetBookValue(&a.cost, basis)
		}
		// A holding with share lots is acquired on its earliest lot's
		// date: a later lot's date would say the oldest shares were
		// acquired more recently than they were.
		change.AcquisitionDate = canonical.EarliestLotDate(a.shareLots)
		if change.AcquisitionDate == nil && a.hasAcquired {
			change.AcquisitionDate = silver.DatePtrFromNullUnix(
				sql.NullInt64{Int64: a.acquiredUnix, Valid: true})
		}
		batch.Positions = append(batch.Positions, change)
		for _, l := range a.shareLots {
			l.SnapshotAt, l.AccountExternalID, l.PositionKey = t, acct, change.PositionKey
			l.InstrumentExternalID, l.Currency = &instKey, a.ccy
			batch.PositionLots = append(batch.PositionLots, l)
		}
		active[eid] = a.ccy
		classesNew[eid] = classNew
		vehicles[eid] = vehicle
	}
	return nil
}

// appendFundAt forward-fills the fund LP positions as of t: each fund's latest
// capital-account delta on/before t (one position per fund). MarketValue =
// net_asset_value (the NAV at that quarter), BookValue = capital_contributed
// less the basis distributed in kind by t (inKind), AcquisitionDate = the
// fund's first capital call. Money arrives as decimal strings, parsed exactly.
func (c *Connection) appendFundAt(ctx context.Context, t int64, acct string, ledger fundLedger, inKind *silver.InKind, batch *canonical.SnapshotBatch, active map[int64]string, classesNew map[int64]canonical.AssetClass, vehicles map[int64]canonical.Vehicle) error {
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
		// 2-D taxonomy: a fund LP interest is private_equity exposure held via
		// the fund vehicle (TAXONOMY.md — legacy private_fund dissolves to
		// private_equity/infrastructure/hedge_fund × fund; a Carta fund
		// investment is read as a venture/PE feeder → private_equity).
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    acct,
			PositionKey:          positionKey(entityID),
			InstrumentExternalID: &instKey,
			AssetClass:           canonical.AssetClassPrivateEquity,
			Vehicle:              canonical.VehicleFund,
			Currency:             ccy,
			AcquisitionDate:      ledger.acquisitionDate(entityID),
			Payload:              json.RawMessage(payload),
		}
		if mv, err := canonical.NewDecimalFromString(nav); err == nil && nav != "" {
			change.MarketValue = &mv
		}
		if bv, err := canonical.NewDecimalFromString(contributed); err == nil && contributed != "" {
			book, basis, extra := inKind.BookValue(change.PositionKey, t, &bv, fundBasis)
			change.SetBookValue(book, basis)
			change.Payload = silver.PayloadWith(payload, extra)
		}
		batch.Positions = append(batch.Positions, change)
		active[entityID] = ccy
		classesNew[entityID] = canonical.AssetClassPrivateEquity
		vehicles[entityID] = canonical.VehicleFund
	}
	return rows.Err()
}
