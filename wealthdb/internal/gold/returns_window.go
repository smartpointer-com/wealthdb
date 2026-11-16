package gold

import (
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

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
