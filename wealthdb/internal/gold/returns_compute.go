package gold

import (
	"fmt"
	"math"
	"sort"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

const valueTol = 1e-6

// computeEntityReturn produces the per-bucket rows (when --period != total) plus
// the since-inception summary row for one entity built from its constituent
// asset accounts. A single-account accounts-grain entity is the degenerate case:
// no synthetic onboarding, no netting — exact.
func computeEntityReturn(assets []*accountData, p ReturnParams, toDay int64, fx fxBounds) []ReturnRow {
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
		return nil
	}
	aggregate := len(assets) > 1 || p.Level != "accounts"

	// Aggregate value at a day: each constituent's carry-forward value, zeroed
	// from its explicit-closure day (atomic with the closure outflow).
	av := func(day int64) (float64, bool) {
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

	flows, qFlowTags := entityFlows(assets, p, winFrom, winTo, av, aggregate)

	// Snapshot-day union (real snapshots) for inception, the canonical headline
	// bucket, and empty- / stale-bucket detection.
	snaps := unionSnapshotDays(assets)
	snapGap, snapGapOK := medianSnapGap(snaps)

	base := ReturnRow{
		SilverSourceID: src, EntityID: entityID, EntityLabel: label,
		StartDay: winFrom, EndDay: winTo,
	}

	// Entity-level quality that applies to every row.
	entityQ := append([]string{}, incFlags...)
	entityQ = append(entityQ, qFlowTags...)
	entityQ = append(entityQ, regimeFlags(assets)...)
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
		return []ReturnRow{r}
	}

	var out []ReturnRow

	// Per-bucket display rows (TWR only). prevEmpty tracks whether the preceding
	// bucket carried forward with no fresh snapshot, so the *receiving* bucket
	// (fresh V1, stale carried V0) that over-attributes the accumulated move can
	// be flagged boundary_same_snapshot — distinct from the donor empty_bucket
	// (review #5).
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
	sr := summaryRow(base, winFrom, winTo, av, flows, snaps, assets, p, entityQ)
	if staleAt(snaps, winTo, snapGap, snapGapOK) {
		sr.Quality = append(sr.Quality, "stale_snapshot")
	}
	out = append(out, sr)
	return out
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
	r.Period = periodLabel(bs, be, period)
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
// the migration-0023 day-0 clamped rate (review #4).
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

func summaryRow(base ReturnRow, winFrom, winTo int64, av func(int64) (float64, bool), flows []returns.Flow, snaps []int64, assets []*accountData, p ReturnParams, entityQ []string) ReturnRow {
	r := base
	r.Period, r.IsSummary = summaryLabel(winFrom, winTo), true
	v0, _ := av(winFrom)
	v1, _ := av(winTo)
	r.StartValue, r.EndValue = decStr(v0), decStr(v1)
	r.NetFlow = decStr(sumFlows(flowsIn(flows, winFrom, winTo)))
	q := append([]string{}, entityQ...)

	// Cumulative TWR chained over the entity's actual valuation (snapshot) days,
	// independent of --period. Snapshot-aligned sub-periods keep every flow in the
	// same bucket as the value change it causes, so a flow landing in a gap
	// between sparse snapshots can't manufacture a sub-(-100%) bucket that poisons
	// the geometric chain — a fixed calendar bucket could, when a period boundary
	// falls inside a snapshot gap and splits a flow from its later value
	// realisation (see RETURNS-NOTES §"snapshot-aligned headline").
	bounds := canonicalChainBounds(winFrom, winTo, snaps)
	var buckets []returns.Bucket
	degenerate := false
	for i := 0; i+1 < len(bounds); i++ {
		bv0, _ := av(bounds[i])
		bv1, _ := av(bounds[i+1])
		rr, ok := returns.ModifiedDietz(bv0, bv1, bounds[i], bounds[i+1], flowsIn(flows, bounds[i], bounds[i+1]))
		buckets = append(buckets, returns.Bucket{R: rr, OK: ok})
		if !ok {
			degenerate = true
		}
	}
	if p.Method == "twr" || p.Method == "both" {
		if cum, ok := returns.Chain(buckets); ok {
			r.TWR = f64(cum)
			days := float64(bounds[len(bounds)-1] - bounds[0])
			if returns.ShouldAnnualize(p.Annualize, days) {
				r.TWRAnnualized = f64(returns.Annualize(cum, days))
			}
		} else if degenerate {
			q = append(q, "dietz_degenerate")
		}
	}

	// MWR over the window.
	if p.Method == "mwr" || p.Method == "both" {
		r.MWR, r.MWRAnnualized, q = computeMWR(v0, v1, winFrom, winTo, flows, assets, p, q)
	}

	r.Quality = dedupeStrings(q)
	return r
}

func computeMWR(v0, v1 float64, winFrom, winTo int64, flows []returns.Flow, assets []*accountData, p ReturnParams, q []string) (*float64, *float64, []string) {
	windowFlows := flowsIn(flows, winFrom, winTo)
	if allNavOnly(assets) || len(windowFlows) == 0 {
		return nil, nil, append(q, "mwr_no_flows")
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
	if mwrNetCapitalNonPositive(v0, windowFlows) {
		return nil, nil, append(q, "mwr_negative_net_capital")
	}
	rate, err := returns.XIRR(v0, v1, winFrom, winTo, windowFlows)
	if err != nil {
		switch err {
		case returns.ErrNoSignChange:
			q = append(q, "mwr_no_sign_change")
		case returns.ErrNoFlows:
			q = append(q, "mwr_no_flows")
		default:
			q = append(q, "mwr_no_converge")
		}
		return nil, nil, q
	}
	if anyNavOnly(assets) {
		q = append(q, "mwr_incomplete_flows")
	}
	if returns.MWRSignChanges(v0, v1, winFrom, winTo, windowFlows) > 1 {
		q = append(q, "mwr_nonunique")
	}
	days := float64(winTo - winFrom)
	var ann *float64
	if returns.ShouldAnnualize(p.Annualize, days) {
		ann = f64(rate) // XIRR is already an annual rate
	}
	// The mwr_% column shows the PERIOD (cumulative-equivalent) figure so it is
	// consistent with the twr_% column; mwr_ann_% holds the annualized XIRR
	// (review #8). For a full-year window the two coincide.
	return f64(returns.DeAnnualize(rate, days)), ann, q
}

// ownedFlow is a constituent flow tagged with its owning account and whether it
// falls in a region the aggregate value series does NOT reflect (a late
// constituent's pre-debut arrival, or a closing constituent's drain into the
// zeroing snapshot). Such flows are SUBSUMED by the synthetic onboarding/closure
// amount instead of being counted again. Netting still runs over the full set
// (subsumed legs included) so genuine internal transfer pairs annihilate before
// subsumption ever applies (the staggered-inception netting interaction).
type ownedFlow struct {
	returns.Flow
	subsumed bool
}

// entityFlows assembles the entity's external flow series at a coarse grain:
//   - deposit/withdrawal flows (never netted — see RETURNS-NOTES);
//   - transfer-like flows, netted to drop internal moves;
//   - the synthetic onboarding/closure flows (which never enter netting).
//
// Each constituent's flows in a region the aggregate value series cannot yet (or
// no longer) reflect are subsumed by the synthetic amount, so each dollar
// crossing the entity boundary is counted exactly once, in the same bucket as the
// value change it causes. See subsumesAt / RETURNS-NOTES §"staggered inception".
func entityFlows(assets []*accountData, p ReturnParams, winFrom, winTo int64, av func(int64) (float64, bool), aggregate bool) ([]returns.Flow, []string) {
	var flows []returns.Flow
	var tags []string

	if !aggregate {
		// Accounts grain, single constituent: every policy-external flow is exact.
		return flowsIn(assets[0].allExternal(), winFrom, winTo), tags
	}

	// Deposits/withdrawals: always external (never netted — see RETURNS-NOTES).
	// Pre-debut flows are normally subsumed because synthetic onboarding books the
	// debut value instead; under OnboardNone there is no onboarding, so a real
	// debut-region deposit IS the capital event and must be kept (otherwise the
	// funded value shows up as pure performance). Closure-drain flows are still
	// subsumed — explicit closure fires regardless of OnboardScope.
	for _, a := range assets {
		onboardNone := a.rpolicy.OnboardScope == returns.OnboardNone
		for _, f := range flowsIn(a.nonTransfer, winFrom, winTo) {
			if subsumesAtClosure(a, f.Day, winFrom, winTo) {
				continue
			}
			if !onboardNone && subsumesAtDebut(a, f.Day, winFrom, winTo) {
				continue
			}
			flows = append(flows, f)
		}
	}

	// Transfer-like: net opposite pairs within the boundary to drop internal moves,
	// THEN subsume any surviving pre-debut / closure-drain leg. Netting runs over the
	// full candidate set (subsumed legs included) so an internal pair whose other leg
	// sits on an already-alive account still annihilates and is never orphaned.
	var cand []ownedFlow
	for _, a := range assets {
		for _, f := range flowsIn(a.transferLike, winFrom, winTo) {
			cand = append(cand, ownedFlow{Flow: f, subsumed: subsumesAt(a, f.Day, winFrom, winTo)})
		}
	}
	if p.Netting {
		kept, unmatched := netOwnedTransfers(cand)
		for _, of := range kept {
			if !of.subsumed { // a survivor that is its own pre-debut/closure capital is subsumed
				flows = append(flows, of.Flow)
			}
		}
		if unmatched > 0 {
			tags = append(tags, fmt.Sprintf("unmatched_transfers=%d", unmatched))
		}
	} else {
		for _, of := range cand {
			if !of.subsumed {
				flows = append(flows, of.Flow)
			}
		}
	}

	// Synthetic onboarding (the late constituent's whole arrival) + explicit closure
	// (its whole exit). The real pre-debut / closure-drain flows were subsumed above,
	// so the synthetic amount is the FULL boundary value — no near-day dedup needed.
	//
	// OnboardScope splits the onboarding GRAIN — the AMOUNT booked when a
	// constituent debuts after winFrom:
	//   - OnboardPerConstituent (default): the debuting account's OWN value. Today's
	//     behavior, untouched — no sibling account moves on a debut day in the
	//     flow-complete sources, so per-account == aggregate step-up there.
	//   - OnboardPerEntityOnce: the newly-debuting constituents' first value MINUS
	//     same-day sibling FUNDING drops only (NOT the raw calendar aggregate delta —
	//     see groupOnboardStep: same-day external deposits and market moves on
	//     siblings are excluded so they aren't double-counted). An internal
	//     cash->securities move inside a UBS
	//     relationship debuts a securities account while its funding cash account
	//     drops by the same amount, so the step-up nets to ~0 and NOTHING is
	//     onboarded — the capital was already booked once (inception value + the
	//     external cash deposit). Genuinely new external value that wasn't captured
	//     as a same-unit deposit still steps the aggregate up and is onboarded. This
	//     is the conduit double-count fix, and it is source-scoped: the grain is read
	//     per constituent from a.rpolicy, so a non-UBS constituent keeps
	//     per-constituent onboarding even in the merged global entity.
	// Conduit-kind accounts never emit their OWN onboarding (ConduitKinds); under
	// per-entity-once their value still enters the aggregate step-up (they hold the
	// inception cash), and under the default they are simply skipped.
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
		// Default per-constituent onboarding: the debuting account's own value.
		debut := a.firstDay()
		if debut > winFrom && debut <= winTo {
			v, _ := a.valueAt(debut)
			if of, ok := returns.OnboardingFlow(debut, v, 0); ok {
				flows = append(flows, of)
			}
		}
	}
	// Per-entity-once: onboard the aggregate step-up at each distinct post-winFrom
	// debut day within the source group. When the group's inception coincides with
	// winFrom (the common case once the window is anchored at the first real
	// snapshot) NO account debuts after winFrom, so nothing is onboarded: the
	// inception value is the opening base V0, external deposits add capital, and
	// internal churn is nothing — capital counted exactly once.
	for _, grp := range perEntityGroups {
		for _, day := range groupDebutDays(grp, winFrom, winTo) {
			step := groupOnboardStep(grp, day)
			if of, ok := returns.OnboardingFlow(day, step, 0); ok {
				flows = append(flows, of)
			}
		}
	}

	// Explicit closure (a constituent's whole exit) is unchanged by OnboardScope /
	// ConduitKinds: the carry-forward spine keeps lastValue past the closure day, so
	// the zeroing outflow is mandatory whether or not the account onboarded.
	for _, a := range assets {
		if cd := a.closureDay(); cd > winFrom && cd <= winTo {
			last, _ := a.valueAt(cd - 1)
			if cf, _, ok := returns.ClosureFlow(cd, last, 0); ok {
				flows = append(flows, cf)
			}
		}
	}
	sortFlows(flows)
	return flows, tags
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
// first value NET OF same-day sibling FUNDING drops only — NOT the raw aggregate
// calendar delta (the group's total value on day minus its total on day-1).
//
// The raw delta absorbed EVERY value movement across ALL group accounts on the
// debut day, so it double-counted two things that are not new onboarding capital:
//
//   - a same-day external cash deposit into a sibling conduit — already booked as
//     its own nonTransfer flow, so sweeping it into the step-up counts it twice;
//   - organic market appreciation / dividends / interest on existing holdings that
//     day — genuine RETURN silently reclassified as a capital inflow.
//
// Both surface as POSITIVE sibling deltas, so we exclude them: only NEGATIVE
// sibling deltas (a funding cash conduit draining to fund the new account) offset
// the debut value. An internal cash->securities move (new securities +X, funding
// cash -X) therefore nets to 0; a genuine external inflow landing straight as a
// new account (no sibling drop) onboards its full value; same-day deposits and
// market moves on siblings do not touch the step-up. Floored at 0 so a debut day
// dominated by sibling drops can never book negative onboarding.
func groupOnboardStep(grp []*accountData, day int64) float64 {
	var debutValue, siblingFundingDrop float64
	for _, a := range grp {
		newlyDebuts := a.firstDay() == day
		if newlyDebuts {
			if v, alive := a.valueAt(day); alive {
				debutValue += returns.ZeroedValue(v, day, a.closureDay())
			}
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
		if drop := prev - cur; drop > 0 {
			siblingFundingDrop += drop
		}
	}
	step := debutValue - siblingFundingDrop
	if step < 0 {
		step = 0
	}
	return step
}

// subsumesAtDebut reports whether a flow on `day` falls in a late constituent's
// pre-debut region, where synthetic onboarding books the debut value instead. It
// must NOT fire for an OnboardNone source: there is no onboarding to replace the
// subsumed flow, so the real deposit IS the capital event and has to be kept.
func subsumesAtDebut(a *accountData, day, winFrom, winTo int64) bool {
	debut := a.firstDay()
	return debut > winFrom && debut <= winTo && day <= debut
}

// subsumesAtClosure reports whether a flow drains into a constituent's closure
// (the explicit closure outflow accounts for it). Independent of OnboardScope —
// explicit closure fires regardless.
func subsumesAtClosure(a *accountData, day, winFrom, winTo int64) bool {
	cd := a.closureDay()
	return cd > winFrom && cd <= winTo && day > a.lastNonzeroDay() && day <= cd
}

// subsumesAt reports whether a constituent's flow on `day` lands in a region the
// aggregate value series does not reflect, so it is subsumed by the synthetic
// onboarding/closure amount rather than counted as a visible flow:
//
//   - PRE-DEBUT: the constituent debuts (joins the value spine) at d > winFrom and
//     the flow is dated on or before d. The aggregate value series is 0 for this
//     constituent until d, so a pre-debut deposit/journal-in caused no visible
//     ΔV; onboarding books the whole firstValue at d instead.
//   - CLOSURE-DRAIN: the constituent closes (value → 0) at cd ≤ winTo and the flow
//     is dated after the last snapshot that still carried a non-zero value, up to
//     cd. The carried value is flat across that gap (no visible ΔV), so a drain
//     there would double-count with the synthetic closure outflow at cd.
func subsumesAt(a *accountData, day, winFrom, winTo int64) bool {
	return subsumesAtDebut(a, day, winFrom, winTo) || subsumesAtClosure(a, day, winFrom, winTo)
}

// netOwnedTransfers greedily matches opposite-direction transfer legs (largest
// first) whose output-currency magnitudes agree within ε and whose days are
// within the netting window, dropping matched pairs as internal. It carries
// per-leg ownership/subsumed tags through unchanged: the deterministic
// (|amount|, day, id) tie-break means internal pairs annihilate the same way
// whether or not a leg is subsumed. Survivors keep their tags so the caller can
// drop subsumed pre-debut / closure-drain legs AFTER netting. Returns the
// survivors and the count of unmatched legs.
func netOwnedTransfers(cand []ownedFlow) (kept []ownedFlow, unmatched int) {
	var pos, neg []ownedFlow
	for _, f := range cand {
		if f.Amount >= 0 {
			pos = append(pos, f)
		} else {
			neg = append(neg, f)
		}
	}
	// Largest-magnitude first, with a fully deterministic tie-break on
	// (day, transaction id) so equal-magnitude legs match reproducibly (review #6).
	byMag := func(s []ownedFlow) {
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
	// snapshot cash tail opened the window years early (UBS 2021). The choice is
	// per constituent, so it is source-scoped even in the merged global entity:
	// only that source's constituents move their anchor; every other source keeps
	// firstDay(). Default policy (InceptionFullWindow) leaves anchorDay == firstDay.
	// Under Inception=first-real-snapshot a conduit account's sparse cash-only
	// pre-history must NOT drag the anchor early (UBS held cash months before the
	// first securities position); the unit's real inception is when a non-conduit
	// account first has a real snapshot. So for that policy the min-anchor is taken
	// over NON-conduit constituents only, using their first real snapshot day. If a
	// group is all conduit, fall back to including conduits so the window is never
	// empty. Every other constituent keeps firstDay() (default), so this is
	// source-scoped.
	// anchorDay is a constituent's inception anchor: firstDay() by default;
	// firstRealSnapshotDay() under Inception=first-real-snapshot. A first-real-
	// snapshot CONDUIT is skipped from the min so its sparse cash pre-history can't
	// anchor the unit early — the anchor prefers a non-conduit real snapshot.
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
		if cfgDay := cfg / 86400; cfgDay > entityInception {
			entityInception = cfgDay
			configured = true
		}
	}

	if p.FromEpoch > 0 {
		from = p.FromEpoch / 86400
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
	return from, to, flags
}

func entityIdentity(level string, assets []*accountData) (id, label string) {
	a := assets[0]
	switch level {
	case "accounts":
		return a.acct, a.label
	case "sources":
		return a.src, a.src
	case "portfolios":
		if a.portfolio == "" {
			return "", "(no portfolio)"
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

func regimeFlags(assets []*accountData) []string {
	var flags []string
	if allNavOnly(assets) {
		flags = append(flags, "nav_only", "nav_only_capital_call_risk")
	} else if anyNavOnly(assets) {
		flags = append(flags, "nav_only_capital_call_risk")
	}
	// Aggregate the per-asset conditions first, then emit in a fixed order, so
	// the flag list is deterministic regardless of asset-iteration order. These
	// flags are a display SET — order carries no meaning — and emitting them in
	// iteration order made a multi-asset entity's flag ordering depend on which
	// constituent happened to be visited first (non-deterministic for e.g.
	// cointracking).
	var crypto, journal, unknown, clamped, dropped bool
	for _, a := range assets {
		crypto = crypto || (a.policy.Regime == returns.RegimeCryptoPartial && a.cryptoExcluded)
		journal = journal || a.journalPresent
		unknown = unknown || !a.policy.Known
		clamped = clamped || a.hasClampedFlow
		dropped = dropped || a.droppedNonzero
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
	return dedupeStrings(flags)
}

func periodLabel(bs, be int64, period string) string {
	t := returns.DayToTimeUTC(be - 1) // a day inside the bucket
	switch period {
	case "monthly":
		return t.Format("2006-01")
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
