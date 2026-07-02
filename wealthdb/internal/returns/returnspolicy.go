package returns

import (
	"sync"

	"github.com/ptu/wealthdb/internal/canonical"
)

// ReturnsPolicy is the per-source superset container for the returns engine. It
// holds the FlowPolicy that classifies transactions into return flows (the ONLY
// part the engine reads today) PLUS the forward knobs from the pluggable-policy
// design (docs/RETURNS-NOTES.md). The forward knobs are DEFINED
// but NOT YET consumed by the engine — they land with the per-source migrations
// (proposal §5) — and every one defaults to reproduce CURRENT behavior, so a
// DefaultReturnsPolicy() is a strict no-op.
//
// A source declares its ReturnsPolicy co-located in its silver package and
// registers it via RegisterPolicy from that package's init(). The engine
// resolves it by adapter kind exactly as it resolves adapters (silver.Get), and
// never enumerates sources itself.
type ReturnsPolicy struct {
	// Flow is the flow-classification policy (regime + external / transfer-like
	// kind sets). This is the only field the engine reads today; FlowPolicyFor
	// returns exactly this member.
	Flow FlowPolicy

	// ---- forward knobs (proposal §2); defined, defaulted, not yet consumed ----

	// OnboardScope: whether synthetic onboarding fires per constituent account
	// (today's behavior) or once per computed entity at inception.
	OnboardScope OnboardScope
	// Inception: full-window (today) vs. anchored at the first real snapshot.
	Inception InceptionMode
	// ConduitKinds: account kinds that are plumbing (e.g. UBS cash), not a
	// return-bearing unit. Empty today.
	ConduitKinds []canonical.AccountKind
	// ExternalOnly: count only boundary-crossing flows. False today (the flow
	// set is governed by Flow.external / Flow.transferLike as now).
	ExternalOnly bool
	// NettingTol: transfer-netting window / epsilon. Zero value = today's
	// netting behavior.
	NettingTol Tolerance
	// SpineDensity: daily vs. sparse-carry-forward value spine. Zero value =
	// today's behavior.
	SpineDensity SpineMode
	// NavOnly: capital-call-risk vehicles (suppress flow-based return, surface
	// NAV growth). The engine currently derives this from Flow.Regime ==
	// RegimeNavOnly; this knob mirrors that for the migration and defaults false.
	NavOnly bool
	// InKindJumpTol: suspected-in-kind honesty-flag tolerance. Zero = today.
	InKindJumpTol canonical.Decimal

	// ---- escape hatches (proposal §2); optional, nil => default behavior ----

	// ClassifyFlow, if non-nil, overrides external-vs-internal flow
	// classification (proposal §4). nil today => the FlowPolicy kind-set rule.
	ClassifyFlow func(FlowCtx) FlowClass
	// OnboardAmount, if non-nil, overrides the synthetic onboarding amount. nil
	// today => the engine's default.
	OnboardAmount func(DebutCtx) canonical.Decimal
}

// OnboardScope selects the grain at which synthetic onboarding fires.
type OnboardScope int

const (
	// OnboardPerConstituent is today's behavior: onboarding fires per
	// constituent account.
	OnboardPerConstituent OnboardScope = iota
	// OnboardPerEntityOnce fires onboarding once per computed entity at
	// inception (proposal §3). Not yet consumed.
	OnboardPerEntityOnce
)

// InceptionMode selects the window anchor.
type InceptionMode int

const (
	// InceptionFullWindow is today's behavior: the full report window.
	InceptionFullWindow InceptionMode = iota
	// InceptionFirstRealSnapshot anchors at the first real value snapshot. Not
	// yet consumed.
	InceptionFirstRealSnapshot
)

// SpineMode selects value-spine density.
type SpineMode int

const (
	// SpineDefault is today's behavior. Not yet consumed.
	SpineDefault SpineMode = iota
	// SpineDaily forces a daily spine.
	SpineDaily
	// SpineSparseCarryForward carries values forward over sparse snapshots.
	SpineSparseCarryForward
)

// Tolerance is a netting window / epsilon knob. Its zero value reproduces
// today's netting behavior. Not yet consumed.
type Tolerance struct {
	Days int
	Eps  canonical.Decimal
}

// FlowCtx is the input to the optional ClassifyFlow hook (proposal §4). Shape is
// provisional; the hook is nil in every default policy today, so nothing reads
// it yet.
type FlowCtx struct {
	Kind   canonical.TxKind
	Amount canonical.Decimal
}

// FlowClass is the result of ClassifyFlow: external (boundary-crossing) vs.
// internal.
type FlowClass int

const (
	// FlowInternal is a within-entity move (not owner capital).
	FlowInternal FlowClass = iota
	// FlowExternal is boundary-crossing owner capital.
	FlowExternal
)

// DebutCtx is the input to the optional OnboardAmount hook. Provisional; nil in
// every default policy today.
type DebutCtx struct {
	Day int64
}

// DefaultReturnsPolicy returns the policy that reproduces CURRENT engine
// behavior for a recognised-but-unmigrated source: the given FlowPolicy plus all
// forward knobs at their zero/default values (no-op). Sources build their
// ReturnsPolicy from this and override only what they need (nothing does yet).
func DefaultReturnsPolicy(flow FlowPolicy) ReturnsPolicy {
	return ReturnsPolicy{
		Flow:    flow,
		NavOnly: flow.Regime == RegimeNavOnly,
	}
}

// ---- kind-keyed policy registry (mirrors internal/silver's adapter registry) ----

var (
	policyMu       sync.RWMutex
	policyRegistry = map[string]ReturnsPolicy{}
)

// RegisterPolicy records a source's ReturnsPolicy under its adapter kind. It is
// called from each silver package's init() (beside silver.Register), so the
// per-source policy lives co-located with that source's domain knowledge. Like
// the adapter registry, a duplicate kind is a build-time bug and panics.
//
// internal/returns must NOT import internal/silver/*; the dependency runs the
// other way (silver/<kind> imports returns and calls this), which is
// cycle-free because returns imports only internal/canonical.
func RegisterPolicy(kind string, p ReturnsPolicy) {
	policyMu.Lock()
	defer policyMu.Unlock()
	if _, exists := policyRegistry[kind]; exists {
		panic("returns: policy for kind " + kind + " already registered")
	}
	policyRegistry[kind] = p
}

// lookupPolicy returns the registered ReturnsPolicy for a kind, if any.
func lookupPolicy(kind string) (ReturnsPolicy, bool) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	p, ok := policyRegistry[kind]
	return p, ok
}
