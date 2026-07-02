package returns

import "github.com/ptu/wealthdb/internal/canonical"

// Regime classifies how much of an entity's return is recoverable from flows.
type Regime int

const (
	// RegimeFlowComplete: real external flows exist (banks, pension, angellist);
	// TWR and MWR are both valid.
	RegimeFlowComplete Regime = iota
	// RegimeCryptoPartial: only fiat flows are usable; crypto transfer legs are
	// an unclassifiable mix (internal moves + airdrops/gifts) and are excluded
	// (cointracking). TWR from the value series + fiat flows; MWR partial.
	RegimeCryptoPartial
	// RegimeNavOnly: no usable external flows (manual = none; carta/equityzen =
	// synthetic double-entries on a 0-pinned sentinel). Returns from the NAV /
	// value series; MWR = n/a; TWR reliable only while capital is static after
	// onboarding (nav_only_capital_call_risk).
	RegimeNavOnly
)

func (r Regime) String() string {
	switch r {
	case RegimeFlowComplete:
		return "flow_complete"
	case RegimeCryptoPartial:
		return "crypto_partial"
	case RegimeNavOnly:
		return "nav_only"
	}
	return "unknown"
}

// FlowPolicy is the per-adapter rule for turning transactions into return flows.
// Verified against every transaction-bearing adapter's kindmap (see
// RETURNS-NOTES.md / proposal §2.1).
type FlowPolicy struct {
	Regime Regime
	// Known is false for an unrecognised adapter kind; the caller surfaces
	// unknown_adapter_policy and falls back to the conservative bank set.
	Known bool
	// external is the set of TxKinds that count as external owner capital for
	// this adapter. The adapter has already encoded distinctions like
	// fiat-vs-crypto into the kind (cointracking: fiat→deposit, crypto→
	// transfer_in), so a kind-level set suffices.
	external map[canonical.TxKind]bool
	// transferLike is the subset that participates in coarse-grain netting.
	transferLike map[canonical.TxKind]bool
}

// IsExternal reports whether a kind is an external owner-capital flow under this
// policy.
func (p FlowPolicy) IsExternal(k canonical.TxKind) bool { return p.external[k] }

// IsTransferLike reports whether a kind participates in coarse-grain netting.
func (p FlowPolicy) IsTransferLike(k canonical.TxKind) bool { return p.transferLike[k] }

func set(ks ...canonical.TxKind) map[canonical.TxKind]bool {
	m := make(map[canonical.TxKind]bool, len(ks))
	for _, k := range ks {
		m[k] = true
	}
	return m
}

// NewFlowPolicy constructs a FlowPolicy from the given regime and external /
// transfer-like kind sets. It is the exported constructor silver packages use to
// declare their co-located policy (the external/transferLike sets are unexported
// map fields, so a constructor is required to build byte-identical sets from
// outside the package). Known is set true — a registered policy is by definition
// a recognised adapter; the Known=false shape is reserved for the FlowPolicyFor
// miss-fallback (see defaultFlowPolicy).
func NewFlowPolicy(regime Regime, external, transferLike []canonical.TxKind) FlowPolicy {
	return FlowPolicy{
		Regime:       regime,
		Known:        true,
		external:     set(external...),
		transferLike: set(transferLike...),
	}
}

// BankExternal is the standard flow-complete external kind set. Including kinds
// an adapter never emits (e.g. fidelity emits only journal) is harmless — no
// such transactions exist — and keeps the policy robust to adapter evolution.
// Exported so the flow-complete bank/pension silver packages register the
// identical set without duplicating the literal.
func BankExternal() []canonical.TxKind {
	return []canonical.TxKind{
		canonical.TxKindDeposit, canonical.TxKindWithdrawal,
		canonical.TxKindTransferIn, canonical.TxKindTransferOut,
		canonical.TxKindJournal,
	}
}

// BankTransferLike is the standard flow-complete transfer-like (netting) subset.
func BankTransferLike() []canonical.TxKind {
	return []canonical.TxKind{
		canonical.TxKindTransferIn, canonical.TxKindTransferOut, canonical.TxKindJournal,
	}
}

// BankFlowPolicy is the flow policy shared by the flow-complete banks/pension
// (ubs, schwab, swissquote, fidelity, relevate, viac). relevate (P2) and viac
// (P3a) map pension contributions to `deposit`, not `contribution`, so the bank
// set covers them; fidelity moves capital only via `journal`.
func BankFlowPolicy() FlowPolicy {
	return NewFlowPolicy(RegimeFlowComplete, BankExternal(), BankTransferLike())
}

// defaultFlowPolicy is the FlowPolicyFor miss-fallback: the conservative bank
// set with Known=false so the caller surfaces unknown_adapter_policy. It differs
// from BankFlowPolicy ONLY by Known=false.
func defaultFlowPolicy() FlowPolicy {
	p := BankFlowPolicy()
	p.Known = false
	return p
}

// FlowPolicyFor returns the flow policy for a silver source's adapter kind
// (gold silver_sources.silver_kind). Each kind's policy is co-located in its
// silver package and registered via RegisterPolicy (from that package's init()).
// An unregistered kind defaults to the bank set with Known=false so the caller
// can flag it. The body no longer enumerates sources — it is a registry lookup.
func FlowPolicyFor(adapterKind string) FlowPolicy {
	if p, ok := lookupPolicy(adapterKind); ok {
		return p.Flow
	}
	return defaultFlowPolicy()
}

// CapitalDirection returns the effect of an external flow kind on the entity's
// capital base: +1 = capital in, -1 = capital out, 0 = source-signed / not a
// pinned-direction kind.
//
// This map is documentary and is, by construction, identical to the canonical
// sign that value_outccy already carries on the cash/funding account where these
// legs are recorded (proposal §3.B): for every external kind,
// Flow.Amount == CapitalDirection(kind)·|amount|, so Modified-Dietz F_i =
// +Flow.Amount and XIRR cf = -Flow.Amount hold without per-kind special-casing.
// It exists as a guard/assert against a hypothetical future fund-perspective
// adapter; it changes zero numbers today. policy_test.go pins it to
// canonical.ApplyCanonicalSign.
func CapitalDirection(k canonical.TxKind) int {
	switch k {
	case canonical.TxKindDeposit, canonical.TxKindTransferIn, canonical.TxKindDistribution:
		return +1
	case canonical.TxKindWithdrawal, canonical.TxKindTransferOut, canonical.TxKindContribution:
		return -1
	default:
		return 0
	}
}
