package all

import (
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// TestEveryKindRegisters pins what the registry holds once every
// adapter is imported: the kinds, the sign pins and the label traits.
func TestEveryKindRegisters(t *testing.T) {
	want := []string{"amex", "angellist", "carta", "chase", "cointracking", "curves", "equityzen",
		"fidelity", "firstcitizens", "fred", "manual", "plaid", "raiffeisen_at", "refdata", "relevate",
		"schwab", "swissquote", "synthetic", "ubs", "viac"}
	if got := silver.Kinds(); !slices.Equal(got, want) {
		t.Errorf("Kinds = %v, want %v", got, want)
	}
	pins := []canonical.SignedKindPin{{SilverKind: "fidelity", Kind: canonical.TxKindCorporateAction},
		{SilverKind: "schwab", Kind: canonical.TxKindCorporateAction}}
	if got := canonical.CashSignedKinds(); !slices.Equal(got, pins) {
		t.Errorf("CashSignedKinds = %v, want %v", got, pins)
	}
	traits := map[string]silver.LabelTraits{
		"angellist":     {TypedAccountIDs: true},
		"chase":         {NicknameIsLabel: true},
		"cointracking":  {TypedAccountLabels: true},
		"firstcitizens": {NicknameIsLabel: true},
		"manual":        {TypedAccountLabels: true, TypedHoldings: true},
		"raiffeisen_at": {NicknameIsLabel: true},
	}
	for _, k := range want {
		if got := silver.TraitsOf(k); got != traits[k] {
			t.Errorf("TraitsOf(%s) = %+v, want %+v", k, got, traits[k])
		}
	}
}
