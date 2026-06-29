package gold

import (
	"fmt"
	"math"
	"sort"

	"github.com/ptu/wealthdb/internal/returns"
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
	// bucket, and empty-bucket detection.
	snaps := unionSnapshotDays(assets)

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
			prevEmpty = empty
			out = append(out, row)
		}
	}

	// Since-inception summary row, cumulative TWR pinned to the canonical bucket.
	out = append(out, summaryRow(base, winFrom, winTo, av, flows, snaps, assets, p, entityQ))
	return out
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

	// Cumulative TWR at the canonical headline bucket (daily-if-dense-else-monthly),
	// independent of --period.
	canon := returns.CanonicalHeadlineBucket(snaps)
	var buckets []returns.Bucket
	degenerate := false
	for _, b := range returns.BucketBoundaries(winFrom, winTo, canon) {
		bv0, _ := av(b[0])
		bv1, _ := av(b[1])
		rr, ok := returns.ModifiedDietz(bv0, bv1, b[0], b[1], flowsIn(flows, b[0], b[1]))
		buckets = append(buckets, returns.Bucket{R: rr, OK: ok})
		if !ok {
			degenerate = true
		}
	}
	if p.Method == "twr" || p.Method == "both" {
		if cum, ok := returns.Chain(buckets); ok {
			r.TWR = f64(cum)
			days := float64(winTo - winFrom)
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
	for _, a := range assets {
		for _, f := range flowsIn(a.nonTransfer, winFrom, winTo) {
			if !subsumesAt(a, f.Day, winFrom, winTo) {
				flows = append(flows, f)
			}
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
	for _, a := range assets {
		debut := a.firstDay()
		if debut > winFrom && debut <= winTo {
			v, _ := a.valueAt(debut)
			if of, ok := returns.OnboardingFlow(debut, v, 0); ok {
				flows = append(flows, of)
			}
		}
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
	if debut := a.firstDay(); debut > winFrom && debut <= winTo && day <= debut {
		return true
	}
	if cd := a.closureDay(); cd > winFrom && cd <= winTo {
		if day > a.lastNonzeroDay() && day <= cd {
			return true
		}
	}
	return false
}

// netTransfers greedily matches opposite-direction transfer legs (largest first)
// whose output-currency magnitudes agree within ε and whose days are within the
// netting window, dropping matched pairs as internal. Returns the survivors and
// the count of unmatched legs. Thin wrapper over netOwnedTransfers for the
// ownership-free case (used directly only in tests).
func netTransfers(cand []returns.Flow) (kept []returns.Flow, unmatched int) {
	owned := make([]ownedFlow, len(cand))
	for i, f := range cand {
		owned[i] = ownedFlow{Flow: f}
	}
	keptOwned, unmatched := netOwnedTransfers(owned)
	for _, of := range keptOwned {
		kept = append(kept, of.Flow)
	}
	return kept, unmatched
}

// netOwnedTransfers is netTransfers carrying per-leg ownership/subsumed tags
// through unchanged: the greedy largest-first match with the deterministic
// (|amount|, day, id) tie-break is identical, so internal pairs annihilate the
// same way whether or not a leg is subsumed. Survivors keep their tags so the
// caller can drop subsumed pre-debut / closure-drain legs AFTER netting.
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
	// Per-constituent inception.
	incMin, incMax := int64(math.MaxInt64), int64(0)
	for _, a := range assets {
		fd := a.firstDay()
		if fd < incMin {
			incMin = fd
		}
		if fd > incMax {
			incMax = fd
		}
	}
	aggregate := len(assets) > 1
	entityInception := incMin
	if p.Inception == "strict" && aggregate {
		entityInception = incMax
	}

	if p.FromEpoch > 0 {
		from = p.FromEpoch / 86400
		if from < entityInception {
			from = entityInception
			flags = append(flags, "partial_window")
		}
	} else {
		from = entityInception
		flags = append(flags, "since_data_inception")
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
	for _, a := range assets {
		if a.policy.Regime == returns.RegimeCryptoPartial && a.cryptoExcluded {
			flags = append(flags, "crypto_unclassified_transfers")
		}
		if a.journalPresent {
			flags = append(flags, "journal_present")
		}
		if !a.policy.Known {
			flags = append(flags, "unknown_adapter_policy")
		}
		if a.hasClampedFlow {
			flags = append(flags, "fx_clamped_flow")
		}
		if a.droppedNonzero {
			flags = append(flags, "dropped_while_nonzero")
		}
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
