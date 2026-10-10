package lots

import (
	"math"
	"testing"
)

const day = int64(86400)

// at is noon of Unix day d.
func at(d int64) int64 { return d*day + day/2 }

func buy(k int32, d int64, txn string, qty, cost float64) Event {
	return Event{At: at(d), Kind: Acquire, Key: k, Txn: txn, Qty: qty, Value: cost, HasValue: true,
		AcqDay: int32(d), Origin: OriginBuy, CostOrigin: CostTrade}
}

func sell(k int32, d int64, txn string, qty, proceeds float64) Event {
	return Event{At: at(d), Kind: Dispose, Key: k, Txn: txn, Qty: qty, Value: proceeds, HasValue: true, Disposal: DisposeSell}
}

func obs(k int32, d int64, qty float64, id int32) Event {
	return Event{At: at(d) + 3600, Kind: Observe, Key: k, Txn: "obs", Qty: qty, Obs: id}
}

func run(t *testing.T, methods []Method, evs ...Event) Result {
	t.Helper()
	keys := make([]KeySpec, len(methods))
	for i, m := range methods {
		keys[i] = KeySpec{ID: string(rune('A' + i)), Method: m}
	}
	SortEvents(evs)
	return Run(keys, evs)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// sales sums the cost relieved by each sale transaction.
func sales(r Result) map[string]float64 {
	out := map[string]float64{}
	for _, d := range r.Disposals {
		if d.Kind == DisposeSell {
			out[d.Txn] += d.Cost
		}
	}
	return out
}

func TestMethodsRelieveInTheirOrder(t *testing.T) {
	// Three lots at unit costs 10, 30, 20; a sale of 15 takes 10 from the
	// first lot in order and 5 from the second.
	cases := []struct {
		m    Method
		cost float64
	}{
		{FIFO, 10*10 + 5*30},
		{LIFO, 10*20 + 5*30},
		{HIFO, 10*30 + 5*20},
		{LOFO, 10*10 + 5*20},
		{Average, 15 * 20},
	}
	for _, c := range cases {
		r := run(t, []Method{c.m},
			buy(0, 1, "b1", 10, 100), buy(0, 2, "b2", 10, 300), buy(0, 3, "b3", 10, 200),
			sell(0, 4, "s1", 15, 600), obs(0, 5, 15, 1))
		if got := sales(r)["s1"]; !near(got, c.cost) {
			t.Errorf("%s: relieved cost %v, want %v", c.m, got, c.cost)
		}
		o := r.Observations[0]
		if !near(o.Qty, 15) || !near(o.Known, 600-c.cost) || o.Unknown != 0 {
			t.Errorf("%s: book %+v, want 15 units at %v", c.m, o, 600-c.cost)
		}
	}
}

func TestPartialReliefKeepsUnitCost(t *testing.T) {
	r := run(t, []Method{HIFO},
		buy(0, 1, "b1", 4, 40), buy(0, 2, "b2", 4, 80),
		sell(0, 3, "s1", 1, 30), sell(0, 4, "s2", 1, 30), sell(0, 5, "s3", 3, 90))
	got := sales(r)
	if !near(got["s1"], 20) || !near(got["s2"], 20) || !near(got["s3"], 40+10) {
		t.Fatalf("relief %v", got)
	}
}

func TestDisposalBeyondTheBookSeeds(t *testing.T) {
	r := run(t, []Method{FIFO}, buy(0, 1, "b1", 2, 20), sell(0, 2, "s1", 5, 100), obs(0, 3, 0, 1))
	var seeds, unknown float64
	for _, l := range r.Lots {
		if l.Origin == OriginSeed {
			seeds += l.Qty
		}
	}
	for _, d := range r.Disposals {
		if !d.CostKnown {
			unknown += d.Qty
		}
	}
	if !near(seeds, 3) || !near(unknown, 3) {
		t.Fatalf("seeded %v, relieved uncosted %v; want 3 and 3", seeds, unknown)
	}
	// The seed is relieved first: an unknown date sorts before any.
	if r.Disposals[0].CostKnown {
		t.Errorf("first part should be the seed: %+v", r.Disposals[0])
	}
	if len(r.Findings) != 1 || r.Findings[0].Kind != FindSeed {
		t.Errorf("findings %+v", r.Findings)
	}
}

func TestObservationSeedsAndImplies(t *testing.T) {
	r := run(t, []Method{FIFO},
		obs(0, 1, 10, 1),       // nothing in the book: a seed of 10
		buy(0, 2, "b1", 5, 50), // 15 held
		obs(0, 3, 12, 2),       // 3 implied, FIFO from the seed
		obs(0, 100, 12, 3))     // agrees
	if len(r.Observations) != 3 {
		t.Fatalf("observations %d", len(r.Observations))
	}
	o := r.Observations[1]
	if !near(o.Qty, 12) || !near(o.Known, 50) || !near(o.Unknown, 7) {
		t.Fatalf("after the implied disposal: %+v", o)
	}
	var implied float64
	for _, d := range r.Disposals {
		if d.Kind == DisposeImplied {
			implied += d.Qty
			if d.Realized() {
				t.Errorf("an implied disposal realized: %+v", d)
			}
		}
	}
	if !near(implied, 3) {
		t.Fatalf("implied %v", implied)
	}
}

func TestBlipRestoresTheLots(t *testing.T) {
	// A snapshot leaves the row out: the account has other rows, so the
	// key observes zero; the next snapshot shows it again.
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 10, 100),
		obs(0, 2, 0, 1),
		obs(0, 30, 10, 2),
		sell(0, 40, "s1", 10, 200))
	if got := sales(r)["s1"]; !near(got, 100) {
		t.Fatalf("the restored lot should carry its cost: relieved %v", got)
	}
	var kinds []FindingKind
	for _, f := range r.Findings {
		kinds = append(kinds, f.Kind)
	}
	// The implied disposal the blip undid in full leaves no finding of
	// its own.
	if len(kinds) != 1 || kinds[0] != FindBlip {
		t.Fatalf("findings %v", kinds)
	}
	for _, l := range r.Lots {
		if l.Origin == OriginSeed {
			t.Errorf("a blip must not seed: %+v", l)
		}
	}
	// The restored lot keeps its date, so the sale is long-term.
	last := r.Lots[r.Disposals[len(r.Disposals)-1].Lot]
	if last.AcqDay != 1 {
		t.Errorf("restored lot dated %d, want 1", last.AcqDay)
	}
}

func TestBlipWindowExpires(t *testing.T) {
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 10, 100),
		obs(0, 2, 0, 1),
		obs(0, 60, 10, 2))
	var seeded bool
	for _, l := range r.Lots {
		seeded = seeded || l.Origin == OriginSeed
	}
	if !seeded {
		t.Fatal("quantity back after the window should seed")
	}
}

func TestSaleDuringBlipTakesThePendingLots(t *testing.T) {
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 10, 100),
		obs(0, 2, 0, 1),
		sell(0, 5, "s1", 10, 300))
	if got := sales(r)["s1"]; !near(got, 100) {
		t.Fatalf("relieved %v, want the pending lot's 100", got)
	}
}

// moveAt is a transfer of qty from key from to key to whose receipt is
// recorded at day in and whose departure at day out.
func moveAt(from, to int32, out, in int64, txn string, qty float64, transit int32) []Event {
	if in < out {
		return []Event{
			{At: at(in), Kind: Flight, Key: from, Txn: txn},
			{At: at(in), Kind: Depart, Key: from, Txn: txn, Qty: qty, Transit: transit},
			{At: at(in), Kind: Arrive, Key: to, Txn: txn, Transit: transit},
			{At: at(out), Kind: Land, Key: from, Txn: txn},
		}
	}
	return []Event{
		{At: at(out), Kind: Depart, Key: from, Txn: txn, Qty: qty, Transit: transit},
		{At: at(in), Kind: Arrive, Key: to, Txn: txn, Transit: transit},
	}
}

func TestMoveCarriesCostAndDate(t *testing.T) {
	ev := append([]Event{
		buy(0, 1, "b1", 10, 100),
		obs(0, 400, 10, 1), // in flight: not reconciled
	}, moveAt(0, 1, 401, 400, "t1", 6, 1)...)
	ev = append(ev,
		obs(0, 402, 4, 2),
		obs(1, 402, 6, 3),
		sell(1, 403, "s1", 6, 120),
	)
	r := run(t, []Method{FIFO, LIFO}, ev...)
	if got := sales(r)["s1"]; !near(got, 60) {
		t.Fatalf("moved lots relieved %v, want 60", got)
	}
	if f := r.Findings[0]; f.Kind != FindSkipped || f.At != at(400)+3600 {
		t.Errorf("the observation in flight is not reconciled: %+v", f)
	}
	for _, f := range r.Findings {
		if f.Kind == FindSeed || f.Kind == FindImplied {
			t.Errorf("unexpected finding %+v", f)
		}
	}
	var moved Lot
	for _, l := range r.Lots {
		if l.Key == 1 {
			moved = l
		}
	}
	if moved.AcqDay != 1 || moved.CostOrigin != CostCarried || moved.Origin != OriginTransferIn || moved.Method != LIFO {
		t.Errorf("moved lot %+v", moved)
	}
}

func TestReorgAndSplit(t *testing.T) {
	r := run(t, []Method{FIFO, FIFO},
		buy(0, 1, "b1", 10, 100),
		Event{At: at(2), Kind: Split, Key: 0, Txn: "sp", Qty: 10},
		Event{At: at(3), Kind: Split, Key: 0, Txn: "sp2", Qty: 20},
		Event{At: at(4), Kind: Reorg, Key: 0, Txn: "m1", Qty: 40, Into: []Into{{Key: 1, Qty: 10, Weight: 1}}},
		obs(0, 5, 0, 1), obs(1, 5, 10, 2),
		sell(1, 6, "s1", 5, 80))
	if o := r.Observations[1]; !near(o.Qty, 10) || !near(o.Known, 100) {
		t.Fatalf("new key %+v", o)
	}
	if o := r.Observations[0]; o.Qty != 0 || o.Lots != 0 {
		t.Fatalf("old key %+v", o)
	}
	if got := sales(r)["s1"]; !near(got, 50) {
		t.Fatalf("relieved %v", got)
	}
	if l := r.Lots[r.Disposals[len(r.Disposals)-1].Lot]; l.AcqDay != 1 {
		t.Errorf("reorg should keep the date: %+v", l)
	}
}

func TestAdjustLowersCost(t *testing.T) {
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 10, 100), buy(0, 2, "b2", 10, 300),
		Event{At: at(3), Kind: Adjust, Key: 0, Txn: "roc", Value: 40, HasValue: true},
		obs(0, 4, 20, 1))
	if o := r.Observations[0]; !near(o.Known, 360) || o.Lots != 2 {
		t.Fatalf("after return of capital %+v", o)
	}
}

func TestAverageMergesAndPools(t *testing.T) {
	r := run(t, []Method{Average},
		buy(0, 1, "b1", 10, 100),
		buy(0, 2, "b2", 10, 300),
		obs(0, 3, 30, 1), // a seed of 10 joins the uncosted pool
		sell(0, 4, "s1", 15, 0))
	// 15 of 30 leave pro rata: 10 of the costed 20 at 20 each, 5 of the
	// uncosted 10.
	var costed, uncosted float64
	for _, d := range r.Disposals {
		if d.Kind != DisposeSell {
			continue
		}
		if d.CostKnown {
			costed += d.Cost
		} else {
			uncosted += d.Qty
		}
	}
	if !near(costed, 200) || !near(uncosted, 5) {
		t.Fatalf("costed %v, uncosted qty %v", costed, uncosted)
	}
	o := r.Observations[0]
	if o.Lots != 2 || !near(o.Known, 400) || !near(o.Unknown, 10) {
		t.Fatalf("pools %+v", o)
	}
}

func TestAnchorKeptWhenTheBookAgrees(t *testing.T) {
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 10, 100),
		Event{At: at(2), Kind: Anchor, Key: 0, Txn: "a1", Lots: []StatedLot{{Qty: 10, Cost: 100, CostKnown: true, AcqDay: 1}}})
	if len(r.Anchors) != 1 || !r.Anchors[0].Kept || !near(r.Anchors[0].MatchedQty, 10) {
		t.Fatalf("anchor %+v", r.Anchors)
	}
	if len(r.Lots) != 1 {
		t.Fatalf("a kept anchor opens nothing: %d lots", len(r.Lots))
	}
}

func TestAnchorResolvesSeeds(t *testing.T) {
	// History opens with 10 units of unknown cost. A later fetch states
	// the 6 that survive a sale: acquired on day 0 at 60.
	r := run(t, []Method{FIFO},
		obs(0, 1, 10, 1),
		obs(0, 2, 10, 2),
		sell(0, 3, "s1", 4, 80),
		obs(0, 4, 6, 3),
		Event{At: at(5), Kind: Anchor, Key: 0, Txn: "a1", Lots: []StatedLot{{Qty: 6, Cost: 60, CostKnown: true, AcqDay: 0}}},
		obs(0, 5, 6, 4),
		sell(0, 6, "s2", 6, 90))
	want := []struct{ known, unknown float64 }{{60, 4}, {60, 4}, {60, 0}, {60, 0}}
	for i, w := range want {
		o := r.Observations[i]
		if !near(o.Known, w.known) || !near(o.Unknown, w.unknown) {
			t.Errorf("observation %d: %+v, want known %v unknown %v", i, o, w.known, w.unknown)
		}
	}
	if got := sales(r)["s2"]; !near(got, 60) {
		t.Errorf("after the anchor the stated lots are relieved: %v", got)
	}
	if r.Anchors[0].Resolved != 6 {
		t.Errorf("anchor %+v", r.Anchors[0])
	}
	// The ledger balances: every lot's disposals sum to its quantity.
	balanced(t, r)
}

func TestSaleResolvesSeedFromStatedLots(t *testing.T) {
	r := run(t, []Method{FIFO},
		obs(0, 1, 10, 1),
		Event{At: at(3), Kind: Dispose, Key: 0, Txn: "s1", Qty: 4, Value: 80, HasValue: true, Disposal: DisposeSell,
			Lots: []StatedLot{{Qty: 4, Cost: 48, CostKnown: true, AcqDay: -100}}},
		obs(0, 4, 6, 2))
	if got := sales(r)["s1"]; !near(got, 48) {
		t.Fatalf("resolved cost %v", got)
	}
	if o := r.Observations[0]; !near(o.Known, 48) || !near(o.Unknown, 6) {
		t.Fatalf("the snapshot before the sale held the resolved part: %+v", o)
	}
	if o := r.Observations[1]; o.Known != 0 || !near(o.Unknown, 6) {
		t.Fatalf("after the sale: %+v", o)
	}
	d := r.Disposals[0]
	if l := r.Lots[d.Lot]; l.CostOrigin != CostResolved || l.AcqDay != -100 {
		t.Errorf("disposal's lot %+v", l)
	}
	balanced(t, r)
}

func TestLotIDsAreDeterministic(t *testing.T) {
	mk := func() []Event {
		return []Event{buy(0, 1, "b1", 1, 1), buy(0, 2, "b2", 1, 2), sell(0, 3, "s", 1, 3)}
	}
	a, b := run(t, []Method{FIFO}, mk()...), run(t, []Method{FIFO}, mk()...)
	for i := range a.Lots {
		if a.Lots[i].ID != b.Lots[i].ID || len(a.Lots[i].ID) != 32 {
			t.Fatalf("ids differ or malformed: %q %q", a.Lots[i].ID, b.Lots[i].ID)
		}
	}
	if a.Lots[0].ID == a.Lots[1].ID {
		t.Fatal("two lots share an id")
	}
}

func TestSameSecondBuyBeforeSell(t *testing.T) {
	ev := []Event{sell(0, 1, "a-sell", 5, 50), buy(0, 1, "z-buy", 5, 40)}
	r := run(t, []Method{FIFO}, ev...)
	for _, l := range r.Lots {
		if l.Origin == OriginSeed {
			t.Fatal("a same-second buy must land before the sell")
		}
	}
}

// balanced checks the ledger: a lot's disposals sum to its quantity
// when it is closed, and to no more while it is open.
func balanced(t *testing.T, r Result) {
	t.Helper()
	out := make([]float64, len(r.Lots))
	for _, d := range r.Disposals {
		out[d.Lot] += d.Qty
	}
	for i, l := range r.Lots {
		if l.ClosedAt != 0 && !near(out[i], l.Qty) {
			t.Errorf("closed lot %d (%s) quantity %v, disposed %v", i, l.Origin, l.Qty, out[i])
		}
		if out[i] > l.Qty+1e-9 {
			t.Errorf("lot %d over-disposed: %v of %v", i, out[i], l.Qty)
		}
	}
}

func TestLedgerBalancesAcrossKinds(t *testing.T) {
	ev := append([]Event{buy(0, 1, "b1", 10, 100), buy(0, 2, "b2", 5, 80)}, moveAt(0, 1, 3, 3, "m", 12, 1)...)
	ev = append(ev, obs(1, 4, 11, 1))
	ev = append(ev, moveAt(1, 2, 5, 5, "m2", 11, 2)...)
	ev = append(ev, buy(2, 6, "b3", 4, 10), sell(2, 7, "s", 9, 100), obs(2, 8, 6, 2))
	r := run(t, []Method{FIFO, HIFO, Average}, ev...)
	balanced(t, r)
}

func TestSnapshotAheadOfTheTradeIsABlip(t *testing.T) {
	// The snapshot shows a buy a day before the transaction that makes
	// it: a seed, then an implied disposal. The implied disposal takes
	// the seed, not the bought lot, even under LIFO, and the pair counts
	// as one blip.
	r := run(t, []Method{LIFO},
		buy(0, 1, "b1", 10, 100),
		obs(0, 2, 15, 1),
		buy(0, 3, "b2", 5, 70),
		obs(0, 3, 15, 2))
	o := r.Observations[1]
	if !near(o.Known, 170) || o.Unknown != 0 || o.Lots != 2 {
		t.Fatalf("book after the trade lands: %+v", o)
	}
	if len(r.Findings) != 1 || r.Findings[0].Kind != FindBlip || !near(r.Findings[0].Qty, 5) {
		t.Fatalf("findings %+v", r.Findings)
	}
	balanced(t, r)
}

// byKey sums the open quantity and cost of each key's lots after a run.
func byKey(r Result) map[int32][2]float64 {
	out := map[int32][2]float64{}
	for _, l := range r.Lots {
		if l.ClosedAt == 0 {
			v := out[l.Key]
			v[0] += l.rem
			v[1] += l.remCost
			out[l.Key] = v
		}
	}
	return out
}

func TestReorgSharesEveryLotAmongItsTargets(t *testing.T) {
	// Two lots of OLD become NEW1 and NEW2, weighted 3:1 by value: each
	// new instrument gets a slice of both lots, with their dates.
	r := run(t, []Method{FIFO, FIFO, FIFO},
		buy(0, 1, "b1", 5, 100), buy(0, 2, "b2", 5, 300),
		Event{At: at(3), Kind: Reorg, Key: 0, Txn: "r", Qty: 10, Into: []Into{{Key: 1, Qty: 6, Weight: 0.75}, {Key: 2, Qty: 2, Weight: 0.25}}})
	got := byKey(r)
	if !near(got[1][0], 6) || !near(got[1][1], 300) || !near(got[2][0], 2) || !near(got[2][1], 100) {
		t.Fatalf("targets %+v", got)
	}
	days := map[int32]map[int32]bool{1: {}, 2: {}}
	for _, l := range r.Lots {
		if l.ClosedAt == 0 && l.Key != 0 {
			days[l.Key][l.AcqDay] = true
		}
	}
	if len(days[1]) != 2 || len(days[2]) != 2 {
		t.Errorf("each target should hold both dates: %v", days)
	}
}

func TestReorgOntoItsOwnKeyIsASplit(t *testing.T) {
	// A 1:2 reverse split stated as a reorg onto the same instrument.
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 10, 100), buy(0, 2, "b2", 10, 500),
		Event{At: at(3), Kind: Reorg, Key: 0, Txn: "r", Qty: 20, Into: []Into{{Key: 0, Qty: 10, Weight: 1}}},
		obs(0, 4, 10, 1))
	open := 0
	for _, l := range r.Lots {
		if l.ClosedAt == 0 {
			open++
			if want := map[int32]float64{1: 100, 2: 500}[l.AcqDay]; !near(l.rem, 5) || !near(l.remCost, want) {
				t.Errorf("lot %+v", l)
			}
		}
	}
	if open != 2 || len(r.Findings) != 0 {
		t.Fatalf("open lots %d, findings %+v", open, r.Findings)
	}
}

func TestAverageKeepsEveryLotInItsPools(t *testing.T) {
	t.Run("seed", func(t *testing.T) {
		// A pool of 10 at unknown cost, a snapshot of 15 seeds 5 more,
		// and a sale of 15 takes all of it.
		r := run(t, []Method{Average},
			Event{At: at(1), Kind: Acquire, Key: 0, Txn: "in", Qty: 10, AcqDay: NoDay, Origin: OriginTransferIn},
			obs(0, 2, 15, 1),
			sell(0, 3, "s1", 15, 150),
			obs(0, 4, 0, 2))
		if o := r.Observations[1]; o.Qty != 0 || o.Lots != 0 {
			t.Fatalf("after the sale %+v", o)
		}
	})
	t.Run("blip", func(t *testing.T) {
		r := run(t, []Method{Average},
			buy(0, 1, "b1", 10, 100), obs(0, 2, 8, 1), obs(0, 3, 10, 2),
			sell(0, 4, "s1", 10, 200), obs(0, 5, 0, 3))
		if got := sales(r)["s1"]; !near(got, 100) {
			t.Fatalf("relieved %v", got)
		}
		if o := r.Observations[2]; o.Qty != 0 {
			t.Fatalf("after the sale %+v", o)
		}
	})
	t.Run("anchor", func(t *testing.T) {
		r := run(t, []Method{Average},
			Event{At: at(1), Kind: Anchor, Key: 0, Txn: "a", Lots: []StatedLot{
				{Qty: 4, Cost: 40, CostKnown: true, AcqDay: 1}, {Qty: 6, Cost: 120, CostKnown: true, AcqDay: 1}}},
			obs(0, 1, 10, 1))
		if o := r.Observations[0]; !near(o.Qty, 10) || !near(o.Known, 160) {
			t.Fatalf("anchored %+v", o)
		}
	})
}

func TestAnchorWaitsForAMoveInFlight(t *testing.T) {
	// The receipt is recorded before the departure, and the sending
	// key's snapshot between them still states the lots that moved: its
	// anchor must not adopt them again.
	anchor := Event{At: at(3), Kind: Anchor, Key: 0, Txn: "a", Lots: []StatedLot{{Qty: 100, Cost: 1000, CostKnown: true, AcqDay: 1}}}
	ev := append([]Event{buy(0, 1, "b1", 100, 1000)}, moveAt(0, 1, 4, 2, "m", 100, 1)...)
	ev = append(ev, anchor, obs(0, 3, 100, 1), obs(0, 5, 0, 2), obs(1, 5, 100, 3))
	r := run(t, []Method{FIFO, FIFO}, ev...)
	if o := r.Observations[1]; o.Qty != 0 || o.Lots != 0 {
		t.Fatalf("the sender after the move %+v", o)
	}
	if o := r.Observations[2]; !near(o.Qty, 100) || !near(o.Known, 1000) {
		t.Fatalf("the receiver after the move %+v", o)
	}
	for _, f := range r.Findings {
		if f.Kind == FindImplied {
			t.Errorf("implied disposal %+v", f)
		}
	}
}

func TestARecentSeedThatReturnsIsOneBlip(t *testing.T) {
	// A snapshot shows 4 more than the trades, then none, then all 10
	// again: the 4 stay a seed, and the dip is a blip of 10.
	r := run(t, []Method{FIFO},
		buy(0, 1, "b1", 6, 60), obs(0, 2, 10, 1), obs(0, 3, 0, 2), obs(0, 4, 10, 3))
	sum := map[FindingKind]float64{}
	for _, f := range r.Findings {
		sum[f.Kind] += f.Qty
	}
	if !near(sum[FindSeed], 4) || !near(sum[FindBlip], 10) || sum[FindImplied] != 0 {
		t.Fatalf("findings %v", sum)
	}
	if o := r.Observations[2]; !near(o.Qty, 10) || !near(o.Known, 60) || !near(o.Unknown, 4) {
		t.Errorf("after the return %+v", o)
	}
}

func TestASaleResolvesOnlySeedsItsLotsPredate(t *testing.T) {
	// A seed opened on day 100 cannot be a lot the custodian dates day
	// 200: the sale's stated lot leaves the seed without a cost.
	s := sell(0, 300, "s1", 10, 900)
	s.Lots = []StatedLot{{Qty: 10, Cost: 500, CostKnown: true, AcqDay: 200}}
	r := run(t, []Method{FIFO},
		obs(0, 100, 10, 1), buy(0, 200, "b1", 10, 500), s)
	for _, f := range r.Findings {
		if f.Kind == FindResolved {
			t.Fatalf("resolved %+v", f)
		}
	}
}

func TestASaleBetweenATransfersLegsSellsWhatStayed(t *testing.T) {
	// The transfer leaves on day 10 and arrives on day 13; the sale on
	// day 11 relieves what stayed, not the lot in transit.
	ev := []Event{buy(0, 1, "b1", 10, 100), buy(0, 5, "b2", 10, 200)}
	ev = append(ev, moveAt(0, 1, 10, 13, "t", 10, 1)...)
	ev = append(ev, sell(0, 11, "s1", 10, 300), obs(1, 14, 10, 1))
	r := run(t, []Method{FIFO, FIFO}, ev...)
	if got := sales(r)["s1"]; !near(got, 200) {
		t.Fatalf("the sale relieved %v, want the day-5 lot's 200", got)
	}
	if o := r.Observations[0]; !near(o.Known, 100) {
		t.Errorf("the moved lot %+v, want the day-1 lot's 100", o)
	}
}

func TestTheReceiverCanSellBeforeTheDepartureIsRecorded(t *testing.T) {
	// The receipt is recorded on day 10, the receiver sells on day 11,
	// the departure is recorded on day 12: the sale relieves the moved
	// lot, and nothing seeds.
	ev := []Event{buy(0, 1, "b1", 10, 100)}
	ev = append(ev, moveAt(0, 1, 12, 10, "t", 10, 1)...)
	ev = append(ev, sell(1, 11, "s1", 10, 300), obs(0, 20, 0, 1))
	r := run(t, []Method{FIFO, FIFO}, ev...)
	if got := sales(r)["s1"]; !near(got, 100) {
		t.Fatalf("the sale relieved %v, want 100", got)
	}
	for _, f := range r.Findings {
		if f.Kind == FindSeed || f.Kind == FindImplied {
			t.Errorf("unexpected finding %+v", f)
		}
	}
}

func TestASeedStaysASeedThroughAReturnOfCapital(t *testing.T) {
	t.Run("resolves", func(t *testing.T) {
		s := sell(0, 30, "s1", 10, 300)
		s.Lots = []StatedLot{{Qty: 10, Cost: 150, CostKnown: true, AcqDay: 0}}
		r := run(t, []Method{FIFO},
			obs(0, 1, 10, 1),
			Event{At: at(10), Kind: Adjust, Key: 0, Txn: "roc", Value: 5, HasValue: true},
			s)
		if got := sales(r)["s1"]; !near(got, 150) {
			t.Fatalf("the sale's seed took %v, want the stated 150", got)
		}
	})
	t.Run("leaves first", func(t *testing.T) {
		r := run(t, []Method{HIFO},
			obs(0, 1, 10, 1), buy(0, 2, "b1", 10, 100),
			Event{At: at(3), Kind: Adjust, Key: 0, Txn: "roc", Value: 0.01, HasValue: true},
			obs(0, 4, 10, 2))
		// The return of capital lowers the costed lot by its share, 0.005.
		if o := r.Observations[1]; !near(o.Unknown, 0) || !near(o.Known, 99.995) {
			t.Fatalf("the implied disposal should take the seed first: %+v", o)
		}
	})
}

func TestABlipRestoresTheLotsThemselves(t *testing.T) {
	// The whole book leaves one snapshot and comes back at the next: the
	// three lots return as themselves, with no copies in the ledger.
	for _, m := range []Method{FIFO, LIFO, HIFO} {
		r := run(t, []Method{m},
			buy(0, 1, "b1", 1, 10), buy(0, 2, "b2", 1, 20), buy(0, 3, "b3", 1, 30),
			obs(0, 4, 0, 1), obs(0, 5, 3, 2), sell(0, 6, "s1", 3, 90))
		if len(r.Lots) != 3 {
			t.Errorf("%v: %d lots, want the 3 bought", m, len(r.Lots))
		}
		if got := sales(r)["s1"]; !near(got, 60) {
			t.Errorf("%v: the sale relieved %v, want all 60", m, got)
		}
		balanced(t, r)
	}
}
