package returns

import (
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

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
	// RegimeNavOnly: no usable external flows (manual — its wires are already
	// captured by the bank collectors). Returns from the NAV / value series;
	// MWR = n/a; TWR reliable only while capital is static after onboarding
	// (nav_only_capital_call_risk).
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
// RETURNS-NOTES.md).
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
// a recognised adapter; the Known=false shape is reserved for the ReturnsPolicyFor
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
// Exported as BankFlowPolicy's building block and for the policy-shape tests.
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

// DepositBankPolicy is the shared policy of the flow-complete deposit-bank
// collectors (checking / savings / money-market conduits). The ledger is the
// complete cash history, so the standard bank external set applies (such
// adapters emit only deposit/withdrawal/interest/fee; the set's unused
// transfer/journal kinds are harmless). AccountsGrainHidden: a deposit
// account is pure cash plumbing — money passes through it between other
// sources — so its rows are noise at every grain (a drained-then-refunded
// account chains a permanent −100%) and none are emitted; the balances and
// flows still enter every aggregate, where transfer legs against tracked
// sources cancel.
func DepositBankPolicy() ReturnsPolicy {
	p := DefaultReturnsPolicy(BankFlowPolicy())
	p.AccountsGrain = AccountsGrainHidden
	return p
}

// CardIssuerPolicy is the shared policy of the card-only collectors — a
// source whose every account is a revolving-credit or charge card.
//
// It changes no return figure and cannot: the engine drops `card` accounts at
// the loader (gold.returnsInvisibleKind), because a card's balance swings are
// purchases and payments, and running them through TWR/MWR would report
// shopping as performance. The policy exists so the source DECLARES that
// rather than falling through to the unknown-adapter default, which is
// reported as a data-quality warning. AccountsGrainHidden states the same
// thing a second way, and holds if a card-only source ever also emits a
// non-card account.
func CardIssuerPolicy() ReturnsPolicy {
	p := DefaultReturnsPolicy(BankFlowPolicy())
	p.AccountsGrain = AccountsGrainHidden
	return p
}

// PrivateMarketLedgerPolicy is the shared policy of private-market collectors
// whose cash-flow ledger is complete double-entry on the custody account: the
// deposit/withdrawal legs are real dated cash crossings of the source
// boundary, so they count as external capital, while
// buy/sell/contribution/distribution are the internal halves of those pairs
// and must NOT count — marking both legs external would cancel every event to
// a net-0 flow. No transfer-like set: deposit/withdrawal never participate in
// netting. CapitalCallRisk keeps the nav_only_capital_call_risk tag on any
// window that observes no flows (a positions-only silver), so value-growth
// returns stay flagged when the ledger is absent. ClosureLedgerExact: an
// exit's withdrawal legs are the realized proceeds, dated on the exit day
// itself (the adapter emits a zero snapshot there), so a full-portfolio
// closure books the real proceeds and the realized-vs-last-mark delta shows
// as return.
func PrivateMarketLedgerPolicy() ReturnsPolicy {
	p := DefaultReturnsPolicy(NewFlowPolicy(
		RegimeFlowComplete,
		[]canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal},
		nil,
	))
	p.CapitalCallRisk = true
	p.ClosureScope = ClosureLedgerExact
	return p
}

// defaultFlowPolicy is the ReturnsPolicyFor miss-fallback: the conservative bank
// set with Known=false so the caller surfaces unknown_adapter_policy. It differs
// from BankFlowPolicy ONLY by Known=false.
func defaultFlowPolicy() FlowPolicy {
	p := BankFlowPolicy()
	p.Known = false
	return p
}

// ParseRegime maps a regime's String() name back to the Regime — the
// vocabulary of the config-side `returns_policy_overrides` block.
func ParseRegime(s string) (Regime, error) {
	switch s {
	case "flow_complete":
		return RegimeFlowComplete, nil
	case "crypto_partial":
		return RegimeCryptoPartial, nil
	case "nav_only":
		return RegimeNavOnly, nil
	}
	return 0, fmt.Errorf("unknown flow regime %q (want flow_complete, crypto_partial, or nav_only)", s)
}

// FlowPolicyForRegime returns the named regime's canonical FlowPolicy — the
// exact kind sets the regime's reference sources register: flow_complete →
// the bank sets, crypto_partial → fiat deposit/withdrawal external with no
// netting set (cointracking's shape), nav_only → empty sets. A config-side
// regime override REPLACES a source's whole flow classification with this
// shape; swapping only the enum would leave the old kind sets attached — a
// hybrid no regime defines.
func FlowPolicyForRegime(r Regime) FlowPolicy {
	switch r {
	case RegimeCryptoPartial:
		return NewFlowPolicy(RegimeCryptoPartial,
			[]canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal}, nil)
	case RegimeNavOnly:
		return NewFlowPolicy(RegimeNavOnly, nil, nil)
	}
	return BankFlowPolicy()
}

// CapitalDirection returns the effect of an external flow kind on the entity's
// capital base: +1 = capital in, -1 = capital out, 0 = source-signed / not a
// pinned-direction kind.
//
// This map is documentary and is, by construction, identical to the canonical
// sign that value_outccy already carries on the cash/funding account where these
// legs are recorded: for every external kind,
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
