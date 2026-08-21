package returns_test

// This external test package blank-imports the silver adapters so their
// init()-time returns.RegisterPolicy calls run, then pins the EXACT FlowPolicy
// each source resolves to (via returns.ReturnsPolicyFor(kind).Flow). It lives
// in package returns_test (not returns) to avoid an import
// cycle: silver/<kind> imports internal/returns, so only an external test
// package may pull the silver adapters in alongside the package under test.

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

	// Blank-import every silver adapter that registers a policy, mirroring
	// cmd/wealthdb/main.go, so registration runs before the assertions.
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/angellist"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/carta"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/chase"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/cointracking"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/equityzen"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/fidelity"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/firstcitizens"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/fred"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/manual"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/raiffeisen_at"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/relevate"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/schwab"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/swissquote"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/ubs"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/viac"
)

func TestRegisteredFlowPolicies(t *testing.T) {
	// The NAV-only source: manual emits no transactions at all, so nothing is
	// countable — nav_only regime, no external kinds.
	man, _ := returns.ReturnsPolicyFor("manual")
	if !man.Flow.Known {
		t.Error("manual: policy must be Known (registered)")
	}
	if man.Flow.Regime != returns.RegimeNavOnly {
		t.Errorf("manual: regime %v, want nav_only", man.Flow.Regime)
	}
	if man.Flow.IsExternal(canonical.TxKindDeposit) || man.Flow.IsExternal(canonical.TxKindContribution) {
		t.Error("manual: nav-only must have no external kinds")
	}

	// carta / equityzen: complete double-entry ledgers on the custody account.
	// The deposit/withdrawal boundary legs are external capital; the holding
	// legs (buy/sell/contribution/distribution) are the internal halves of
	// those pairs and must not count. CapitalCallRisk keeps the honesty tag on
	// flow-less windows.
	for _, k := range []string{"carta", "equityzen"} {
		rp, _ := returns.ReturnsPolicyFor(k)
		if !rp.Flow.Known {
			t.Errorf("%s: policy must be Known (registered)", k)
		}
		if rp.Flow.Regime != returns.RegimeFlowComplete {
			t.Errorf("%s: regime %v, want flow_complete", k, rp.Flow.Regime)
		}
		if !rp.Flow.IsExternal(canonical.TxKindDeposit) || !rp.Flow.IsExternal(canonical.TxKindWithdrawal) {
			t.Errorf("%s: deposit/withdrawal must be external", k)
		}
		if rp.Flow.IsExternal(canonical.TxKindBuy) || rp.Flow.IsExternal(canonical.TxKindSell) ||
			rp.Flow.IsExternal(canonical.TxKindContribution) || rp.Flow.IsExternal(canonical.TxKindDistribution) {
			t.Errorf("%s: holding legs are INTERNAL (counting both halves cancels every event)", k)
		}
		if rp.Flow.IsTransferLike(canonical.TxKindDeposit) || rp.Flow.IsTransferLike(canonical.TxKindWithdrawal) {
			t.Errorf("%s: deposit/withdrawal must never net", k)
		}
		if !rp.CapitalCallRisk {
			t.Errorf("%s: CapitalCallRisk must be set", k)
		}
		if rp.ClosureScope != returns.ClosureLedgerExact {
			t.Errorf("%s: ClosureScope must be ledger-exact (the exit ledger is authoritative)", k)
		}
	}

	// cointracking: crypto_partial, fiat deposit/withdrawal external, crypto
	// transfer legs excluded.
	ct, _ := returns.ReturnsPolicyFor("cointracking")
	if ct.Flow.Regime != returns.RegimeCryptoPartial {
		t.Errorf("cointracking regime %v, want crypto_partial", ct.Flow.Regime)
	}
	if !ct.Flow.IsExternal(canonical.TxKindDeposit) || !ct.Flow.IsExternal(canonical.TxKindWithdrawal) {
		t.Error("cointracking: fiat deposit/withdrawal must be external")
	}
	if ct.Flow.IsExternal(canonical.TxKindTransferIn) || ct.Flow.IsExternal(canonical.TxKindTransferOut) {
		t.Error("cointracking: crypto transfer legs must be excluded")
	}
	if ct.AccountsGrain != returns.AccountsGrainBlanked {
		t.Error("cointracking: AccountsGrain must be blanked (per-wallet rows are noise, but shown)")
	}

	// fidelity: bank set — journal is its only capital-movement kind.
	fid, _ := returns.ReturnsPolicyFor("fidelity")
	if !fid.Flow.IsExternal(canonical.TxKindJournal) {
		t.Error("fidelity: journal must be external (its only capital-movement kind)")
	}

	// angellist: deposit/withdrawal external; contribution/distribution internal.
	al, _ := returns.ReturnsPolicyFor("angellist")
	if !al.Flow.IsExternal(canonical.TxKindDeposit) || !al.Flow.IsExternal(canonical.TxKindWithdrawal) {
		t.Error("angellist: deposit/withdrawal must be external")
	}
	if al.Flow.IsExternal(canonical.TxKindContribution) || al.Flow.IsExternal(canonical.TxKindDistribution) {
		t.Error("angellist: contribution/distribution are INTERNAL (funding<->deals)")
	}
	if al.Flow.IsTransferLike(canonical.TxKindTransferIn) {
		t.Error("angellist: no transfer-like set")
	}

	// The six flow-complete banks/pension all resolve the shared bank policy.
	for _, k := range []string{"ubs", "schwab", "swissquote", "fidelity", "relevate", "viac"} {
		rp, _ := returns.ReturnsPolicyFor(k)
		if !rp.Flow.Known {
			t.Errorf("%s: policy must be Known (registered)", k)
		}
		if rp.Flow.Regime != returns.RegimeFlowComplete {
			t.Errorf("%s: regime %v, want flow_complete", k, rp.Flow.Regime)
		}
		for _, tk := range returns.BankExternal() {
			if !rp.Flow.IsExternal(tk) {
				t.Errorf("%s: %s must be external", k, tk)
			}
		}
		if !rp.Flow.IsTransferLike(canonical.TxKindTransferIn) {
			t.Errorf("%s: transfer_in must be transfer-like", k)
		}
		if rp.Flow.IsTransferLike(canonical.TxKindDeposit) {
			t.Errorf("%s: deposit must not be transfer-like", k)
		}
	}

	// The cash-conduit deposit banks: complete ledgers on the shared bank set,
	// but the accounts are pure plumbing — no rows of their own anywhere,
	// values and flows still aggregate.
	for _, k := range []string{"chase", "firstcitizens", "raiffeisen_at"} {
		rp, _ := returns.ReturnsPolicyFor(k)
		if !rp.Flow.Known {
			t.Errorf("%s: policy must be Known (registered)", k)
		}
		if rp.Flow.Regime != returns.RegimeFlowComplete {
			t.Errorf("%s: regime %v, want flow_complete", k, rp.Flow.Regime)
		}
		if !rp.Flow.IsExternal(canonical.TxKindDeposit) || !rp.Flow.IsExternal(canonical.TxKindWithdrawal) {
			t.Errorf("%s: deposit/withdrawal must be external", k)
		}
		if rp.AccountsGrain != returns.AccountsGrainHidden {
			t.Errorf("%s: AccountsGrain must be hidden (cash plumbing)", k)
		}
	}

	// fred is blank-imported but registers NO policy — it must fall to the
	// Known=false default.
	if rp, _ := returns.ReturnsPolicyFor("fred"); rp.Flow.Known {
		t.Error("fred: registers no policy, must fall to Known=false default")
	}
}
