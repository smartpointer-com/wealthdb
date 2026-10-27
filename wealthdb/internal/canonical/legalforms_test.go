package canonical

import "testing"

// The forms a municipal issuer may share are legal forms: the bond
// reader's company-only forms are LegalForms less them.
func TestLegalFormsHoldTheSharedForms(t *testing.T) {
	for w := range SharedLegalForms {
		if !LegalForms[w] {
			t.Errorf("%s is shared but no legal form", w)
		}
	}
}
