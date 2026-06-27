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

// bankExternal is the standard flow-complete external set. Including kinds an
// adapter never emits (e.g. fidelity emits only journal) is harmless — no such
// transactions exist — and keeps the policy robust to adapter evolution.
var bankExternal = []canonical.TxKind{
	canonical.TxKindDeposit, canonical.TxKindWithdrawal,
	canonical.TxKindTransferIn, canonical.TxKindTransferOut,
	canonical.TxKindJournal,
}

var bankTransferLike = []canonical.TxKind{
	canonical.TxKindTransferIn, canonical.TxKindTransferOut, canonical.TxKindJournal,
}

// FlowPolicyFor returns the flow policy for a silver source's adapter kind
// (gold silver_sources.silver_kind). An unknown kind defaults to the bank set
// with Known=false so the caller can flag it.
func FlowPolicyFor(adapterKind string) FlowPolicy {
	switch adapterKind {
	case "ubs", "schwab", "swissquote", "fidelity", "relevate", "viac":
		// Flow-complete banks/pension. relevate (P2) and viac (P3a) map pension
		// contributions to `deposit`, not `contribution`, so the bank set covers
		// them. fidelity moves capital only via `journal`.
		return FlowPolicy{Regime: RegimeFlowComplete, Known: true,
			external: set(bankExternal...), transferLike: set(bankTransferLike...)}

	case "angellist":
		// Real funding-wallet ledger: deposit/withdrawal are genuine bank wires.
		// contribution/distribution are INTERNAL (funding wallet ↔ tracked
		// deals) and are deliberately NOT external.
		return FlowPolicy{Regime: RegimeFlowComplete, Known: true,
			external:     set(canonical.TxKindDeposit, canonical.TxKindWithdrawal),
			transferLike: set()}

	case "cointracking":
		// Crypto: only FIAT deposit/withdrawal are real external capital. Crypto
		// transfer_in/out are an unclassifiable mix (wallet-to-wallet internal +
		// airdrops/gifts which are return) with no discriminator surviving to
		// gold → excluded (flag crypto_unclassified_transfers).
		return FlowPolicy{Regime: RegimeCryptoPartial, Known: true,
			external:     set(canonical.TxKindDeposit, canonical.TxKindWithdrawal),
			transferLike: set()}

	case "carta", "equityzen", "manual":
		// NAV-only: manual emits no transactions; carta/equityzen emit synthetic
		// balanced double-entries on a 0-pinned sentinel. No usable flows.
		return FlowPolicy{Regime: RegimeNavOnly, Known: true,
			external: set(), transferLike: set()}

	default:
		return FlowPolicy{Regime: RegimeFlowComplete, Known: false,
			external: set(bankExternal...), transferLike: set(bankTransferLike...)}
	}
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
