package lots

import (
	"cmp"
	"encoding/hex"
	"hash"
	"hash/fnv"
	"math"
	"slices"
	"strconv"
)

// KeySpec is one key of the replay: an instrument held in one account,
// or in one pooled portfolio, of one source. The engine knows a key by
// its index into the slice handed to Run.
type KeySpec struct {
	// ID names the key stably across runs; a lot's id hashes it.
	ID     string
	Method Method
	// Fees stamps the cost of the lots the key opens.
	Fees Fees
}

// StatedLot is a lot a source states: a lot of an anchor, or a realized
// lot of a sale.
type StatedLot struct {
	Qty       float64
	Cost      float64
	CostKnown bool
	AcqDay    int32 // NoDay when the source states none
}

// EventKind is what an event does. The order of the constants is the
// order of events within one second: a transfer moves before the trades
// of its second, acquisitions come before disposals so a same-second
// buy and sell never dips below zero, and the snapshot comes last
// because it shows the second's trades.
type EventKind uint8

const (
	// Flight marks Key in flight from At until the Land that follows: a
	// transfer whose receipt is recorded before its departure moves at
	// the receipt, and the sending key's snapshots until the departure
	// still show what left. They are not reconciled.
	Flight EventKind = iota
	// Land ends a Flight on Key.
	Land
	// Depart relieves Qty from Key by Key's method and holds the lots in
	// transit under Transit.
	Depart
	// Arrive opens the lots held in transit under Transit on Key, with
	// their cost and acquisition date.
	Arrive
	// Reorg relieves Qty from Key and opens each relieved lot again on
	// every key of Into, with its date: a merger, a conversion, a split
	// that changes the instrument, one instrument that becomes several.
	Reorg
	// Split adds Qty shares to Key's open lots, each scaled by (book +
	// Qty) ÷ book with its cost. On an empty book the shares are a
	// spin-off: a lot of unknown cost.
	Split
	// Adjust lowers the cost of Key's open lots by Value, pro rata by
	// quantity: a return of capital.
	Adjust
	// Acquire opens a lot of Qty costing Value (unknown when !HasValue).
	Acquire
	// Dispose relieves Qty from Key by its method, for proceeds Value
	// (unknown when !HasValue). Lots, when set, are the realized lots a
	// source states for the sale: a seed the sale relieves takes its
	// cost and date from them.
	Dispose
	// Anchor replaces Key's book with Lots, the lots a source states at
	// a snapshot.
	Anchor
	// Observe reconciles Key's book with Qty, the quantity a snapshot
	// shows, and records the book under Obs.
	Observe
)

// Event is one input to the replay.
type Event struct {
	At   int64
	Kind EventKind
	Key  int32
	// Transit pairs a Depart with its Arrive.
	Transit int32
	// Into are the receiving keys of a Reorg.
	Into []Into
	// Txn is the transaction behind the event; Account the account it
	// names (a wallet of a pooled key).
	Txn     string
	Account string
	Qty     float64
	// Value is a cost, proceeds or an amount, as the kind says.
	Value    float64
	HasValue bool
	// AcqDay is an acquisition's date, NoDay when unknown.
	AcqDay     int32
	Origin     Origin
	CostOrigin CostOrigin
	Disposal   DisposalKind
	Lots       []StatedLot
	// Obs identifies an observation for the caller.
	Obs int32
}

// Into is one key a Reorg's lots go to: the quantity it receives in
// all, and its share of their cost. A reorg's weights add up to one.
type Into struct {
	Key    int32
	Qty    float64
	Weight float64
}

// SortEvents orders events for Run: by time, then by kind (see
// EventKind), then by transaction and key, so a replay is deterministic.
func SortEvents(evs []Event) {
	slices.SortStableFunc(evs, func(a, b Event) int {
		if c := cmp.Compare(a.At, b.At); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Txn, b.Txn); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
}

// Lot is one lot the replay opened.
type Lot struct {
	ID       string
	Key      int32
	OpenedAt int64
	// AcqDay is the date the holding period counts from, NoDay when
	// unknown. Various marks an average pool merged from several
	// acquisitions.
	AcqDay     int32
	Various    bool
	Origin     Origin
	Txn        string
	Qty        float64
	Cost       float64
	CostKnown  bool
	CostOrigin CostOrigin
	Fees       Fees
	Method     Method
	// ClosedAt is when the lot's quantity reached zero, 0 while open.
	ClosedAt int64

	rem, remCost float64
	seq          int64
	// seed marks quantity a seed opened, through every split, return of
	// capital, move and reorg since; seedAt is when the seed opened.
	seed   bool
	seedAt int64
	// finding is the index of the seed finding that opened the lot,
	// plus one; 0 for none, or once the lot has left its key.
	finding int32
}

// Disposal is quantity relieved from one lot.
type Disposal struct {
	Lot           int32
	Txn           string
	Account       string
	At            int64
	Qty           float64
	Proceeds      float64
	ProceedsKnown bool
	Cost          float64
	CostKnown     bool
	Kind          DisposalKind
}

// Realized reports whether the disposal is a realized lot: a kind that
// realizes, with known proceeds.
func (d Disposal) Realized() bool { return d.Kind.Realizes() && d.ProceedsKnown }

// Observation is a key's book at a snapshot, after reconciliation.
type Observation struct {
	Obs int32
	Key int32
	At  int64
	// Qty is the book's quantity; Known the cost of the lots that have
	// one; Unknown the quantity of the lots that do not.
	Qty, Known, Unknown float64
	Lots                int32
}

// Finding is one thing the replay could not take at face value.
type Finding struct {
	Kind    FindingKind
	Key     int32
	Account string
	At      int64
	Qty     float64
	Txn     string
}

// AnchorCheck compares the book with a source's stated lots at an
// anchor, before adopting them.
type AnchorCheck struct {
	Key int32
	At  int64
	// BookQty and StatedQty are the two quantities; MatchedQty the
	// quantity of book lots a stated lot matches by acquisition date and
	// quantity. BookCost and StatedCost sum the matched lots that both
	// sides cost.
	BookQty, StatedQty, MatchedQty float64
	BookCost, StatedCost           float64
	// Resolved is the seed quantity the stated lots resolved.
	Resolved float64
	// Kept is an anchor that equals the book, which is then left as is.
	Kept bool
}

// Result is a replay's output.
type Result struct {
	Lots         []Lot
	Disposals    []Disposal
	Observations []Observation
	Findings     []Finding
	Anchors      []AnchorCheck
}

// BlipWindow is how long, in seconds, an implied disposal stays
// restorable: quantity that comes back within 45 days was a snapshot
// leaving a row out, not a disposal.
const BlipWindow = 45 * 86400

type pendingPart struct {
	lot       int32
	at        int64
	qty, cost float64
	// disposal is the implied disposal that took the part; finding the
	// implied finding it counts in.
	disposal, finding int32
}

type patch struct {
	key             int32
	from, to        int64
	known, unknownQ float64
}

type engine struct {
	keys    []KeySpec
	books   []book
	res     Result
	keyObs  [][]int32
	patches []patch
	opened  []int32
	seq     int64
	hash    hash.Hash
	buf     []byte
	// transit holds what each Depart relieved until its Arrive.
	transit map[int32]transit
}

// part is quantity relieved from one lot, with its cost.
type part struct {
	li      int32
	q, cost float64
}

type transit struct {
	parts []part
	qty   float64
}

// Run replays events, which SortEvents has ordered, over keys.
func Run(keys []KeySpec, events []Event) Result {
	e := &engine{
		keys:    keys,
		books:   make([]book, len(keys)),
		keyObs:  make([][]int32, len(keys)),
		opened:  make([]int32, len(keys)),
		hash:    fnv.New128a(),
		transit: map[int32]transit{},
	}
	for i := range e.books {
		e.books[i].init(keys[i].Method)
	}
	// Size the results from the events up front: growing them by
	// doubling copies a large history several times over.
	var nAcq, nDisp, nObs int
	for i := range events {
		switch events[i].Kind {
		case Acquire:
			nAcq++
		case Dispose, Depart, Reorg:
			nDisp++
		case Observe:
			nObs++
		}
	}
	e.res.Lots = make([]Lot, 0, nAcq+nAcq/4)
	e.res.Disposals = make([]Disposal, 0, 2*nDisp)
	e.res.Observations = make([]Observation, 0, nObs)
	for i := range events {
		e.step(&events[i])
	}
	e.applyPatches()
	// A finding the replay undid in full (a seed an implied disposal
	// took back, an implied disposal a blip restored) says nothing.
	e.res.Findings = slices.DeleteFunc(e.res.Findings, func(f Finding) bool { return f.Qty <= epsOf(f.Qty) })
	return e.res
}

func (e *engine) step(ev *Event) {
	switch ev.Kind {
	case Flight:
		e.books[ev.Key].flight++
	case Land:
		e.books[ev.Key].flight--
	case Depart:
		parts := e.relieveParts(ev, ev.Qty, DisposeMove)
		e.transit[ev.Transit] = transit{parts, ev.Qty}
	case Arrive:
		t := e.transit[ev.Transit]
		delete(e.transit, ev.Transit)
		e.placeParts(ev, t.parts, t.qty, OriginTransferIn, []Into{{Key: ev.Key, Qty: t.qty, Weight: 1}})
	case Reorg:
		e.reorg(ev)
	case Split:
		e.split(ev)
	case Adjust:
		e.adjust(ev)
	case Acquire:
		e.acquire(ev)
	case Dispose:
		e.dispose(ev)
	case Anchor:
		e.anchor(ev)
	case Observe:
		e.observe(ev)
	}
}

// lotID hashes a key, the transaction that opened the lot and the
// key's lot count, so a replay of the same input yields the same ids.
func (e *engine) lotID(k int32, txn string) string {
	b := append(e.buf[:0], e.keys[k].ID...)
	b = append(b, 0)
	b = append(b, txn...)
	b = append(b, 0)
	b = strconv.AppendInt(b, int64(e.opened[k]), 10)
	e.opened[k]++
	e.hash.Reset()
	e.hash.Write(b)
	e.buf = e.hash.Sum(b[:0])
	return hex.EncodeToString(e.buf)
}

// open adds l to the ledger and, unless detached, to its key's book.
// The caller sets everything but the id, method, sequence and running
// state; a zero quantity opens nothing.
func (e *engine) open(l Lot, detached bool) int32 {
	if !(l.Qty > 0) {
		return -1
	}
	if !l.CostKnown {
		l.Cost = 0
	}
	l.ID = e.lotID(l.Key, l.Txn)
	l.Method = e.keys[l.Key].Method
	if l.seq == 0 {
		e.seq++
		l.seq = e.seq
	}
	l.rem, l.remCost = l.Qty, l.Cost
	e.res.Lots = append(e.res.Lots, l)
	li := int32(len(e.res.Lots) - 1)
	if !detached {
		e.books[l.Key].add(e, li)
	}
	return li
}

// place opens l on its key's book; an average book folds it into the
// pool of its kind instead (merge).
func (e *engine) place(l Lot, ev *Event) int32 {
	if e.books[l.Key].method == Average {
		return e.merge(l, ev)
	}
	return e.open(l, false)
}

// carriedOrigin is the cost origin of a lot that takes its cost from
// src.
func carriedOrigin(src Lot) CostOrigin {
	if src.CostKnown {
		return CostCarried
	}
	return CostNone
}

// epsOf is the quantity below which what is left of q is rounding.
func epsOf(q float64) float64 { return math.Max(1e-12, 1e-12*math.Abs(q)) }

// relieve takes up to qty from k's book by its method; the caller has
// covered it (cover). part receives each lot's share and must not open
// a lot on k: that lot would be relieved in turn.
func (e *engine) relieve(k int32, qty float64, at int64, part func(li int32, q, cost float64)) {
	b := &e.books[k]
	if b.method == Average {
		e.relieveAverage(k, qty, at, part)
		return
	}
	for qty > epsOf(qty) {
		li := b.next()
		if li < 0 {
			return
		}
		q, cost := e.take(k, li, qty, at)
		qty -= q
		part(li, q, cost)
	}
}

// take relieves up to qty from lot li of key k and returns what it
// took. A lot used up leaves its book and is closed at at.
func (e *engine) take(k, li int32, qty float64, at int64) (q, cost float64) {
	b := &e.books[k]
	l := &e.res.Lots[li]
	whole := qty >= l.rem-epsOf(l.rem)
	if whole {
		q, cost = l.rem, l.remCost
	} else {
		q = qty
		cost = l.remCost * q / l.rem
	}
	l.rem -= q
	l.remCost -= cost
	b.qty -= q
	if l.CostKnown {
		b.known -= cost
	} else {
		b.unknown -= q
	}
	if whole {
		l.rem, l.remCost = 0, 0
		l.ClosedAt = at
		b.remove(e, li)
	}
	return q, cost
}

// relieveAverage takes up to qty from an average book's two pools, the
// costed and the uncosted, pro rata by quantity.
func (e *engine) relieveAverage(k int32, qty float64, at int64, part func(li int32, q, cost float64)) {
	b := &e.books[k]
	total := b.qty
	if total <= epsOf(qty) {
		return
	}
	want := math.Min(qty, total)
	pools := [2]int32{b.pool[0], b.pool[1]}
	for i, li := range pools {
		if li < 0 {
			continue
		}
		q := want * e.res.Lots[li].rem / total
		if i == 1 || b.pool[1] < 0 {
			// The last pool takes what is left, so rounding cannot
			// strand a sliver of either.
			q = want
		}
		if q <= 0 {
			continue
		}
		got, cost := e.take(k, li, q, at)
		want -= got
		part(li, got, cost)
	}
}

func (e *engine) disposal(li int32, ev *Event, q, cost float64, kind DisposalKind) int32 {
	l := &e.res.Lots[li]
	e.res.Disposals = append(e.res.Disposals, Disposal{
		Lot: li, Txn: ev.Txn, Account: ev.Account, At: ev.At, Qty: q,
		Cost: cost, CostKnown: l.CostKnown, Kind: kind,
	})
	return int32(len(e.res.Disposals) - 1)
}

func (e *engine) finding(kind FindingKind, k int32, ev *Event, q float64) int32 {
	e.res.Findings = append(e.res.Findings, Finding{Kind: kind, Key: k, Account: ev.Account, At: ev.At, Qty: q, Txn: ev.Txn})
	return int32(len(e.res.Findings) - 1)
}

// undo takes q off finding i, a seed or an implied disposal the replay
// found to be a blip.
func (e *engine) undo(i int32, q float64) {
	e.res.Findings[i].Qty = math.Max(e.res.Findings[i].Qty-q, 0)
}

// cover makes k's book hold at least need: quantity an implied disposal
// took within the blip window comes back first, and a seed of unknown
// cost opens for the rest.
func (e *engine) cover(k int32, need float64, ev *Event) {
	short := need - e.books[k].qty
	if short <= Tolerance(need) {
		return
	}
	short = e.restore(k, short, ev)
	if short > 0 {
		fi := e.finding(FindSeed, k, ev, short)
		e.place(Lot{Key: k, OpenedAt: ev.At, AcqDay: NoDay, Origin: OriginSeed, Txn: ev.Txn, Qty: short,
			Fees: e.keys[k].Fees, seed: true, seedAt: ev.At, finding: fi + 1}, ev)
	}
}

// restore gives back up to need of what implied disposals took from k
// within the blip window, latest first, and returns what it could not.
// A lot comes back as itself: its implied disposal shrinks by what
// returns, and a lot it had closed reopens, so a blip adds no lot. An
// average book takes the quantity back into its pools instead, which a
// merge may have replaced since.
func (e *engine) restore(k int32, need float64, ev *Event) float64 {
	b := &e.books[k]
	restored := 0.0
	var back []int32
	reopened := map[int32]bool{}
	for len(b.pending) > 0 && need > epsOf(need) {
		p := &b.pending[len(b.pending)-1]
		if ev.At-p.at > BlipWindow {
			// Older parts are older still: the rest have expired.
			b.pending = b.pending[:0]
			break
		}
		q := math.Min(need, p.qty)
		cost := p.cost * q / p.qty
		l := &e.res.Lots[p.lot]
		switch {
		case b.method == Average:
			src := *l
			e.place(Lot{Key: k, OpenedAt: ev.At, AcqDay: src.AcqDay, Various: src.Various, Origin: src.Origin,
				Txn: src.Txn, Qty: q, Cost: cost, CostKnown: src.CostKnown, CostOrigin: src.CostOrigin,
				Fees: src.Fees, seq: src.seq, seed: src.seed, seedAt: src.seedAt}, ev)
		case l.ClosedAt != 0:
			l.ClosedAt, l.rem, l.remCost = 0, q, cost
			back = append(back, p.lot)
			reopened[p.lot] = true
		case reopened[p.lot]:
			l.rem += q
			l.remCost += cost
		default:
			// Still open in the book: it grows by what returns.
			l.rem += q
			l.remCost += cost
			b.qty += q
			if l.CostKnown {
				b.known += cost
			} else {
				b.unknown += q
			}
		}
		if b.method != Average {
			d := &e.res.Disposals[p.disposal]
			d.Qty -= q
			d.Cost -= cost
		}
		e.undo(p.finding, q)
		p.qty -= q
		p.cost -= cost
		need -= q
		restored += q
		if p.qty <= epsOf(q) {
			b.pending = b.pending[:len(b.pending)-1]
		}
	}
	b.addMany(e, back)
	if restored > 0 {
		e.finding(FindBlip, k, ev, restored)
	}
	if need <= epsOf(need) {
		return 0
	}
	return need
}

func (e *engine) acquire(ev *Event) {
	k := ev.Key
	co := ev.CostOrigin
	if !ev.HasValue {
		co = CostNone
	}
	e.place(Lot{Key: k, OpenedAt: ev.At, AcqDay: ev.AcqDay, Origin: ev.Origin, Txn: ev.Txn, Qty: ev.Qty,
		Cost: math.Max(ev.Value, 0), CostKnown: ev.HasValue, CostOrigin: co, Fees: e.keys[k].Fees}, ev)
}

// merge folds a lot into an average book's pool of the same kind,
// costed or not: the pool closes and reopens with the combined quantity
// and cost. A merged pool is no seed a blip could take back, so it
// carries no seed finding.
func (e *engine) merge(l Lot, ev *Event) int32 {
	b := &e.books[l.Key]
	i := 0
	if !l.CostKnown {
		i = 1
	}
	if old := b.pool[i]; old >= 0 {
		q, cost := e.take(l.Key, old, e.res.Lots[old].rem, ev.At)
		e.disposal(old, ev, q, cost, DisposeMerge)
		l.Qty += q
		l.Cost += cost
		prev := e.res.Lots[old]
		l.AcqDay, l.Various, l.finding = NoDay, true, 0
		l.seed, l.seedAt = l.seed && prev.seed, min(l.seedAt, prev.seedAt)
		if l.CostOrigin != prev.CostOrigin {
			l.CostOrigin = CostCarried
		}
	}
	return e.open(l, false)
}

func (e *engine) dispose(ev *Event) {
	k := ev.Key
	if !(ev.Qty > 0) {
		return
	}
	e.cover(k, ev.Qty, ev)
	first := len(e.res.Disposals)
	e.relieve(k, ev.Qty, ev.At, func(li int32, q, cost float64) {
		di := e.disposal(li, ev, q, cost, ev.Disposal)
		if ev.HasValue {
			d := &e.res.Disposals[di]
			d.Proceeds = ev.Value * q / ev.Qty
			d.ProceedsKnown = true
		}
	})
	if len(ev.Lots) > 0 {
		e.resolveFromSale(ev, first)
	}
}

// resolveFromSale gives the seed parts of a sale the cost and date of
// the realized lots the source states for it, by anchor's rule
// (resolves). A costed part claims the stated lot of its own date and
// quantity first; the seed parts share what is left, earliest stated
// lot first.
func (e *engine) resolveFromSale(ev *Event, first int) {
	if !slices.ContainsFunc(e.res.Disposals[first:], func(d Disposal) bool {
		return !d.CostKnown && e.res.Lots[d.Lot].seed
	}) {
		return
	}
	left := make([]float64, len(ev.Lots))
	for i, s := range ev.Lots {
		left[i] = s.Qty
	}
	for di := first; di < len(e.res.Disposals); di++ {
		d := e.res.Disposals[di]
		l := e.res.Lots[d.Lot]
		if !d.CostKnown {
			continue
		}
		for i, s := range ev.Lots {
			if left[i] > 0 && s.AcqDay == l.AcqDay && math.Abs(left[i]-d.Qty) <= Tolerance(d.Qty) {
				left[i] = 0
				break
			}
		}
	}
	order := statedOrder(ev.Lots)
	n := len(e.res.Disposals)
	for di := first; di < n; di++ {
		if e.res.Disposals[di].CostKnown || !e.res.Lots[e.res.Disposals[di].Lot].seed {
			continue
		}
		seed := e.res.Disposals[di].Lot
		openDay := DayOf(e.res.Lots[seed].seedAt)
		for _, i := range order {
			d := e.res.Disposals[di]
			s := ev.Lots[i]
			if d.Lot != seed || d.Qty <= epsOf(d.Qty) || !resolves(s, left[i], openDay) {
				continue
			}
			q := math.Min(d.Qty, left[i])
			left[i] -= q
			ri := e.resolve(seed, q, s.Cost*q/s.Qty, s.AcqDay, ev)
			cost := e.res.Lots[ri].Cost
			if q >= d.Qty-epsOf(d.Qty) {
				// The whole part resolved: the row moves to the new lot.
				e.res.Disposals[di].Lot, e.res.Disposals[di].Cost, e.res.Disposals[di].CostKnown = ri, cost, true
				continue
			}
			// The part splits: the resolved quantity gets a row of its
			// own on the new lot, the rest stays on the seed.
			nd := d
			nd.Lot, nd.Qty, nd.Cost, nd.CostKnown = ri, q, cost, true
			if d.ProceedsKnown {
				nd.Proceeds = d.Proceeds * q / d.Qty
				e.res.Disposals[di].Proceeds -= nd.Proceeds
			}
			e.res.Disposals[di].Qty -= q
			e.res.Disposals = append(e.res.Disposals, nd)
		}
	}
}

// statedOrder is the order stated lots are handed to seeds in: earliest
// acquisition first, undated last.
func statedOrder(lots []StatedLot) []int {
	order := make([]int, len(lots))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		da, db := lots[a].AcqDay, lots[b].AcqDay
		if (da == NoDay) != (db == NoDay) {
			if da == NoDay {
				return 1
			}
			return -1
		}
		return cmp.Compare(da, db)
	})
	return order
}

// resolve splits q of seed lot si off into a lot of its own costing
// cost, acquired on acqDay, opened when the seed was and closed at ev,
// so every snapshot between the seed's opening and ev holds it at that
// cost. The seed keeps the rest.
func (e *engine) resolve(si int32, q, cost float64, acqDay int32, ev *Event) int32 {
	seed := e.res.Lots[si]
	ri := e.open(Lot{Key: seed.Key, OpenedAt: seed.OpenedAt, AcqDay: acqDay, Origin: OriginSeed, Txn: seed.Txn,
		Qty: q, Cost: cost, CostKnown: true, CostOrigin: CostResolved, Fees: seed.Fees, seq: seed.seq}, true)
	r := &e.res.Lots[ri]
	r.rem, r.remCost, r.ClosedAt = 0, 0, ev.At
	e.res.Lots[si].Qty -= q
	e.patches = append(e.patches, patch{key: seed.Key, from: seed.OpenedAt, to: ev.At, known: cost, unknownQ: -q})
	e.finding(FindResolved, seed.Key, ev, q)
	return ri
}

func (e *engine) reorg(ev *Event) {
	if slices.ContainsFunc(ev.Into, func(t Into) bool { return t.Qty > 0 }) {
		e.placeParts(ev, e.relieveParts(ev, ev.Qty, DisposeReorg), ev.Qty, OriginReorg, ev.Into)
	}
}

// relieveParts relieves qty from ev.Key by its method, covering it
// first, and returns what each lot gave, in date order: a fifo or lifo
// target then appends the parts rather than inserting each in place.
func (e *engine) relieveParts(ev *Event, qty float64, kind DisposalKind) []part {
	if !(qty > 0) {
		return nil
	}
	e.cover(ev.Key, qty, ev)
	var parts []part
	e.relieve(ev.Key, qty, ev.At, func(li int32, q, cost float64) {
		e.disposal(li, ev, q, cost, kind)
		parts = append(parts, part{li, q, cost})
	})
	slices.SortFunc(parts, func(a, b part) int {
		switch {
		case e.dateBefore(a.li, b.li):
			return -1
		case e.dateBefore(b.li, a.li):
			return 1
		}
		return 0
	})
	return parts
}

// placeParts opens each part relieved from qty again on every target,
// with its date: the quantity scaled to the target's share of qty, the
// cost to its weight. A seed stays a seed. The parts were all relieved
// before any opens, so a target that is the key itself (a reverse split
// stated as a reorg) does not relieve its own new lots. A target's lots
// join its book together (addMany); an average target takes each pool's
// parts in one merge.
func (e *engine) placeParts(ev *Event, parts []part, qty float64, origin Origin, into []Into) {
	for _, t := range into {
		if !(t.Qty > 0) || len(parts) == 0 {
			continue
		}
		carried := func(p part) Lot {
			src := e.res.Lots[p.li]
			return Lot{Key: t.Key, OpenedAt: ev.At, AcqDay: src.AcqDay, Various: src.Various, Origin: origin,
				Txn: ev.Txn, Qty: p.q * t.Qty / qty, Cost: p.cost * t.Weight, CostKnown: src.CostKnown,
				CostOrigin: carriedOrigin(src), Fees: src.Fees, seed: src.seed, seedAt: src.seedAt}
		}
		b := &e.books[t.Key]
		if b.method == Average {
			var pools [2]*Lot
			for _, p := range parts {
				l := carried(p)
				i := 0
				if !l.CostKnown {
					i = 1
				}
				if pl := pools[i]; pl == nil {
					pools[i] = &l
				} else {
					pl.Qty += l.Qty
					pl.Cost += l.Cost
					pl.AcqDay, pl.Various = NoDay, true
					pl.seed, pl.seedAt = pl.seed && l.seed, min(pl.seedAt, l.seedAt)
				}
			}
			for _, pl := range pools {
				if pl != nil {
					e.merge(*pl, ev)
				}
			}
			continue
		}
		lis := make([]int32, 0, len(parts))
		for _, p := range parts {
			if li := e.open(carried(p), true); li >= 0 {
				lis = append(lis, li)
			}
		}
		b.addMany(e, lis)
	}
}

// drained is what a lot held when drain closed it.
type drained struct {
	li      int32
	q, cost float64
}

// drain closes every open lot of k at ev with a disposal of kind and
// empties the book in one sweep. It returns what each lot held, in the
// book's order: date order for fifo and lifo.
func (e *engine) drain(k int32, ev *Event, kind DisposalKind) []drained {
	b := &e.books[k]
	open := b.lots()
	out := make([]drained, 0, len(open))
	for _, li := range open {
		l := &e.res.Lots[li]
		d := drained{li, l.rem, l.remCost}
		l.rem, l.remCost, l.ClosedAt = 0, 0, ev.At
		if d.q > 0 {
			e.disposal(li, ev, d.q, d.cost, kind)
		}
		out = append(out, d)
	}
	b.reset()
	return out
}

// reopen closes every open lot of k with a disposal of kind and opens
// each again through f, which returns the lot to open in its place.
func (e *engine) reopen(k int32, ev *Event, kind DisposalKind, f func(src Lot, q, cost float64) Lot) {
	for _, d := range e.drain(k, ev, kind) {
		src := e.res.Lots[d.li]
		l := f(src, d.q, d.cost)
		l.Key, l.OpenedAt, l.Txn, l.Fees, l.seq = k, ev.At, ev.Txn, src.Fees, src.seq
		l.AcqDay, l.Various = src.AcqDay, src.Various
		l.seed, l.seedAt, l.finding = src.seed, src.seedAt, src.finding
		e.place(l, ev)
	}
}

func (e *engine) split(ev *Event) {
	k := ev.Key
	b := &e.books[k]
	if b.qty <= Tolerance(ev.Qty) {
		// Shares that arrive on an instrument not held are a spin-off: a
		// lot whose cost no event states.
		e.place(Lot{Key: k, OpenedAt: ev.At, AcqDay: NoDay, Origin: OriginReorg, Txn: ev.Txn, Qty: ev.Qty, Fees: e.keys[k].Fees}, ev)
		return
	}
	ratio := (b.qty + ev.Qty) / b.qty
	if !(ratio > 0) || math.Abs(ratio-1) < 1e-12 {
		return
	}
	e.reopen(k, ev, DisposeSplit, func(src Lot, q, cost float64) Lot {
		return Lot{Origin: OriginSplit, Qty: q * ratio, Cost: cost, CostKnown: src.CostKnown, CostOrigin: carriedOrigin(src)}
	})
}

func (e *engine) adjust(ev *Event) {
	k := ev.Key
	b := &e.books[k]
	if !ev.HasValue || b.qty <= 0 || ev.Value == 0 {
		return
	}
	perUnit := ev.Value / b.qty
	e.reopen(k, ev, DisposeAdjust, func(src Lot, q, cost float64) Lot {
		l := Lot{Origin: OriginAdjust, Qty: q, Cost: cost, CostKnown: src.CostKnown, CostOrigin: src.CostOrigin}
		if src.CostKnown {
			l.Cost = math.Max(cost-perUnit*q, 0)
		}
		return l
	})
}

func (e *engine) observe(ev *Event) {
	k := ev.Key
	b := &e.books[k]
	if b.flight > 0 {
		e.finding(FindSkipped, k, ev, ev.Qty)
	} else {
		want := math.Max(ev.Qty, 0)
		switch diff := want - b.qty; {
		case diff > Tolerance(want):
			e.cover(k, want, ev)
		case -diff > Tolerance(want):
			e.imply(k, -diff, ev)
		}
	}
	e.res.Observations = append(e.res.Observations, Observation{Obs: ev.Obs, Key: k, At: ev.At,
		Qty: b.qty, Known: b.known, Unknown: b.unknown, Lots: b.n})
	e.keyObs[k] = append(e.keyObs[k], int32(len(e.res.Observations)-1))
}

// imply relieves qty a snapshot no longer shows and keeps the relieved
// parts restorable for the blip window. It takes the seeds first, the
// latest first, whatever the method: an implied disposal is no sale,
// and a seed is the least the book knows. A seed it takes back within
// the blip window was quantity a snapshot showed before the trade that
// explains it: a blip, not a seed and a disposal, and nothing to
// restore.
func (e *engine) imply(k int32, qty float64, ev *Event) {
	b := &e.books[k]
	// Expired parts can never come back.
	keep := b.pending[:0]
	for _, p := range b.pending {
		if ev.At-p.at <= BlipWindow {
			keep = append(keep, p)
		}
	}
	b.pending = keep
	fi := e.finding(FindImplied, k, ev, qty)
	part := func(li int32, q, cost float64) {
		di := e.disposal(li, ev, q, cost, DisposeImplied)
		b.pending = append(b.pending, pendingPart{lot: li, at: ev.At, qty: q, cost: cost, disposal: di, finding: fi})
	}
	blip := 0.0
	for qty > epsOf(qty) {
		li := b.lastSeed(e)
		if li < 0 {
			break
		}
		l := e.res.Lots[li]
		q, cost := e.take(k, li, math.Min(qty, l.rem), ev.At)
		qty -= q
		if l.finding > 0 && ev.At-l.seedAt <= BlipWindow {
			e.disposal(li, ev, q, cost, DisposeImplied)
			e.undo(l.finding-1, q)
			e.undo(fi, q)
			blip += q
			continue
		}
		part(li, q, cost)
	}
	if qty > epsOf(qty) {
		e.relieve(k, qty, ev.At, part)
	}
	if blip > 0 {
		e.finding(FindBlip, k, ev, blip)
	}
}

// anchor adopts the lots a source states at a snapshot. Before it does,
// it records how the book compares (AnchorCheck) and resolves the seeds
// that survive into the anchor: a stated lot acquired on or before a
// seed's opening that no costed book lot accounts for is what the seed
// was made of. While a move touching the key is in flight the book is
// short of the moving lots or ahead of them, so the anchor waits for
// the next one.
func (e *engine) anchor(ev *Event) {
	k := ev.Key
	b := &e.books[k]
	if b.flight > 0 {
		return
	}
	stated := ev.Lots
	open := b.lots()
	chk := AnchorCheck{Key: k, At: ev.At, BookQty: b.qty}
	left := make([]float64, len(stated))
	byDay := make(map[int32][]int, len(stated))
	for i, s := range stated {
		left[i] = s.Qty
		chk.StatedQty += s.Qty
		byDay[s.AcqDay] = append(byDay[s.AcqDay], i)
	}
	same := len(open) == len(stated)
	var seeds []int32
	for _, li := range open {
		l := e.res.Lots[li]
		if l.seed && !l.CostKnown {
			seeds = append(seeds, li)
			same = false
			continue
		}
		matched := false
		for _, i := range byDay[l.AcqDay] {
			s := stated[i]
			if left[i] <= 0 || math.Abs(s.Qty-l.rem) > Tolerance(s.Qty) {
				continue
			}
			left[i] = 0
			matched = true
			chk.MatchedQty += l.rem
			if l.CostKnown && s.CostKnown {
				chk.BookCost += l.remCost
				chk.StatedCost += s.Cost
			}
			if l.CostKnown != s.CostKnown || (s.CostKnown && math.Abs(s.Cost-l.remCost) > 0.005) {
				same = false
			}
			break
		}
		if !matched {
			same = false
		}
	}
	if same {
		chk.Kept = true
		e.res.Anchors = append(e.res.Anchors, chk)
		return
	}
	var resolved []int32
	order := statedOrder(stated)
	for _, si := range seeds {
		seed := e.res.Lots[si]
		need := seed.rem
		openDay := DayOf(seed.seedAt)
		for _, i := range order {
			s := stated[i]
			if need <= epsOf(seed.rem) || !resolves(s, left[i], openDay) {
				continue
			}
			q := math.Min(need, left[i])
			left[i] -= q
			need -= q
			chk.Resolved += q
			// The resolved part leaves the seed here, and closes as a
			// lot of its own below.
			e.res.Lots[si].rem -= q
			resolved = append(resolved, e.resolve(si, q, s.Cost*q/s.Qty, s.AcqDay, ev))
		}
	}
	e.res.Anchors = append(e.res.Anchors, chk)
	e.drain(k, ev, DisposeAnchor)
	for _, ri := range resolved {
		r := e.res.Lots[ri]
		e.disposal(ri, ev, r.Qty, r.Cost, DisposeAnchor)
	}
	for _, s := range stated {
		co := CostStated
		if !s.CostKnown {
			co = CostNone
		}
		e.place(Lot{Key: k, OpenedAt: ev.At, AcqDay: s.AcqDay, Origin: OriginAnchor, Txn: ev.Txn, Qty: s.Qty,
			Cost: s.Cost, CostKnown: s.CostKnown, CostOrigin: co, Fees: e.keys[k].Fees}, ev)
	}
}

// resolves reports whether a stated lot, with left of it unclaimed, can
// give a seed opened on openDay its cost: it states a cost, and a date
// on or before the seed's opening.
func resolves(s StatedLot, left float64, openDay int32) bool {
	return left > 0 && s.CostKnown && s.AcqDay != NoDay && s.AcqDay <= openDay
}

// applyPatches carries each resolution back over the observations it
// covers: a resolved seed was in the book, at that cost, at every
// snapshot from its opening to the event that resolved it.
func (e *engine) applyPatches() {
	if len(e.patches) == 0 {
		return
	}
	type delta struct{ known, unknown []float64 }
	deltas := map[int32]*delta{}
	for _, p := range e.patches {
		obs := e.keyObs[p.key]
		byAt := func(o int32, t int64) int { return cmp.Compare(e.res.Observations[o].At, t) }
		i, _ := slices.BinarySearchFunc(obs, p.from, byAt)
		j, _ := slices.BinarySearchFunc(obs, p.to, byAt)
		if i >= j {
			continue
		}
		d := deltas[p.key]
		if d == nil {
			d = &delta{known: make([]float64, len(obs)+1), unknown: make([]float64, len(obs)+1)}
			deltas[p.key] = d
		}
		d.known[i] += p.known
		d.known[j] -= p.known
		d.unknown[i] += p.unknownQ
		d.unknown[j] -= p.unknownQ
	}
	for k, d := range deltas {
		var kn, un float64
		for i, oi := range e.keyObs[k] {
			kn += d.known[i]
			un += d.unknown[i]
			o := &e.res.Observations[oi]
			o.Known += kn
			o.Unknown = math.Max(o.Unknown+un, 0)
		}
	}
}
