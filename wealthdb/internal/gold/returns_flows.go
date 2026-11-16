package gold

import (
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"sort"

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

// flowLeg is one of an entity's flows in a window, classified once: its account,
// whether the netter may pair it (a transfer-like kind), and its region.
type flowLeg struct {
	returns.Flow
	acct     *accountData
	nettable bool
	region   region
}

// onSpine says the leg's account holds value by the leg's day.
func (l flowLeg) onSpine() bool { return l.Day > l.acct.firstDay() }

// shown says the account also values the leg by the window's end, so a
// synthetic flow can count on the money being inside.
func (l flowLeg) shown(w daySpan) bool { return l.onSpine() && l.ValuedOn != 0 && l.ValuedOn <= w.to }

// funds says the leg falls before its account's debut, where onboarding books
// the first value, alone or in its per-entity-once group's step.
func (l flowLeg) funds() bool { return l.region == regionPreDebut && onboardsFirstValue(l.acct) }

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

// fateOf is the fate of a leg no pair claims, by its region. Two cells are the
// source policy's:
//
//	region    fate                               policy cell
//	live      flow
//	preDebut  dropped: the first value holds it  OnboardNone: a deposit or withdrawal is a flow
//	zeroTail  exit, or dropped on a final-day    ClosureLedgerExact: flow
//	          zeroing (the closure books the
//	          carried value)
//
// Under OnboardNone no onboarding replaces a pre-debut deposit, so it is the
// capital event. A ledger-exact source dates its exit legs itself.
func fateOf(l flowLeg) fate {
	p := l.acct.rpolicy
	switch l.region {
	case regionPreDebut:
		if !l.nettable && p.OnboardScope == returns.OnboardNone {
			return fateFlow
		}
		return fateDropped
	case regionZeroTail:
		switch {
		case p.ClosureScope == returns.ClosureLedgerExact:
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
	funded  map[*accountData]float64        // onboarding: what the first value holds through moves, signed
	closing map[*accountData]float64        // closure: what the last value holds that stays inside or left already, signed
	exits   map[*accountData]float64        // closure: what the zero tail's surviving legs moved out of the entity, signed
	moves   map[*accountData][]returns.Flow // each account's flows and inside legs
	// fundedFrom is the other side of funded: each partner's legs that funded a
	// debut, signed, by the day the partner's value shows them. A sibling's drop
	// on a per-entity-once debut day that they explain is funded already.
	fundedFrom map[*accountData]map[int64]float64
	// passIn is each closing account's zero-tail legs that netting cancelled
	// against money coming in from inside: what a sibling passed through the
	// emptied account after its zeroing, which the zeroing never showed.
	passIn map[*accountData][]returns.Flow
}

func newMovedLegs() *movedLegs {
	return &movedLegs{funded: map[*accountData]float64{}, closing: map[*accountData]float64{},
		exits: map[*accountData]float64{}, moves: map[*accountData][]returns.Flow{},
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

// settle applies the fate of a leg no pair claims, or of a cross-source pair's
// leg (fateInside), and returns flows with the leg appended where it is a flow.
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
// only where neither leg is dropped. A funding leg leaves its debut's
// onboarding, and its partner's legs that the funding explains are marked on
// the partner's valuation day. Money leaving a closing account stays inside
// where the partner is shown or funds a debut, so the final-day closure leaves
// it out. A zero-tail exit leg that money came in on is passed through.
func (m *movedLegs) pair(a, b flowLeg, w daySpan) {
	fates := [2]fate{pairFate(a, b, w), pairFate(b, a, w)}
	if fates[0] != fateDropped && fates[1] != fateDropped {
		m.add(a.acct, a.Flow)
		m.add(b.acct, b.Flow)
	}
	for i, p := range [2][2]flowLeg{{a, b}, {b, a}} {
		l, partner := p[0], p[1]
		if fates[i] == fateFundsDebut {
			m.funded[l.acct] += l.Amount
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
func (m *movedLegs) rolled(assets []*accountData, winTo int64) []returns.Flow {
	var out []returns.Flow
	for _, a := range assets {
		gaps, cd := map[int64][]returns.Flow{}, a.closureDay()
		for _, f := range m.moves[a] {
			if f.ValuedOn != 0 && f.ValuedOn != cd {
				gaps[f.ValuedOn] = append(gaps[f.ValuedOn], f)
			}
		}
		for _, on := range slices.Sorted(maps.Keys(gaps)) {
			if a.confirms(on, winTo, gaps[on]) {
				out = append(out, gaps[on]...)
			}
		}
	}
	return out
}

// entityFlows assembles the entity's external flow series, its quality tags and
// the moves the chain may read before their accounts value (rolled). Each dollar
// that crosses the entity's boundary counts once, in the same bucket as the value
// change it causes (RETURNS-NOTES §"staggered inception"):
//
//  1. every leg is classified once: its region in its account's life;
//  2. legs pair: a cross-source matched pair whose both legs would be flows on
//     their own drops (crossMatchedDrops), and the netter cancels opposite
//     transfer-like legs. It runs over every transfer-like leg, pre-debut and
//     zero-tail legs included, so an internal pair whose other leg sits on a
//     live account still cancels. A deposit or withdrawal never nets by
//     heuristic (RETURNS-NOTES);
//  3. each leg takes its fate (fateOf, pairFate) and is summed per account into
//     the three synthetic amounts (movedLegs);
//  4. the synthetic onboarding and closure flows book the regions the value
//     series cannot show yet or any more, less those amounts;
//  5. the flows are sorted by day.
//
// The flows are appended in a fixed order: each account's deposits and
// withdrawals in member order, then the transfer-like legs pairing keeps, then
// the synthetic flows. sortFlows is not stable, so that order fixes every float
// sum over the series.
func entityFlows(assets []*accountData, p ReturnParams, winFrom, winTo int64, aggregate bool) ([]returns.Flow, []string, []returns.Flow) {
	if !aggregate {
		// Accounts grain, single constituent: every policy-external flow is exact.
		// (Cross-source links never net here: the partner is by definition in
		// another source, so it can't be a member of a single-account entity.)
		var flows []returns.Flow
		for f := range assets[0].legs(winFrom, winTo) {
			flows = append(flows, f)
		}
		return flows, nil, nil
	}
	w := daySpan{winFrom, winTo}

	// Cross-source matched pairs obey the same --netting switch as the
	// heuristic netter: netting off is the diagnostic view of every raw leg.
	var tags []string
	var crossDrop map[string]bool
	if p.Netting {
		var crossPairs int
		crossDrop, crossPairs = crossMatchedDrops(assets, winFrom, winTo)
		if crossPairs > 0 {
			tags = append(tags, fmt.Sprintf("cross_source_netted=%d", crossPairs))
		}
	}

	var flows []returns.Flow
	var cand []flowLeg
	moved := newMovedLegs()
	for _, a := range assets {
		key := acctKey(a.src, a.acct)
		for f, nettable := range a.legs(winFrom, winTo) {
			f.ValuedOn = a.valuedOn(f.Day)
			l := flowLeg{Flow: f, acct: a, nettable: nettable, region: a.region(f.Day, w)}
			switch {
			case crossDrop != nil && crossDrop[acctKey(key, f.ID)]:
				flows = moved.settle(flows, l, fateInside)
			case nettable:
				cand = append(cand, l)
			default:
				flows = moved.settle(flows, l, fateOf(l))
			}
		}
	}
	kept := cand
	if p.Netting {
		var unmatched int
		kept, unmatched = netTransfers(cand, func(a, b flowLeg) { moved.pair(a, b, w) })
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
// is needed.
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
		if a.rpolicy.OnboardScope == returns.OnboardNone {
			// Crypto-sweep source: mid-window debuts are funded by within-entity
			// transfers already excluded from flows, so any onboarding here would
			// double-count. Read source-scoped from a.rpolicy exactly like the
			// per-entity-once check, so only this source's constituents skip.
			continue
		}
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
			if of, ok := returns.FundedOnboardingFlow(debut, v, m.funded[a]); ok {
				flows = append(flows, of)
			}
		}
	}
	// Per-entity-once: onboard the aggregate step-up at each distinct post-w.from
	// debut day within the source group. When the group's inception coincides with
	// w.from (the common case once the window is anchored at the first real
	// snapshot) NO account debuts after w.from, so nothing is onboarded: the
	// inception value is the opening base V0, external deposits add capital, and
	// internal churn is nothing — capital counted exactly once.
	for _, grp := range perEntityGroups {
		for _, day := range groupDebutDays(grp, w.from, w.to) {
			step := groupOnboardStep(grp, day, m)
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
// per-entity-once source group gains a constituent after winFrom (a constituent's
// firstDay() in (winFrom, winTo]). The aggregate step-up is booked on each such
// day.
func groupDebutDays(grp []*accountData, winFrom, winTo int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, a := range grp {
		d := a.firstDay()
		if d > winFrom && d <= winTo && !seen[d] {
			seen[d] = true
			out = append(out, d)
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
// market moves on siblings do not touch the step-up. Floored at 0 so a debut day
// dominated by sibling drops can never book negative onboarding.
func groupOnboardStep(grp []*accountData, day int64, moved *movedLegs) float64 {
	var debutValue, siblingFundingDrop float64
	for _, a := range grp {
		newlyDebuts := a.firstDay() == day
		if newlyDebuts {
			if v, alive := a.valueAt(day); alive {
				debutValue += returns.ZeroedValue(v, day, a.closureDay())
			}
			debutValue -= moved.funded[a]
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

// crossMatchedDrops resolves the cross-source transfer links (crossLinks,
// written by matchCrossTransfers) against THIS entity window: a linked pair
// nets — both legs dropped — strictly when BOTH legs are live members, i.e.
// the partner's account is in the entity, both legs' days are inside
// (winFrom, winTo], and each leg would be a flow on its own (fateOf): a
// pre-debut or closure-drain leg is represented by its synthetic
// onboarding/closure amount, so dropping its live partner too would count the
// capital asymmetrically. Legs failing any condition keep their normal
// treatment: at finer grains — and for pairs straddling the window — each leg
// stays the real boundary flow it is for that view. Keys are
// acctKey(acctKey(src,acct), txID); the returned count is netted PAIRS.
func crossMatchedDrops(assets []*accountData, winFrom, winTo int64) (map[string]bool, int) {
	any := false
	for _, a := range assets {
		if len(a.crossLinks) > 0 {
			any = true
			break
		}
	}
	if !any {
		return nil, 0
	}
	members := make(map[string]*accountData, len(assets))
	for _, a := range assets {
		members[acctKey(a.src, a.acct)] = a
	}
	w := daySpan{winFrom, winTo}
	live := func(a *accountData, day int64, nettable bool) bool {
		return day > winFrom && day <= winTo &&
			fateOf(flowLeg{Flow: returns.Flow{Day: day}, acct: a, nettable: nettable, region: a.region(day, w)}) == fateFlow
	}
	drops := map[string]bool{}
	pairs := 0
	for _, a := range assets {
		if len(a.crossLinks) == 0 {
			continue // links are sparse; skip unlinked members entirely
		}
		for f, nettable := range a.legs(winFrom, winTo) {
			link, ok := a.crossLinks[f.ID]
			if !ok {
				continue
			}
			// Process each pair once, from its lexicographically-smaller leg.
			me, other := acctKey(acctKey(a.src, a.acct), f.ID), acctKey(acctKey(link.src, link.acct), link.txID)
			if me > other {
				continue
			}
			partner := members[acctKey(link.src, link.acct)]
			if partner == nil || !live(a, f.Day, nettable) ||
				!live(partner, link.day, partner.transferLikeIDs[link.txID]) {
				continue
			}
			drops[me], drops[other] = true, true
			pairs++
		}
	}
	return drops, pairs
}

// netTransfers greedily matches opposite-direction transfer legs (largest
// first) whose output-currency magnitudes agree within ε and whose days are
// within the netting window, and hands each matched pair to pair, positive leg
// first, in match order. The deterministic (|amount|, day, id) tie-break means
// internal pairs cancel the same way whatever a leg's region. Returns the legs
// left unpaired, positive ones first, and their count.
func netTransfers(cand []flowLeg, pair func(pos, neg flowLeg)) (kept []flowLeg, unmatched int) {
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
			eps := nettingEpsFloor
			if r := nettingEpsRel * math.Max(math.Abs(pf.Amount), math.Abs(nf.Amount)); r > eps {
				eps = r
			}
			if math.Abs(pf.Amount+nf.Amount) <= eps && absDay(pf.Day-nf.Day) <= nettingWindowDay {
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
	switch a.rpolicy.OnboardScope {
	case returns.OnboardPerEntityOnce:
		return true
	case returns.OnboardPerConstituent:
		return !a.isConduit()
	}
	return false
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

func sumFlows(flows []returns.Flow) float64 {
	var s float64
	for _, f := range flows {
		s += f.Amount
	}
	return s
}

// sumFlowsIn is sumFlows(flowsIn(flows, from, to)), summed in the same order.
func sumFlowsIn(flows []returns.Flow, from, to int64) float64 {
	var s float64
	for _, f := range flows {
		if f.Day > from && f.Day <= to {
			s += f.Amount
		}
	}
	return s
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

// netting tolerances.
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
