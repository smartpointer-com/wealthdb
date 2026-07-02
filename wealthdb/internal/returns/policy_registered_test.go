package returns_test

// This external test package blank-imports the silver adapters so their
// init()-time returns.RegisterPolicy calls run, then pins that each source
// resolves the EXACT FlowPolicy it had under the old central FlowPolicyFor
// switch. It lives in package returns_test (not returns) to avoid an import
// cycle: silver/<kind> imports internal/returns, so only an external test
// package may pull the silver adapters in alongside the package under test.

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/returns"

	// Blank-import every silver adapter that registers a policy, mirroring
	// cmd/wealthdb/main.go, so registration runs before the assertions.
	_ "github.com/ptu/wealthdb/internal/silver/angellist"
	_ "github.com/ptu/wealthdb/internal/silver/carta"
	_ "github.com/ptu/wealthdb/internal/silver/cointracking"
	_ "github.com/ptu/wealthdb/internal/silver/equityzen"
	_ "github.com/ptu/wealthdb/internal/silver/fidelity"
	_ "github.com/ptu/wealthdb/internal/silver/fred"
	_ "github.com/ptu/wealthdb/internal/silver/manual"
	_ "github.com/ptu/wealthdb/internal/silver/relevate"
	_ "github.com/ptu/wealthdb/internal/silver/schwab"
	_ "github.com/ptu/wealthdb/internal/silver/swissquote"
	_ "github.com/ptu/wealthdb/internal/silver/ubs"
	_ "github.com/ptu/wealthdb/internal/silver/viac"
)

func TestRegisteredFlowPolicies(t *testing.T) {
	// NAV-only sources: nav_only regime, no external kinds.
	for _, k := range []string{"manual", "carta", "equityzen"} {
		p := returns.FlowPolicyFor(k)
		if !p.Known {
			t.Errorf("%s: policy must be Known (registered)", k)
		}
		if p.Regime != returns.RegimeNavOnly {
			t.Errorf("%s: regime %v, want nav_only", k, p.Regime)
		}
		if p.IsExternal(canonical.TxKindDeposit) || p.IsExternal(canonical.TxKindContribution) {
			t.Errorf("%s: nav-only must have no external kinds", k)
		}
	}

	// cointracking: crypto_partial, fiat deposit/withdrawal external, crypto
	// transfer legs excluded.
	ct := returns.FlowPolicyFor("cointracking")
	if ct.Regime != returns.RegimeCryptoPartial {
		t.Errorf("cointracking regime %v, want crypto_partial", ct.Regime)
	}
	if !ct.IsExternal(canonical.TxKindDeposit) || !ct.IsExternal(canonical.TxKindWithdrawal) {
		t.Error("cointracking: fiat deposit/withdrawal must be external")
	}
	if ct.IsExternal(canonical.TxKindTransferIn) || ct.IsExternal(canonical.TxKindTransferOut) {
		t.Error("cointracking: crypto transfer legs must be excluded")
	}

	// fidelity: bank set — journal is its only capital-movement kind.
	fid := returns.FlowPolicyFor("fidelity")
	if !fid.IsExternal(canonical.TxKindJournal) {
		t.Error("fidelity: journal must be external (its only capital-movement kind)")
	}

	// angellist: deposit/withdrawal external; contribution/distribution internal.
	al := returns.FlowPolicyFor("angellist")
	if !al.IsExternal(canonical.TxKindDeposit) || !al.IsExternal(canonical.TxKindWithdrawal) {
		t.Error("angellist: deposit/withdrawal must be external")
	}
	if al.IsExternal(canonical.TxKindContribution) || al.IsExternal(canonical.TxKindDistribution) {
		t.Error("angellist: contribution/distribution are INTERNAL (funding<->deals)")
	}
	if al.IsTransferLike(canonical.TxKindTransferIn) {
		t.Error("angellist: no transfer-like set")
	}

	// The six flow-complete banks/pension all resolve the shared bank policy.
	for _, k := range []string{"ubs", "schwab", "swissquote", "fidelity", "relevate", "viac"} {
		p := returns.FlowPolicyFor(k)
		if !p.Known {
			t.Errorf("%s: policy must be Known (registered)", k)
		}
		if p.Regime != returns.RegimeFlowComplete {
			t.Errorf("%s: regime %v, want flow_complete", k, p.Regime)
		}
		for _, tk := range returns.BankExternal() {
			if !p.IsExternal(tk) {
				t.Errorf("%s: %s must be external", k, tk)
			}
		}
		if !p.IsTransferLike(canonical.TxKindTransferIn) {
			t.Errorf("%s: transfer_in must be transfer-like", k)
		}
		if p.IsTransferLike(canonical.TxKindDeposit) {
			t.Errorf("%s: deposit must not be transfer-like", k)
		}
	}

	// fred is blank-imported but registers NO policy — it must fall to the
	// Known=false default, exactly as under the old switch.
	if fp := returns.FlowPolicyFor("fred"); fp.Known {
		t.Error("fred: registers no policy, must fall to Known=false default")
	}
}
