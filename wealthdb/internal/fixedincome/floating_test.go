package fixedincome

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A note resetting in three months has a duration of about a quarter of
// a year, and a DV01 near 100 × 0.25 × 0.0001.
func TestFloaterRisk(t *testing.T) {
	t.Parallel()
	settle, reset := ymd(2026, 1, 15), ymd(2026, 4, 15)
	r := FloaterRisk(settle, reset, 4, 4)
	near(t, "Macaulay", r.Macaulay, 0.25, 0.01)
	near(t, "Macaulay exact", r.Macaulay, 90.0/365, 1e-15)
	near(t, "modified", r.Modified, r.Macaulay/1.01, 1e-15)
	near(t, "DV01", r.DV01, 100*0.25*0.0001, 1e-4)
	near(t, "DV01 exact", r.DV01, 100*r.Modified*0.0001, 1e-15)
	near(t, "convexity", r.Convexity, r.Macaulay*(r.Macaulay+0.25)/(1.01*1.01), 1e-15)
	// A frequency of 0 is simple interest to the reset.
	s := FloaterRisk(settle, reset, 4, 0)
	near(t, "simple modified", s.Modified, s.Macaulay/(1+0.04*s.Macaulay), 1e-15)
	// A reset on or before settlement leaves no risk.
	for _, at := range []int{0, -10} {
		if r := FloaterRisk(settle, settle.AddDate(0, 0, at), 4, 4); r != (Risk{}) {
			t.Errorf("reset %d days away: risk %+v, want none", at, r)
		}
	}
}

// A note to its next reset is a bond that pays its current coupon and
// redeems at par then. At its own coupon rate it prices near par, and its
// risk is the one FloaterRisk states.
func TestFloaterNearPar(t *testing.T) {
	t.Parallel()
	settle, reset := ymd(2026, 2, 20), ymd(2026, 4, 15)
	coupon := 4.0
	note := Bond{Coupon: coupon, Frequency: 4, Maturity: reset, DayCount: canonical.DayCountAct360}
	_, clean := note.Price(settle, coupon)
	near(t, "clean price", clean, 100, 0.01)
	want, got := FloaterRisk(settle, reset, coupon, 4), note.Risk(settle, coupon)
	near(t, "Macaulay", got.Macaulay, want.Macaulay, 0.005)
	near(t, "DV01", got.DV01, want.DV01, 0.02*want.DV01)
}
