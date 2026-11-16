package gold

import (
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

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

func allNavOnly(assets []*accountData) bool {
	for _, a := range assets {
		if a.rpolicy.Flow.Regime != returns.RegimeNavOnly {
			return false
		}
	}
	return true
}

func anyNavOnly(assets []*accountData) bool {
	for _, a := range assets {
		if a.rpolicy.Flow.Regime == returns.RegimeNavOnly {
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
	var journal, unknown, clamped, dropped, capRiskFlowless bool
	for _, a := range assets {
		journal = journal || a.journalPresent
		unknown = unknown || !a.rpolicy.Flow.Known
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
