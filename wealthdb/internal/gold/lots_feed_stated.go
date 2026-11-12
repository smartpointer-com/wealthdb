package gold

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// readRealized gives each sale the realized lots its source states for
// it (docs/LOTS.md §5.3), where a seed it relieves takes their cost and
// date (§4): the lots that name its instrument and fall within two days
// of it, up to its quantity, on the sale's own account first, then on
// the other accounts of its portfolio. A document's CUSIP counts as the
// ticker the trades use where the source's instruments link the two and
// no key uses the CUSIP itself (lotAliases).
func (f *lotFeed) readRealized(ctx context.Context, db *sql.DB) error {
	stated, err := f.readStatedLots(ctx, db)
	if err != nil {
		return err
	}
	// The sales come in time order, so a group's lots claimed in full
	// before the current sale's window stay behind a cursor.
	claimed := map[lotStatedGroup]int{}
	for i := range f.events {
		ev := &f.events[i]
		if ev.Kind != lots.Dispose || ev.Disposal != lots.DisposeSell {
			continue
		}
		info := f.info[ev.Key]
		port := f.portOf(info.src.id, ev.Account)
		// The group is in date order: start at the first lot two days
		// before the sale, stop past two days after.
		day := lots.DayOf(ev.At)
		g := lotStatedGroup{info.src.id, port, info.inst}
		group := stated[g]
		c := claimed[g]
		for c < len(group) && group[c].left <= 0 {
			c++
		}
		claimed[g] = c
		j, _ := slices.BinarySearchFunc(group[c:], day-2, func(s *lotStated, d int32) int { return cmp.Compare(s.day, d) })
		j += c
		need := ev.Qty
		for _, own := range []bool{true, false} {
			for _, s := range group[j:] {
				if s.day > day+2 || need <= lots.Tolerance(ev.Qty) {
					break
				}
				if s.left <= 0 || (s.acct == ev.Account) != own {
					continue
				}
				q := math.Min(need, s.left)
				part := s.lot
				part.Qty, part.Cost = q, s.lot.Cost*q/s.lot.Qty
				ev.Lots = append(ev.Lots, part)
				s.left -= q
				need -= q
			}
		}
	}
	return nil
}

// lotStated is a stated primary realized lot, and what of it the sales
// have not yet claimed.
type lotStated struct {
	acct string
	day  int32
	lot  lots.StatedLot
	left float64
}

type lotStatedGroup struct{ src, port, inst string }

// readStatedLots reads the stated primary realized lots of the
// replayed sources, grouped by source, port and instrument, each group
// in date order.
func (f *lotFeed) readStatedLots(ctx context.Context, db *sql.DB) (map[lotStatedGroup][]*lotStated, error) {
	alias, err := f.lotAliases(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, account_external_id, lot_key(instrument_external_id, instrument_hint, description),
       CAST(epoch(realized_effective_date(disposal_date, settlement_date, tax_year)) AS BIGINT),
       CAST(quantity AS DOUBLE), CAST(book_value AS DOUBLE), CAST(epoch(acquisition_date) AS BIGINT)
  FROM realized_lots
 WHERE is_primary AND quantity <> 0 AND `+inLotSources("silver_source_id")+`
 ORDER BY 1, 2, 3, 4, acquisition_date, realized_lot_external_id`, f.ids)
	if err != nil {
		return nil, fmt.Errorf("lots: read realized lots: %w", err)
	}
	defer rows.Close()
	// The ids the keys use: a stated id that is one stays as it is.
	keyed := make(map[[2]string]bool, len(f.info))
	for _, info := range f.info {
		keyed[[2]string{info.src.id, info.inst}] = true
	}
	out := map[lotStatedGroup][]*lotStated{}
	for rows.Next() {
		var (
			src, acct, inst string
			at              int64
			qty             float64
			cost            sql.NullFloat64
			acq             sql.NullInt64
		)
		if err := rows.Scan(&src, &acct, &inst, &at, &qty, &cost, &acq); err != nil {
			return nil, err
		}
		if t, ok := alias[[2]string{src, inst}]; ok && !keyed[[2]string{src, inst}] {
			inst = t
		}
		sl := statedLot(qty, cost.Float64, cost.Valid, acq)
		g := lotStatedGroup{src, f.portOf(src, acct), inst}
		out[g] = append(out[g], &lotStated{acct: acct, day: lots.DayOf(at), lot: sl, left: sl.Qty})
	}
	for _, g := range out {
		slices.SortStableFunc(g, func(a, b *lotStated) int { return cmp.Compare(a.day, b.day) })
	}
	return out, rows.Err()
}

// statedLot is a lot a source states, from its quantity, cost and
// acquisition time; an absent cost or date is unknown.
func statedLot(qty, cost float64, costKnown bool, acq sql.NullInt64) lots.StatedLot {
	sl := lots.StatedLot{Qty: math.Abs(qty), Cost: math.Abs(cost), CostKnown: costKnown, AcqDay: lots.NoDay}
	if acq.Valid {
		sl.AcqDay = lots.DayOf(acq.Int64)
	}
	return sl
}

// lotAliases maps an instrument a source keys by its CUSIP to the ticker
// the source also keys it by (instrument_aliases): a
// statement names a security by CUSIP and the trades by ticker.
func (f *lotFeed) lotAliases(ctx context.Context, db *sql.DB) (map[[2]string]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT src, alias, k FROM instrument_aliases() WHERE `+inLotSources("src"), f.ids)
	if err != nil {
		return nil, fmt.Errorf("lots: read instrument aliases: %w", err)
	}
	defer rows.Close()
	out := map[[2]string]string{}
	for rows.Next() {
		var src, id, ticker string
		if err := rows.Scan(&src, &id, &ticker); err != nil {
			return nil, err
		}
		out[[2]string{src, id}] = ticker
	}
	return out, rows.Err()
}

// portOf is the account's portfolio, or the account itself outside
// any: what a stated lot and a sale must share.
func (f *lotFeed) portOf(src, acct string) string {
	if p := f.pid[[2]string{src, acct}]; p != "" {
		return "p:" + p
	}
	return "a:" + acct
}

// readAnchors turns the lots a source states at a snapshot into
// anchors, one per position with lots. A pooled key has no anchor: a
// wallet's lots do not describe the pool.
func (f *lotFeed) readAnchors(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT l.silver_source_id, l.snapshot_at, lot_obs_at(l.snapshot_at), l.account_external_id,
       holding_key(p.instrument_external_id, l.position_key), p.instrument_external_id IS NULL,
       CAST(l.quantity AS DOUBLE), CAST(l.book_value AS DOUBLE), CAST(epoch(l.acquisition_date) AS BIGINT)
  FROM position_lots l
  JOIN positions p ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snapshot_at
   AND p.account_external_id = l.account_external_id AND p.position_key = l.position_key
 WHERE `+inLotSources("l.silver_source_id")+`
 ORDER BY 1, 2, 4, 5, l.acquisition_date, l.lot_key`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read anchors: %w", err)
	}
	defer rows.Close()
	var (
		cur  lots.Event
		have bool
		bad  bool
	)
	emit := func() {
		if have && !bad && len(cur.Lots) > 0 {
			f.add(cur, lotAux{})
		}
		have, bad = false, false
	}
	for rows.Next() {
		var (
			src, acct, inst string
			snap, obsAt     int64
			isHint          bool
			qty, cost       sql.NullFloat64
			acq             sql.NullInt64
		)
		if err := rows.Scan(&src, &snap, &obsAt, &acct, &inst, &isHint, &qty, &cost, &acq); err != nil {
			return err
		}
		s := f.sources[src]
		if s.pol.Grain == lots.GrainPortfolio {
			continue
		}
		k := f.key(s, acct, inst, isHint)
		if k < 0 {
			continue
		}
		if !have || cur.Key != k || cur.At != obsAt {
			emit()
			cur = lots.Event{At: obsAt, Kind: lots.Anchor, Key: k, Txn: fmt.Sprintf("anchor:%d", snap), Account: acct}
			have = true
		}
		if !qty.Valid || qty.Float64 <= 0 {
			// A lot without a positive quantity cannot stand for the
			// position: the anchor is incomplete, and is not adopted.
			bad = true
			continue
		}
		cur.Lots = append(cur.Lots, statedLot(qty.Float64, cost.Float64, cost.Valid, acq))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	emit()
	return nil
}
