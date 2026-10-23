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
// (yield.RecordOf: the ex day where the dividend calendar gives the days,
// else the record day of its market's lag), where one is known and the
// account held some then, else as its pay day began, in the newest
// snapshot that speaks for the account (unitAccountsCTE), moved by the
// trades of its key since, in the instrument's units at the instant. An
// instrument goes by the id the matching folds it into
// (yield.Inputs.Fold), under the account that holds or trades it. A
// trade booked on a cash account that holds none of its instrument moves
// the holding of the one account of its portfolio that does (tradeKey): a
// bank books a trade there, beside the custody account.

// unitKey names an instrument held in one account of a source, by the id
// the account's aliases fold it into.
type unitKey struct{ src, acct, iid string }

// unitPoint is a key's quantity from a snapshot that speaks for its
// account on, to its next point: nil where the snapshot states none, 0
// where the key is not in it. prev is the snapshot before it that speaks
// for the account, 0 for none.
type unitPoint struct {
	at, prev int64
	qty      *float64
}

// unitChange is a change of an instrument's units: from the snapshot at
// on, each old unit is k new ones. ok is false where no whole factor
// explains the change; k is then 1.
type unitChange struct {
	at int64
	k  float64
	ok bool
}

// yieldUnits are the quantities of the instruments paid, in each account
// that held them, at the points they change, and the trades that move
// each (unitMove, tradeKey); the snapshots that speak for each account; the
// accounts that held each instrument; the changes of each instrument's
// units; the spans of snapshots over which some account held each with
// its units read (cover); and the last snapshot of each source. late
// holds what after found.
type yieldUnits struct {
	points   map[unitKey][]unitPoint
	trades   map[unitKey][]unitTrade
	snaps    map[accountKey][]int64
	late     map[unitKey]map[int64]bool
	accounts map[yield.InstrumentKey][]string
	changes  map[yield.InstrumentKey][]unitChange
	read     map[yield.InstrumentKey][][2]int64
	last     map[string]int64
	// instants are the snapshots of each source that speak for some
	// account, sorted.
	instants map[string][]int64
	// siblings memoises portfolioKeys for tradeKey.
	siblings map[unitKey][]unitKey
}

// accountKey names an account of a source.
type accountKey struct{ src, acct string }

// unitWindowCTE are the instruments $1 and $2 name (one source and one
// id a pair), by the id the matching folds an instrument into (paid); and
// the snapshots of their sources that hold on some day from $3 to $4: from
// the oldest of the accounts' active snapshots at $3 (pos_active_at,
// migration 0140), else the first, every one to $4.
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
// observation on (pos_key_spans, migration 0140), where it holds
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

// readYieldUnits reads the units of the instruments paid, by the ids the
// matching folds them into, from from to asOf. Each account's quantity of
// each is the sum of its rows whose instrument its aliases fold into it,
// as holdingAliasJoins reads a holding's key (a CUSIP id and its ticker,
// say), in each snapshot that speaks for the account (unitAccountsCTE),
// the account holding none where they are absent: the points it changes
// at (unitPoint). Between two such snapshots, the key's trades, read
// as the source kind's lot policy reads them (lots.PolicyFor) and keyed
// by tradeKey, and its quantity and value tell a change of units
// (unitStep.change). Every account's changes are the instrument's
// (addChange).
func readYieldUnits(ctx context.Context, db querier, in *yield.Inputs, paid map[yield.InstrumentKey]bool,
	from, asOf int64) (*yieldUnits, error) {
	u := &yieldUnits{points: map[unitKey][]unitPoint{}, snaps: map[accountKey][]int64{},
		late: map[unitKey]map[int64]bool{}, accounts: map[yield.InstrumentKey][]string{},
		changes: map[yield.InstrumentKey][]unitChange{}, read: map[yield.InstrumentKey][][2]int64{},
		last: map[string]int64{}, instants: map[string][]int64{}, siblings: map[unitKey][]unitKey{}}
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
		step     unitStep
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
		if err := rows.Scan(&k.src, &k.acct, &k.iid, &at, &qty, &value, &prev, &prevQty, &prevValue,
			&last); err != nil {
			return err
		}
		if _, seen := u.points[k]; !seen {
			ik := yield.InstrumentKey{Source: k.src, Instrument: k.iid}
			u.accounts[ik] = append(u.accounts[ik], k.acct)
		}
		u.points[k] = append(u.points[k], unitPoint{at: at, prev: prev.Int64, qty: nullFloatToPtr(qty)})
		u.last[k.src] = last
		if prev.Valid && prevQty.Valid && qty.Valid {
			steps = append(steps, stepAt{key: k, prev: prev.Int64, at: at, step: unitStep{before: prevQty.Float64,
				after: qty.Float64, valueBefore: nullFloatToPtr(prevValue), valueAfter: nullFloatToPtr(value)}})
		}
		return nil
	}, srcs, iids, from, asOf); err != nil {
		return nil, err
	}
	if err := scanAll(ctx, db, unitWindowCTE+unitAccountsCTE+`
SELECT silver_source_id, account_external_id, snapshot_at FROM account_grid ORDER BY 1, 2, 3`,
		func(rows *sql.Rows) error {
			var k accountKey
			var at int64
			if err := rows.Scan(&k.src, &k.acct, &at); err != nil {
				return err
			}
			u.snaps[k] = append(u.snaps[k], at)
			u.instants[k.src] = append(u.instants[k.src], at)
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
	trades := map[unitKey][]unitTrade{}
	for _, b := range booked {
		k := u.tradeKey(in, b)
		trades[k] = append(trades[k], b.trade)
	}
	u.trades = trades
	for _, s := range steps {
		for _, t := range trades[s.key] {
			if (t.at > s.prev && t.at < s.at) || (t.at == s.prev && u.after(s.key, s.prev)) ||
				(t.at == s.at && !u.after(s.key, s.at)) {
				s.step.trades = append(s.step.trades, t)
			}
		}
		if k, ok := s.step.change(); k != 1 || !ok {
			u.addChange(yield.InstrumentKey{Source: s.key.src, Instrument: s.key.iid}, unitChange{at: s.at, k: k, ok: ok})
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
	holds := func(p unitPoint) bool { return p.qty != nil && *p.qty > lots.Tolerance(*p.qty) }
	for k, ps := range u.points {
		ik := yield.InstrumentKey{Source: k.src, Instrument: k.iid}
		for j := 0; j < len(ps); j++ {
			if !holds(ps[j]) {
				continue
			}
			from := ps[j].at
			for j+1 < len(ps) && holds(ps[j+1]) {
				j++
			}
			to := u.last[k.src]
			if j+1 < len(ps) {
				to = ps[j+1].prev
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
	trade unitTrade
}

// tradeKey is the key a trade moves the quantity of: its booking
// account's, where that account holds its instrument as the trade
// happens (holdsAt). Else, for a trade booked on a cash account, the one
// other account of its portfolio that holds it then, its instrument
// folded under that account or under the booking account
// (portfolioKeys, b.key). Where none does, or several do, the booking
// key: which account the trade went to is not known, and it moves no
// holding. A trade of any other account moves only its own.
func (u *yieldUnits) tradeKey(in *yield.Inputs, b bookedTrade) unitKey {
	if u.holdsAt(b.key, b.trade) ||
		in.Accounts[yield.AccountKey{Source: b.key.src, Account: b.key.acct}].Kind != string(canonical.AccountKindCash) {
		return b.key
	}
	own := unitKey{b.key.src, b.key.acct, b.iid}
	siblings, ok := u.siblings[own]
	if !ok {
		siblings = portfolioKeys(in, b.key.src, b.key.acct, b.iid)[1:]
		u.siblings[own] = siblings
	}
	var holder []unitKey
	for _, k := range siblings {
		for _, c := range []unitKey{k, {k.src, k.acct, b.key.iid}} {
			if u.holdsAt(c, b.trade) {
				holder = append(holder, c)
				break
			}
		}
	}
	if len(holder) != 1 {
		return b.key
	}
	return holder[0]
}

// holdsAt says key k holds its instrument as trade t happens: its newest
// point at or before the trade holds some, or, for a buy, the first after
// it does.
func (u *yieldUnits) holdsAt(k unitKey, t unitTrade) bool {
	ps := u.points[k]
	i := sort.Search(len(ps), func(i int) bool { return ps[i].at > t.at })
	holds := func(p unitPoint) bool { return p.qty != nil && *p.qty > lots.Tolerance(*p.qty) }
	return (i > 0 && holds(ps[i-1])) || (t.qty > 0 && i < len(ps) && holds(ps[i]))
}

// readUnitTrades reads the transactions that move a quantity over the
// snapshots readYieldUnits reads, oldest first (unitMove), each under its
// booking account's key, by the id that account's aliases fold its
// instrument into. One stamped at a snapshot's instant counts in it or
// after it, as the snapshot shows (after).
func readUnitTrades(ctx context.Context, db querier, srcs, iids []string, from, asOf int64) ([]bookedTrade,
	error) {
	kinds := map[string]string{}
	if err := scanAll(ctx, db, `SELECT silver_source_id, silver_kind FROM silver_sources`, func(rows *sql.Rows) error {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return err
		}
		kinds[id] = kind
		return nil
	}); err != nil {
		return nil, err
	}
	var out []bookedTrade
	err := scanAll(ctx, db, unitWindowCTE+`,
folded AS (
    SELECT t.*, COALESCE(ha.to_key, hd.to_key, ia.k, t.instrument_external_id) AS k
      FROM transactions t`+holdingAliasJoins("t.silver_source_id", "t.account_external_id", "t.instrument_external_id",
		"NULL")+`
     WHERE t.instrument_external_id IS NOT NULL AND t.quantity <> 0 AND t.occurred_at <= $4
       AND t.silver_source_id IN (SELECT src FROM paid)),
-- A trade under a paid id, or under an id an alias of any account folds
-- into one: the account that holds it may fold it (tradeKey).
named AS (
    SELECT src, k AS id FROM paid
    UNION SELECT h.silver_source_id, h.from_key FROM holding_aliases h
            JOIN paid ON paid.src = h.silver_source_id AND paid.k = h.to_key
    UNION SELECT ia.src, ia.alias FROM instrument_aliases() ia
            JOIN paid ON paid.src = ia.src AND paid.k = ia.k)
SELECT t.silver_source_id, t.account_external_id, t.k, t.instrument_external_id, t.occurred_at,
       t.transaction_external_id, t.kind, CAST(t.quantity AS DOUBLE), `+lotTxnColumns+`
  FROM folded t
  JOIN first f ON f.silver_source_id = t.silver_source_id
 WHERE t.occurred_at >= f.first_at
   AND EXISTS (SELECT 1 FROM named n WHERE n.src = t.silver_source_id
                  AND (n.id = t.k OR n.id = t.instrument_external_id))
 ORDER BY t.occurred_at, t.silver_source_id, t.transaction_external_id`, func(rows *sql.Rows) error {
		var k unitKey
		var iid string
		var at int64
		var t lots.Txn
		if err := rows.Scan(append([]any{&k.src, &k.acct, &k.iid, &iid, &at, &t.ID, &t.Kind, &t.Quantity},
			lotTxnFields(&t)...)...); err != nil {
			return err
		}
		pol, _ := lots.PolicyFor(kinds[k.src])
		classify := pol.Classify
		if classify == nil {
			classify = lots.DefaultClassify
		}
		if move, corp := unitMove(classify(t), t.Quantity); move != 0 {
			out = append(out, bookedTrade{key: k, iid: iid, trade: unitTrade{at: at, qty: move, corp: corp}})
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

// quantity is what the accounts of keys (keysOf) held of their
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
		if snaps := u.snaps[accountKey{k.src, k.acct}]; len(keys) > 1 && (len(snaps) == 0 || snaps[0] > end) {
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
	ik := yield.InstrumentKey{Source: keys[0].src, Instrument: keys[0].iid}
	k, ok := 1.0, true
	for _, c := range u.changes[ik] {
		if c.at > end {
			k *= c.k
			ok = ok && c.ok
		}
	}
	return held * k, true, !ok || !u.covered(ik, end)
}

// portfolioKeys are the keys a line of account acct reads the instrument
// iid names under, iid folded under each account (yield.Inputs.Fold):
// acct's own, then each other account's of its portfolio, by account.
func portfolioKeys(in *yield.Inputs, src, acct, iid string) []unitKey {
	keys := []unitKey{{src, acct, in.Fold(src, acct, iid)}}
	portfolio := in.Accounts[yield.AccountKey{Source: src, Account: acct}].Portfolio
	if portfolio == "" {
		return keys
	}
	for a, x := range in.Accounts {
		if a.Source == src && a.Account != acct && x.Portfolio == portfolio {
			keys = append(keys, unitKey{src, a.Account, in.Fold(src, a.Account, iid)})
		}
	}
	slices.SortFunc(keys[1:], func(a, b unitKey) int { return cmp.Compare(a.acct, b.acct) })
	return keys
}

// keysOf are the keys a payment to account acct of the instrument iid
// names reads its quantities under (portfolioKeys): its own where the
// account held it, else its portfolio's accounts', else that of the one
// account of its source that held it. A bank books a dividend to a cash
// account beside the custody account, or in another portfolio.
func (u *yieldUnits) keysOf(in *yield.Inputs, src, acct, iid string) []unitKey {
	all := portfolioKeys(in, src, acct, iid)
	if _, ok := u.points[all[0]]; ok {
		return all[:1]
	}
	var keys []unitKey
	for _, k := range all[1:] {
		if _, ok := u.points[k]; ok {
			keys = append(keys, k)
		}
	}
	own := all[0].iid
	if accts := u.accounts[yield.InstrumentKey{Source: src, Instrument: own}]; len(keys) == 0 && len(accts) == 1 {
		keys = []unitKey{{src, accts[0], own}}
	}
	return keys
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
		for _, s := range u.snaps[accountKey{k.src, k.acct}] {
			if s >= from && s < to {
				at = append(at, s+1)
			}
		}
		for _, t := range u.trades[k] {
			if t.at >= from && t.at < to {
				at = append(at, t.at+1)
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
		snaps := u.snaps[accountKey{k.src, k.acct}]
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
	at := u.instants[keys[0].src]
	for i := sort.Search(len(at), func(i int) bool { return at[i] > from }); i < len(at) && at[i] <= end; i++ {
		if q, _ := u.quantity(keys, at[i]+1); q != nil {
			return q
		}
	}
	return nil
}

// heldOn is a key's quantity as instant at begins: what the newest
// snapshot that speaks for its account at or before at states, moved by
// the trades of the key since (tradeKey), before at; a trade stamped at the
// snapshot's instant among them where it counts after it (after). The
// day's own trades so earn none of its payment. Trades move only a
// quantity the snapshot holds: one that holds none may hold the
// instrument by another id, as a statement's history can. nil before the
// account's first snapshot, or where the snapshot states none.
func (u *yieldUnits) heldOn(k unitKey, at int64) *float64 {
	snaps := u.snaps[accountKey{k.src, k.acct}]
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
		if !t.corp && t.at < at && (t.at > s || (t.at == s && u.after(k, s))) {
			v += t.qty
		}
	}
	return &v
}

// qtyAt is a key's quantity at s, a snapshot that speaks for its account:
// its newest point at or before s. nil before its first point.
func (u *yieldUnits) qtyAt(k unitKey, s int64) *float64 {
	ps := u.points[k]
	i := sort.Search(len(ps), func(i int) bool { return ps[i].at > s })
	if i == 0 {
		return nil
	}
	return ps[i-1].qty
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
	snaps := u.snaps[accountKey{k.src, k.acct}]
	if i, found := slices.BinarySearch(snaps, s); found && i > 0 {
		prev := snaps[i-1]
		qs, qp := u.qtyAt(k, s), u.qtyAt(k, prev)
		between, same := 0.0, 0.0
		for _, t := range u.trades[k] {
			switch {
			case t.corp:
			case t.at == s:
				same += t.qty
			case t.at > prev && t.at < s, t.at == prev && u.after(k, prev):
				between += t.qty
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

// unitTrade is a transaction that moves a key's quantity (unitMove).
type unitTrade struct {
	at   int64
	qty  float64
	corp bool
}

// unitStep is a key's quantity and value at two consecutive snapshots of
// its source, and the transactions between them, oldest first.
type unitStep struct {
	before, after           float64
	valueBefore, valueAfter *float64
	trades                  []unitTrade
}

// unitChangeMin is the least change of units, either way, that a price
// or a corporate action tells from a trade: a three-for-two split. A
// smaller corporate delivery, such as a scrip dividend, is a trade.
const unitChangeMin = 1.5

// change is the change of units a step shows: each old unit became k new
// ones, 1 where its trades explain its quantities. A change they do not
// explain is one of units where a corporate action's leg names it, or
// where the price per unit moved by its inverse (priced); else it is a
// trade no transaction records, and k is 1. Its factor is the quantities'
// (wholeFactor), with the step's trades before the change or after it.
// The legs count once each, as the lot engine dedupes them (LOTS.md
// §3.5), and must state the change: their sum is the units delivered, or
// a leg states the new total. ok is false where no whole factor fits, or
// the legs state another change.
func (s unitStep) change() (k float64, ok bool) {
	if !(s.before > lots.Tolerance(s.before)) || !(s.after > lots.Tolerance(s.after)) {
		// A position opened or closed tells nothing of its units.
		return 1, true
	}
	traded := 0.0
	var legs []float64
	for _, t := range s.trades {
		switch {
		case !t.corp:
			traded += t.qty
		case !slices.Contains(legs, t.qty):
			legs = append(legs, t.qty)
		}
	}
	if math.Abs(s.after-s.before-traded) <= lots.Tolerance(s.after) {
		return 1, true
	}
	small := false
	for _, q := range [][2]float64{{s.before + traded, s.after}, {s.before, s.after - traded}} {
		if q[0] <= 0 || q[1] <= 0 {
			continue
		}
		r := q[1] / q[0]
		small = small || (r < unitChangeMin && r > 1/unitChangeMin)
		k, whole := wholeFactor(q[0], r)
		switch {
		case !whole:
		case len(legs) > 0 && stated(legs, q[0], q[1]):
			return k, true
		case len(legs) == 0 && s.priced(k):
			return k, true
		}
	}
	switch {
	case small:
		return 1, true
	case len(legs) > 0:
		return 1, false
	}
	if k := s.after / (s.before + traded); k >= unitChangeMin || k <= 1/unitChangeMin {
		return 1, !s.priced(k)
	}
	return 1, true
}

// stated says corporate action legs state the change of a quantity from
// before to after: they sum to the units delivered, or one states the new
// total, short at most of a fraction paid in cash.
func stated(legs []float64, before, after float64) bool {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1+lots.Tolerance(b) }
	sum := 0.0
	for _, l := range legs {
		sum += l
	}
	return near(sum, after-before) || slices.ContainsFunc(legs, func(l float64) bool { return l > 0 && near(l, after) })
}

// priced says the price per unit moved by the inverse of k between the
// step's snapshots: in ratio, nearer to k than to staying put. false
// where a value is unknown.
func (s unitStep) priced(k float64) bool {
	if s.valueBefore == nil || s.valueAfter == nil || !(*s.valueBefore > 0) || !(*s.valueAfter > 0) {
		return false
	}
	moved := (*s.valueBefore / s.before) / (*s.valueAfter / s.after)
	return math.Abs(math.Log(moved/k)) < math.Abs(math.Log(moved))
}

// wholeFactorMost bounds the factors wholeFactor tries: past it, so many
// fit that none tells.
const wholeFactorMost = 1000

// wholeFactor reads ratio, what each of before units became, as a whole
// number n of at least two new units for each old one, or as one new
// unit for n old ones: the new quantity is n·before, or before/n, exactly
// or short of the fraction of a unit paid in cash. false where neither
// fits, or where more than one does, as on a small position whose new
// quantity rounds down to the same whole units under several.
func wholeFactor(before, ratio float64) (float64, bool) {
	if !(before > 0) || !(ratio > 0) {
		return 0, false
	}
	after := before * ratio
	fits := func(target float64) bool {
		tol := lots.Tolerance(target)
		return math.Abs(after-target) <= tol || math.Abs(after-math.Floor(target)) <= tol
	}
	var found []float64
	try := func(lo, hi float64, factor, target func(n float64) float64) bool {
		if hi-lo > wholeFactorMost {
			return false
		}
		for n := max(lo, 2); n <= hi; n++ {
			if fits(target(n)) {
				found = append(found, factor(n))
			}
		}
		return true
	}
	if !try(math.Floor(after/before), math.Ceil((after+1)/before),
		func(n float64) float64 { return n }, func(n float64) float64 { return n * before }) ||
		!try(math.Floor(before/(after+1)), math.Ceil(before/after),
			func(n float64) float64 { return 1 / n }, func(n float64) float64 { return before / n }) ||
		len(found) != 1 {
		return 0, false
	}
	return found[0], true
}
