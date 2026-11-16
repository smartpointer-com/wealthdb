package gold

import (
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// TestPerEntityOnceOnboardingOrderStable holds the order of the onboarding
// flows of two per-entity-once sources in one entity. Both debut on the same
// day, so the day sort leaves them in the order they were booked, and every
// float sum over the series follows it. The groups are booked by source id,
// the same way on every run.
func TestPerEntityOnceOnboardingOrderStable(t *testing.T) {
	t.Parallel()
	acct := func(src string, v float64) *accountData {
		return &accountData{src: src, acct: "A", kind: "brokerage",
			rpolicy:  returns.ReturnsPolicy{OnboardScope: returns.OnboardPerEntityOnce},
			series:   []dayVal{{day: 50, val: v}, {day: 100, val: v}},
			snapDays: []int64{50, 100}}
	}
	// Members in (src, acct) order, as groupAccounts sorts them.
	assets := []*accountData{acct("s1", 100), acct("s2", 200), acct("s3", 300)}
	p := ReturnParams{Level: "global", Netting: true}
	want := []returns.Flow{
		{Day: 50, Amount: 100, Synthetic: true},
		{Day: 50, Amount: 200, Synthetic: true},
		{Day: 50, Amount: 300, Synthetic: true},
	}
	for range 50 {
		flows, _, _ := entityFlows(assets, p, 0, 100, true)
		if !slices.Equal(flows, want) {
			t.Fatalf("onboarding flows = %v, want %v", flows, want)
		}
	}
}
