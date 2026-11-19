package gold

import (
	"cmp"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// daySpan is an entity window, (from, to] in epoch days as flowsIn reads it.
type daySpan struct{ from, to int64 }

// region is where a leg's day falls in its account's life, for a window.
type region uint8

const (
	// regionLive: the account's value shows the leg.
	regionLive region = iota
	// regionPreDebut: on or before a debut the window holds. Onboarding
	// books the first value instead of what came before it.
	regionPreDebut
	// regionZeroTail: in the terminal zero tail, which shows no change for
	// the leg. The closure on the zeroing day books it instead.
	regionZeroTail
)

// region is where day falls in a's life for the window w. The pre-debut region
// and the zero tail never overlap: the last nonzero day is never before the
// debut.
func (a *accountData) region(day int64, w daySpan) region {
	if debut := a.firstDay(); debut > w.from && debut <= w.to && day <= debut {
		return regionPreDebut
	}
	if cd := a.closureDay(); cd > w.from && day > a.lastNonzeroDay() && day <= cd && day <= w.to {
		return regionZeroTail
	}
	return regionLive
}

// flowLeg is one of an entity's flows in a window, classified once: its account
// and its region, and whether the netter may pair it (a transfer-like leg).
type flowLeg struct {
	returns.Flow
	acct     *accountData
	region   region
	nettable bool
	// rest is the share of the leg's quantity a stored pair leaves over
	// (storedPairing.rest), split off; 0 for a whole leg.
	rest float64
}

// onSpine says the leg's account holds value by the leg's day.
func (l flowLeg) onSpine() bool { return l.Day > l.acct.firstDay() }

// shown says the account also values the leg by the window's end, so a
// synthetic flow can count on the money being inside.
func (l flowLeg) shown(w daySpan) bool { return l.onSpine() && l.ValuedOn != 0 && l.ValuedOn <= w.to }

// funds says the leg falls before its account's debut, where onboarding books
// the first value, alone or in its per-entity-once group's step.
func (l flowLeg) funds() bool { return l.region == regionPreDebut && onboardsFirstValue(l.acct) }

// fundsLate says a leg that funds its account's debut brought money in before
// a zero first value that a nonzero one follows (lateDebut). That valuation,
// the first to show the money, is the debut the leg funds. A leg that took
// money out funds the zero itself: a zero is what paying out everything
// leaves.
func (l flowLeg) fundsLate() bool { return l.Amount > 0 && l.acct.lateDebut() != 0 }

// closes says the closure day is the first valuation to show the leg, so the
// synthetic closure would book it again.
func (l flowLeg) closes(w daySpan) bool {
	cd := l.acct.closureDay()
	return cd > w.from && cd <= w.to && l.ValuedOn == cd &&
		l.acct.rpolicy.ClosureScope != returns.ClosureLedgerExact
}

// fate is what becomes of a leg in the entity's flow series.
type fate uint8

const (
	// fateFlow: a flow of the entity.
	fateFlow fate = iota
	// fateInside: money that moved between the entity's accounts. No flow
	// books it, and the chain still reads it between valuations.
	fateInside
	// fateFundsDebut: money a debut's first value holds that the entity
	// held already, so onboarding leaves it out.
	fateFundsDebut
	// fateExit: a zero-tail leg the closure on the zeroing day books.
	fateExit
	// fateDropped: a synthetic flow stands for it, or nothing the entity
	// holds shows it.
	fateDropped
)

// fateOf is the fate of a leg no pair claims, by its region. One cell is the
// source policy's:
//
//	region    fate                               policy cell
//	live      flow
//	preDebut  dropped: the first value holds it
//	zeroTail  exit, or dropped on a final-day    ClosureLedgerExact: flow
//	          zeroing (the closure books the
//	          carried value)
//
// A ledger-exact source dates its exit legs itself.
func fateOf(l flowLeg) fate {
	switch l.region {
	case regionPreDebut:
		return fateDropped
	case regionZeroTail:
		switch {
		case l.acct.rpolicy.ClosureScope == returns.ClosureLedgerExact:
			return fateFlow
		case l.acct.zeroesBeforeLastDay():
			return fateExit
		}
		return fateDropped
	}
	return fateFlow
}

// pairFate is the fate of a leg the netter paired with partner: the money
// stayed inside the entity. A pre-debut leg funds the debut where the
// partner's value shows the money; a leg on the spine is an inside move. A
// leg before the spine is dropped otherwise: the money is new to the entity
// where it lands.
func pairFate(l, partner flowLeg, w daySpan) fate {
	switch {
	case l.funds() && partner.shown(w):
		return fateFundsDebut
	case l.onSpine():
		return fateInside
	}
	return fateDropped
}

// fund records a leg that funds its account's debut, by its value: the first
// valuation (funded), or the late debut (late). A leg that moved a holding
// the valuation it funds holds is also kept by holding, so heldAt can value
// it at that valuation instead.
func (m *movedLegs) fund(l flowLeg) {
	if l.fundsLate() {
		m.late[l.acct] += l.Amount
	} else {
		m.funded[l.acct] += l.Amount
	}
	h, ok := l.acct.inKind[l.ID]
	if !ok || h.qty <= 0 || l.Amount <= 0 || l.acct.debut[h.holding].qty <= 0 {
		return
	}
	if m.fundedHeld[l.acct] == nil {
		m.fundedHeld[l.acct] = map[string]heldFunding{}
	}
	f := m.fundedHeld[l.acct][h.holding]
	f.qty += h.qty * (1 - l.rest)
	f.value += l.Amount
	f.exact = f.exact || h.exact
	if f.usd == 0 {
		f.usd = h.usd
	}
	m.fundedHeld[l.acct][h.holding] = f
}

// heldFunding is what the legs funding a debut moved of one holding: the
// quantity, the legs' value, whether any pairs only on the exact quantity,
// and a coin's USD price on the first such leg's day.
type heldFunding struct {
	qty, value, usd float64
	exact           bool
}

// byQuantity says the legs that funded a's debut with holding h moved the
// quantity the valuation they fund holds, as one transfer would deliver it
// (quantitiesAgree): one holding, one unit. Only then does that value stand
// for them.
func (m *movedLegs) byQuantity(a *accountData, h string) bool {
	f, d := m.fundedHeld[a][h], a.debut[h]
	return f.qty > 0 && quantitiesAgree(f.qty, d.qty, f.usd, f.exact)
}

// fundedAt is what a's first value v holds through moves (funded), at its
// holdings (heldAt). Where a late debut follows a zero first value, the money
// that came in funds that one (lateAt), and the zero holds what a paid out,
// at the legs' values.
func (m *movedLegs) fundedAt(a *accountData, v float64) float64 {
	if a.lateDebut() != 0 {
		return m.funded[a]
	}
	return m.heldAt(a, m.funded[a], v)
}

// lateAt is what a's late debut value v holds through the moves before its
// zero first value (late).
func (m *movedLegs) lateAt(a *accountData, v float64) float64 { return m.heldAt(a, m.late[a], v) }

// lateShown is a's late debut, where the window holds it and money moved in
// on or before a's zero first value (movedInBeforeZero), with what its value
// there shows beyond a's own legs since the zero: those legs are flows or
// inside moves of their own. A zero first value is no valuation of that
// money, so the late debut is the debut it funds, paired or not, wherever the
// window opens: it books what it shows beyond the legs pairing found (late),
// as a debut books its first value.
func (a *accountData) lateShown(w daySpan) (day int64, shown float64, ok bool) {
	d := a.lateDebut()
	if d == 0 || d <= w.from || d > w.to || !a.movedInBeforeZero() {
		return 0, 0, false
	}
	return d, valueOn(a, d) - a.netIn(a.firstDay(), d), true
}

// movedInBeforeZero says some leg of a, paired or not, moved money in on or
// before its first valuation.
func (a *accountData) movedInBeforeZero() bool {
	first := a.firstDay()
	for _, flows := range [2][]returns.Flow{a.nonTransfer, a.transferLike} {
		for _, f := range flows {
			if f.Day <= first && f.Amount > 0 {
				return true
			}
		}
	}
	return false
}

// valueOn is a's value on day, 0 where it holds none.
func valueOn(a *accountData, day int64) float64 {
	v, _ := a.valueAt(day)
	return v
}

// heldAt is what the value v of the valuation a's legs fund holds through
// moves that sum to funded. A holding whose funding legs moved the quantity
// that valuation holds (byQuantity) funds it by its share of v, at that
// day's prices, so the price move up to it is return. Every other leg funds
// it by its value: where that value is above v, the debut books the
// difference as capital that left, since nothing says the same holding
// arrived.
func (m *movedLegs) heldAt(a *accountData, funded, v float64) float64 {
	held := m.fundedHeld[a]
	var hs []string
	for _, h := range slices.Sorted(maps.Keys(held)) {
		if m.byQuantity(a, h) {
			hs = append(hs, h)
		}
	}
	if len(hs) == 0 {
		return funded
	}
	var s returns.Sum
	s.Add(funded)
	for _, h := range hs {
		s.Add(v*a.debut[h].share - held[h].value)
	}
	return s.Total()
}

// zeroesBeforeLastDay says a's value goes to zero before the spine's last
// day, so its zero tail holds exit legs for the closure on the zeroing day.
func (a *accountData) zeroesBeforeLastDay() bool {
	zd := a.zeroingDay()
	return zd != 0 && zd != a.closureDay()
}

// movedLegs is what an entity's accounts moved, summed per account into the
// three synthetic amounts. Pairing cancels some of it, which the synthetic
// flows must then not book again, and the chain still reads all of it between
// valuations (rolled).
type movedLegs struct {
	funded map[*accountData]float64 // onboarding: what the first value holds through moves, signed, at the legs' values
	// late is what a late debut holds through the moves that came in before
	// the zero first value (fundsLate), at the legs' values.
	late map[*accountData]float64
	// fundedHeld is the part of funded, or of late, that moved a holding the
	// valuation it funds holds (accountData.debut), by holding, which heldAt
	// may value at that valuation instead.
	fundedHeld map[*accountData]map[string]heldFunding
	closing    map[*accountData]float64        // closure: what the last value holds that stays inside or left already, signed
	exits      map[*accountData]float64        // closure: what the zero tail's surviving legs moved out of the entity, signed
	moves      map[*accountData][]returns.Flow // each account's flows and inside legs
	// fundedFrom is the other side of funded: each partner's legs that funded a
	// debut, signed, by the day the partner's value shows them. A sibling's drop
	// on a per-entity-once debut day that they explain is funded already.
	fundedFrom map[*accountData]map[int64]float64
	// carries are the pairs that carry their transit, in pair order.
	carries []carry
	// passIn is each closing account's zero-tail legs that netting cancelled
	// against money coming in from inside: what a sibling passed through the
	// emptied account after its zeroing, which the zeroing never showed.
	passIn map[*accountData][]returns.Flow
}

func newMovedLegs() *movedLegs {
	return &movedLegs{funded: map[*accountData]float64{}, late: map[*accountData]float64{},
		fundedHeld: map[*accountData]map[string]heldFunding{},
		closing:    map[*accountData]float64{},
		exits:      map[*accountData]float64{}, moves: map[*accountData][]returns.Flow{},
		fundedFrom: map[*accountData]map[int64]float64{}, passIn: map[*accountData][]returns.Flow{}}
}

// left records a leg the entity books as a flow, or drops as a matched pair with
// a live member, which a's closure day is the first valuation to show. Its money
// is out of the account already, so a final-day closure must not book it again.
func (m *movedLegs) left(a *accountData, f returns.Flow) {
	if cd := a.closureDay(); cd != 0 && f.ValuedOn == cd {
		m.closing[a] += f.Amount
	}
}

func (m *movedLegs) add(a *accountData, f returns.Flow) { m.moves[a] = append(m.moves[a], f) }

// carry is the money of a pair that carries its transit (carriesTransit),
// between the legs, as the chain reads it (movedLegs.rolled). Between the two
// legs the money is in neither account, and still the household's.
type carry struct {
	out, in flowLeg
	// to is the day the receiving account's value shows the money: the
	// debut a leg funds, else the leg's own valuation.
	to int64
}

// carriesTransit says the pair of pos and neg carries its transit: the value
// netter's rule would not pair them. That is a stored pair of legs days or a
// price move apart, or with a deposit or withdrawal leg, or legs paired by
// holding and quantity alone. A pair that rule pairs keeps the reading the
// value netter gives it.
func carriesTransit(pos, neg flowLeg) bool {
	return !(pos.nettable && neg.nettable && valueMatches(pos.Flow, neg.Flow))
}

// settle applies the fate of a leg no pair claims, or of a stored pair's leg
// (fateInside), and returns flows with the leg appended where it is a flow.
func (m *movedLegs) settle(flows []returns.Flow, l flowLeg, f fate) []returns.Flow {
	switch f {
	case fateFlow:
		m.add(l.acct, l.Flow)
		m.left(l.acct, l.Flow)
		return append(flows, l.Flow)
	case fateInside:
		m.add(l.acct, l.Flow)
		m.left(l.acct, l.Flow)
	case fateExit:
		m.exits[l.acct] += l.Amount
	}
	return flows
}

// pair settles two legs the netter cancelled. They stay what the accounts moved
// only where neither leg is dropped; with transit, or into a late debut, the
// receiving leg's money is carried between the legs instead (carry). A
// funding leg leaves its debut's onboarding, and its partner's legs that the
// funding explains are marked on the partner's valuation day. Money leaving a
// closing account stays inside where the partner is shown or funds a debut, so
// the final-day closure leaves it out. A zero-tail exit leg that money came in
// on is passed through.
func (m *movedLegs) pair(a, b flowLeg, w daySpan, transit bool) {
	fates := [2]fate{pairFate(a, b, w), pairFate(b, a, w)}
	if fates[0] != fateDropped && fates[1] != fateDropped {
		m.add(a.acct, a.Flow)
		m.add(b.acct, b.Flow)
		out, in := b, a
		if a.Amount < 0 {
			out, in = a, b
		}
		// A late debut is days or years on, whatever the value netter says.
		late := in.funds() && in.fundsLate()
		if transit || late {
			to := in.ValuedOn
			switch {
			case late:
				to = in.acct.lateDebut()
			case in.funds():
				to = in.acct.firstDay()
			}
			if to != 0 && out.ValuedOn != 0 {
				m.carries = append(m.carries, carry{out: out, in: in, to: to})
			}
		}
	}
	for i, p := range [2][2]flowLeg{{a, b}, {b, a}} {
		l, partner := p[0], p[1]
		if fates[i] == fateFundsDebut {
			m.fund(l)
			if m.fundedFrom[partner.acct] == nil {
				m.fundedFrom[partner.acct] = map[int64]float64{}
			}
			m.fundedFrom[partner.acct][partner.ValuedOn] += partner.Amount
		}
		if l.closes(w) && l.shown(w) && (partner.shown(w) || partner.funds()) {
			m.closing[l.acct] += l.Amount
		}
		if fateOf(l) == fateExit && l.Amount > 0 {
			m.passIn[l.acct] = append(m.passIn[l.acct], l.Flow)
		}
	}
}

// rolled is the moves the chain may read before their accounts value: those of
// each gap between valuations that the closing valuation confirms
// (accountData.confirms). A gap into a closure the synthetic flow books stays out:
// that flow already stands for the account's whole value.
//
// Then the carries: each pair's money from the day the chain stops counting
// it in the sending account, the leg's day where its move is read here, else
// its valuation, until the receiving account's value shows it (carry.to). The
// receiving leg's own move gives way to its carry, so the chain counts the
// money once throughout, whether or not either gap confirms. A carry that
// would start on or after the day the money shows carries nothing: there is
// no transit to carry.
func (m *movedLegs) rolled(assets []*accountData, winTo int64) []returns.Flow {
	type gap struct {
		a  *accountData
		on int64
	}
	type gapMoves struct {
		gap
		moves []returns.Flow
	}
	// Without a carry, the confirmed gaps' moves are the whole answer.
	var out []returns.Flow
	carrying := len(m.carries) > 0
	var read []gapMoves
	var confirmed map[gap]bool
	if carrying {
		confirmed = map[gap]bool{}
	}
	for _, a := range assets {
		gaps, cd := map[int64][]returns.Flow{}, a.closureDay()
		for _, f := range m.moves[a] {
			if f.ValuedOn != 0 && f.ValuedOn != cd {
				gaps[f.ValuedOn] = append(gaps[f.ValuedOn], f)
			}
		}
		for _, on := range slices.Sorted(maps.Keys(gaps)) {
			switch {
			case !a.confirms(on, winTo, gaps[on]):
			case carrying:
				read = append(read, gapMoves{gap{a, on}, gaps[on]})
				confirmed[gap{a, on}] = true
			default:
				out = append(out, gaps[on]...)
			}
		}
	}
	if !carrying {
		return out
	}
	type leg struct {
		a  *accountData
		id string
	}
	var carried map[leg]bool
	var carries []returns.Flow
	for _, c := range m.carries {
		from := c.out.ValuedOn
		if confirmed[gap{c.out.acct, c.out.ValuedOn}] {
			from = c.out.Day
		}
		if from >= c.to {
			continue
		}
		if carried == nil {
			carried = map[leg]bool{}
		}
		carried[leg{c.in.acct, c.in.ID}] = true
		carries = append(carries, returns.Flow{Day: from, ValuedOn: c.to, Amount: c.in.Amount, ID: c.in.ID})
	}
	for _, g := range read {
		for _, f := range g.moves {
			if !carried[leg{g.a, f.ID}] {
				out = append(out, f)
			}
		}
	}
	return append(out, carries...)
}

// entityFlows assembles the entity's external flow series, its quality tags and
// the moves the chain may read before their accounts value (rolled). Each dollar
// that crosses the entity's boundary counts once, in the same bucket as the value
// change it causes (RETURNS-NOTES §"staggered inception"):
//
//  1. every leg is classified once: its region in its account's life;
//  2. legs pair. A stored pair binds where the entity holds both legs in
//     the window, at any day and value gap (storedPairs). Where both legs
//     would be flows on their own the pair drops; otherwise it settles as
//     a netted pair. The netter then cancels opposite transfer-like legs no
//     stored pair claimed: a leg that moved a holding by holding and
//     quantity (pairInKind), a leg of cash by value (netMatch). It runs
//     over every such leg, pre-debut and zero-tail legs included, so an
//     internal pair whose other leg sits on a live account still cancels.
//     A deposit or withdrawal never nets by heuristic (RETURNS-NOTES);
//  3. each leg takes its fate (fateOf, pairFate) and is summed per account into
//     the three synthetic amounts (movedLegs);
//  4. the synthetic onboarding and closure flows book the regions the value
//     series cannot show yet or any more, less those amounts;
//  5. the flows are sorted by day.
//
// The flows are appended in a fixed order: each account's legs no pair claims
// (deposits, withdrawals and a stored pair's rest) in member order, then the
// transfer-like legs pairing keeps, then the synthetic flows. sortFlows is
// not stable, so that order fixes the series' order; its sums are
// compensated and do not follow it.
func entityFlows(assets []*accountData, p ReturnParams, winFrom, winTo int64, aggregate bool) ([]returns.Flow, []string, []returns.Flow) {
	if !aggregate {
		// Accounts grain, single constituent: every policy-external flow is exact.
		// (Links never net here: each leg is a boundary flow of its account,
		// even where a stored pair keeps both legs on one account.)
		var flows []returns.Flow
		for f := range assets[0].legs(winFrom, winTo) {
			flows = append(flows, f)
		}
		return flows, nil, nil
	}
	w := daySpan{winFrom, winTo}

	// Stored pairs obey the same --netting switch as the heuristic netter:
	// netting off is the diagnostic view of every raw leg.
	var tags []string
	var stored storedPairing
	if p.Netting {
		// The tag counts every stored pair that binds, the pairs inside one
		// source among them.
		stored = storedPairs(assets, winFrom, winTo)
		if stored.pairs > 0 {
			tags = append(tags, fmt.Sprintf("cross_source_netted=%d", stored.pairs))
		}
	}

	var flows []returns.Flow
	var cand []flowLeg
	var held map[string]flowLeg
	unpairedRest := 0
	moved := newMovedLegs()
	for _, a := range assets {
		key := acctKey(a.src, a.acct)
		for f, nettable := range a.legs(winFrom, winTo) {
			f.ValuedOn = a.valuedOn(f.Day)
			l := flowLeg{Flow: f, acct: a, region: a.region(f.Day, w), nettable: nettable}
			// Only a bound stored pair keys its legs; with none, every
			// stored lookup below misses.
			var k string
			if stored.pairs > 0 {
				k = acctKey(key, f.ID)
			}
			if share := stored.rest[k]; share > 0 {
				// What the pair leaves over is a transfer of its own.
				var r flowLeg
				l, r = splitRest(l, share)
				flows = moved.settle(flows, r, fateOf(r))
				unpairedRest++
			}
			switch {
			case stored.drops[k]:
				flows = moved.settle(flows, l, fateInside)
			case stored.settled[k]:
				if held == nil {
					held = map[string]flowLeg{}
				}
				held[k] = l
			case nettable:
				cand = append(cand, l)
			default:
				flows = moved.settle(flows, l, fateOf(l))
			}
		}
	}
	for _, pr := range stored.settle {
		x, y := held[pr[0]], held[pr[1]]
		if x.Amount < 0 {
			x, y = y, x
		}
		moved.pair(x, y, w, carriesTransit(x, y))
	}
	kept := cand
	if p.Netting {
		var unmatched int
		kept, unmatched = netLegs(cand, func(a, b flowLeg, transit bool) { moved.pair(a, b, w, transit) })
		unmatched += unpairedRest
		if unmatched > 0 {
			tags = append(tags, fmt.Sprintf("unmatched_transfers=%d", unmatched))
		}
	}
	for _, l := range kept {
		flows = moved.settle(flows, l, fateOf(l))
	}

	flows = moved.onboarding(flows, assets, w)
	flows = moved.closures(flows, assets, w)
	sortFlows(flows)
	// The opening value is as stale as any other: what its accounts moved before
	// the window and have not shown yet counts for the chain too.
	for _, a := range assets {
		if a.firstDay() <= winFrom {
			moved.moves[a] = append(moved.moves[a], a.unshownAt(winFrom)...)
		}
	}
	return flows, tags, moved.rolled(assets, winTo)
}

// onboarding appends the synthetic onboarding flows: a late constituent's whole
// arrival, booked as its debut value less what pairing found the entity held
// already (movedLegs). Its real pre-debut legs took no flow, so no near-day dedup
// is needed. Where its first value is zero and a nonzero one follows, the
// money that came in before the zero is booked on that late debut instead
// (lateShown), and the zero books what the account paid out before it.
//
// OnboardScope splits the onboarding GRAIN — the AMOUNT booked when a
// constituent debuts after w.from:
//   - OnboardPerConstituent (default): the debuting account's OWN value, less
//     the legs netting cancelled against a live partner (movedLegs).
//   - OnboardPerEntityOnce: the newly-debuting constituents' first value MINUS
//     the legs netting cancelled against a live partner, as above, and MINUS
//     same-day sibling FUNDING drops those legs do not explain (NOT the raw
//     calendar aggregate delta — see groupOnboardStep: same-day external
//     deposits and market moves on siblings are excluded so they aren't
//     double-counted). An internal cash->securities move inside a UBS
//     relationship debuts a securities account while its funding cash
//     account drops by the same amount, so the step-up nets to ~0 and
//     NOTHING is onboarded — the capital was already booked once (inception
//     value + the external cash deposit). Genuinely new external value that
//     wasn't captured as a same-unit deposit still steps the aggregate up and
//     is onboarded. This prevents the conduit double-count, and it is
//     source-scoped: the grain is read per constituent from a.rpolicy, so a
//     non-UBS constituent keeps per-constituent onboarding even in the
//     merged global entity.
//
// Conduit-kind accounts never emit their OWN onboarding (ConduitKinds); under
// per-entity-once their value still enters the aggregate step-up (they hold the
// inception cash), and under the default they are simply skipped.
func (m *movedLegs) onboarding(flows []returns.Flow, assets []*accountData, w daySpan) []returns.Flow {
	perEntityGroups := map[string][]*accountData{}
	for _, a := range assets {
		if a.rpolicy.OnboardScope == returns.OnboardPerEntityOnce {
			perEntityGroups[a.src] = append(perEntityGroups[a.src], a)
			continue
		}
		if a.isConduit() {
			continue // conduit: value only, no per-account onboarding
		}
		// Default per-constituent onboarding: the debuting account's own value,
		// less what netting already cancelled as a sibling's funding of it. That
		// money was in the entity before the debut, and its out-leg is gone, so
		// it is no new capital.
		debut := a.firstDay()
		if debut > w.from && debut <= w.to {
			v, _ := a.valueAt(debut)
			if of, ok := returns.FundedOnboardingFlow(debut, v, m.fundedAt(a, v)); ok {
				flows = append(flows, of)
			}
		}
		if d, shown, ok := a.lateShown(w); ok {
			if of, ok := returns.FundedOnboardingFlow(d, shown, m.lateAt(a, valueOn(a, d))); ok {
				flows = append(flows, of)
			}
		}
	}
	// Per-entity-once: onboard the aggregate step-up at each distinct post-w.from
	// debut day within the source group. When the group's inception coincides with
	// w.from (the common case once the window is anchored at the first real
	// snapshot) NO account debuts after w.from, so nothing is onboarded: the
	// inception value is the opening base V0, external deposits add capital, and
	// internal churn is nothing — capital counted exactly once. The groups are
	// booked by source id, so same-day steps reach the day sort in one order.
	for _, src := range slices.Sorted(maps.Keys(perEntityGroups)) {
		grp := perEntityGroups[src]
		for _, day := range groupDebutDays(grp, w) {
			step := groupOnboardStep(grp, day, w, m)
			if of, ok := returns.OnboardingFlow(day, step); ok {
				flows = append(flows, of)
			}
		}
	}
	return flows
}

// closures appends the explicit closure flows, unchanged by OnboardScope /
// ConduitKinds, the mirror of onboarding: dated on the zeroing day, the first
// valuation that shows the account empty. A ledger-exact closure books nothing
// here — its kept real flows are the exit.
//   - An account that zeroes on the spine's final day has no exit leg yet. The
//     closure books its carried value, less what netting found moved inside and
//     what already left as a flow since its last valuation.
//   - Inside a longer zero tail the closure books what the tail's surviving
//     legs moved out, so the money that moved on leaves on the day the value
//     shows it gone. A window that ends before an exit leg still holds the
//     zeroing, so the account's own tail legs past the window's end count
//     too, unnetted, as confirms reads flows past w.to. With no leg the
//     closure books nothing: the zeroing reads as a loss, a write-off.
func (m *movedLegs) closures(flows []returns.Flow, assets []*accountData, w daySpan) []returns.Flow {
	for _, a := range assets {
		if a.rpolicy.ClosureScope == returns.ClosureLedgerExact {
			continue
		}
		zd := a.zeroingDay()
		if zd <= w.from || zd > w.to {
			continue
		}
		amount, exit := -m.exits[a], true
		if cd := a.closureDay(); zd == cd {
			last, _ := a.valueAt(cd - 1)
			amount, exit = last+m.closing[a], false
		} else {
			amount -= tailLegsAfter(a, w.to)
			// Money a sibling passed in after the zeroing and the exit legs paid
			// on was never in the zeroing's value: it leaves on the day it came
			// in, and the zeroing day books only the rest.
			passed := passThrough(m.passIn[a], m.exits[a])
			for _, f := range passed {
				amount += f.Amount
			}
			flows = append(flows, passed...)
		}
		if cf, ok := returns.ClosureFlow(zd, amount); ok {
			// An exit is the legs' own money on another day, so it is a real
			// flow: the bridge reads it as money that left, not as tracking.
			cf.Synthetic = !exit
			flows = append(flows, cf)
			// The zeroing valuation shows the exit, so the gap it ends weighs it
			// with the gap's own legs (movedLegs.rolled).
			cf.ValuedOn = zd
			m.add(a, cf)
		}
	}
	return flows
}

// tailLegsAfter is the signed sum of a's own legs in its zero tail dated after
// winTo, up to the tail's end: the exit a window that ends before them holds.
func tailLegsAfter(a *accountData, winTo int64) float64 {
	cd := a.closureDay()
	if cd <= winTo {
		return 0
	}
	return a.netIn(max(winTo, a.lastNonzeroDay()), cd)
}

// passThrough is the outflows, as real flows on the days the money came in, of
// the inside legs `in` that a closed account paid on through its surviving exit
// legs, which moved `exits` (signed) in all: at most what those legs paid out.
func passThrough(in []returns.Flow, exits float64) []returns.Flow {
	left := -exits
	var out []returns.Flow
	for _, f := range slices.SortedStableFunc(slices.Values(in), func(x, y returns.Flow) int { return int(x.Day - y.Day) }) {
		if left <= valueTol {
			break
		}
		amt := math.Min(f.Amount, left)
		left -= amt
		out = append(out, returns.Flow{Day: f.Day, Amount: -amt, ID: f.ID})
	}
	return out
}

// groupDebutDays returns the ascending, de-duplicated set of days on which a
// per-entity-once source group gains a constituent after w.from (a constituent's
// firstDay() in (w.from, w.to]), or a late debut (lateShown).
// The aggregate step-up is booked on each such day.
func groupDebutDays(grp []*accountData, w daySpan) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(d int64) {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, a := range grp {
		if d := a.firstDay(); d > w.from && d <= w.to {
			add(d)
		}
		if d, _, ok := a.lateShown(w); ok {
			add(d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// groupOnboardStep is the capital onboarded when one or more constituents of a
// per-entity-once group debut on `day`. It is the newly-debuting constituents'
// first value NET OF two offsets — NOT the raw aggregate calendar delta (the
// group's total value on day minus its total on day-1):
//
//   - the pre-debut legs netting cancelled against a live partner (moved.funded),
//     as per-constituent onboarding offsets them: a sibling's funding days before
//     the debut, or another source's at a grain that holds both;
//   - same-day sibling FUNDING drops, less what such cancelled legs already
//     explain (moved.fundedFrom), so one move never offsets twice.
//
// The raw delta would absorb EVERY value movement across ALL group accounts on
// the debut day, and so double-count two things that are not new onboarding
// capital:
//
//   - a same-day external cash deposit into a sibling conduit — already booked as
//     its own nonTransfer flow, so sweeping it into the step-up counts it twice;
//   - organic market appreciation / dividends / interest on existing holdings that
//     day — genuine RETURN silently reclassified as a capital inflow.
//
// Both surface as POSITIVE sibling deltas, so they are excluded: only NEGATIVE
// sibling deltas (a funding cash conduit draining to fund the new account) offset
// the debut value. An internal cash->securities move (new securities +X, funding
// cash -X) therefore nets to 0; a genuine external inflow landing straight as a
// new account (no sibling drop) onboards its full value; same-day deposits and
// market moves on siblings do not touch the step-up. A late debut on `day` adds
// what it shows less what its paired legs brought (lateShown), as a debut would.
// Floored at 0 so a debut day dominated by sibling drops can never book
// negative onboarding.
func groupOnboardStep(grp []*accountData, day int64, w daySpan, moved *movedLegs) float64 {
	var debutValue, siblingFundingDrop float64
	for _, a := range grp {
		// A late debut on day is the debut the money moved in before its
		// zero funds: what it shows beyond its own later legs, less what
		// the paired legs among that money brought.
		if d, shown, ok := a.lateShown(w); ok && d == day {
			debutValue += shown - moved.lateAt(a, valueOn(a, d))
			continue
		}
		newlyDebuts := a.firstDay() == day
		if newlyDebuts {
			var first float64
			if v, alive := a.valueAt(day); alive {
				first = returns.ZeroedValue(v, day, a.closureDay())
				debutValue += first
			}
			debutValue -= moved.fundedAt(a, first)
			continue
		}
		// Already-alive sibling: count only a value DECREASE across the debut day
		// as funding (the conduit draining into the new account). Positive deltas
		// (market gains, external deposits) are NOT onboarding capital.
		vPrev, alivePrev := a.valueAt(day - 1)
		vCur, aliveCur := a.valueAt(day)
		if !alivePrev || !aliveCur {
			continue
		}
		prev := returns.ZeroedValue(vPrev, day-1, a.closureDay())
		cur := returns.ZeroedValue(vCur, day, a.closureDay())
		// A cancelled leg that funded a debut and that this day's value shows
		// is offset through funded already.
		if drop := prev - cur + moved.fundedFrom[a][day]; drop > 0 {
			siblingFundingDrop += drop
		}
	}
	step := debutValue - siblingFundingDrop
	if step < 0 {
		step = 0
	}
	return step
}

// storedPairing is how the stored transfer links bind in one entity window
// (storedPairs). Keys are acctKey(acctKey(src, acct), txID).
type storedPairing struct {
	// drops are the legs of pairs whose both legs would be flows on their
	// own: the pair stays inside the entity, and no flow books it.
	drops map[string]bool
	// settled are the legs of the other pairs, and settle those pairs,
	// each as its two leg keys, in the order they are found. Such a pair
	// settles as a netted pair does (movedLegs.pair).
	settled map[string]bool
	settle  [][2]string
	// rest is, for the larger leg of a pair that moved one holding in
	// quantities whose difference is a transfer of its own
	// (excessIsTransfer, whatever the gap between their days), the share
	// of its quantity beyond the other's:
	// that part stays an unpaired transfer. A pair whose legs name two
	// holdings binds whole.
	rest map[string]float64
	// pairs counts every pair that binds.
	pairs int
}

// storedPairs resolves the stored transfer links (links, bound by
// bindStoredLinks) against THIS entity window. A link states that its two
// legs are one move, so a pair binds wherever the entity holds both legs:
// the partner's account is in the entity and both legs' days are inside
// (winFrom, winTo], at any day gap between them and any gap between their
// values. The price move between the two days is then return, not capital.
// Where both legs would be flows on their own (fateOf) the pair drops both.
// Where one sits before its account's debut or in a zero tail, the pair
// settles as a netted pair does: such a leg funds the debut, or the closure
// books it. Legs failing a condition keep their normal treatment: at finer
// grains, and for pairs straddling the window, each leg stays the real
// boundary flow it is for that view.
func storedPairs(assets []*accountData, winFrom, winTo int64) storedPairing {
	var out storedPairing
	any := false
	for _, a := range assets {
		if len(a.links) > 0 {
			any = true
			break
		}
	}
	if !any {
		return out
	}
	members := make(map[string]*accountData, len(assets))
	for _, a := range assets {
		members[acctKey(a.src, a.acct)] = a
	}
	w := daySpan{winFrom, winTo}
	live := func(a *accountData, day int64) bool {
		return fateOf(flowLeg{Flow: returns.Flow{Day: day}, acct: a, region: a.region(day, w)}) == fateFlow
	}
	out.drops, out.settled = map[string]bool{}, map[string]bool{}
	for _, a := range assets {
		if len(a.links) == 0 {
			continue // links are sparse; skip unlinked members entirely
		}
		for f := range a.legs(winFrom, winTo) {
			link, ok := a.links[f.ID]
			if !ok {
				continue
			}
			// Process each pair once, from its lexicographically-smaller leg.
			me, other := acctKey(acctKey(a.src, a.acct), f.ID), acctKey(acctKey(link.src, link.acct), link.txID)
			if me > other {
				continue
			}
			partner := members[acctKey(link.src, link.acct)]
			if partner == nil || link.day <= winFrom || link.day > winTo {
				continue
			}
			if out.drops[me] || out.drops[other] || out.settled[me] || out.settled[other] {
				continue // a leg binds in one pair only
			}
			out.pairs++
			hx, okx := a.inKind[f.ID]
			hy, oky := partner.inKind[link.txID]
			sent, received := hx, hy
			if f.Amount >= 0 {
				sent, received = hy, hx
			}
			if okx && oky && hx.holding == hy.holding && hx.qty > 0 && hy.qty > 0 &&
				excessIsTransfer(sent.qty, received.qty, sent.usd, hx.exact || hy.exact) {
				if out.rest == nil {
					out.rest = map[string]float64{}
				}
				if hx.qty > hy.qty {
					out.rest[me] = (hx.qty - hy.qty) / hx.qty
				} else {
					out.rest[other] = (hy.qty - hx.qty) / hy.qty
				}
			}
			if live(a, f.Day) && live(partner, link.day) {
				out.drops[me], out.drops[other] = true, true
				continue
			}
			out.settled[me], out.settled[other] = true, true
			out.settle = append(out.settle, [2]string{me, other})
		}
	}
	return out
}

// splitRest splits off share of l's quantity, at its share of the value: the
// part a stored pair pairs, and the rest, which keeps the leg's days and
// takes an id of its own.
func splitRest(l flowLeg, share float64) (paired, rest flowLeg) {
	rest = l
	rest.Amount = l.Amount * share
	rest.ID = l.ID + "\x00rest"
	paired = l
	paired.Amount = l.Amount - rest.Amount
	paired.rest = share
	return paired, rest
}

// netLegs cancels the transfer-like legs no stored pair claimed and hands
// each matched pair to pair, positive leg first, with whether it carries its
// transit (carriesTransit). The value netter runs first, over every leg, but
// pairs two legs only where they may pair (mayNet): two legs of cash, or two
// legs of one holding and quantity. The legs that moved a holding and are
// left then pair by holding and quantity (pairInKind). It returns the legs
// left unpaired, in the value netter's order, and their count.
func netLegs(cand []flowLeg, pair func(pos, neg flowLeg, transit bool)) (kept []flowLeg, unmatched int) {
	kept, _ = netMatch(cand, func(pos, neg flowLeg) { pair(pos, neg, false) }, mayNet)
	var held []flowLeg
	for _, l := range kept {
		if _, ok := l.acct.inKind[l.ID]; ok {
			held = append(held, l)
		}
	}
	if len(held) == 0 {
		return kept, len(kept)
	}
	paired := map[*accountData]map[string]bool{}
	mark := func(l flowLeg) {
		if paired[l.acct] == nil {
			paired[l.acct] = map[string]bool{}
		}
		paired[l.acct][l.ID] = true
	}
	pairInKind(held, func(pos, neg flowLeg) {
		mark(pos)
		mark(neg)
		pair(pos, neg, carriesTransit(pos, neg))
	})
	if len(paired) == 0 {
		return kept, len(kept)
	}
	var left []flowLeg
	for _, l := range kept {
		if !paired[l.acct][l.ID] {
			left = append(left, l)
		}
	}
	return left, len(left)
}

// mayNet says the value netter may pair pos and neg: both are legs of cash,
// or both moved one holding as one move: a security in the exact quantity,
// a coin by the coin rule (neg sends, pos receives).
func mayNet(pos, neg flowLeg) bool {
	hp, okP := pos.acct.inKind[pos.ID]
	hn, okN := neg.acct.inKind[neg.ID]
	if !okP || !okN {
		return okP == okN
	}
	if hp.holding != hn.holding || !(hp.qty > 0) || !(hn.qty > 0) {
		return false
	}
	exact := hp.exact || hn.exact
	return (exact || coinDays(pos.Day, neg.Day)) && quantitiesAgree(hn.qty, hp.qty, hn.usd, exact)
}

// inKindLeg is a leg that moved a holding, as pairInKind pairs it.
type inKindLeg struct {
	flowLeg
	heldMove
}

// inKindLegs are legs that moved a holding, sorted by holding, then day, for
// pairByQuantity. A leg that states no quantity pairs with none.
type inKindLegs []inKindLeg

func (l inKindLegs) holding(i int) string { return l[i].heldMove.holding }
func (l inKindLegs) at(i int) int64       { return l[i].Day }
func (l inKindLegs) in(i int) bool        { return l[i].Amount >= 0 }
func (l inKindLegs) qty(i int) float64    { return l[i].heldMove.qty }
func (l inKindLegs) exact(i int) bool     { return l[i].heldMove.exact }
func (l inKindLegs) usd(i int) float64    { return l[i].heldMove.usd }
func (l inKindLegs) may(i, j int) bool    { return l[i].heldMove.qty > 0 && l[j].heldMove.qty > 0 }

// pairInKind pairs legs that moved a holding (accountData.inKind) by holding
// and quantity, never by value: one coin out, one coin in. Two legs pair
// where they move one holding in opposite directions as one move (oneMove):
// a security within inKindWindowDay of each other on the exact quantity, a
// coin by the coin rule. A coin's shortfall is a fee in kind, whose value is
// a loss. Each leg, in day order within its holding, takes the open leg of
// the other direction that ranks best, the latest on a tie
// (pairByQuantity). A leg that states no quantity pairs only through a
// stored link. It hands each pair to pair, positive leg first, in the order
// of their earlier legs.
func pairInKind(cand []flowLeg, pair func(pos, neg flowLeg)) {
	legs := make(inKindLegs, len(cand))
	for i, l := range cand {
		legs[i] = inKindLeg{l, l.acct.inKind[l.ID]}
	}
	slices.SortStableFunc(legs, func(a, b inKindLeg) int {
		if c := strings.Compare(a.holding, b.holding); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Day, b.Day); c != 0 {
			return c
		}
		if c := strings.Compare(a.acct.src, b.acct.src); c != 0 {
			return c
		}
		if c := strings.Compare(a.acct.acct, b.acct.acct); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	mate := make([]int, len(legs))
	for i := range mate {
		mate[i] = -1
	}
	pairByQuantity(legs, inKindWindowDay, mate)
	for i, j := range mate {
		if j > i {
			pos, neg := legs[i].flowLeg, legs[j].flowLeg
			if pos.Amount < 0 {
				pos, neg = neg, pos
			}
			pair(pos, neg)
		}
	}
}

// valueMatches is the value netter's rule for a leg in and a leg out: their
// output-currency magnitudes agree within ε and their days are within the
// netting window.
func valueMatches(pos, neg returns.Flow) bool {
	eps := nettingEpsFloor
	if r := nettingEpsRel * math.Max(math.Abs(pos.Amount), math.Abs(neg.Amount)); r > eps {
		eps = r
	}
	return math.Abs(pos.Amount+neg.Amount) <= eps && absDay(pos.Day-neg.Day) <= nettingWindowDay
}

// netMatch greedily matches opposite-direction legs (largest first) whose
// output-currency magnitudes agree within ε and whose days are within the
// netting window (valueMatches), where may allows the pair (every pair
// where may is nil), and hands each matched pair to pair, positive leg
// first, in match order. The deterministic (|amount|, day, id) tie-break
// means internal pairs cancel the same way whatever a leg's region. It
// returns the legs left unpaired, positive ones first, and their count.
func netMatch(cand []flowLeg, pair func(pos, neg flowLeg), may func(pos, neg flowLeg) bool) (kept []flowLeg, unmatched int) {
	var pos, neg []flowLeg
	for _, f := range cand {
		if f.Amount >= 0 {
			pos = append(pos, f)
		} else {
			neg = append(neg, f)
		}
	}
	// Largest-magnitude first, with a fully deterministic tie-break on
	// (day, transaction id) so equal-magnitude legs match reproducibly.
	byMag := func(s []flowLeg) {
		sort.SliceStable(s, func(i, j int) bool {
			mi, mj := math.Abs(s[i].Amount), math.Abs(s[j].Amount)
			if mi != mj {
				return mi > mj
			}
			if s[i].Day != s[j].Day {
				return s[i].Day < s[j].Day
			}
			return s[i].ID < s[j].ID
		})
	}
	byMag(pos)
	byMag(neg)
	usedNeg := make([]bool, len(neg))
	for _, pf := range pos {
		matched := false
		for j, nf := range neg {
			if usedNeg[j] {
				continue
			}
			if valueMatches(pf.Flow, nf.Flow) && (may == nil || may(pf, nf)) {
				usedNeg[j] = true
				matched = true
				pair(pf, nf)
				break
			}
		}
		if !matched {
			kept = append(kept, pf)
			unmatched++
		}
	}
	for j, nf := range neg {
		if !usedNeg[j] {
			kept = append(kept, nf)
			unmatched++
		}
	}
	return kept, unmatched
}

// onboardsFirstValue reports whether a debut of a books a's first value, alone
// or in its per-entity-once group's step, so that value stands for everything
// a's pre-debut legs moved.
func onboardsFirstValue(a *accountData) bool {
	return a.rpolicy.OnboardScope == returns.OnboardPerEntityOnce || !a.isConduit()
}

func flowsIn(flows []returns.Flow, from, to int64) []returns.Flow {
	var out []returns.Flow
	for _, f := range flows {
		if f.Day > from && f.Day <= to {
			out = append(out, f)
		}
	}
	return out
}

// sumFlows is the flows' net amount, a compensated sum, so it does not
// follow their order.
func sumFlows(flows []returns.Flow) float64 {
	var s returns.Sum
	for _, f := range flows {
		s.Add(f.Amount)
	}
	return s.Total()
}

// sumFlowsIn is sumFlows(flowsIn(flows, from, to)).
func sumFlowsIn(flows []returns.Flow, from, to int64) float64 {
	var s returns.Sum
	for _, f := range flows {
		if f.Day > from && f.Day <= to {
			s.Add(f.Amount)
		}
	}
	return s.Total()
}

func sortFlows(flows []returns.Flow) {
	sort.Slice(flows, func(i, j int) bool { return flows[i].Day < flows[j].Day })
}

func absDay(d int64) int64 {
	if d < 0 {
		return -d
	}
	return d
}

// netting tolerances, for legs of cash.
const (
	nettingEpsFloor  = 1.00  // absolute floor, output currency
	nettingEpsRel    = 0.005 // 0.5% of the larger leg
	nettingWindowDay = 3     // ±3 calendar days
)

// legs yields a's flows dated in (from, to], with whether the netter may pair
// each: its deposits and withdrawals first, then its transfer-like legs, each in
// ledger order. The flow series and every float sum over them keep this order.
func (a *accountData) legs(from, to int64) iter.Seq2[returns.Flow, bool] {
	return func(yield func(returns.Flow, bool) bool) {
		for i, flows := range [2][]returns.Flow{a.nonTransfer, a.transferLike} {
			for _, f := range flows {
				if f.Day > from && f.Day <= to && !yield(f, i == 1) {
					return
				}
			}
		}
	}
}

// netIn is the signed sum of a's flows dated in (from, to]. Its deposits and
// withdrawals and its transfer-like legs are summed apart and then added, the
// order the tables are pinned by.
func (a *accountData) netIn(from, to int64) float64 {
	return sumFlowsIn(a.nonTransfer, from, to) + sumFlowsIn(a.transferLike, from, to)
}
