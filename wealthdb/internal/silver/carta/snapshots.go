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
// deltas (DESIGN.md §5.1 in the collector) and the funds' capital-account
// deltas (silver.Book). The silver stores a row for a security lot only on a
// day its state changes; gold's as-of query, by contrast, takes each
// account's latest snapshot and reads ALL its positions. So every event date
// gets a COMPLETE forward-filled snapshot: each lot's latest delta on/before
// that date, keep only the `held` ones, then aggregate them per company into
// a single position. An exited holding drops out exactly at its disposition
// date; a full portfolio exists at every date. A company the portal stopped
// listing drops out at the first dump that omitted it (removals), a snapshot
// time where the roster is emitted without it and every other position keeps
// its own mark date (ValuedAt). When the LAST holding leaves, that date gets
// the exit-day zero snapshot instead (silver.ClosureMarkerBatch).
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
	removals, err := c.removals(ctx)
	if err != nil {
		return nil, err
	}
	states, err := c.states(ctx)
	if err != nil {
		return nil, err
	}
	entityDates, err := silver.SnapshotTimesOf(ctx, c.db, w, nil, "entities")
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
	// The closure marker depends on ChangeWindow spanning the full content
	// history (it does — see status.go): a window starting between the last
	// held date and the exit date would miss the transition and re-emit
	// nothing.
	batches, err := silver.Book{
		States:   states,
		Extra:    append(append(ledger.carryDates(), inKind.PeriodEnds()...), entityDates...),
		Removals: removals,
		Account:  func(t int64) canonical.AccountChange { return custodyAccount(acct, t) },
		Emit: func(t int64, held []silver.Holding) (canonical.SnapshotBatch, error) {
			return emit(t, held, meta, acct, ledger, inKind, removals)
		},
		Closure: true,
	}.Batches(w)
	if err != nil {
		return nil, err
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
	a := silver.CustodyAccount(acct, t)
	name := "Carta"
	a.DisplayName = &name
	return a
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

// removals are the companies the latest complete dump no longer lists, each
// with the time of the first such dump that omitted them, keyed by position
// key (silver.Removals). A dump's manifest (dump_runs.payload) names every
// entity the run read, and a run reads the whole portfolio; a dry run, or a
// manifest that names no entity, says nothing about the roster. Whether a
// listed company still holds anything is buildBatch's question, so a
// removal of a company whose lots had all exited is a no-op there.
func (c *Connection) removals(ctx context.Context) (silver.Removals, error) {
	const q = `
WITH runs AS (
    SELECT snapshot_at, payload FROM dump_runs
     WHERE dry_run = 0 AND json_array_length(json_extract(payload, '$.entities')) > 0),
listed AS (
    SELECT CAST(json_extract(e.value, '$.corporation_id') AS INTEGER) AS eid,
           MAX(r.snapshot_at) AS last_listed
      FROM runs r, json_each(r.payload, '$.entities') e
     WHERE json_extract(e.value, '$.corporation_id') IS NOT NULL
     GROUP BY 1)
SELECT CAST(l.eid AS TEXT),
       (SELECT MIN(r.snapshot_at) FROM runs r WHERE r.snapshot_at > l.last_listed)
  FROM listed l
 WHERE l.last_listed < (SELECT MAX(snapshot_at) FROM runs)`
	byEntity, err := silver.QueryRemovals(ctx, c.db, "carta", q)
	if err != nil {
		return nil, err
	}
	out := make(silver.Removals, len(byEntity))
	for id, at := range byEntity {
		eid, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("carta removals: entity id %q: %w", id, err)
		}
		out[positionKey(eid)] = at
	}
	return out, nil
}

// lotState is one securities delta (silver.Holding.Row).
type lotState struct {
	entityID, secID, rowid int64
	at                     int64
	secType, ccy, label    string
	issueDate, acquired    string
	quantity, cost, mv     sql.NullFloat64
	strike, exFMV          sql.NullFloat64
	exType, exDate         sql.NullString
}

// fundState is one fund_metrics delta (silver.Holding.Row).
type fundState struct {
	entityID, rowid, at   int64
	ccy, nav, contributed string
	payload               string
}

// states are every security lot's deltas and every fund's capital-account
// deltas, each holding oldest first (silver.Book). A lot is held while its
// latest delta is `held`; a fund's latest delta is its position. Both are
// part of their company's position (positionKey).
func (c *Connection) states(ctx context.Context) ([]silver.Holding, error) {
	exercise := `NULL, NULL, NULL`
	if c.schema.exercise {
		exercise = `exercise_type, exercise_date, exercise_fmv`
	}
	q := `
SELECT entity_external_id, security_type, security_external_id, snapshot_at,
       COALESCE(position_status = 'held', 0), rowid,
       COALESCE(currency, 'USD'), quantity, cost, market_value,
       COALESCE(label, ''), COALESCE(issue_date, ''), exercise_price,
       COALESCE(json_extract(payload, '$.original_acquisition_date'), ''),
       ` + exercise + `
  FROM securities
 ORDER BY entity_external_id, security_type, security_external_id, snapshot_at`
	states, err := silver.Query(ctx, c.db, "lotStates", q, func(rows *sql.Rows) (silver.Holding, error) {
		var (
			h silver.Holding
			l lotState
		)
		err := rows.Scan(&l.entityID, &l.secType, &l.secID, &h.At, &h.Open, &l.rowid, &l.ccy,
			&l.quantity, &l.cost, &l.mv, &l.label, &l.issueDate, &l.strike, &l.acquired,
			&l.exType, &l.exDate, &l.exFMV)
		l.at = h.At
		h.Key = "lot:" + strconv.FormatInt(l.entityID, 10) + ":" + l.secType + ":" + strconv.FormatInt(l.secID, 10)
		h.Position = positionKey(l.entityID)
		h.Row = l
		return h, err
	})
	if err != nil {
		return nil, err
	}
	funds, err := silver.Query(ctx, c.db, "fundStates", `
SELECT entity_external_id, snapshot_at, rowid, COALESCE(currency, 'USD'),
       COALESCE(net_asset_value, ''), COALESCE(capital_contributed, ''), payload
  FROM fund_metrics
 ORDER BY entity_external_id, snapshot_at`, func(rows *sql.Rows) (silver.Holding, error) {
		var (
			h silver.Holding
			f fundState
		)
		err := rows.Scan(&f.entityID, &h.At, &f.rowid, &f.ccy, &f.nav, &f.contributed, &f.payload)
		f.at = h.At
		h.Key = "fund:" + strconv.FormatInt(f.entityID, 10)
		h.Position = positionKey(f.entityID)
		h.Open = true
		h.Row = f
		return h, err
	})
	if err != nil {
		return nil, err
	}
	return append(states, funds...), nil
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
	out := make(map[int64]entInfo)
	if err := silver.ScanRows(ctx, c.db, "entityMeta", q, func(rows *sql.Rows) error {
		var (
			id          int64
			isFundInt   int
			name, payld string
		)
		if err := rows.Scan(&id, &isFundInt, &name, &payld); err != nil {
			return err
		}
		out[id] = entInfo{name: name, isFund: isFundInt != 0, payload: payld}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// emit is the batch at t of the companies held then: one position per held
// company (cap-table lots aggregated, or the fund's capital account, or
// before the fund's first NAV its called capital), plus one instrument per
// held company (so gold's FK from positions is satisfied and the
// instrument's seen-range merges across batches). The book carries the
// single Carta account. A fund's book value is its capital paid in less the
// basis its K-1s say left in kind by t (inKind). A company the portal had
// stopped listing by t (removals) holds no lot and no fund delta here, and
// its carried value is dropped (dropRemoved). Each position is valued at the
// date of the latest delta or NAV it is built from; a fund carried at its
// called capital is valued at t.
func emit(t int64, held []silver.Holding, meta map[int64]entInfo, acct string, ledger fundLedger, inKind *silver.InKind, removals silver.Removals) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	active := make(map[int64]string)                   // entity id -> holdings currency
	classesNew := make(map[int64]canonical.AssetClass) // entity id -> exposure (asset_class)
	vehicles := make(map[int64]canonical.Vehicle)      // entity id -> vehicle

	var lots []lotState
	var funds []fundState
	for _, h := range held {
		switch r := h.Row.(type) {
		case lotState:
			lots = append(lots, r)
		case fundState:
			funds = append(funds, r)
		}
	}
	// The lots aggregate by company in security id order, and the funds
	// follow in table order, as each was read in.
	sort.Slice(lots, func(i, j int) bool {
		a, b := lots[i], lots[j]
		if a.entityID != b.entityID {
			return a.entityID < b.entityID
		}
		if a.secID != b.secID {
			return a.secID < b.secID
		}
		return a.rowid < b.rowid
	})
	sort.Slice(funds, func(i, j int) bool { return funds[i].rowid < funds[j].rowid })

	if err := appendCapTableAt(t, acct, lots, &batch, active, classesNew, vehicles); err != nil {
		return batch, err
	}
	appendFundAt(t, acct, ledger, inKind, funds, &batch, active, classesNew, vehicles)
	if err := appendFundCarryAt(t, acct, ledger, inKind, &batch, active, classesNew, vehicles); err != nil {
		return batch, err
	}
	dropRemoved(t, &batch, active, removals)
	if len(active) == 0 {
		return batch, nil // nothing held at t
	}

	ids := make([]int64, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

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

// dropRemoved takes the funds carried for companies the portal had stopped
// listing by t out of the batch: their positions and their place among the
// active entities, so no instrument is emitted for them either. A carried
// fund has no state for the book to leave out, and it is built before this
// check, as every other position is.
func dropRemoved(t int64, batch *canonical.SnapshotBatch, active map[int64]string, removals silver.Removals) {
	if len(removals) == 0 {
		return
	}
	for eid := range active {
		if removals.Removed(positionKey(eid), t) {
			delete(active, eid)
		}
	}
	kept := batch.Positions[:0]
	for _, p := range batch.Positions {
		if !removals.Removed(p.PositionKey, t) {
			kept = append(kept, p)
		}
	}
	batch.Positions = kept
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
// them, and its value at exercise (shares × that value), which the
// position's book value counts instead of the cash paid (exerciseValue).
// An option or a convertible is not a lot: neither holds shares until it
// is exercised or converts, and then the certificate it becomes is one.
// The caller sets the position's keys and currency.
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
	if v, ok := exerciseValue(quantity, exFMV); ok {
		ex["value_at_exercise"] = v.String()
	}
	if len(ex) > 0 {
		l.Payload = silver.PayloadWith("{}", ex)
	}
	return l
}

// exerciseValue is a certificate's value on the day it was exercised: its
// shares at the fair-market value per share Carta states for the
// exercise. False where silver states no such value.
func exerciseValue(quantity, fmv sql.NullFloat64) (decimal.Decimal, bool) {
	if !quantity.Valid || !fmv.Valid {
		return decimal.Decimal{}, false
	}
	return decimal.NewFromFloat(quantity.Float64).Mul(decimal.NewFromFloat(fmv.Float64)), true
}

// appendCapTableAt aggregates the lots held at t into ONE position per
// company: each held lot's latest delta on/before t, summed. Quantity is the share count
// (share-type lots only — an unexercised option is a different unit and would
// also double-count the certificates it became, so it stays a 0-value lot in
// the payload). MarketValue sums every held lot's per-date FMV valuation
// (collector DESIGN.md §5.1). BookValue sums every held line at its value
// when acquired: a certificate born from an exercise at its value at
// exercise where Carta states one (exerciseValue), every other line at the
// cash paid. The per-lot detail rides in the position payload, and each
// held share certificate is one open lot of the position (shareLot), whose
// own book value stays the cash paid. The book value is stamped by what it
// sums: exerciseValueBasis where a value at exercise is in it, else its
// lots' cost where every costed line is a share certificate (shareBasis),
// else the cash paid (cashPaidBasis).
func appendCapTableAt(t int64, acct string, held []lotState, batch *canonical.SnapshotBatch, active map[int64]string, classesNew map[int64]canonical.AssetClass, vehicles map[int64]canonical.Vehicle) error {
	// The sums are decimal so that a position's quantity and book value are
	// exactly the sums of its share lots'.
	type agg struct {
		ccy                         string
		shareQty, mv, book          decimal.Decimal
		hasShareQty, hasMV, hasBook bool
		atExercise                  bool  // book counts a value at exercise
		hasEquity                   bool  // any non-convertible lot (share/option/…)
		hasStockLike                bool  // any real share-settled lot (share/rsu/rsa/piu/equity_grant)
		costsOther                  bool  // a costed line that is no share certificate
		acquiredUnix                int64 // earliest date among the lines that are no share lot
		hasAcquired                 bool
		valuedAt                    int64 // the latest delta the position is built from
		lots                        []lot
		shareLots                   []canonical.PositionLotChange
	}
	aggs := make(map[int64]*agg)
	var order []int64
	for _, r := range held {
		a := aggs[r.entityID]
		if a == nil {
			a = &agg{ccy: normCcy(r.ccy)}
			aggs[r.entityID] = a
			order = append(order, r.entityID)
		}
		a.valuedAt = max(a.valuedAt, r.at)
		if r.secType != "convertible" {
			a.hasEquity = true // a SAFE/note is a convertible_note only until real equity appears
		}
		if isStockVehicleType(r.secType) {
			a.hasStockLike = true // share-settled ownership → stock vehicle (vs option/warrant/sar)
		}
		if r.secType == "share" {
			if r.quantity.Valid {
				a.shareQty = a.shareQty.Add(decimal.NewFromFloat(r.quantity.Float64))
				a.hasShareQty = true
			}
			a.shareLots = append(a.shareLots, shareLot(r.secID, r.quantity, r.cost, r.mv, r.acquired, r.exType, r.exDate, r.exFMV))
		}
		if r.mv.Valid {
			a.mv = a.mv.Add(decimal.NewFromFloat(r.mv.Float64))
			a.hasMV = true
		}
		if v, ok := exerciseValue(r.quantity, r.exFMV); ok && r.secType == "share" {
			a.book, a.hasBook, a.atExercise = a.book.Add(v), true, true
		} else if r.cost.Valid {
			a.book, a.hasBook = a.book.Add(decimal.NewFromFloat(r.cost.Float64)), true
			a.costsOther = a.costsOther || r.secType != "share"
		}
		l := lot{SecurityType: r.secType, SecurityID: r.secID, Label: r.label,
			IssueDate: r.issueDate, AcquiredOn: r.acquired}
		// A holding without a dated share lot is acquired on the EARLIEST
		// date its other held lines carry. A convertible is not re-issued
		// on a split or transfer the way a certificate is, so where Carta
		// states no acquisition date its issue date is the day it was
		// bought.
		acquired := r.acquired
		if acquired == "" && r.secType == "convertible" {
			acquired = r.issueDate
		}
		if acq, ok := flowDateUnix(acquired); ok && r.secType != "share" && (!a.hasAcquired || acq < a.acquiredUnix) {
			a.acquiredUnix, a.hasAcquired = acq, true
		}
		if r.quantity.Valid {
			v := r.quantity.Float64
			l.Quantity = &v
		}
		if r.cost.Valid {
			v := r.cost.Float64
			l.Cost = &v
		}
		if r.mv.Valid {
			v := r.mv.Float64
			l.MarketValue = &v
		}
		if r.strike.Valid {
			v := r.strike.Float64
			l.ExercisePrice = &v
		}
		a.lots = append(a.lots, l)
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
			ValuedAt:             a.valuedAt,
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
		if a.hasBook {
			basis := shareBasis
			switch {
			case a.atExercise:
				basis = exerciseValueBasis
			case a.costsOther:
				basis = cashPaidBasis
			}
			change.SetBookValue(&a.book, basis)
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

// appendFundAt emits the fund LP positions held at t: each fund's latest
// capital-account delta on/before t (one position per fund), in table order. MarketValue =
// net_asset_value (the NAV at that quarter), BookValue = capital_contributed
// less the basis distributed in kind by t (inKind), AcquisitionDate = the
// fund's first capital call. Money arrives as decimal strings, parsed exactly.
func appendFundAt(t int64, acct string, ledger fundLedger, inKind *silver.InKind, held []fundState, batch *canonical.SnapshotBatch, active map[int64]string, classesNew map[int64]canonical.AssetClass, vehicles map[int64]canonical.Vehicle) {
	for _, f := range held {
		ccy := normCcy(f.ccy)
		instKey := instrumentID(f.entityID)
		// 2-D taxonomy: a fund LP interest is private_equity exposure held via
		// the fund vehicle (TAXONOMY.md: a Carta fund investment is read as a
		// venture/PE feeder).
		change := canonical.PositionChange{
			SnapshotAt:           t,
			ValuedAt:             f.at,
			AccountExternalID:    acct,
			PositionKey:          positionKey(f.entityID),
			InstrumentExternalID: &instKey,
			AssetClass:           canonical.AssetClassPrivateEquity,
			Vehicle:              canonical.VehicleFund,
			Currency:             ccy,
			AcquisitionDate:      ledger.acquisitionDate(f.entityID),
			Payload:              json.RawMessage(f.payload),
		}
		if mv, err := canonical.NewDecimalFromString(f.nav); err == nil && f.nav != "" {
			change.MarketValue = &mv
		}
		if bv, err := canonical.NewDecimalFromString(f.contributed); err == nil && f.contributed != "" {
			book, basis, extra := inKind.BookValue(change.PositionKey, t, &bv, fundBasis)
			change.SetBookValue(book, basis)
			change.Payload = silver.PayloadWith(f.payload, extra)
		}
		batch.Positions = append(batch.Positions, change)
		active[f.entityID] = ccy
		classesNew[f.entityID] = canonical.AssetClassPrivateEquity
		vehicles[f.entityID] = canonical.VehicleFund
	}
}
