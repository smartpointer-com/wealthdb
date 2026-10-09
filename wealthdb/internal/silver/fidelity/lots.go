package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Open lots (fidelity-web migration 0011). The collector fetches a
// holding's lot table only when the holding is new or its quantity or
// cost basis total changed, and silver files the lots under the dump
// that fetched them. A holding that did not change has no lot rows
// under later dumps, so the lots of a position at snapshot X are those
// of the latest fetch at or before X.
//
// A fetch the collector deferred or that failed leaves the same gap as
// an unchanged holding. The lots are therefore carried to X only while
// they still describe the position: their quantities sum to its
// quantity, and their costs to its cost basis total where both are
// stated. Otherwise the position has no lots at X.
//
// A carried lot keeps what cannot have changed since the fetch: its
// quantity, cost and acquisition date. Its market value is the fetch
// day's, so it stays in the payload beside `fetched_at`. Its term is
// the page's statement (docs/DESIGN.md §7.4) and is kept only while
// the fetch still vouches for it (agedTerm).

// holding names one position across snapshots.
type holding struct{ account, instrument string }

// openLot is one row of a lot table as silver holds it.
type openLot struct {
	index      int
	quantity   *canonical.Decimal
	costBasis  *canonical.Decimal
	acquired   string
	term       string
	currency   string
	sourceSHA  string
	payload    string // the cells as printed
	value      sql.NullString
	unitCost   sql.NullString
	unrealized sql.NullString
	cusip      sql.NullString
}

// lotFetch is one fetch of a holding's lot table: the dump that
// fetched it, and its lots in print order.
type lotFetch struct {
	at   int64
	lots []openLot
}

// lotCarry attaches the open lots to the positions of one Snapshots
// call and counts the positions whose latest fetch no longer describes
// them.
type lotCarry struct {
	fetches map[holding][]lotFetch // ascending by at
	// stale counts positions whose latest fetch predates them and no
	// longer sums to them; mismatched, those whose own dump's fetch
	// does not.
	stale, mismatched int
}

// readOpenLots reads every open lot silver holds, once per Snapshots
// call: the fetch a position takes can predate the load window. A
// silver without the table (before migration 0011, and the svb
// silvers, which state no lots) carries none.
func (c *Connection) readOpenLots(ctx context.Context) (*lotCarry, error) {
	carry := &lotCarry{fetches: map[holding][]lotFetch{}}
	ok, err := silver.HasTables(ctx, c.db, "open_lots")
	if err != nil || !ok {
		return carry, err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT snapshot_at, account_external_id, instrument_key, lot_index, cusip,
       CAST(quantity             AS VARCHAR),
       CAST(unit_cost            AS VARCHAR),
       CAST(cost_basis           AS VARCHAR),
       COALESCE(acquired_date, ''),
       CAST(unrealized_gain_loss AS VARCHAR),
       CAST(current_value        AS VARCHAR),
       COALESCE(term, ''),
       currency, source_sha256, payload
  FROM open_lots
 ORDER BY account_external_id, instrument_key, snapshot_at, lot_index`)
	if err != nil {
		return nil, fmt.Errorf("readOpenLots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			at        int64
			h         holding
			l         openLot
			qty, cost sql.NullString
		)
		if err := rows.Scan(&at, &h.account, &h.instrument, &l.index, &l.cusip,
			&qty, &l.unitCost, &cost, &l.acquired, &l.unrealized, &l.value,
			&l.term, &l.currency, &l.sourceSHA, &l.payload); err != nil {
			return nil, err
		}
		l.quantity = silver.DecimalPtrOrNil(qty)
		l.costBasis = silver.DecimalPtrOrNil(cost)
		fs := carry.fetches[h]
		if n := len(fs); n == 0 || fs[n-1].at != at {
			fs = append(fs, lotFetch{at: at})
		}
		fs[len(fs)-1].lots = append(fs[len(fs)-1].lots, l)
		carry.fetches[h] = fs
	}
	return carry, rows.Err()
}

// latest returns the holding's latest fetch at or before t, or nil.
func (lc *lotCarry) latest(h holding, t int64) *lotFetch {
	fs := lc.fetches[h]
	i := sort.Search(len(fs), func(i int) bool { return fs[i].at > t })
	if i == 0 {
		return nil
	}
	return &fs[i-1]
}

// attach appends the open lots of p, a live position whose book value
// is its cost basis total, to batch, and dates p by its earliest lot.
// A position with no fetch at or before its snapshot, or whose latest
// fetch no longer describes it, gets none.
func (lc *lotCarry) attach(batch *canonical.SnapshotBatch, p *canonical.PositionChange) {
	f := lc.latest(holding{p.AccountExternalID, p.PositionKey}, p.SnapshotAt)
	if f == nil {
		return
	}
	if !f.describes(p.Quantity, p.BookValue) {
		if f.at == p.SnapshotAt {
			lc.mismatched++
		} else {
			lc.stale++
		}
		return
	}
	lots := make([]canonical.PositionLotChange, len(f.lots))
	for i, l := range f.lots {
		lots[i] = l.change(p, f.at)
	}
	p.AcquisitionDate = canonical.EarliestLotDate(lots)
	batch.PositionLots = append(batch.PositionLots, lots...)
}

// logSkipped reports the positions left without lots.
func (lc *lotCarry) logSkipped() {
	if lc.stale+lc.mismatched > 0 {
		log.Printf("fidelity adapter: %d position snapshot(s) carry no lots: "+
			"%d whose latest lot fetch predates a change, %d whose own dump's lots do not sum to them",
			lc.stale+lc.mismatched, lc.stale, lc.mismatched)
	}
}

// describes reports whether the fetch's lots still add up to a
// position of quantity qty and cost basis total cost: the quantities
// within a millionth of the quantity (at least 1e-6), and the costs
// within a cent per lot where the total and every lot's cost are
// stated. A position with no quantity has nothing to check against.
func (f *lotFetch) describes(qty, cost *canonical.Decimal) bool {
	if qty == nil || len(f.lots) == 0 {
		return false
	}
	var sumQty, sumCost canonical.Decimal
	costsStated := cost != nil
	for _, l := range f.lots {
		if l.quantity == nil {
			return false
		}
		sumQty = sumQty.Add(l.quantity.Abs())
		if l.costBasis == nil {
			costsStated = false
		} else {
			sumCost = sumCost.Add(*l.costBasis)
		}
	}
	scale := qty.Abs()
	if one := canonical.NewDecimalFromInt(1); scale.LessThan(one) {
		scale = one
	}
	if sumQty.Sub(qty.Abs()).Abs().GreaterThan(canonical.NewDecimalFromFloat(1e-6).Mul(scale)) {
		return false
	}
	if costsStated {
		perLot := canonical.NewDecimalFromFloat(0.01).Mul(canonical.NewDecimalFromInt(int64(len(f.lots))))
		if sumCost.Sub(*cost).Abs().GreaterThan(perLot) {
			return false
		}
	}
	return true
}

// change maps one lot of the fetch at fetchedAt onto position p. Its
// quantity takes the position's sign. The acquired date is a date only
// where the table prints one; other text stays in the payload, laid
// over the cells silver keeps. A lot carried past its fetch states no
// market value and only the term it still has; the fetch's value and
// term stay in the payload.
func (l openLot) change(p *canonical.PositionChange, fetchedAt int64) canonical.PositionLotChange {
	qty := *l.quantity
	if p.Quantity.IsNegative() != qty.IsNegative() {
		qty = qty.Neg()
	}
	extra := map[string]any{"fetched_at": fetchedAt}
	putNumber(extra, "unit_cost", l.unitCost) // a bond's is per 100 of par
	putNumber(extra, "unrealized_gain_loss", l.unrealized)
	putText(extra, "cusip", l.cusip)
	acquired := silver.ISODate(l.acquired)
	if acquired == nil && l.acquired != "" {
		extra["acquired_date"] = l.acquired
	}
	value, term := silver.DecimalPtrOrNil(l.value), canonical.ParseLotTerm(l.term)
	if fetchedAt != p.SnapshotAt {
		putNumber(extra, "current_value", l.value)
		if l.term != "" {
			extra["term"] = l.term
		}
		value, term = nil, agedTerm(term, acquired, p.SnapshotAt)
	}
	lot := canonical.PositionLotChange{
		SnapshotAt:           p.SnapshotAt,
		AccountExternalID:    p.AccountExternalID,
		PositionKey:          p.PositionKey,
		LotKey:               strconv.Itoa(l.index),
		InstrumentExternalID: p.InstrumentExternalID,
		Currency:             l.currency,
		Quantity:             &qty,
		MarketValue:          value,
		AcquisitionDate:      acquired,
		Term:                 term,
		// The source document is the lot table's API response, by its
		// sha256: the positions page serves it, no PDF states it.
		SourceDocument: silver.StrPtrIfNonEmpty(l.sourceSHA),
		Payload:        silver.PayloadWith(l.payload, extra),
	}
	lot.SetBookValue(l.costBasis, lotBasis.Origin)
	return lot
}

// agedTerm is the term a lot the page stated as t still has at
// snapshot at, a time after the fetch. A long lot stays long. A short
// one stays short only while a year from its acquisition date is still
// ahead; past that, or with no date to tell, the fetch no longer
// vouches for a term and none is stated.
func agedTerm(t canonical.LotTerm, acquired *time.Time, at int64) canonical.LotTerm {
	switch {
	case t == canonical.LotTermLong:
		return t
	case t == canonical.LotTermShort && acquired != nil &&
		acquired.AddDate(1, 0, 0).After(time.Unix(at, 0).UTC()):
		return t
	}
	return ""
}

// putNumber sets a payload key to a numeric silver column, as printed,
// unless it is NULL.
func putNumber(m map[string]any, key string, v sql.NullString) {
	if v.Valid && v.String != "" {
		m[key] = json.Number(v.String)
	}
}

// putText sets a payload key to a text silver column unless it is NULL
// or empty.
func putText(m map[string]any, key string, v sql.NullString) {
	if v.Valid && v.String != "" {
		m[key] = v.String
	}
}
