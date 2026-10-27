package canonical

import "testing"

// The forms a municipal issuer may share, and the forms that are also a
// given name, are legal forms: each reader subtracts them from LegalForms.
func TestLegalFormsHoldTheSharedForms(t *testing.T) {
	for w := range SharedLegalForms {
		if !LegalForms[w] {
			t.Errorf("%s is shared but no legal form", w)
		}
	}
	for w := range NameLikeLegalForms {
		if !LegalForms[w] {
			t.Errorf("%s is name-like but no legal form", w)
		}
	}
}
