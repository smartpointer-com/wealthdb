package canonical

import "testing"

// A distribution's kind and status are of their words.
func TestDividendEventVocabulary(t *testing.T) {
	t.Parallel()
	for _, k := range DividendEventKinds {
		if !k.Valid() {
			t.Errorf("%s is not valid", k)
		}
	}
	if DividendEventKind("interest").Valid() || DividendEventKind("").Valid() {
		t.Error("a kind outside the vocabulary is valid")
	}
	if !DividendFinal.Valid() || !DividendEstimated.Valid() || DividendEventStatus("paid").Valid() {
		t.Error("status vocabulary")
	}
}
