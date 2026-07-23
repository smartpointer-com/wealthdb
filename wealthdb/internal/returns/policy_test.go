package returns

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
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
	// The default is the conservative bank set (differs from BankFlowPolicy only
	// by Known=false).
	for _, k := range BankExternal() {
		if !u.IsExternal(k) {
			t.Errorf("default fallback: %s must be external", k)
		}
	}
	for _, k := range BankTransferLike() {
		if !u.IsTransferLike(k) {
			t.Errorf("default fallback: %s must be transfer-like", k)
		}
	}
	if u.Regime != RegimeFlowComplete {
		t.Errorf("default fallback regime %v, want flow_complete", u.Regime)
	}
}

// TestBankFlowPolicyShape pins the shared bank policy shape (used by the six
// flow-complete bank/pension silver packages) without depending on registration.
func TestBankFlowPolicyShape(t *testing.T) {
	bank := BankFlowPolicy()
	if !bank.Known {
		t.Error("BankFlowPolicy must be Known")
	}
	for _, k := range BankExternal() {
		if !bank.IsExternal(k) {
			t.Errorf("bank: %s must be external", k)
		}
	}
	if !bank.IsTransferLike(canonical.TxKindTransferIn) {
		t.Error("bank transfer_in must be transfer-like (netting candidate)")
	}
	if bank.IsTransferLike(canonical.TxKindDeposit) {
		t.Error("deposit must not be transfer-like (never netted)")
	}
}

// TestOnboardNoneAndAccountsGrainMeaninglessDefaults pins the cointracking-
// migration knobs: OnboardNone is a distinct enum value appended after the existing
// scopes (so their numeric values are preserved), and DefaultReturnsPolicy leaves
// OnboardScope==OnboardPerConstituent and AccountsGrainMeaningless==false — a strict
// no-op for every unmigrated source.
func TestOnboardNoneAndAccountsGrainMeaninglessDefaults(t *testing.T) {
	// Numeric values of the pre-existing scopes are unchanged by the append.
	if OnboardPerConstituent != 0 || OnboardPerEntityOnce != 1 {
		t.Errorf("existing OnboardScope values shifted: PerConstituent=%d PerEntityOnce=%d, want 0,1",
			OnboardPerConstituent, OnboardPerEntityOnce)
	}
	// OnboardNone is a distinct constant, appended last.
	if OnboardNone == OnboardPerConstituent || OnboardNone == OnboardPerEntityOnce {
		t.Errorf("OnboardNone must be distinct from the existing scopes (got %d)", OnboardNone)
	}
	if OnboardNone != 2 {
		t.Errorf("OnboardNone = %d, want 2 (appended after the existing values)", OnboardNone)
	}

	// DefaultReturnsPolicy leaves both new knobs at their no-op defaults.
	dp := DefaultReturnsPolicy(BankFlowPolicy())
	if dp.OnboardScope != OnboardPerConstituent {
		t.Errorf("default OnboardScope = %d, want OnboardPerConstituent", dp.OnboardScope)
	}
	if dp.AccountsGrainMeaningless {
		t.Error("default AccountsGrainMeaningless must be false")
	}
}

func TestRegimeString(t *testing.T) {
	cases := map[Regime]string{
		RegimeFlowComplete:  "flow_complete",
		RegimeCryptoPartial: "crypto_partial",
		RegimeNavOnly:       "nav_only",
		Regime(99):          "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("Regime(%d).String() = %q, want %q", r, got, want)
		}
	}
}

// TestCapitalDirectionMatchesCanonical pins the documentary direction map to the
// canonical sign that value_outccy already carries: for every
// fixed-direction external kind, CapitalDirection == sign(ApplyCanonicalSign).
func TestCapitalDirectionMatchesCanonical(t *testing.T) {
	kinds := []canonical.TxKind{
		canonical.TxKindDeposit, canonical.TxKindTransferIn, canonical.TxKindDistribution,
		canonical.TxKindWithdrawal, canonical.TxKindTransferOut, canonical.TxKindContribution,
	}
	one := canonical.NewDecimalFromInt(1)
	for _, k := range kinds {
		signed := canonical.ApplyCanonicalSign(k, &one)
		if signed == nil {
			t.Fatalf("%s: ApplyCanonicalSign returned nil", k)
		}
		if got, want := CapitalDirection(k), signed.Sign(); got != want {
			t.Errorf("%s: CapitalDirection=%d, canonical sign=%d", k, got, want)
		}
	}
}
