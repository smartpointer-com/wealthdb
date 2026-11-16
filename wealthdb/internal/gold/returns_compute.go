package gold

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

const valueTol = 1e-6

// computeEntityReturn produces the per-bucket rows (when --period != total) plus
// the since-inception summary row for one entity built from its constituent
// asset accounts. A single-account accounts-grain entity is the degenerate case:
// no synthetic onboarding, no netting — exact.
func computeEntityReturn(assets []*accountData, p ReturnParams, toDay int64, fx fxBounds) []ReturnRow {
	rows, _ := computeEntityReturnFlows(assets, p, toDay, fx)
	return rows
}

// computeEntityReturnFlows is computeEntityReturn with the entity's flow series
// beside its rows, for a caller that reads both (the bridge).
func computeEntityReturnFlows(assets []*accountData, p ReturnParams, toDay int64, fx fxBounds) ([]ReturnRow, []returns.Flow) {
	// The accounts/sources/portfolios grains group by source, so every asset
	// shares one src; the global grain spans all sources, so there is no single
	// silver_source — assets[0].src would be a non-deterministic map pick.
	src := ""
	if p.Level != "global" {
		src = assets[0].src
	}
	entityID, label := entityIdentity(p.Level, assets)

	winFrom, winTo, incFlags := entityWindow(assets, p, toDay)
	if winTo <= winFrom {
		return nil, nil
	}
	aggregate := len(assets) > 1 || p.Level != "accounts"

	av := entityValue(assets)

	flows, qFlowTags, rolled := entityFlows(assets, p, winFrom, winTo, aggregate)

	// Snapshot-day union (real snapshots) for inception, the canonical headline
	// bucket, and empty- / stale-bucket detection.
	snaps := unionSnapshotDays(assets)
	snapGap, snapGapOK := medianSnapGap(snaps)

	base := ReturnRow{
		SilverSourceID: src, EntityID: entityID, EntityLabel: label,
		StartDay: winFrom, EndDay: winTo,
	}

	// Entity-level quality, carried on the summary row (and the
	// nonpositive-base row that replaces it).
	entityQ := append([]string{}, incFlags...)
	entityQ = append(entityQ, qFlowTags...)
	entityQ = append(entityQ, regimeFlags(assets, winFrom, winTo)...)
	if preFxHistory(assets, p.OutCcy, winFrom, fx) {
		entityQ = append(entityQ, "pre_fx_history")
	}
	entityQ = append(entityQ, "after_tax")

	v0, ok0 := av(winFrom)
	if !ok0 || v0 <= valueTol {
		// Non-positive / unpriceable opening base ⇒ no meaningful return.
		r := base
		r.Period, r.IsSummary = "total", true
		r.StartValue = decStrOrNil(v0, ok0)
		if vEnd, okE := av(winTo); okE {
			r.EndValue = decStr(vEnd)
		}
		r.Quality = append(entityQ, "nonpositive_base")
		return []ReturnRow{r}, flows
	}

	var out []ReturnRow

	// Per-bucket display rows (TWR only). prevEmpty tracks whether the preceding
	// bucket carried forward with no fresh snapshot, so the *receiving* bucket
	// (fresh V1, stale carried V0) that over-attributes the accumulated move can
	// be flagged boundary_same_snapshot — distinct from the donor empty_bucket.
	if p.Period != "total" {
		prevEmpty := false
		for _, b := range returns.BucketBoundaries(winFrom, winTo, periodKind(p.Period)) {
			row := bucketRow(base, b[0], b[1], p.Period, av, flows)
			empty := !hasSnapshotIn(snaps, b[0], b[1])
			switch {
			case empty:
				row.Quality = append(row.Quality, "empty_bucket", "carried_forward")
			case prevEmpty:
				row.Quality = append(row.Quality, "boundary_same_snapshot")
			}
			// A bucket can hold snapshots yet end on a value far older
			// than the source's own cadence (a feed that died
			// mid-bucket) — invisible to the empty/carried flags.
			if !empty && staleAt(snaps, b[1], snapGap, snapGapOK) {
				row.Quality = append(row.Quality, "stale_snapshot")
			}
			prevEmpty = empty
			out = append(out, row)
		}
	}

	// Since-inception summary row, cumulative TWR pinned to the canonical bucket.
	residue := residueEnds(assets, p, toDay, aggregate)
	sr := summaryRow(base, winFrom, winTo, av, flows, rolled, snaps, residue, assets, p, entityQ)
	if staleAt(snaps, winTo, snapGap, snapGapOK) {
		sr.Quality = append(sr.Quality, "stale_snapshot")
	}
	out = append(out, sr)
	return out, flows
}

// entityValue is the aggregate value at a day: each constituent's
// carry-forward value, zeroed from its closure day (ZeroedValue).
func entityValue(assets []*accountData) func(day int64) (float64, bool) {
	return func(day int64) (float64, bool) {
		var sum float64
		var any bool
		for _, a := range assets {
			v, alive := a.valueAt(day)
			if !alive {
				continue
			}
			any = true
			sum += returns.ZeroedValue(v, day, a.closureDay())
		}
		return sum, any
	}
}

// staleFactor is the source-relative staleness multiple: an end day
// valued from a snapshot older than staleFactor times the entity's
// median snapshot gap is flagged stale_snapshot.
const staleFactor = 3

// medianSnapGap returns the median gap between consecutive snapshot
// days. ok is false with fewer than 3 gaps — too little history to
// call a cadence, so staleness is never inferred.
func medianSnapGap(snaps []int64) (gap float64, ok bool) {
	if len(snaps) < 4 {
		return 0, false
	}
	gaps := make([]int64, 0, len(snaps)-1)
	for i := 1; i < len(snaps); i++ {
		gaps = append(gaps, snaps[i]-snaps[i-1])
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	if n := len(gaps); n%2 == 1 {
		return float64(gaps[n/2]), true
	} else {
		return float64(gaps[n/2-1]+gaps[n/2]) / 2, true
	}
}

// staleAt reports whether the freshest snapshot at or before day is
// older than staleFactor x the median gap. Strict >, so a daily
// source's ordinary weekend gap stays quiet.
func staleAt(snaps []int64, day int64, gap float64, ok bool) bool {
	if !ok {
		return false
	}
	i := sort.Search(len(snaps), func(i int) bool { return snaps[i] > day })
	if i == 0 {
		return false
	}
	return float64(day-snaps[i-1]) > staleFactor*gap
}

func bucketRow(base ReturnRow, bs, be int64, period string, av func(int64) (float64, bool), flows []returns.Flow) ReturnRow {
	r := base
	r.StartDay, r.EndDay = bs, be
	r.Period = PeriodLabel(returns.DayToTimeUTC(be-1), period) // a day inside the bucket
	v0, ok0 := av(bs)
	v1, ok1 := av(be)
	r.StartValue = decStrOrNil(v0, ok0)
	r.EndValue = decStrOrNil(v1, ok1)
	bf := flowsIn(flows, bs, be)
	r.NetFlow = decStr(sumFlows(bf))
	if ok0 && ok1 {
		if twr, ok := returns.ModifiedDietz(v0, v1, bs, be, bf); ok {
			r.TWR = f64(twr)
		} else {
			r.Quality = append(r.Quality, "dietz_degenerate")
		}
	}
	return r
}

// preFxHistory reports whether the entity's window starts before the earliest FX
// rate for a held non-output currency, so its boundary values were converted off
// fx_daily's day-0 floor rate (derived/10_fx.sql).
func preFxHistory(assets []*accountData, outCcy string, winFrom int64, fx fxBounds) bool {
	for _, a := range assets {
		if a.baseCurrency == "" || a.baseCurrency == outCcy {
			continue
		}
		if e, ok := fx.earliest[a.baseCurrency]; ok && winFrom < e {
			return true
		}
	}
	return false
}

func summaryRow(base ReturnRow, winFrom, winTo int64, av func(int64) (float64, bool), flows, rolled []returns.Flow, snaps []int64, residue map[int64]bool, assets []*accountData, p ReturnParams, entityQ []string) ReturnRow {
	r := base
	r.Period, r.IsSummary = summaryLabel(winFrom, winTo), true
	v0, _ := av(winFrom)
	v1, _ := av(winTo)
	r.StartValue, r.EndValue = decStr(v0), decStr(v1)
	r.NetFlow = decStr(sumFlowsIn(flows, winFrom, winTo))
	q := append([]string{}, entityQ...)

	// Cumulative TWR chained over the entity's actual valuation (snapshot) days,
	// independent of --period. Snapshot-aligned sub-periods keep every flow in the
	// same bucket as the value change it causes (unvaluedAt), so a flow landing in
	// a gap between sparse snapshots can't manufacture a sub-(-100%) bucket that
	// poisons the geometric chain — a fixed calendar bucket could, when a period
	// boundary falls inside a snapshot gap and splits a flow from its later value
	// realisation (see RETURNS-NOTES §"snapshot-aligned headline").
	bounds := canonicalChainBounds(winFrom, winTo, snaps)
	var buckets []returns.Bucket
	degenerate, skipped := false, false
	for _, sp := range chainSubPeriods(bounds, av, rolled, flows) {
		if sp.empty {
			continue
		}
		if residue[sp.to] {
			skipped = true
			continue
		}
		rr, ok := returns.ModifiedDietz(sp.v0, sp.v1, sp.from, sp.to, sp.flows)
		buckets = append(buckets, returns.Bucket{R: rr, OK: ok})
		if !ok {
			degenerate = true
		}
	}
	if skipped {
		q = append(q, "post_exit_residue")
	}
	if p.Method == "twr" || p.Method == "both" {
		cum, ok := returns.Chain(buckets)
		if len(buckets) == 0 && skipped {
			// A window wholly on a residue: each sub-period a factor of 1.
			cum, ok = 0, true
		}
		if ok {
			r.TWR = f64(cum)
			days := float64(bounds[len(bounds)-1] - bounds[0])
			if returns.ShouldAnnualize(p.Annualize, days) {
				r.TWRAnnualized = f64(returns.Annualize(cum, days))
			}
		} else if degenerate {
			q = append(q, "dietz_degenerate")
		}
	}

	// MWR over the window, on the values the chain reads: the window's edges with
	// what moved but its accounts do not show yet rolled in. start, end and
	// net_flow stay the carried values, which holdings and the bridge read.
	edges := unvaluedAt([]int64{winFrom, winTo}, rolled)
	edges[0], edges[1] = unshownValue(edges[0]), unshownValue(edges[1])
	if edges[0] != 0 || edges[1] != 0 {
		q = append(q, "rolled_forward")
	}
	if p.Method == "mwr" || p.Method == "both" {
		r.MWR, r.MWRAnnualized, q = computeMWR(v0+edges[0], v1+edges[1], winFrom, winTo, flows, assets, p, q)
	}

	r.Quality = dedupeStrings(q)
	return r
}

// The post-exit residue rule (RETURNS-NOTES, "Post-exit residue"). An exit is
// a sub-period, or a run of them, after which the value left is below
// residueShare of the value before it, outflows having taken at least
// exitOutflowShare of that value. The residue is every later sub-period that
// opens below residueShare of it. Which sub-periods are residue is decided in
// residueCurrency alone and applied in every reporting currency: flows and
// values convert at their own days' rates, so a share near a threshold could
// fall on either side of it in another currency.
const (
	residueShare     = 0.01
	exitOutflowShare = 0.90
	residueCurrency  = "USD"
)

// openingValue is an entity's value at from as the chain and MWR read it: what
// its accounts moved before from and do not show yet is in it.
func openingValue(av func(int64) (float64, bool), from int64, rolled []returns.Flow) float64 {
	v, _ := av(from)
	return v + unshownValue(unvaluedAt([]int64{from}, rolled)[0])
}

// unshownValue is an unvaluedAt figure as money: float dust from moves that
// cancel reads as nothing, so a value with nothing unshown stays as carried.
func unshownValue(v float64) float64 {
	if math.Abs(v) <= valueTol {
		return 0
	}
	return v
}

// subPeriod is one sub-period of the chain: its edges' values with the moves
// rolled in, and its flows.
type subPeriod struct {
	from, to int64
	v0, v1   float64
	flows    []returns.Flow
	// empty: the entity holds nothing and moves nothing, so there is no return
	// to chain, and a closed account's later zero valuations leave the chain
	// where it ended.
	empty bool
}

// chainSubPeriods are the sub-periods between bounds, read from the entity's
// value av, its moves rolled and its flows.
func chainSubPeriods(bounds []int64, av func(int64) (float64, bool), rolled, flows []returns.Flow) []subPeriod {
	unvalued := unvaluedAt(bounds, rolled)
	out := make([]subPeriod, 0, len(bounds))
	for i := 0; i+1 < len(bounds); i++ {
		sp := subPeriod{from: bounds[i], to: bounds[i+1], flows: flowsIn(flows, bounds[i], bounds[i+1])}
		sp.v0, _ = av(bounds[i])
		sp.v1, _ = av(bounds[i+1])
		sp.v0, sp.v1 = sp.v0+unvalued[i], sp.v1+unvalued[i+1]
		sp.empty = math.Abs(sp.v0) <= valueTol && math.Abs(sp.v1) <= valueTol && len(sp.flows) == 0
		out = append(out, sp)
	}
	return out
}

// residueSkips is, for each sub-period, whether it sits on the residue of an
// exit, by the day the sub-period ends.
func residueSkips(sps []subPeriod) map[int64]bool {
	skips := map[int64]bool{}
	var w exitWatch
	for _, sp := range sps {
		if !sp.empty && w.residue(sp.v0, sp.v1, sp.flows) {
			skips[sp.to] = true
		}
	}
	return skips
}

// residueEnds is the entity's residue decision: the end days of the
// sub-periods that sit on the residue of an exit. It is read once, on the
// entity's since-inception chain, so a window that opens after an exit or
// inside one skips the same sub-periods: a window's first sub-period ends on
// the same day as the inception chain's. It is read in residueCurrency on the
// accounts' twins, so every reporting currency applies one decision. An
// account with no twin, where that currency's dataset could not be loaded,
// leaves the reporting currency to decide alone.
func residueEnds(assets []*accountData, p ReturnParams, toDay int64, aggregate bool) map[int64]bool {
	dec := assets
	if p.OutCcy != residueCurrency {
		dec = make([]*accountData, len(assets))
		for i, a := range assets {
			if dec[i] = a.twin; dec[i] == nil {
				dec = assets
				break
			}
		}
	}
	since := p
	since.FromEpoch = 0
	compute := func() map[int64]bool {
		from, to, _ := entityWindow(dec, since, toDay)
		if to <= from {
			return nil
		}
		flows, _, rolled := entityFlows(dec, since, from, to, aggregate)
		return residueSkips(chainSubPeriods(canonicalChainBounds(from, to, unionSnapshotDays(dec)), entityValue(dec), rolled, flows))
	}
	if dec[0].residue == nil {
		return compute()
	}
	keys := make([]string, len(dec))
	for i, a := range dec {
		keys[i] = acctKey(a.src, a.acct)
	}
	slices.Sort(keys)
	key := fmt.Sprintf("%s|%d|%t|%t|%s|%p|%s", p.Level, toDay, aggregate, p.Netting, p.Inception,
		p.InceptionOverrides, strings.Join(keys, "|"))
	return dec[0].residue.get(key, compute)
}

// exitWatch follows a chain's sub-periods in order and says which sit on the
// residue of an exit, so they add nothing to the chain. A run is consecutive
// sub-periods that each move money out, or move none; it holds the opening
// value of its first plus its inflows, and a sub-period with a net inflow ends
// it. A sale spread over several days is one exit. A sub-period is also
// measured alone, so a run that opened higher cannot hide an exit.
type exitWatch struct {
	running      bool
	held, out    float64 // the run's opening value plus its inflows, and its outflows
	residueBelow float64 // > 0 after an exit: the opening value below which a sub-period is residue
}

// residue reports whether the sub-period from v0 to v1 with flows sits on an
// exit's residue, and otherwise records it towards the next exit.
func (w *exitWatch) residue(v0, v1 float64, flows []returns.Flow) bool {
	if w.residueBelow > 0 {
		if v0 < w.residueBelow {
			return true
		}
		w.residueBelow = 0
	}
	var in, out float64
	for _, f := range flows {
		if f.Amount > 0 {
			in += f.Amount
		} else {
			out -= f.Amount
		}
	}
	if in > out {
		w.running = false
		return false
	}
	if !w.running {
		w.running, w.held, w.out = true, v0, 0
	}
	w.held += in
	w.out += out
	exit := func(held, out float64) bool {
		return held > valueTol && v1 < residueShare*held && out >= exitOutflowShare*held
	}
	switch {
	case exit(w.held, w.out):
		w.residueBelow = residueShare * w.held
	case exit(v0+in, out):
		w.residueBelow = residueShare * (v0 + in)
	default:
		return false
	}
	w.running = false
	return false
}

func computeMWR(v0, v1 float64, winFrom, winTo int64, flows []returns.Flow, assets []*accountData, p ReturnParams, q []string) (*float64, *float64, []string) {
	rate, q := solveMWR(returns.XIRR, v0, v1, winFrom, winTo, flowsIn(flows, winFrom, winTo), assets, q)
	if rate == nil {
		return nil, nil, q
	}
	days := float64(winTo - winFrom)
	var ann *float64
	if returns.ShouldAnnualize(p.Annualize, days) {
		ann = rate // XIRR is already an annual rate
	}
	// The mwr_pct column shows the PERIOD (cumulative-equivalent) figure so it is
	// consistent with the twr_pct column; mwr_ann_pct holds the annualized XIRR.
	// For a full-year window the two coincide.
	return f64(returns.DeAnnualize(*rate, days)), ann, q
}

// solveMWR is the annual money-weighted rate of a window's flows through
// solve, or nil with the mwr_* flag that says why there is none.
func solveMWR(solve func(v0, v1 float64, from, to int64, flows []returns.Flow) (float64, error),
	v0, v1 float64, winFrom, winTo int64, windowFlows []returns.Flow, assets []*accountData, q []string) (*float64, []string) {
	if allNavOnly(assets) || len(windowFlows) == 0 {
		return nil, append(q, "mwr_no_flows")
	}
	// If the entity's net invested capital over the window — the opening base plus
	// the total net external flow (deposits minus withdrawals) — is zero or below,
	// the investor has on net been repaid at least everything they committed, so
	// the money-weighted return is a return on non-positive capital: XIRR yields an
	// extreme, non-unique root. Surface n/a. This is a WHOLE-WINDOW measure on
	// purpose: an interim contribution dip (one constituent's large distribution
	// before another's later deposits, or a journal-out whose funding trade isn't a
	// counted flow) does not mean the capital was ever truly negative, so a window
	// that ends net-positive still gets a valid MWR.
	//
	// One shape is exempt: a FULLY-REALIZED window (end value ~0) whose cash-flow
	// sequence has exactly one sign change — the textbook profitable exit
	// (committed capital repaid at more than cost, e.g. a ledger-exact closure
	// sold above its invested basis). Its IRR is unique and well-defined; blanking
	// it would deny an MWR to precisely the gains the flow-counting policies
	// exist to surface, while losses (net capital still positive) compute one.
	if mwrNetCapitalNonPositive(v0, windowFlows) &&
		(v1 > valueTol || returns.MWRSignChanges(v0, v1, winFrom, winTo, windowFlows) != 1) {
		return nil, append(q, "mwr_negative_net_capital")
	}
	rate, err := solve(v0, v1, winFrom, winTo, windowFlows)
	if err != nil {
		switch err {
		case returns.ErrNoSignChange:
			q = append(q, "mwr_no_sign_change")
		case returns.ErrNoFlows:
			q = append(q, "mwr_no_flows")
		default:
			q = append(q, "mwr_no_converge")
		}
		return nil, q
	}
	if anyNavOnly(assets) {
		q = append(q, "mwr_incomplete_flows")
	}
	if returns.MWRSignChanges(v0, v1, winFrom, winTo, windowFlows) > 1 {
		q = append(q, "mwr_nonunique")
	}
	return &rate, q
}

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

// ---- liability + window + identity --------------------------------------

func liabilityRow(a *accountData, toDay int64) ReturnRow {
	from := a.firstDay()
	v0, ok0 := a.valueAt(from)
	v1, ok1 := a.valueAt(toDay)
	return ReturnRow{
		SilverSourceID: a.src, EntityID: a.acct, EntityLabel: a.label,
		Period: "total", IsSummary: true, StartDay: from, EndDay: toDay,
		StartValue: decStrOrNil(v0, ok0), EndValue: decStrOrNil(v1, ok1),
		Quality: []string{"nonpositive_base", "after_tax"},
	}
}

func entityWindow(assets []*accountData, p ReturnParams, toDay int64) (from, to int64, flags []string) {
	to = toDay
	// Per-constituent inception. Under a constituent's Inception=first-real-
	// snapshot policy its anchor is the first REAL snapshot day rather than the
	// first spine day, which kills the tiny-base artifact where a sparse pre-
	// snapshot cash tail opened the window years early. The choice is per
	// constituent, so it is source-scoped even in the merged global entity:
	// only that source's constituents move their anchor; every other source keeps
	// firstDay(). Default policy (InceptionFullWindow) leaves anchorDay == firstDay.
	// anchorDay is a constituent's inception anchor: firstDay() by default;
	// firstRealSnapshotDay() under Inception=first-real-snapshot. A first-real-
	// snapshot CONDUIT is skipped from the min so its sparse cash-only pre-history
	// (cash held months before the first securities position) can't anchor the
	// unit early — the anchor prefers a non-conduit real snapshot; if a group is
	// all conduit, the min falls back to including conduits so the window is
	// never empty.
	anchorDay := func(a *accountData) (day int64, skipForMin bool) {
		if a.rpolicy.Inception == returns.InceptionFirstRealSnapshot {
			return a.firstRealSnapshotDay(), a.isConduit()
		}
		return a.firstDay(), false
	}
	incMin, incMax := int64(math.MaxInt64), int64(0)
	for _, a := range assets {
		fd, skip := anchorDay(a)
		if fd > incMax {
			incMax = fd
		}
		if !skip && fd < incMin {
			incMin = fd
		}
	}
	// Fallback: an all-conduit first-real-snapshot group contributed nothing to the
	// min above — anchor over every constituent so the window is never empty.
	if incMin == int64(math.MaxInt64) {
		for _, a := range assets {
			fd, _ := anchorDay(a)
			if fd < incMin {
				incMin = fd
			}
		}
	}
	aggregate := len(assets) > 1
	entityInception := incMin
	if p.Inception == "strict" && aggregate {
		entityInception = incMax
	}

	// User-configured inception floor (wealthdb.cfg inception_overrides),
	// resolved per grain most-specific-first from the entity's own identity.
	// It can only move the anchor LATER, never earlier, and only for the entity
	// whose key matched — so an unconfigured entity (nil overrides, or no key)
	// keeps its data-derived inception exactly. The global grain is never
	// resolved (resolve returns false), so a merged global entity is untouched.
	a0 := assets[0]
	configured := false
	if cfg, ok := p.InceptionOverrides.resolve(p.Level, a0.src, a0.portfolio, a0.acct); ok {
		if cfgDay := EpochDay(cfg); cfgDay > entityInception {
			entityInception = cfgDay
			configured = true
		}
	}

	if p.FromEpoch > 0 {
		from = EpochDay(p.FromEpoch)
		if from < entityInception {
			from = entityInception
			flags = append(flags, "partial_window")
		}
	} else {
		from = entityInception
		if !configured {
			flags = append(flags, "since_data_inception")
		}
	}
	if configured {
		flags = append(flags, "configured_inception")
	}
	if aggregate && incMax > from {
		flags = append(flags, "staggered_inception")
	}
	// Observed external ledger flows dated strictly before the resolved
	// inception are outside every window this entity can produce (flowsIn is
	// (from, to], and from never precedes the inception), so the measurement
	// cannot see them. Surface the exclusion: with a configured override the
	// flag documents exactly what the override cuts off; without one it means
	// the transaction history reaches further back than the value spine
	// supports. Compared against the inception, NOT the window start, so a
	// FROM clip or a windowed partition never raises it.
	for _, a := range assets {
		if hasExternalFlowBefore(a, entityInception) {
			flags = append(flags, "flows_before_inception")
			break
		}
	}
	return from, to, flags
}

// hasExternalFlowBefore reports whether the constituent's ledger carries any
// policy-external flow dated strictly before day. A flow ON the day is not an
// exclusion — it is subsumed into the opening base, like any debut-day flow.
// Both flow slices are appended in occurred_at order (the loaders' pinned
// ORDER BY), so checking each slice's first element is exact.
func hasExternalFlowBefore(a *accountData, day int64) bool {
	return (len(a.nonTransfer) > 0 && a.nonTransfer[0].Day < day) ||
		(len(a.transferLike) > 0 && a.transferLike[0].Day < day)
}

// NoPortfolioLabel names the accounts of a source outside any
// portfolio, wherever a report lists them as one.
const NoPortfolioLabel = "(no portfolio)"

func entityIdentity(level string, assets []*accountData) (id, label string) {
	a := assets[0]
	switch level {
	case "accounts":
		return a.acct, a.label
	case "sources":
		return a.src, a.src
	case "portfolios":
		if a.portfolio == "" {
			return "", NoPortfolioLabel
		}
		// EntityID stays the stable portfolio_external_id; the label prefers the
		// resolved display name (falling back to the id when unresolved).
		if a.portfolioName != "" {
			return a.portfolio, a.portfolioName
		}
		return a.portfolio, a.portfolio
	default:
		return "", "global"
	}
}

// ---- small helpers -------------------------------------------------------

func periodKind(period string) returns.BucketKind {
	switch period {
	case "monthly":
		return returns.BucketMonthly
	case "annual":
		return returns.BucketAnnual
	case "total":
		return returns.BucketTotal
	default:
		return returns.BucketQuarterly
	}
}

func unionSnapshotDays(assets []*accountData) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, a := range assets {
		for _, d := range a.snapDays {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func hasSnapshotIn(snaps []int64, from, to int64) bool {
	i := sort.Search(len(snaps), func(k int) bool { return snaps[k] > from })
	return i < len(snaps) && snaps[i] <= to
}

// canonicalChainBounds returns the valuation-day boundaries for the canonical
// cumulative TWR: winFrom, then every snapshot day in (winFrom, winTo]. Each
// sub-period therefore spans one real valuation interval, so a flow is always
// chained against the snapshot-to-snapshot value change it belongs to — never
// against a carried-flat calendar bucket, which is what let a flow in a sparse
// snapshot gap poison the chain with a sub-(-100%) sub-period. A carried tail
// past the last snapshot (winTo with no fresh valuation) is not a sub-period and
// is excluded; a window with no interior valuation falls back to one
// [winFrom, winTo] bucket. snaps must be ascending (unionSnapshotDays).
func canonicalChainBounds(winFrom, winTo int64, snaps []int64) []int64 {
	bounds := []int64{winFrom}
	for _, d := range snaps {
		if d > winFrom && d <= winTo {
			bounds = append(bounds, d)
		}
	}
	if len(bounds) == 1 {
		bounds = append(bounds, winTo)
	}
	return bounds
}

// unvaluedAt is, for each of bounds, what moves has rolled forward there: what the
// entity's accounts moved since each last valued, which their carried values do
// not show yet. A coarse grain's valuation days are the union of its accounts', so
// a boundary can fall between a flow into a sparsely valued account and the
// valuation that shows it. Without this, one sub-period holds the flow against a
// flat value and a later one the value change against no flow, and the chain keeps
// the difference, about the square of the flow's share of the entity. On an
// account's own valuation days this is nothing. bounds must be ascending.
func unvaluedAt(bounds []int64, moves []returns.Flow) []float64 {
	type step struct {
		day    int64
		amount float64
	}
	steps := make([]step, 0, 2*len(moves))
	for _, m := range moves {
		steps = append(steps, step{m.Day, m.Amount}, step{m.ValuedOn, -m.Amount})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].day < steps[j].day })
	out := make([]float64, len(bounds))
	sum, next := 0.0, 0
	for i, b := range bounds {
		for ; next < len(steps) && steps[next].day <= b; next++ {
			sum += steps[next].amount
		}
		out[i] = sum
	}
	return out
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

// mwrNetCapitalNonPositive reports whether the entity's net invested capital over
// the window — the opening base v0 plus the total net external flow (deposits
// positive, withdrawals negative, per the net_flow convention) — is zero or
// below. It is a whole-window sum, not a running minimum: an interim dip in
// cumulative contributions is not a genuinely-negative capital position (the
// account value never went there), so it must not disqualify the MWR.
func mwrNetCapitalNonPositive(v0 float64, flows []returns.Flow) bool {
	return v0+sumFlows(flows) <= valueTol
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

func allNavOnly(assets []*accountData) bool {
	for _, a := range assets {
		if a.policy.Regime != returns.RegimeNavOnly {
			return false
		}
	}
	return true
}

func anyNavOnly(assets []*accountData) bool {
	for _, a := range assets {
		if a.policy.Regime == returns.RegimeNavOnly {
			return true
		}
	}
	return false
}

func regimeFlags(assets []*accountData, winFrom, winTo int64) []string {
	var flags []string
	if allNavOnly(assets) {
		flags = append(flags, "nav_only", "nav_only_capital_call_risk")
	} else if anyNavOnly(assets) {
		flags = append(flags, "nav_only_capital_call_risk")
	}
	// Aggregate the per-asset conditions first, then emit in a fixed order, so
	// the flag list is deterministic regardless of asset-iteration order. These
	// flags are a display SET — order carries no meaning — and emitting them in
	// iteration order would make a multi-asset entity's flag ordering depend on
	// which constituent happened to be visited first.
	var crypto, journal, unknown, clamped, dropped, capRiskFlowless bool
	for _, a := range assets {
		crypto = crypto || (a.policy.Regime == returns.RegimeCryptoPartial && a.cryptoExcluded)
		journal = journal || a.journalPresent
		unknown = unknown || !a.policy.Known
		clamped = clamped || a.hasClampedFlow
		dropped = dropped || a.droppedNonzero
		// A CapitalCallRisk constituent whose OWN accounts observed no
		// external ledger flow in the window: its value growth may embed
		// unobserved capital calls, so the summary keeps the honesty tag even
		// under a flow-counting regime. Deliberately per constituent — the
		// assembled entity flow series would let a sibling source's flows (at
		// coarse grains) or a synthetic onboarding/closure flow mask the gap.
		capRiskFlowless = capRiskFlowless ||
			(a.rpolicy.CapitalCallRisk && !hasObservedFlowIn(a, winFrom, winTo))
	}
	if crypto {
		flags = append(flags, "crypto_unclassified_transfers")
	}
	if journal {
		flags = append(flags, "journal_present")
	}
	if unknown {
		flags = append(flags, "unknown_adapter_policy")
	}
	if clamped {
		flags = append(flags, "fx_clamped_flow")
	}
	if dropped {
		flags = append(flags, "dropped_while_nonzero")
	}
	if capRiskFlowless {
		flags = append(flags, "nav_only_capital_call_risk")
	}
	return dedupeStrings(flags)
}

// hasObservedFlowIn reports whether the account itself observed an external
// ledger flow inside (from, to] — the same bounds flowsIn uses. Only real
// attached flows count: synthetic onboarding/closure flows are assembled per
// entity at compute time and never stored on the account.
func hasObservedFlowIn(a *accountData, from, to int64) bool {
	for range a.legs(from, to) {
		return true
	}
	return false
}

// monthLabelLayout is a monthly bucket's label, YYYY-MM: the key
// ReconcileStatedPeriods reads the engine's monthly rows by.
const monthLabelLayout = "2006-01"

// PeriodLabel is the calendar label of the period bucket t falls in:
// a day or a week by its date (DuckDB truncates a week to its Monday,
// so a week's first second names it), a month YYYY-MM, a year YYYY, a
// quarter YYYY-Qn. Every report's period column reads it.
func PeriodLabel(t time.Time, period string) string {
	switch period {
	case "daily", "weekly":
		return t.Format("2006-01-02")
	case "monthly":
		return t.Format(monthLabelLayout)
	case "annual":
		return t.Format("2006")
	default: // quarterly
		return fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
	}
}

func summaryLabel(from, to int64) string {
	tf := returns.DayToTimeUTC(from)
	tt := returns.DayToTimeUTC(to - 1)
	if tf.Year() == tt.Year() {
		return tf.Format("2006")
	}
	return "total"
}

func absDay(d int64) int64 {
	if d < 0 {
		return -d
	}
	return d
}

func f64(v float64) *float64 { return &v }

func decStrOrNil(v float64, ok bool) *string {
	if !ok {
		return nil
	}
	return decStr(v)
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
