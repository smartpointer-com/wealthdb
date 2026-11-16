package gold

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotRow is a position row an observation writes to.
type lotRow struct {
	snapshot     int64
	acct, posKey string
	qty          float64
	stated       bool
}

// lotObs is one observation's position rows, their total, and the row
// that holds most: the account a pooled key's observation names, and
// the row its lot count goes on.
type lotObs struct {
	rows  []lotRow
	total float64
	top   int
}

// lotWallet is what one account of a pooled key holds: what it held
// when it last appeared in a snapshot, and its trades since, up to
// next (readPositions).
type lotWallet struct {
	acct string
	qty  float64
	next int
}

// lotWalletRef names one account of a pooled key.
type lotWalletRef struct {
	key  int32
	acct string
}

// lotWalletTrade is a trade's signed quantity on one account of a
// pooled key.
type lotWalletTrade struct {
	at  int64
	qty float64
}

// lotTrade is one transaction time of a key, for the zero observations
// (readPositions).
type lotTrade struct {
	at  int64
	key int32
}

// readPositions turns the snapshots into observations. Each key held
// at a snapshot observes the quantity its rows sum to. A key the
// account held at its previous snapshot, or traded since, observes
// zero when the account appears without it. An account absent from a
// snapshot observes nothing: sources leave accounts out of snapshots.
// So a pooled key's wallet absent from a snapshot holds what it held
// when it last appeared, moved by its trades since, and the key
// observes that too. A wallet its trades emptied holds nothing: a
// source drops an empty wallet from its snapshots.
func (f *lotFeed) readPositions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, snapshot_at, lot_obs_at(snapshot_at), account_external_id, position_key,
       holding_key(instrument_external_id, position_key), instrument_external_id IS NULL, currency,
       COALESCE(CAST(quantity AS DOUBLE), 0), stated_basis(book_value, basis_origin)
  FROM positions WHERE `+inBoundList("silver_source_id")+`
 ORDER BY silver_source_id, snapshot_at, account_external_id, position_key`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read positions: %w", err)
	}
	defer rows.Close()

	w := newSnapshotWalk(f)
	for rows.Next() {
		var (
			src, acct, posKey, inst, ccy string
			snap, obsAt                  int64
			isHint, stated               bool
			qty                          float64
		)
		if err := rows.Scan(&src, &snap, &obsAt, &acct, &posKey, &inst, &isHint, &ccy, &qty, &stated); err != nil {
			return fmt.Errorf("lots: read positions: %w", err)
		}
		if src != w.curSrc || snap != w.curSnap {
			w.flush()
			w.curSrc, w.curSnap, w.curAt = src, snap, obsAt
		}
		s := f.sources[src]
		a := lotAcctRef{src, f.keyAccount(s, acct)}
		if _, ok := w.heldBy[a]; !ok {
			w.heldBy[a] = nil
		}
		w.present[acct] = true
		k := f.key(s, acct, inst, isHint)
		if k < 0 {
			continue
		}
		if _, ok := w.held[k]; !ok {
			w.heldBy[a] = append(w.heldBy[a], k)
		}
		f.setCurrency(k, ccy, true)
		w.held[k] = append(w.held[k], lotRow{snapshot: snap, acct: acct, posKey: posKey, qty: qty, stated: stated})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	w.flush()
	return nil
}

// lotAcctRef is a source and a key account.
type lotAcctRef = [2]string

// snapshotWalk is readPositions' state between the snapshots it walks.
type snapshotWalk struct {
	f              *lotFeed
	curSrc         string
	curSnap, curAt int64
	held           map[int32][]lotRow
	// heldBy lists the keys each present account holds at the
	// snapshot; lastHeld at its previous one.
	heldBy   map[lotAcctRef][]int32
	lastHeld map[lotAcctRef][]int32
	// cursor is how far into f.trades each account's previous
	// snapshots have read.
	cursor map[lotAcctRef]int
	// holder is the account that last held each key: the one a
	// pooled key's zero observation names.
	holder map[int32]string
	gone   map[int32]bool
	// wallets is what each account of a key held when it last
	// appeared; present marks the accounts of this snapshot.
	wallets map[int32][]lotWallet
	present map[string]bool
}

func newSnapshotWalk(f *lotFeed) *snapshotWalk {
	return &snapshotWalk{
		f:        f,
		curSnap:  math.MinInt64,
		held:     map[int32][]lotRow{},
		heldBy:   map[lotAcctRef][]int32{},
		lastHeld: map[lotAcctRef][]int32{},
		cursor:   map[lotAcctRef]int{},
		holder:   map[int32]string{},
		gone:     map[int32]bool{},
		wallets:  map[int32][]lotWallet{},
		present:  map[string]bool{},
	}
}

// carried is what k's accounts absent from this snapshot hold at
// now; the present ones' entries go, for this snapshot's rows to
// replace, and so do the ones their trades emptied.
func (w *snapshotWalk) carried(k int32, now int64) float64 {
	sum, keep := 0.0, w.wallets[k][:0]
	for _, wl := range w.wallets[k] {
		if w.present[wl.acct] {
			continue
		}
		ts := w.f.walletTrades[lotWalletRef{k, wl.acct}]
		for ; wl.next < len(ts) && ts[wl.next].at <= now; wl.next++ {
			wl.qty += ts[wl.next].qty
		}
		if wl.qty > lots.Tolerance(wl.qty) {
			sum += wl.qty
			keep = append(keep, wl)
		}
	}
	w.wallets[k] = keep
	return sum
}

// seen records what each of k's accounts holds at now.
func (w *snapshotWalk) seen(k int32, now int64, rs []lotRow) {
	for _, r := range rs {
		ts := w.f.walletTrades[lotWalletRef{k, r.acct}]
		next, _ := slices.BinarySearchFunc(ts, now+1, func(t lotWalletTrade, at int64) int { return cmp.Compare(t.at, at) })
		if i := len(w.wallets[k]) - 1; i >= 0 && w.wallets[k][i].acct == r.acct {
			w.wallets[k][i].qty += r.qty
			continue
		}
		w.wallets[k] = append(w.wallets[k], lotWallet{r.acct, r.qty, next})
	}
}

// flush observes the snapshot the walk is in.
func (w *snapshotWalk) flush() {
	if w.curSrc == "" {
		return
	}
	at := w.curAt
	txn := "snapshot:" + strconv.FormatInt(at, 10)
	ks := make([]int32, 0, len(w.held))
	for k := range w.held {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	for _, k := range ks {
		rs := w.held[k]
		o := lotObs{rows: rs}
		for i, r := range rs {
			o.total += r.qty
			if r.qty > rs[o.top].qty {
				o.top = i
			}
		}
		qty := o.total + w.carried(k, at)
		w.seen(k, at, rs)
		w.f.observe(k, at, txn, qty, rs[o.top].acct, o)
		w.holder[k] = rs[o.top].acct
	}
	accts := make([]lotAcctRef, 0, len(w.heldBy))
	for a := range w.heldBy {
		accts = append(accts, a)
	}
	slices.SortFunc(accts, func(a, b lotAcctRef) int { return cmp.Compare(a[1], b[1]) })
	for _, a := range accts {
		// What the account held last time, and what it traded
		// since, and does not hold now, observes zero.
		clear(w.gone)
		for _, k := range w.lastHeld[a] {
			w.gone[k] = true
		}
		ts, i := w.f.trades[a], w.cursor[a]
		for ; i < len(ts) && ts[i].at <= at; i++ {
			w.gone[ts[i].key] = true
		}
		w.cursor[a] = i
		for _, k := range w.heldBy[a] {
			delete(w.gone, k)
		}
		zs := make([]int32, 0, len(w.gone))
		for k := range w.gone {
			zs = append(zs, k)
		}
		slices.Sort(zs)
		for _, k := range zs {
			acct := w.holder[k]
			if acct == "" {
				acct = a[1]
			}
			w.f.observe(k, at, txn, w.carried(k, at), acct, lotObs{})
		}
		w.lastHeld[a] = append(w.lastHeld[a][:0], w.heldBy[a]...)
	}
	clear(w.held)
	clear(w.heldBy)
	clear(w.present)
}

func (f *lotFeed) observe(k int32, at int64, txn string, qty float64, acct string, o lotObs) {
	id := int32(len(f.targets))
	f.targets = append(f.targets, o)
	f.add(lots.Event{At: at, Kind: lots.Observe, Key: k, Txn: txn, Account: acct, Qty: qty, Obs: id}, lotAux{})
}
