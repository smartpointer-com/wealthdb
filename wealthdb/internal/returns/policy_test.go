package returns

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestReturnsPolicyForUnknownDefault pins the ReturnsPolicyFor miss-fallback. The
// per-kind assertions (ubs/fidelity/cointracking/angellist/manual/…) live in
// policy_registered_test.go (package returns_test), which blank-imports the
// silver adapters so their init()-time RegisterPolicy calls run — here in bare
// package returns no source is registered, so every kind would (correctly) fall
// to the Known=false default.
func TestReturnsPolicyForUnknownDefault(t *testing.T) {
	p, _ := ReturnsPolicyFor("totally-new-source")
	u := p.Flow
	if u.Known {
		t.Error("unknown adapter must report Known=false")
	}
	// The default is the flow-complete regime (it differs from
	// BankFlowPolicy only by Known=false): every capital kind counts.
	for _, k := range canonical.TxKinds() {
		capital, nettable := canonical.CapitalKind(k)
		if u.IsExternal(k, false) != capital || u.IsTransferLike(k) != nettable {
			t.Errorf("default fallback: %s external=%v transfer-like=%v, want %v %v",
				k, u.IsExternal(k, false), u.IsTransferLike(k), capital, nettable)
		}
	}
	if u.Regime != RegimeFlowComplete {
		t.Errorf("default fallback regime %v, want flow_complete", u.Regime)
	}
}

// TestPolicyKnobDefaults pins the opt-in policy knobs: DefaultReturnsPolicy
// leaves OnboardScope == OnboardPerConstituent, AccountsGrain ==
// AccountsGrainNormal, CapitalCallRisk == false and ClosureScope ==
// ClosureSubsumeDrains — a strict no-op for every source that doesn't set
// them.
func TestPolicyKnobDefaults(t *testing.T) {
	dp := DefaultReturnsPolicy(BankFlowPolicy())
	if dp.OnboardScope != OnboardPerConstituent {
		t.Errorf("default OnboardScope = %d, want OnboardPerConstituent", dp.OnboardScope)
	}
	if dp.AccountsGrain != AccountsGrainNormal {
		t.Errorf("default AccountsGrain = %d, want AccountsGrainNormal", dp.AccountsGrain)
	}
	if dp.CapitalCallRisk {
		t.Error("default CapitalCallRisk must be false")
	}
	if dp.ClosureScope != ClosureSubsumeDrains {
		t.Errorf("default ClosureScope = %d, want ClosureSubsumeDrains", dp.ClosureScope)
	}
}

func TestRegimeString(t *testing.T) {
	cases := map[Regime]string{
		RegimeFlowComplete: "flow_complete",
		RegimeNavOnly:      "nav_only",
		Regime(99):         "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("Regime(%d).String() = %q, want %q", r, got, want)
		}
	}
}

// TestExternalKindSignsMatchCanonical pins the Flow.Amount sign convention
// to the canonical sign that value_outccy already carries: capital-in kinds
// are positive and capital-out kinds negative under ApplyCanonicalSign.
func TestExternalKindSignsMatchCanonical(t *testing.T) {
	want := map[canonical.TxKind]int{
		canonical.TxKindDeposit: +1, canonical.TxKindTransferIn: +1, canonical.TxKindDistribution: +1,
		canonical.TxKindWithdrawal: -1, canonical.TxKindTransferOut: -1, canonical.TxKindContribution: -1,
	}
	one := canonical.NewDecimalFromInt(1)
	for k, dir := range want {
		signed := canonical.ApplyCanonicalSign(k, &one)
		if signed == nil {
			t.Fatalf("%s: ApplyCanonicalSign returned nil", k)
		}
		if got := signed.Sign(); got != dir {
			t.Errorf("%s: canonical sign=%d, want %d", k, got, dir)
		}
	}
}

// TestCompletenessCells pins what each regime counts: every capital kind
// when complete, and only the ledger's legs when none. No regime counts a kind
// outside the table, and nettability is the table's alone.
func TestCompletenessCells(t *testing.T) {
	for _, k := range canonical.TxKinds() {
		capital, nettable := canonical.CapitalKind(k)
		for _, ledger := range []bool{false, true} {
			cells := map[Regime]bool{
				RegimeFlowComplete: capital,
				RegimeNavOnly:      capital && ledger,
			}
			for r, want := range cells {
				if got := NewFlowPolicy(r).IsExternal(k, ledger); got != want {
					t.Errorf("%s %s ledger=%v: external %v, want %v", r, k, ledger, got, want)
				}
				if got := NewFlowPolicy(r).IsTransferLike(k); got != nettable {
					t.Errorf("%s %s: transfer-like %v, want %v", r, k, got, nettable)
				}
			}
		}
	}
	for _, k := range []canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal} {
		if c, n := canonical.CapitalKind(k); !c || n {
			t.Errorf("%s: capital=%v nettable=%v, want capital and never netted", k, c, n)
		}
	}
}
