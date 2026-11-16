package returns_test

// This external test package blank-imports silver/all so silver.Register
// forwards each kind's policy (returns.RegisterPolicy), then pins the EXACT
// FlowPolicy each source resolves to (via returns.ReturnsPolicyFor(kind).Flow).
// It lives in package returns_test (not returns) to avoid an import cycle:
// silver imports internal/returns, so only an external test package may pull
// the silver adapters in alongside the package under test.

import (
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"

	// Every adapter, as cmd/wealthdb/main.go imports them, so registration
	// runs before the assertions.
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/all"
)

func TestRegisteredFlowPolicies(t *testing.T) {
	// The NAV-only source: no COLLECTED row counts — nav_only regime, deposits
	// and the private-market kinds stay outside. The ledger's legs are the
	// one exception: value that moved in from another tracked vehicle is
	// booked through the equity-transfer ledger as a transfer_in /
	// transfer_out on this source, and only those count — otherwise a claim
	// arriving from an escrow reads as performance. They net against a
	// partner in a grain that holds both, as every ledger leg does.
	man, _ := returns.ReturnsPolicyFor("manual")
	if !man.Flow.Known {
		t.Error("manual: policy must be Known (registered)")
	}
	if man.Flow.Regime != returns.RegimeNavOnly {
		t.Errorf("manual: regime %v, want nav_only", man.Flow.Regime)
	}
	if man.Flow.IsExternal(canonical.TxKindDeposit, false) || man.Flow.IsExternal(canonical.TxKindContribution, false) ||
		man.Flow.IsExternal(canonical.TxKindTransferIn, false) {
		t.Error("manual: a collected deposit, contribution or transfer must not count")
	}
	if !man.Flow.IsExternal(canonical.TxKindTransferIn, true) || !man.Flow.IsExternal(canonical.TxKindTransferOut, true) {
		t.Error("manual: the ledger's transfer_in / transfer_out legs must count")
	}
	if !man.Flow.IsTransferLike(canonical.TxKindTransferIn) {
		t.Error("manual: a ledger leg nets where a grain holds its partner")
	}
	if man.ClosureScope != returns.ClosureLedgerExact {
		t.Error("manual: a hand-dated release leg on the day a claim zeroes is a real exit, not a drain to subsume")
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
		if !rp.Flow.IsExternal(canonical.TxKindDeposit, false) || !rp.Flow.IsExternal(canonical.TxKindWithdrawal, false) {
			t.Errorf("%s: deposit/withdrawal must be external", k)
		}
		if rp.Flow.IsExternal(canonical.TxKindBuy, false) || rp.Flow.IsExternal(canonical.TxKindSell, false) ||
			rp.Flow.IsExternal(canonical.TxKindContribution, false) || rp.Flow.IsExternal(canonical.TxKindDistribution, false) {
			t.Errorf("%s: holding legs are INTERNAL (counting both halves cancels every event)", k)
		}
		if rp.Flow.IsTransferLike(canonical.TxKindDeposit) || rp.Flow.IsTransferLike(canonical.TxKindWithdrawal) {
			t.Errorf("%s: deposit/withdrawal must never net", k)
		}
		if !rp.Flow.IsExternal(canonical.TxKindTransferIn, true) || !rp.Flow.IsExternal(canonical.TxKindTransferOut, true) {
			t.Errorf("%s: a ledger transfer leg counts, as on every flow-complete source", k)
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
	if !ct.Flow.IsExternal(canonical.TxKindDeposit, false) || !ct.Flow.IsExternal(canonical.TxKindWithdrawal, false) {
		t.Error("cointracking: fiat deposit/withdrawal must be external")
	}
	if ct.Flow.IsExternal(canonical.TxKindTransferIn, false) || ct.Flow.IsExternal(canonical.TxKindTransferOut, false) {
		t.Error("cointracking: crypto transfer legs must be excluded")
	}
	if ct.AccountsGrain != returns.AccountsGrainBlanked {
		t.Error("cointracking: AccountsGrain must be blanked (per-wallet rows are noise, but shown)")
	}

	// fidelity: bank set — a transfer moves capital in a direction, a
	// journal in none, and both net at the coarse grains.
	fid, _ := returns.ReturnsPolicyFor("fidelity")
	for _, k := range []canonical.TxKind{canonical.TxKindTransferIn, canonical.TxKindTransferOut, canonical.TxKindJournal} {
		if !fid.Flow.IsExternal(k, false) || !fid.Flow.IsTransferLike(k) {
			t.Errorf("fidelity: %s must be external and transfer-like", k)
		}
	}

	// angellist: deposit/withdrawal external; contribution/distribution internal.
	al, _ := returns.ReturnsPolicyFor("angellist")
	if !al.Flow.IsExternal(canonical.TxKindDeposit, false) || !al.Flow.IsExternal(canonical.TxKindWithdrawal, false) {
		t.Error("angellist: deposit/withdrawal must be external")
	}
	if al.Flow.IsExternal(canonical.TxKindContribution, false) || al.Flow.IsExternal(canonical.TxKindDistribution, false) {
		t.Error("angellist: contribution/distribution are INTERNAL (funding<->deals)")
	}
	// The equity-transfer ledger's kinds — an exit paid in shares — are
	// external and transfer-like, so the vehicle's out-leg counts at its own
	// grain and nets against the receiving source's in-leg at global.
	if !al.Flow.IsExternal(canonical.TxKindTransferOut, false) || !al.Flow.IsExternal(canonical.TxKindTransferIn, false) {
		t.Error("angellist: the ledger's transfer kinds must be external")
	}
	if !al.Flow.IsTransferLike(canonical.TxKindTransferOut) || !al.Flow.IsTransferLike(canonical.TxKindTransferIn) {
		t.Error("angellist: the ledger's transfer kinds must be transfer-like")
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
		for _, tk := range capitalKinds() {
			if !rp.Flow.IsExternal(tk, false) {
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

	// The cash-conduit deposit banks: complete ledgers on the shared bank set.
	// Their accounts are cash, which the engine hides on any source, so the
	// policy needs no grain mode of its own.
	for _, k := range []string{"chase", "firstcitizens", "raiffeisen_at"} {
		rp, _ := returns.ReturnsPolicyFor(k)
		if !rp.Flow.Known {
			t.Errorf("%s: policy must be Known (registered)", k)
		}
		if rp.Flow.Regime != returns.RegimeFlowComplete {
			t.Errorf("%s: regime %v, want flow_complete", k, rp.Flow.Regime)
		}
		if !rp.Flow.IsExternal(canonical.TxKindDeposit, false) || !rp.Flow.IsExternal(canonical.TxKindWithdrawal, false) {
			t.Errorf("%s: deposit/withdrawal must be external", k)
		}
		if rp.AccountsGrain != returns.AccountsGrainNormal {
			t.Errorf("%s: AccountsGrain %v, want normal (the engine hides cash)", k, rp.AccountsGrain)
		}
	}

	// amex: the card-only source. No knob here can move a figure — the engine
	// drops `card` accounts at the loader — so what the policy declares is
	// what there is to pin: Known, so the kind never reports
	// unknown_adapter_policy; the shared bank set, on which none of the card
	// kinds count as owner capital.
	amx, _ := returns.ReturnsPolicyFor("amex")
	if !amx.Flow.Known {
		t.Error("amex: policy must be Known (registered)")
	}
	if amx.Flow.Regime != returns.RegimeFlowComplete {
		t.Errorf("amex: regime %v, want flow_complete", amx.Flow.Regime)
	}
	for _, tk := range capitalKinds() {
		if !amx.Flow.IsExternal(tk, false) {
			t.Errorf("amex: %s must be external", tk)
		}
	}
	for _, tk := range []canonical.TxKind{canonical.TxKindPurchase,
		canonical.TxKindRefund, canonical.TxKindCardPayment} {
		if amx.Flow.IsExternal(tk, false) {
			t.Errorf("amex: %s must not count as owner capital", tk)
		}
	}
	if amx.AccountsGrain != returns.AccountsGrainNormal {
		t.Errorf("amex: AccountsGrain %v, want normal (the engine drops cards)", amx.AccountsGrain)
	}

	// synthetic: the generic kind cannot know which institution a source
	// models, so it registers the bank-style default — flow_complete on the
	// shared bank sets, every other knob untouched — and a deployment states
	// each source's own regime with returns_policy_overrides.
	syn, _ := returns.ReturnsPolicyFor("synthetic")
	if !syn.Flow.Known {
		t.Error("synthetic: policy must be Known (registered)")
	}
	if syn.Flow.Regime != returns.RegimeFlowComplete {
		t.Errorf("synthetic: regime %v, want flow_complete", syn.Flow.Regime)
	}
	for _, tk := range capitalKinds() {
		if !syn.Flow.IsExternal(tk, false) {
			t.Errorf("synthetic: %s must be external", tk)
		}
	}
	for _, tk := range nettableKinds() {
		if !syn.Flow.IsTransferLike(tk) {
			t.Errorf("synthetic: %s must be transfer-like", tk)
		}
	}
	if syn.Flow.IsTransferLike(canonical.TxKindDeposit) || syn.Flow.IsTransferLike(canonical.TxKindWithdrawal) {
		t.Error("synthetic: deposit/withdrawal must never net")
	}
	if !reflect.DeepEqual(syn, returns.DefaultReturnsPolicy(returns.BankFlowPolicy())) {
		t.Error("synthetic: every knob but the flow policy must keep its default")
	}

	// plaid: one Item may be a bank, a broker or a card issuer, so it
	// registers the same bank-style default as synthetic; the engine treats
	// each account by its kind, and returns_policy_overrides states an Item's
	// own regime.
	pl, _ := returns.ReturnsPolicyFor("plaid")
	if !pl.Flow.Known {
		t.Error("plaid: policy must be Known (registered)")
	}
	if !reflect.DeepEqual(pl, returns.DefaultReturnsPolicy(returns.BankFlowPolicy())) {
		t.Error("plaid: the bank-style default, every other knob untouched")
	}

	// The reference sources are blank-imported but register NO policy —
	// they must fall to the Known=false default.
	for _, k := range []string{"curves", "fred", "refdata"} {
		if rp, _ := returns.ReturnsPolicyFor(k); rp.Flow.Known {
			t.Errorf("%s: registers no policy, must fall to Known=false default", k)
		}
	}
}

// capitalKinds and nettableKinds are the kinds the one table marks capital
// and nettable (canonical.CapitalKind).
func capitalKinds() []canonical.TxKind {
	var out []canonical.TxKind
	for _, k := range canonical.TxKinds() {
		if c, _ := canonical.CapitalKind(k); c {
			out = append(out, k)
		}
	}
	return out
}

func nettableKinds() []canonical.TxKind {
	var out []canonical.TxKind
	for _, k := range canonical.TxKinds() {
		if _, n := canonical.CapitalKind(k); n {
			out = append(out, k)
		}
	}
	return out
}
