package gold

import (
	"cmp"
	"context"
	"database/sql"
	"math"
	"slices"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The quantity a dividend was paid on (docs/YIELD.md §4): what its
// account held of its instrument as the day its record day reads began
// (yield.RecordOf: the ex day where the dividend calendar gives the
// days, else the record day of its market's lag), where one is known
// and the account held some then, else as its pay day began, in the
// newest snapshot that speaks for the account (unitAccountsCTE), moved
// by the trades of its key since, in the instrument's units at the
// instant. An instrument goes by the id the matching folds it into
// (yield.Inputs.Fold), under the account that holds or trades it. A
// trade booked on a cash account that holds none of its instrument
// moves the holding of the one account of its portfolio that does
// (yield.Holders.TradeKey): a bank books a trade there, beside the
// custody account.

// unitKey names an instrument held in one account of a source
// (yield.UnitKey).
type unitKey = yield.UnitKey

// unitChange is a change of an instrument's units: from the snapshot at
// on, each old unit is k new ones. ok is false where no whole factor
// explains the change; k is then 1.
type unitChange struct {
	at int64
	k  float64
	ok bool
}

// yieldUnits are the quantities of the instruments paid, in each account
// that held them, at the points they change, and the accounts that held
// each instrument (yield.Holders); the trades that move each (unitMove,
// yield.Holders.TradeKey); the snapshots that speak for each account; the
// changes of each instrument's units; the spans of snapshots over which
// some account held each with its units read (cover); and the last
// snapshot of each source. late holds what after found.
type yieldUnits struct {
	*yield.Holders
	trades  map[unitKey][]yield.UnitTrade
	snaps   map[accountKey][]int64
	late    map[unitKey]map[int64]bool
	changes map[yield.InstrumentKey][]unitChange
	read    map[yield.InstrumentKey][][2]int64
	last    map[string]int64
	// instants are the snapshots of each source that speak for some
	// account, sorted.
	instants map[string][]int64
}

// accountKey names an account of a source.
type accountKey struct{ src, acct string }

// unitWindowCTE are the instruments $1 and $2 name (one source and one
// id a pair), by the id the matching folds an instrument into (paid); and
// the snapshots of their sources that hold on some day from $3 to $4: from
// the oldest of the accounts' active snapshots at $3 (pos_active_at),
// else the first, every one to $4.
const unitWindowCTE = `
WITH paid AS (SELECT DISTINCT unnest($1::VARCHAR[]) AS src, unnest($2::VARCHAR[]) AS k),
snaps AS (
    SELECT DISTINCT silver_source_id, snapshot_at FROM positions
     WHERE snapshot_at <= $4 AND silver_source_id IN (SELECT src FROM paid)),
active AS (SELECT src, MIN(snap) AS snap FROM pos_active_at([$3::BIGINT]) GROUP BY ALL),
first AS (
    SELECT s.silver_source_id, COALESCE(MIN(a.snap), MIN(s.snapshot_at)) AS first_at
      FROM snaps s LEFT JOIN active a ON a.src = s.silver_source_id
     GROUP BY ALL),
window_snaps AS (
    SELECT s.* FROM snaps s JOIN first f ON f.silver_source_id = s.silver_source_id
     WHERE s.snapshot_at >= f.first_at)`

// unitAccountsCTE are the snapshots that speak for each account of the
// sources (account_grid): those that name it, by a position or a cash
// balance, and every one from the end of its positions' last
// observation on (pos_key_spans), where it holds
// nothing. A snapshot of only some accounts, such as a statement's, says
// nothing of the others.
const unitAccountsCTE = `,
account_snaps AS (
    SELECT DISTINCT x.silver_source_id, x.account_external_id, x.snapshot_at
      FROM (SELECT silver_source_id, account_external_id, snapshot_at FROM positions
            UNION ALL
            SELECT silver_source_id, account_external_id, snapshot_at FROM cash_balances) x
      JOIN window_snaps s ON s.silver_source_id = x.silver_source_id AND s.snapshot_at = x.snapshot_at),
account_ends AS (
    SELECT src, acct, until AS ended_at FROM pos_key_spans()
     WHERE src IN (SELECT src FROM paid)
   QUALIFY snap = MAX(snap) OVER (PARTITION BY src, acct)),
account_grid AS (
    SELECT l.silver_source_id, l.account_external_id, s.snapshot_at
      FROM (SELECT DISTINCT silver_source_id, account_external_id FROM account_snaps) l
      JOIN window_snaps s ON s.silver_source_id = l.silver_source_id
      LEFT JOIN account_snaps a ON a.silver_source_id = l.silver_source_id
           AND a.account_external_id = l.account_external_id AND a.snapshot_at = s.snapshot_at
      LEFT JOIN account_ends e ON e.src = l.silver_source_id AND e.acct = l.account_external_id
     WHERE a.snapshot_at IS NOT NULL OR s.snapshot_at >= e.ended_at)`

// readYieldUnits reads the units of the instruments paid, by the ids
// the matching folds them into, from from to asOf. Each account's
// quantity of each is the sum of its rows whose instrument its aliases
// fold into it, as holdingAliasJoins reads a holding's key (a CUSIP id
// and its ticker, say), in each snapshot that speaks for the account
// (unitAccountsCTE), the account holding none where they are absent:
// the points it changes at (yield.UnitPoint). Between two such
// snapshots, the key's trades, read as the source kind's lot policy
// reads them (lots.PolicyFor) and keyed by yield.Holders.TradeKey, and
// its quantity and value tell a change of units
// (yield.UnitStep.Change). Every account's changes are the instrument's
// (addChange).
func readYieldUnits(ctx context.Context, db querier, in *yield.Inputs, paid map[yield.InstrumentKey]bool,
	from, asOf int64) (*yieldUnits, error) {
	u := &yieldUnits{Holders: &yield.Holders{In: in, Points: map[unitKey][]yield.UnitPoint{},
		Accounts: map[yield.InstrumentKey][]string{}}, snaps: map[accountKey][]int64{},
		late: map[unitKey]map[int64]bool{}, changes: map[yield.InstrumentKey][]unitChange{},
		read: map[yield.InstrumentKey][][2]int64{}, last: map[string]int64{}, instants: map[string][]int64{}}
	if len(paid) == 0 {
		return u, nil
	}
	var srcs, iids []string
	for k := range paid {
		srcs, iids = append(srcs, k.Source), append(iids, k.Instrument)
	}
	type stepAt struct {
		key      unitKey
		prev, at int64
		step     yield.UnitStep
	}
	var steps []stepAt
	if err := scanAll(ctx, db, unitWindowCTE+unitAccountsCTE+`,
folded AS (
    SELECT p.silver_source_id, p.account_external_id, p.snapshot_at, p.quantity, p.market_value,
           COALESCE(ha.to_key, hd.to_key, ia.k, p.instrument_external_id) AS k
      FROM positions p`+holdingAliasJoins("p.silver_source_id", "p.account_external_id", "p.instrument_external_id",
		"p.position_key")+`
     WHERE p.instrument_external_id IS NOT NULL AND p.snapshot_at <= $4
       AND p.silver_source_id IN (SELECT src FROM paid)),
held AS (
    SELECT p.silver_source_id, p.account_external_id, p.k AS instrument_external_id, p.snapshot_at,
           CASE WHEN bool_or(p.quantity IS NULL) THEN NULL ELSE SUM(p.quantity) END::DOUBLE AS qty,
           SUM(p.market_value)::DOUBLE AS value
      FROM folded p
      JOIN paid ON paid.src = p.silver_source_id AND paid.k = p.k
      JOIN window_snaps s ON s.silver_source_id = p.silver_source_id AND s.snapshot_at = p.snapshot_at
     GROUP BY ALL),
keys AS (SELECT DISTINCT silver_source_id, account_external_id, instrument_external_id FROM held),
grid AS (
    SELECT k.silver_source_id, k.account_external_id, k.instrument_external_id, s.snapshot_at,
           CASE WHEN h.snapshot_at IS NULL THEN 0 ELSE h.qty END AS qty, h.value
      FROM keys k
      JOIN account_grid s ON s.silver_source_id = k.silver_source_id
           AND s.account_external_id = k.account_external_id
      LEFT JOIN held h ON h.silver_source_id = k.silver_source_id AND h.account_external_id = k.account_external_id
           AND h.instrument_external_id = k.instrument_external_id AND h.snapshot_at = s.snapshot_at),
steps AS (
    SELECT *, lag(snapshot_at) OVER w AS prev_at, lag(qty) OVER w AS prev_qty, lag(value) OVER w AS prev_value,
           row_number() OVER w AS i, MAX(snapshot_at) OVER (PARTITION BY silver_source_id) AS last_at
      FROM grid
    WINDOW w AS (PARTITION BY silver_source_id, account_external_id, instrument_external_id ORDER BY snapshot_at))
SELECT silver_source_id, account_external_id, instrument_external_id, snapshot_at, qty, value, prev_at, prev_qty,
       prev_value, last_at
  FROM steps
 WHERE i = 1 OR qty IS DISTINCT FROM prev_qty
 ORDER BY 1, 2, 3, 4`, func(rows *sql.Rows) error {
		var k unitKey
		var at, last int64
		var prev sql.NullInt64
		var qty, value, prevQty, prevValue sql.NullFloat64
		if err := rows.Scan(&k.Source, &k.Account, &k.Instrument, &at, &qty, &value, &prev, &prevQty, &prevValue,
			&last); err != nil {
			return err
		}
		if _, seen := u.Points[k]; !seen {
			ik := yield.InstrumentKey{Source: k.Source, Instrument: k.Instrument}
			u.Accounts[ik] = append(u.Accounts[ik], k.Account)
		}
		u.Points[k] = append(u.Points[k], yield.UnitPoint{At: at, Prev: prev.Int64, Qty: nullFloatToPtr(qty)})
		u.last[k.Source] = last
		if prev.Valid && prevQty.Valid && qty.Valid {
			steps = append(steps, stepAt{key: k, prev: prev.Int64, at: at, step: yield.UnitStep{Before: prevQty.Float64,
				After: qty.Float64, ValueBefore: nullFloatToPtr(prevValue), ValueAfter: nullFloatToPtr(value)}})
		}
		return nil
	}, srcs, iids, from, asOf); err != nil {
		return nil, err
	}
	if err := scanAll(ctx, db, unitWindowCTE+unitAccountsCTE+`
SELECT silver_source_id, account_external_id, snapshot_at FROM account_grid ORDER BY 1, 2, 3`,
		func(rows *sql.Rows) error {
			var a accountKey
			var at int64
			if err := rows.Scan(&a.src, &a.acct, &at); err != nil {
				return err
			}
			u.snaps[a] = append(u.snaps[a], at)
			u.instants[a.src] = append(u.instants[a.src], at)
			return nil
		}, srcs, iids, from, asOf); err != nil {
		return nil, err
	}
	for src, at := range u.instants {
		slices.Sort(at)
		u.instants[src] = slices.Compact(at)
	}
	booked, err := readUnitTrades(ctx, db, srcs, iids, from, asOf)
	if err != nil {
		return nil, err
	}
	trades := map[unitKey][]yield.UnitTrade{}
	for _, b := range booked {
		k := u.TradeKey(b.key, b.iid, b.trade)
		trades[k] = append(trades[k], b.trade)
	}
	u.trades = trades
	for _, s := range steps {
		for _, t := range trades[s.key] {
			if (t.At > s.prev && t.At < s.at) || (t.At == s.prev && u.after(s.key, s.prev)) ||
				(t.At == s.at && !u.after(s.key, s.at)) {
				s.step.Trades = append(s.step.Trades, t)
			}
		}
		if k, ok := s.step.Change(); k != 1 || !ok {
			u.addChange(yield.InstrumentKey{Source: s.key.Source, Instrument: s.key.Instrument}, unitChange{at: s.at, k: k, ok: ok})
		}
	}
	for ik := range u.changes {
		slices.SortFunc(u.changes[ik], func(a, b unitChange) int { return cmp.Compare(a.at, b.at) })
	}
	u.cover()
	return u, nil
}

// cover gives each instrument the spans of snapshots over which some
// account held it, each step between them read for a change of units:
// from a point the account held some of it to the last snapshot before
// it held none or stated none, else to its source's last snapshot. The
// spans of every account of an instrument merge where they meet.
func (u *yieldUnits) cover() {
	holds := func(p yield.UnitPoint) bool { return p.Qty != nil && *p.Qty > lots.Tolerance(*p.Qty) }
	for k, ps := range u.Points {
		ik := yield.InstrumentKey{Source: k.Source, Instrument: k.Instrument}
		for j := 0; j < len(ps); j++ {
			if !holds(ps[j]) {
				continue
			}
			from := ps[j].At
			for j+1 < len(ps) && holds(ps[j+1]) {
				j++
			}
			to := u.last[k.Source]
			if j+1 < len(ps) {
				to = ps[j+1].Prev
			}
			u.read[ik] = append(u.read[ik], [2]int64{from, to})
		}
	}
	for ik, spans := range u.read {
		slices.SortFunc(spans, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
		merged := spans[:1]
		for _, s := range spans[1:] {
			if n := len(merged) - 1; s[0] <= merged[n][1] {
				merged[n][1] = max(merged[n][1], s[1])
			} else {
				merged = append(merged, s)
			}
		}
		u.read[ik] = merged
	}
}

// covered says some account held instrument ik with its units read from
// a snapshot at or before end to its source's last: a change of units
// since end, had there been one, shows.
func (u *yieldUnits) covered(ik yield.InstrumentKey, end int64) bool {
	return slices.ContainsFunc(u.read[ik], func(s [2]int64) bool { return s[0] <= end && s[1] >= u.last[ik.Source] })
}

// bookedTrade is a trade (unitMove) under the key of the account it is
// booked on, and the id it names its instrument by.
type bookedTrade struct {
	key   unitKey
	iid   string
	trade yield.UnitTrade
}

// readUnitTrades reads the transactions that move a quantity over the
// snapshots readYieldUnits reads, oldest first (unitMove), each under its
// booking account's key, by the id that account's aliases fold its
// instrument into. One stamped at a snapshot's instant counts in it or
// after it, as the snapshot shows (after).
func readUnitTrades(ctx context.Context, db querier, srcs, iids []string, from, asOf int64) ([]bookedTrade,
	error) {
	var out []bookedTrade
	err := scanAll(ctx, db, unitWindowCTE+`,
folded AS (
    SELECT t.*, COALESCE(ha.to_key, hd.to_key, ia.k, t.instrument_external_id) AS k
      FROM transactions t`+holdingAliasJoins("t.silver_source_id", "t.account_external_id", "t.instrument_external_id",
		"NULL")+`
     WHERE t.instrument_external_id IS NOT NULL AND t.quantity <> 0 AND t.occurred_at <= $4
       AND t.silver_source_id IN (SELECT src FROM paid)),
-- A trade under a paid id, or under an id an alias of any account folds
-- into one: the account that holds it may fold it (Holders.TradeKey).
named AS (
    SELECT src, k AS id FROM paid
    UNION SELECT h.silver_source_id, h.from_key FROM holding_aliases h
            JOIN paid ON paid.src = h.silver_source_id AND paid.k = h.to_key
    UNION SELECT ia.src, ia.alias FROM instrument_aliases() ia
            JOIN paid ON paid.src = ia.src AND paid.k = ia.k)
SELECT t.silver_source_id, t.account_external_id, t.k, t.instrument_external_id, t.occurred_at,
       t.kind, CAST(t.quantity AS DOUBLE), COALESCE(t.action, '')
  FROM folded t
  JOIN first f ON f.silver_source_id = t.silver_source_id
 WHERE t.occurred_at >= f.first_at
   AND EXISTS (SELECT 1 FROM named n WHERE n.src = t.silver_source_id
                  AND (n.id = t.k OR n.id = t.instrument_external_id))
 ORDER BY t.occurred_at, t.silver_source_id, t.transaction_external_id`, func(rows *sql.Rows) error {
		var k unitKey
		var iid string
		var at int64
		var kind string
		var qty float64
		var action canonical.TxAction
		if err := rows.Scan(&k.Source, &k.Account, &k.Instrument, &iid, &at, &kind, &qty, &action); err != nil {
			return err
		}
		if move, corp := unitMove(lotClassify(kind, qty, action), qty); move != 0 {
			out = append(out, bookedTrade{key: k, iid: iid, trade: yield.UnitTrade{At: at, Qty: move, Corp: corp}})
		}
		return nil
	}, srcs, iids, from, asOf)
	return out, err
}

// unitMove is what a transaction does to its key's quantity: a trade
// moves it by move; a corporate action's leg (corp) delivers or takes
// back units, as a change of units does. Cash in lieu and an expiry that
// name a quantity take it away as a sale does.
func unitMove(c lots.Classified, qty float64) (move float64, corp bool) {
	q := math.Abs(qty)
	switch c.Action {
	case lots.Buy, lots.In, lots.Income, lots.Acquired:
		return q, false
	case lots.Sell, lots.Out, lots.Exchange, lots.Gone:
		return -q, false
	case lots.Corporate:
		switch c.Rule {
		case lots.CorpReorg:
			return qty, true
		case lots.CorpCashInLieu, lots.CorpExpiry:
			if qty < 0 {
				return qty, false
			}
		}
	}
	return 0, false
}

// addChange adds a change of an instrument's units that one account
// shows. The accounts that show one at the same snapshot agree, or the
// change explains nothing.
func (u *yieldUnits) addChange(ik yield.InstrumentKey, c unitChange) {
	cs := u.changes[ik]
	for i := range cs {
		if cs[i].at == c.at {
			if !cs[i].ok || !c.ok || math.Abs(cs[i].k-c.k) > 1e-9*c.k {
				cs[i] = unitChange{at: c.at, k: 1}
			}
			return
		}
	}
	u.changes[ik] = append(cs, c)
}

// quantity is what the accounts of keys (yield.Holders.KeysOf) held of their
// instrument as end began, in the instrument's units at the instant
// (heldOn). Of a portfolio's accounts, one first seen after end held
// none. nil where none is known, or none was held. units says it may be
// in end's units: a change of units since that no whole factor explains,
// or a span since end over which no account held the instrument with its
// units read (covered).
func (u *yieldUnits) quantity(keys []unitKey, end int64) (qty *float64, units bool) {
	held, known, units := u.holding(keys, end)
	if !known || held <= 0 {
		return nil, false
	}
	return f64(held), units
}

// holding is what quantity reads, 0 where the accounts held none; known
// is false where none is known: before an account's first snapshot, or
// where a snapshot states none.
func (u *yieldUnits) holding(keys []unitKey, end int64) (held float64, known, units bool) {
	for _, k := range keys {
		if snaps := u.snaps[accountKey{k.Source, k.Account}]; len(keys) > 1 && (len(snaps) == 0 || snaps[0] > end) {
			continue
		}
		q := u.heldOn(k, end)
		if q == nil {
			return 0, false, false
		}
		held, known = held+*q, true
	}
	if !known || held <= 0 {
		return 0, known, false
	}
	ik := yield.InstrumentKey{Source: keys[0].Source, Instrument: keys[0].Instrument}
	k, ok := 1.0, true
	for _, c := range u.changes[ik] {
		if c.at > end {
			k *= c.k
			ok = ok && c.ok
		}
	}
	return held * k, true, !ok || !u.covered(ik, end)
}

// span is the least and the most the accounts of keys held of their
// instrument as each instant from from to to began (holding), where they
// held some: at from and at to, and after each of their snapshots and
// trades between. To is the instant Record reads, where the window is
// that day alone or a lag's window ends on its record day, so a snapshot
// stamped at it counts, as in Record. A day they held none is no record day of a payment received.
// The least is 0 where some of the span is unknown.
func (u *yieldUnits) span(keys []unitKey, from, to int64) (least, most *float64) {
	at := []int64{from, to}
	for _, k := range keys {
		for _, s := range u.snaps[accountKey{k.Source, k.Account}] {
			if s >= from && s < to {
				at = append(at, s+1)
			}
		}
		for _, t := range u.trades[k] {
			if t.At >= from && t.At < to {
				at = append(at, t.At+1)
			}
		}
	}
	lo, hi := math.Inf(1), 0.0
	for _, a := range at {
		v, known, _ := u.holding(keys, a)
		switch {
		case !known:
			lo = 0
		case v > 0:
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	if math.IsInf(lo, 1) {
		lo = 0
	}
	return f64(lo), f64(hi)
}

// seenBy says an account of keys shows a snapshot at or before at.
func (u *yieldUnits) seenBy(keys []unitKey, at int64) bool {
	return slices.ContainsFunc(keys, func(k unitKey) bool {
		snaps := u.snaps[accountKey{k.Source, k.Account}]
		return len(snaps) > 0 && snaps[0] <= at
	})
}

// firstHeld is the first quantity the accounts of keys show held at a
// snapshot of their source after from and no later than end, the pay
// day's start: quantity as the second after that snapshot began. nil
// where none shows any.
func (u *yieldUnits) firstHeld(keys []unitKey, from, end int64) *float64 {
	if len(keys) == 0 {
		return nil
	}
	at := u.instants[keys[0].Source]
	for i := sort.Search(len(at), func(i int) bool { return at[i] > from }); i < len(at) && at[i] <= end; i++ {
		if q, _ := u.quantity(keys, at[i]+1); q != nil {
			return q
		}
	}
	return nil
}

// heldOn is a key's quantity as instant at begins: what the newest
// snapshot that speaks for its account at or before at states, moved by
// the trades of the key since (yield.Holders.TradeKey), before at; a
// trade stamped at the snapshot's instant among them where it counts
// after it (after). The day's own trades so earn none of its payment.
// Trades move only a quantity the snapshot holds: one that holds none
// may hold the instrument by another id, as a statement's history can.
// nil before the account's first snapshot, or where the snapshot states
// none.
func (u *yieldUnits) heldOn(k unitKey, at int64) *float64 {
	snaps := u.snaps[accountKey{k.Source, k.Account}]
	i := sort.Search(len(snaps), func(i int) bool { return snaps[i] > at })
	if i == 0 {
		return nil
	}
	s := snaps[i-1]
	q := u.qtyAt(k, s)
	if q == nil || *q <= 0 {
		return q
	}
	v := *q
	for _, t := range u.trades[k] {
		if !t.Corp && t.At < at && (t.At > s || (t.At == s && u.after(k, s))) {
			v += t.Qty
		}
	}
	return &v
}

// qtyAt is a key's quantity at s, a snapshot that speaks for its account:
// its newest point at or before s. nil before its first point.
func (u *yieldUnits) qtyAt(k unitKey, s int64) *float64 {
	ps := u.Points[k]
	i := sort.Search(len(ps), func(i int) bool { return ps[i].At > s })
	if i == 0 {
		return nil
	}
	return ps[i-1].Qty
}

// after says a key's trades stamped at the instant of s, a snapshot that
// speaks for its account, count after s: its quantity is the previous
// snapshot's, moved by the trades between and not by these, as a
// statement that leaves out its last day's trades shows. Else they count
// in it, as the lot engine reads a snapshot (LOTS.md §3.1).
func (u *yieldUnits) after(k unitKey, s int64) bool {
	if v, ok := u.late[k][s]; ok {
		return v
	}
	if u.late == nil {
		u.late = map[unitKey]map[int64]bool{}
	}
	late := false
	snaps := u.snaps[accountKey{k.Source, k.Account}]
	if i, found := slices.BinarySearch(snaps, s); found && i > 0 {
		prev := snaps[i-1]
		qs, qp := u.qtyAt(k, s), u.qtyAt(k, prev)
		between, same := 0.0, 0.0
		for _, t := range u.trades[k] {
			switch {
			case t.Corp:
			case t.At == s:
				same += t.Qty
			case t.At > prev && t.At < s, t.At == prev && u.after(k, prev):
				between += t.Qty
			}
		}
		if qs != nil && qp != nil {
			tol := lots.Tolerance(*qs)
			late = math.Abs(*qs-*qp-between) <= tol && math.Abs(*qs-*qp-between-same) > tol
		}
	}
	if u.late[k] == nil {
		u.late[k] = map[int64]bool{}
	}
	u.late[k][s] = late
	return late
}
