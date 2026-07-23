package returns

import (
	"sync"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// ReturnsPolicy is the per-source superset container for the returns engine. It
// holds the FlowPolicy that classifies transactions into return flows PLUS the
// pluggable per-source knobs (see docs/RETURNS-NOTES.md, "Pluggable per-source
// policy").
//
// The engine consumes these knobs: OnboardScope (returns_compute.go,
// per-entity-once onboarding), Inception (entityWindow, first-real-snapshot
// anchor), ConduitKinds via IsConduit (returns.go), the ClassifyFlow/ExternalOnly
// hook path (attachFlows in returns.go), and AccountsGrainMeaningless
// (returns.go, suppressing the per-wallet accounts grain for crypto sources).
// The remaining knobs — NettingTol, SpineDensity, InKindJumpTol, the NavOnly
// mirror, and the OnboardAmount hook — are a declared forward contract for the
// per-source returns-policy migration: defined and defaulted here so a source
// can declare intent that takes effect once the corresponding engine site reads
// the knob. Every knob defaults to the engine's baseline behavior, so
// DefaultReturnsPolicy() is a strict no-op: a source without a registered
// policy stays byte-identical, and each consumed knob only changes numbers when
// a source opts into a non-default value.
//
// A source declares its ReturnsPolicy co-located in its silver package and
// registers it via RegisterPolicy from that package's init(). The engine
// resolves it by adapter kind exactly as it resolves adapters (silver.Get), and
// never enumerates sources itself.
type ReturnsPolicy struct {
	// Flow is the flow-classification policy (regime + external / transfer-like
	// kind sets); ReturnsPolicyFor(kind).Flow exposes exactly this member.
	Flow FlowPolicy

	// ---- consumed knobs ----

	// OnboardScope: whether synthetic onboarding fires per constituent account
	// (default), once per computed entity at inception, or never.
	OnboardScope OnboardScope
	// AccountsGrainMeaningless: per-wallet (accounts-grain) return rows are
	// economically meaningless for this source (coins sweep between wallets on
	// arrival, so a single wallet's return is noise); the portfolios/sources/global
	// grains stay valid because they aggregate coherent units. Default false leaves
	// every grain's TWR/MWR computed.
	AccountsGrainMeaningless bool
	// Inception: full-window (default) vs. anchored at the first real snapshot.
	Inception InceptionMode
	// ConduitKinds: account kinds that are plumbing (e.g. UBS cash), not a
	// return-bearing unit. Empty by default.
	ConduitKinds []canonical.AccountKind
	// ExternalOnly: count only boundary-crossing flows. It gates the ClassifyFlow
	// hook in the engine (attachFlows): a source that ships ClassifyFlow drops
	// flows the hook calls internal. A source that pre-tags external/internal in
	// silver (UBS demotes internal rows to a non-flow kind) leaves ClassifyFlow
	// nil, so this flag is then a silver-side contract and inert in the engine.
	// False by default (the flow set is governed by Flow.external / Flow.
	// transferLike).
	ExternalOnly bool

	// ---- forward-contract knobs; defined and defaulted, awaiting engine sites ----

	// NettingTol: transfer-netting window / epsilon. Zero value matches the
	// engine's module-level netting constants.
	NettingTol Tolerance
	// SpineDensity: daily vs. sparse-carry-forward value spine. Zero value =
	// the engine's baseline spine.
	SpineDensity SpineMode
	// NavOnly: capital-call-risk vehicles (suppress flow-based return, surface
	// NAV growth). The engine derives NAV-only from Flow.Regime ==
	// RegimeNavOnly; this knob mirrors that (set by DefaultReturnsPolicy) as
	// the forward contract for reading it directly, and defaults false.
	NavOnly bool
	// InKindJumpTol: suspected-in-kind honesty-flag tolerance. Zero = the
	// engine's baseline tolerance.
	InKindJumpTol canonical.Decimal

	// ---- escape hatches; optional, nil => default behavior ----

	// ClassifyFlow, if non-nil, overrides external-vs-internal flow
	// classification under ExternalOnly. nil => the FlowPolicy
	// kind-set rule (UBS pre-tags in silver instead, so it ships nil).
	ClassifyFlow func(FlowCtx) FlowClass
	// OnboardAmount, if non-nil, overrides the synthetic onboarding amount.
	// nil => the engine's default. Forward contract: no engine site reads it.
	OnboardAmount func(DebutCtx) canonical.Decimal
}

// OnboardScope selects the grain at which synthetic onboarding fires.
type OnboardScope int

const (
	// OnboardPerConstituent is the default: onboarding fires per
	// constituent account.
	OnboardPerConstituent OnboardScope = iota
	// OnboardPerEntityOnce fires onboarding once per computed entity at
	// inception. Consumed by the engine (groupOnboardStep) and set
	// live by UBS.
	OnboardPerEntityOnce
	// OnboardNone never injects synthetic onboarding for this source's
	// constituents — for crypto-sweep sources whose mid-window debuts are funded
	// by within-entity transfers already excluded from flows, so onboarding would
	// double-count. Appended last so OnboardPerConstituent(0) and
	// OnboardPerEntityOnce(1) keep their numeric values.
	OnboardNone
)

// InceptionMode selects the window anchor.
type InceptionMode int

const (
	// InceptionFullWindow is the default: the full report window.
	InceptionFullWindow InceptionMode = iota
	// InceptionFirstRealSnapshot anchors at the first real value snapshot.
	// Consumed by the engine (entityWindow) and set live by UBS.
	InceptionFirstRealSnapshot
)

// SpineMode selects value-spine density.
type SpineMode int

const (
	// SpineDefault is the engine's baseline spine density.
	SpineDefault SpineMode = iota
	// SpineDaily forces a daily spine.
	SpineDaily
	// SpineSparseCarryForward carries values forward over sparse snapshots.
	SpineSparseCarryForward
)

// Tolerance is a netting window / epsilon knob. Its zero value matches the
// engine's module-level netting constants (forward contract).
type Tolerance struct {
	Days int
	Eps  canonical.Decimal
}

// FlowCtx is the input to the optional ClassifyFlow hook. Shape is
// provisional; the hook is nil in every default policy.
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

// DebutCtx is the input to the optional OnboardAmount hook. Provisional; part
// of the OnboardAmount forward contract.
type DebutCtx struct {
	Day int64
}

// DefaultReturnsPolicy returns the baseline policy: the given FlowPolicy plus
// all other knobs at their zero/default values (no-op). Sources build their
// ReturnsPolicy from this and override only what they need.
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

// ReturnsPolicyFor returns the full ReturnsPolicy registered for a silver
// source's adapter kind (gold silver_sources.silver_kind), so the compute
// engine can read the forward knobs (OnboardScope, Inception, ConduitKinds,
// ExternalOnly, …) — not just its Flow member (the flow-classification policy).
// An unregistered kind returns DefaultReturnsPolicy(defaultFlowPolicy()) with ok
// false; the default knobs are the engine baseline, so a miss is a strict
// no-op at every knob site. Because kind is resolved per source and the engine
// carries each constituent's src, the policy is naturally source-scoped even
// inside the merged global entity — a UBS constituent keeps the UBS policy while
// every other source keeps its own default.
func ReturnsPolicyFor(adapterKind string) (ReturnsPolicy, bool) {
	if p, ok := lookupPolicy(adapterKind); ok {
		return p, true
	}
	return DefaultReturnsPolicy(defaultFlowPolicy()), false
}

// IsConduit reports whether an account of the given kind is plumbing under this
// policy (a member of ConduitKinds): it contributes to the value spine but emits
// no per-account synthetic onboarding. Empty ConduitKinds (the default) makes
// this always false, so no source treats any account as a conduit unless it opts
// in.
func (p ReturnsPolicy) IsConduit(kind canonical.AccountKind) bool {
	for _, k := range p.ConduitKinds {
		if k == kind {
			return true
		}
	}
	return false
}
