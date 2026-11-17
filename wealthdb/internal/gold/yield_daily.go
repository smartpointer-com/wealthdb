package gold

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"math"
	"slices"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// readAmounts reads each position's amount on each day of the twelve
// months to the instant, one yield.Daily per currency it is valued in,
// and the instrument it names: a position as positions_at reads it on the
// day, a cash balance as cash_chosen does, a day it was not held as zero.
// Both read a day at its last second, the instant's own day at the instant,
// and take each account's active snapshot at it (pos_active_at,
// cash_active_at). So an observation's amounts hold over its span
// (pos_key_spans, cash_key_spans), from the first day read at or after it to
// the first day read at or after its end, and one read of the observations
// whose span meets the window gives every day.
func (b *yieldBase) readAmounts(ctx context.Context, db querier) error {
	w := newDayWindow(b.in)
	b.amounts, b.instrument = map[amountKey]*yield.Daily{}, map[yield.HoldingKey]string{}
	if err := w.readSpans(ctx, db, `
SELECT p.silver_source_id, p.snapshot_at, s.until, p.account_external_id, p.position_key,
       MAX(COALESCE(p.instrument_external_id, '')), MAX(COALESCE(p.currency, '')), SUM(p.market_value)::DOUBLE
  FROM positions p
  JOIN pos_key_spans() s ON s.src = p.silver_source_id AND s.acct = p.account_external_id
   AND s.snap = p.snapshot_at
 WHERE p.snapshot_at <= $1 AND (s.until IS NULL OR s.until > $2)
 GROUP BY ALL
 ORDER BY 1, 2`, b.amounts, b.instrument); err != nil {
		return err
	}
	return w.readSpans(ctx, db, `
SELECT cb.silver_source_id, cb.snapshot_at, s.until, cb.account_external_id, 'cash:' || cb.currency, '',
       cb.currency, cb.amount::DOUBLE
  FROM cash_balances cb
  JOIN cash_key_spans() s ON s.src = cb.silver_source_id AND s.acct = cb.account_external_id
   AND s.ccy = cb.currency AND s.snap = cb.snapshot_at
 WHERE cb.snapshot_at <= $1 AND (s.until IS NULL OR s.until > $2)
QUALIFY row_number() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                           ORDER BY `+cashKindRank+`, cb.balance_kind) = 1
 ORDER BY 1, 2`, b.amounts, b.instrument)
}

// cashKindRank orders a snapshot's balances of one account and currency
// as cash_chosen does: the current balance first, then the closing one,
// and so on.
const cashKindRank = `CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                   WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7
                   ELSE 99 END`

// dayWindow is the days of the twelve months to an instant, each read at
// its last second, the instant's own day at the instant.
type dayWindow struct {
	// first is the first day's last second.
	first, asOf int64
	days        int
}

func newDayWindow(in *yield.Inputs) dayWindow {
	start := in.Day.AddDate(-1, 0, 0).AddDate(0, 0, 1)
	return dayWindow{first: start.Unix() + 86399, asOf: in.AsOf, days: int(in.Day.Sub(start).Hours()/24) + 1}
}

// instant is the instant day i of the window is read at.
func (w dayWindow) instant(i int) int64 {
	if i == w.days-1 {
		return w.asOf
	}
	return w.first + int64(i)*SecondsPerDay
}

// from is the first day of the window an observation taken at at holds
// on: the first day read at or after it.
func (w dayWindow) from(at int64) int {
	return sort.Search(w.days, func(i int) bool { return w.instant(i) >= at })
}

// amountKey names a position's amounts in one currency.
type amountKey struct {
	yield.HoldingKey
	ccy string
}

// readSpans adds to out the amounts q reads: every holding's amount in
// each observation whose span meets the window, the span's end NULL while
// it holds on, ordered by source and observation, beside the instrument
// it names ("" for none), which instrument records, and its currency. q
// takes the instant and the first day's instant.
func (w dayWindow) readSpans(ctx context.Context, db querier, q string, out map[amountKey]*yield.Daily,
	instrument map[yield.HoldingKey]string) error {
	return scanAll(ctx, db, q, func(rows *sql.Rows) error {
		var k yield.HoldingKey
		var at int64
		var until sql.NullInt64
		var iid, ccy string
		var v sql.NullFloat64
		if err := rows.Scan(&k.Source, &at, &until, &k.Account, &k.PositionKey, &iid, &ccy, &v); err != nil {
			return err
		}
		if iid != "" {
			instrument[k] = iid
		}
		from, to := w.from(at), w.days
		if until.Valid {
			to = w.from(until.Int64)
		}
		if a := (amountKey{k, ccy}); from < to && v.Float64 != 0 {
			out[a] = addSpan(out[a], w.days, ccy, yield.Span{From: from, To: to, Amount: v.Float64})
		}
		return nil
	}, w.asOf, w.first)
}

// addSpan adds a span to a holding's daily amounts in ccy; one that goes
// on from the last at its amount extends it.
func addSpan(d *yield.Daily, days int, ccy string, s yield.Span) *yield.Daily {
	if d == nil {
		d = &yield.Daily{Days: days, Currency: ccy}
	}
	if n := len(d.Spans); n > 0 && d.Spans[n-1].To == s.From && d.Spans[n-1].Amount == s.Amount {
		d.Spans[n-1].To = s.To
		return d
	}
	d.Spans = append(d.Spans, s)
	return d
}

// daily gives each holding of in its daily amounts, in its own currency,
// and each instrument a holding pools (yield.Inputs.FormerDaily) the
// amounts of its positions that are no holding of in, one Daily per
// currency: an instrument held, by the id the matching folds it into, or
// one of a security held, at any source. A holding's amounts in another
// currency than its own, as a statement and a feed may value one
// position, convert at the instant's rates; where none converts them they
// count nothing.
func (b *yieldBase) daily(in *yield.Inputs) {
	own, held, securities := map[yield.HoldingKey]string{}, map[yield.InstrumentKey]bool{}, map[string]bool{}
	for _, h := range in.Holdings {
		own[h.HoldingKey] = h.Currency
		if h.Instrument != "" {
			held[yield.InstrumentKey{Source: h.Source, Instrument: in.Fold(h.Source, h.Account, h.Instrument)}] = true
		}
		if h.SecurityID != "" {
			securities[h.SecurityID] = true
		}
	}
	keys := slices.SortedFunc(maps.Keys(b.amounts), func(x, y amountKey) int {
		return cmp.Or(cmp.Compare(x.Source, y.Source), cmp.Compare(x.Account, y.Account),
			cmp.Compare(x.PositionKey, y.PositionKey), cmp.Compare(x.ccy, y.ccy))
	})
	for _, k := range keys {
		a := b.amounts[k]
		if ccy, ok := own[k.HoldingKey]; ok {
			rate := 1.0
			if k.ccy != ccy && k.ccy != "" && ccy != "" {
				if rate = in.FXOf(k.ccy) / in.FXOf(ccy); math.IsNaN(rate) || math.IsInf(rate, 0) || rate == 0 {
					continue
				}
			}
			d := in.Daily[k.HoldingKey]
			d.Days, d.Currency = a.Days, ccy
			for _, s := range a.Spans {
				d.Spans = append(d.Spans, yield.Span{From: s.From, To: s.To, Amount: s.Amount * rate})
			}
			in.Daily[k.HoldingKey] = d
			continue
		}
		iid := b.instrument[k.HoldingKey]
		if iid == "" {
			continue
		}
		ik := yield.InstrumentKey{Source: k.Source, Instrument: in.Fold(k.Source, k.Account, iid)}
		if !held[ik] && !securities[in.HeldSecurity[ik]] {
			continue
		}
		ds := in.FormerDaily[ik]
		i := slices.IndexFunc(ds, func(d yield.Daily) bool { return d.Currency == a.Currency })
		if i < 0 {
			i, ds = len(ds), append(ds, yield.Daily{Days: a.Days, Currency: a.Currency})
		}
		ds[i].Spans = append(ds[i].Spans, a.Spans...)
		in.FormerDaily[ik] = ds
	}
}
