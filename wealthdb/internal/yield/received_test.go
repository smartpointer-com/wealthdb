package yield

import "testing"

// A holding's income of several types totals the same in any order of
// its types: the exact sum, not the order a map yields them in.
func TestReceivedTotalIsExact(t *testing.T) {
	t.Parallel()
	r := Received{TypeInterest: 1e16, TypeCoupons: 1, TypeDividends: -1e16}
	for i := 0; i < 200; i++ {
		if got := r.Total(); got != 1 {
			t.Fatalf("total %v, want 1", got)
		}
	}
}
