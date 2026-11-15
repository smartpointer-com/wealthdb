package canonical

import (
	"slices"
	"testing"
)

// The dividend calendar's enum columns admit the kinds and statuses a
// distribution may carry, and no other word.
func TestDividendEventVocabulary(t *testing.T) {
	t.Parallel()
	enums := RefTableOf("dividend_calendar").Enums
	kinds := make([]string, len(DividendEventKinds))
	for i, k := range DividendEventKinds {
		kinds[i] = string(k)
	}
	if !slices.Equal(enums["kind"], kinds) {
		t.Errorf("kind enum = %q, want %q", enums["kind"], kinds)
	}
	if want := []string{string(DividendFinal), string(DividendEstimated)}; !slices.Equal(enums["status"], want) {
		t.Errorf("status enum = %q, want %q", enums["status"], want)
	}
}
