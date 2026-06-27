package returns

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
)

func TestFlowPolicyRegimes(t *testing.T) {
	navOnly := []string{"manual", "carta", "equityzen"}
	for _, k := range navOnly {
		p := FlowPolicyFor(k)
		if p.Regime != RegimeNavOnly {
			t.Errorf("%s: regime %v, want nav_only", k, p.Regime)
		}
		if p.IsExternal(canonical.TxKindDeposit) || p.IsExternal(canonical.TxKindContribution) {
			t.Errorf("%s: nav-only must have no external kinds", k)
		}
	}

	ct := FlowPolicyFor("cointracking")
	if ct.Regime != RegimeCryptoPartial {
		t.Errorf("cointracking regime %v, want crypto_partial", ct.Regime)
	}
	if !ct.IsExternal(canonical.TxKindDeposit) || !ct.IsExternal(canonical.TxKindWithdrawal) {
		t.Error("cointracking: fiat deposit/withdrawal must be external")
	}
	if ct.IsExternal(canonical.TxKindTransferIn) || ct.IsExternal(canonical.TxKindTransferOut) {
		t.Error("cointracking: crypto transfer legs must be excluded")
	}

	fid := FlowPolicyFor("fidelity")
	if !fid.IsExternal(canonical.TxKindJournal) {
		t.Error("fidelity: journal must be external (its only capital-movement kind)")
	}

	al := FlowPolicyFor("angellist")
	if !al.IsExternal(canonical.TxKindDeposit) || !al.IsExternal(canonical.TxKindWithdrawal) {
		t.Error("angellist: deposit/withdrawal must be external")
	}
	if al.IsExternal(canonical.TxKindContribution) || al.IsExternal(canonical.TxKindDistribution) {
		t.Error("angellist: contribution/distribution are INTERNAL (funding↔deals)")
	}

	ubs := FlowPolicyFor("ubs")
	for _, k := range []canonical.TxKind{
		canonical.TxKindDeposit, canonical.TxKindWithdrawal,
		canonical.TxKindTransferIn, canonical.TxKindTransferOut, canonical.TxKindJournal,
	} {
		if !ubs.IsExternal(k) {
			t.Errorf("ubs: %s must be external", k)
		}
	}

	if u := FlowPolicyFor("totally-new-source"); u.Known {
		t.Error("unknown adapter must report Known=false")
	}
}

// TestCapitalDirectionMatchesCanonical pins the documentary direction map to the
// canonical sign that value_outccy already carries (proposal §3.B): for every
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
