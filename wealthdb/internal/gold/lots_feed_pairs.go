package gold

import (
	"cmp"
	"math"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotLeg is one leg of a transfer, before pairing.
type lotLeg struct {
	src  *lotSource
	key  int32
	inst string
	txn  string
	acct string
	at   int64
	qty  float64
	in   bool
	// cost is the cost basis a hand-kept equity-transfer ledger row
	// states for what it moved (loader.TransferEntry), 0 when none.
	cost float64
}

// lotCorpLeg is one leg of a corporate action, before grouping.
type lotCorpLeg struct {
	src  *lotSource
	key  int32
	acct string
	txn  string
	at   int64
	qty  float64 // signed: negative leaves, positive arrives
	amt  float64 // in ccy
	ccy  string
	has  bool
	rule lots.CorporateRule
}

// pairTransfers pairs each transfer in with a transfer out of the same
// instrument within lots.PairWindow and lots.PairTolerance. Inside one
// key (a pooled portfolio) a pair cancels: nothing moves. Across keys a
// pair is a move; across sources only between sources of one kind,
// whose instrument ids mean the same thing. Either way, what the
// receipt falls short by leaves as a fee in kind, and what it exceeds
// the send by opens a lot of unknown cost. What pairs with nothing is a
// receipt of unknown cost, or a departure with no proceeds.
func (f *lotFeed) pairTransfers() {
	legs := f.legs
	slices.SortStableFunc(legs, func(a, b lotLeg) int {
		if c := cmp.Compare(a.inst, b.inst); c != 0 {
			return c
		}
		if c := cmp.Compare(a.at, b.at); c != 0 {
			return c
		}
		return cmp.Compare(a.txn, b.txn)
	})
	mate := make([]int, len(legs))
	for i := range mate {
		mate[i] = -1
	}
	// open lists the earlier legs of the instrument still unpaired, per
	// direction (outs, ins), oldest first: a leg looks only at the other
	// direction's, within the window, latest first.
	var open [2][]int
	dir := func(l *lotLeg) int {
		if l.in {
			return 1
		}
		return 0
	}
	for i := range legs {
		a := &legs[i]
		if i == 0 || legs[i-1].inst != a.inst {
			open[0], open[1] = open[0][:0], open[1][:0]
		}
		other := 1 - dir(a)
		q := open[other][:0]
		for _, j := range open[other] {
			if mate[j] < 0 && a.at-legs[j].at <= lots.PairWindow {
				q = append(q, j)
			}
		}
		open[other] = q
		best, bestDiff := -1, math.Inf(1)
		for x := len(q) - 1; x >= 0; x-- {
			b := &legs[q[x]]
			out, recv := b, a
			if b.in {
				out, recv = a, b
			}
			if out.src != recv.src && out.src.kind != recv.src.kind {
				continue
			}
			diff := math.Abs(out.qty - recv.qty)
			if diff > lots.PairTolerance*math.Max(out.qty, recv.qty) {
				continue
			}
			if diff < bestDiff {
				best, bestDiff = q[x], diff
			}
		}
		if best >= 0 {
			mate[i], mate[best] = best, i
		} else {
			open[dir(a)] = append(open[dir(a)], i)
		}
	}
	for i := range legs {
		a := legs[i]
		j := mate[i]
		if j < 0 {
			f.unpaired(a)
			continue
		}
		if a.in {
			// Each pair is emitted once, from its outgoing leg.
			continue
		}
		f.pair(a, legs[j])
	}
	f.legs = nil
}

func (f *lotFeed) unpaired(l lotLeg) {
	ev := lots.Event{At: l.at, Key: l.key, Txn: l.txn, Account: l.acct, Qty: l.qty, AcqDay: lots.DayOf(l.at)}
	if l.in {
		ev.Kind, ev.Origin = lots.Acquire, lots.OriginTransferIn
		if l.cost > 0 {
			// A hand-kept ledger row states the basis it arrived with.
			ev.CostOrigin, ev.Value, ev.HasValue = lots.CostStated, l.cost, true
		}
		// The cost and holding period of a receipt nothing explains are
		// unknown.
		ev.AcqDay = lots.NoDay
	} else {
		ev.Kind, ev.Disposal = lots.Dispose, lots.DisposeTransferOut
	}
	f.add(ev, lotAux{})
	f.findings = append(f.findings, lots.Finding{Kind: lots.FindUnpairedTransfer, Key: l.key, Account: l.acct, At: l.at, Qty: l.qty, Txn: l.txn})
}

// pair moves a paired transfer's lots: they leave at the departure and
// arrive at the receipt, so a sale on either side between the two
// relieves what each side held. A receipt recorded before its
// departure moves the lots at the receipt; the sender's snapshots until
// the departure still show them, and are not reconciled (lots.Flight).
func (f *lotFeed) pair(out, in lotLeg) {
	moved := math.Min(out.qty, in.qty)
	if out.key != in.key {
		f.transits++
		departAt := out.at
		if in.at < out.at {
			departAt = in.at
			f.add(lots.Event{At: in.at, Kind: lots.Flight, Key: out.key, Txn: in.txn, Account: out.acct}, lotAux{})
			f.add(lots.Event{At: out.at, Kind: lots.Land, Key: out.key, Txn: in.txn, Account: out.acct}, lotAux{})
		}
		f.add(lots.Event{At: departAt, Kind: lots.Depart, Key: out.key, Transit: f.transits, Txn: in.txn,
			Account: out.acct, Qty: moved}, lotAux{})
		f.add(lots.Event{At: in.at, Kind: lots.Arrive, Key: in.key, Transit: f.transits, Txn: in.txn,
			Account: in.acct}, lotAux{})
	}
	switch {
	case out.qty-moved > lots.Tolerance(out.qty):
		// The receipt is short by a network fee: that quantity left as
		// a fee, at its market value.
		f.add(lots.Event{At: out.at, Kind: lots.Dispose, Key: out.key, Txn: out.txn, Account: out.acct,
			Qty: out.qty - moved, Disposal: lots.DisposeFee}, lotAux{fmv: true})
	case in.qty-moved > lots.Tolerance(in.qty):
		ev := lots.Event{At: in.at, Kind: lots.Acquire, Key: in.key, Txn: in.txn, Account: in.acct,
			Qty: in.qty - moved, AcqDay: lots.NoDay, Origin: lots.OriginTransferIn}
		f.add(ev, lotAux{})
	}
}

// corporateActions turns the corporate action legs into events. Legs
// group by account and day. A rule other than CorpReorg acts on its
// leg alone. CorpReorg legs pair within their group: each outgoing
// instrument hands its lots to the incoming ones (docs/LOTS.md §3.5).
func (f *lotFeed) corporateActions() {
	type gk struct {
		src  string
		acct string
		day  int32
	}
	groups := map[gk][]lotCorpLeg{}
	var order []gk
	for _, c := range f.corp {
		switch c.rule {
		case lots.CorpIgnore:
			continue
		case lots.CorpReturnOfCapital:
			if c.has {
				f.add(lots.Event{At: c.at, Kind: lots.Adjust, Key: c.key, Txn: c.txn, Account: c.acct, Value: math.Abs(c.amt), HasValue: true},
					lotAux{ccy: c.ccy})
			}
			continue
		case lots.CorpCashInLieu, lots.CorpExpiry:
			if c.qty >= 0 {
				// Cash in lieu names no quantity: the fraction leaves at
				// the next snapshot.
				continue
			}
			ev := lots.Event{At: c.at, Kind: lots.Dispose, Key: c.key, Txn: c.txn, Account: c.acct, Qty: -c.qty,
				Disposal: lots.DisposeCashInLieu}
			switch {
			case c.rule == lots.CorpExpiry:
				ev.Disposal, ev.HasValue = lots.DisposeExpiry, true
			case c.has:
				ev.Value, ev.HasValue = math.Abs(c.amt), true
			}
			f.add(ev, lotAux{ccy: c.ccy})
			continue
		}
		g := gk{c.src.id, f.keyAccount(c.src, c.acct), lots.DayOf(c.at)}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], c)
	}
	for _, g := range order {
		f.reorgGroup(groups[g])
	}
	f.corp = nil
}

// reorgGroup pairs one account's corporate action legs of one day. The
// same leg stated twice (a source with two feeds) counts once.
func (f *lotFeed) reorgGroup(legs []lotCorpLeg) {
	var outs, ins []lotCorpLeg
	type legID struct {
		out bool
		key int32
		qty float64
	}
	seen := map[legID]int{}
	for _, l := range legs {
		if l.qty == 0 {
			continue
		}
		side := &ins
		if l.qty < 0 {
			side = &outs
		}
		id := legID{l.qty < 0, l.key, math.Abs(l.qty)}
		if i, dup := seen[id]; dup {
			if !(*side)[i].has && l.has {
				(*side)[i] = l
			}
			continue
		}
		seen[id] = len(*side)
		*side = append(*side, l)
	}
	switch {
	case len(outs) == 0:
		for _, l := range ins {
			f.add(lots.Event{At: l.at, Kind: lots.Split, Key: l.key, Txn: l.txn, Account: l.acct, Qty: l.qty}, lotAux{})
		}
	case len(ins) == 0:
		for _, l := range outs {
			ev := lots.Event{At: l.at, Kind: lots.Dispose, Key: l.key, Txn: l.txn, Account: l.acct, Qty: -l.qty, Disposal: lots.DisposeReorg}
			if l.has {
				// Shares that leave for cash alone: a cash merger.
				ev.Disposal, ev.Value, ev.HasValue = lots.DisposeTender, math.Abs(l.amt), true
			}
			f.add(ev, lotAux{ccy: l.ccy})
		}
	case len(outs) == 1:
		// One instrument becomes one or several: its lots are shared
		// among them by their stated values.
		o, w := outs[0], legWeights(ins)
		ev := lots.Event{At: o.at, Kind: lots.Reorg, Key: o.key, Txn: o.txn, Account: o.acct, Qty: -o.qty}
		for j, n := range ins {
			ev.At = max(ev.At, n.at)
			ev.Into = append(ev.Into, lots.Into{Key: n.key, Qty: n.qty, Weight: w[j]})
		}
		if len(ins) == 1 {
			ev.Txn, ev.Account = ins[0].txn, ins[0].acct
		}
		f.add(ev, lotAux{})
	case len(ins) == 1:
		// Several instruments become one: each hands over its lots and
		// its share of the new quantity, by the stated values.
		n, w := ins[0], legWeights(outs)
		for i, o := range outs {
			f.add(lots.Event{At: max(o.at, n.at), Kind: lots.Reorg, Key: o.key, Txn: n.txn, Account: n.acct,
				Qty: -o.qty, Into: []lots.Into{{Key: n.key, Qty: n.qty * w[i], Weight: 1}}}, lotAux{})
		}
	default:
		// Several on each side: pair by the closest stated value, else
		// in order; what is left acts alone.
		used := make([]bool, len(ins))
		var restOut []lotCorpLeg
		for _, o := range outs {
			best := -1
			for j, n := range ins {
				if used[j] {
					continue
				}
				if best < 0 || (o.has && n.has && math.Abs(math.Abs(o.amt)-math.Abs(n.amt)) < math.Abs(math.Abs(o.amt)-math.Abs(ins[best].amt))) {
					best = j
				}
			}
			if best < 0 {
				restOut = append(restOut, o)
				continue
			}
			used[best] = true
			n := ins[best]
			f.add(lots.Event{At: max(o.at, n.at), Kind: lots.Reorg, Key: o.key, Txn: n.txn, Account: n.acct,
				Qty: -o.qty, Into: []lots.Into{{Key: n.key, Qty: n.qty, Weight: 1}}}, lotAux{})
		}
		var restIn []lotCorpLeg
		for j, n := range ins {
			if !used[j] {
				restIn = append(restIn, n)
			}
		}
		if len(restOut) > 0 || len(restIn) > 0 {
			f.reorgGroup(append(restOut, restIn...))
		}
	}
}

// legWeights shares one side of a reorg by the legs' stated values,
// else equally.
func legWeights(legs []lotCorpLeg) []float64 {
	w := make([]float64, len(legs))
	total := 0.0
	for _, l := range legs {
		if !l.has {
			total = 0
			break
		}
		total += math.Abs(l.amt)
	}
	for i, l := range legs {
		if total > 0 {
			w[i] = math.Abs(l.amt) / total
		} else {
			w[i] = 1 / float64(len(legs))
		}
	}
	return w
}
