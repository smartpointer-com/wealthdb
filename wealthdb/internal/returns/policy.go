package returns

import (
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Regime is a source's flow completeness: which of its collected rows of
// the capital kinds (canonical.CapitalKind) are owner capital. A leg the
// equity-transfer ledger asserts is hand-stated, not collected, and counts
// under every regime but crypto's.
type Regime int

const (
	// RegimeFlowComplete: the ledger is complete; every capital kind counts
	// (banks, pension, the private-market ledgers). TWR and MWR are both
	// valid.
	RegimeFlowComplete Regime = iota
	// RegimeCryptoPartial: fiat only. Deposits and withdrawals count; crypto
	// transfer legs are an unclassifiable mix (internal moves and
	// airdrops/gifts) and are excluded (cointracking). TWR from the value
	// series and the fiat flows; MWR partial.
	RegimeCryptoPartial
	// RegimeNavOnly: none. No collected row counts (manual — its wires are
	// already captured by the bank collectors); only the ledger's legs do,
	// for value that moved in from another tracked vehicle rather than from
	// a bank. Returns from the NAV / value series; MWR = n/a on the source
	// alone; TWR reliable only while capital is static after onboarding
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

// FlowPolicy is the per-adapter rule for turning transactions into return
// flows: the source's completeness. Which kinds move capital, and which of
// them net, is one table for every source (canonical.CapitalKind).
type FlowPolicy struct {
	Regime Regime
	// Known is false for an unrecognised adapter kind; the caller surfaces
	// unknown_adapter_policy and falls back to the flow-complete regime.
	Known bool
}

// IsExternal reports whether a row of kind k is an external owner-capital
// flow under this policy. ledger marks a leg the equity-transfer ledger
// asserts.
func (p FlowPolicy) IsExternal(k canonical.TxKind, ledger bool) bool {
	capital, nettable := canonical.CapitalKind(k)
	switch {
	case !capital:
		return false
	case p.Regime == RegimeCryptoPartial:
		return !nettable
	case p.Regime == RegimeNavOnly:
		return ledger
	}
	return true
}

// IsTransferLike reports whether a kind participates in coarse-grain netting.
func (p FlowPolicy) IsTransferLike(k canonical.TxKind) bool {
	_, nettable := canonical.CapitalKind(k)
	return nettable
}

// NewFlowPolicy is the registered flow policy of a source of the given
// completeness. Known is set true — a registered policy is by definition a
// recognised adapter; the Known=false shape is reserved for the
// ReturnsPolicyFor miss-fallback (see defaultFlowPolicy). A config-side regime
// override replaces a source's policy with this one too.
func NewFlowPolicy(regime Regime) FlowPolicy {
	return FlowPolicy{Regime: regime, Known: true}
}

// BankFlowPolicy is the flow-complete policy the banks, the pensions and the
// deposit and card collectors register.
func BankFlowPolicy() FlowPolicy { return NewFlowPolicy(RegimeFlowComplete) }

// PrivateMarketLedgerPolicy is the shared policy of private-market collectors
// whose cash-flow ledger is complete double-entry on the custody account: the
// deposit/withdrawal legs are real dated cash crossings of the source
// boundary, so they count as external capital, while
// buy/sell/contribution/distribution are the internal halves of those pairs
// and are no capital kind. CapitalCallRisk keeps the
// nav_only_capital_call_risk tag on any window that observes no flows (a positions-only silver), so value-growth
// returns stay flagged when the ledger is absent. ClosureLedgerExact: an
// exit's withdrawal legs are the realized proceeds, dated on the exit day
// itself (the adapter emits a zero snapshot there), so a full-portfolio
// closure books the real proceeds and the realized-vs-last-mark delta shows
// as return.
func PrivateMarketLedgerPolicy() ReturnsPolicy {
	p := DefaultReturnsPolicy(BankFlowPolicy())
	p.CapitalCallRisk = true
	p.ClosureScope = ClosureLedgerExact
	return p
}

// defaultFlowPolicy is the ReturnsPolicyFor miss-fallback: the flow-complete
// regime with Known=false so the caller surfaces unknown_adapter_policy. It
// differs from BankFlowPolicy ONLY by Known=false.
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
