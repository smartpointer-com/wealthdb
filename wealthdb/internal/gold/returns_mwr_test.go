package gold

import (
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// TestMwrNetCapitalNonPositive checks the whole-window predicate: opening base
// plus TOTAL net flow <= 0. An interim contribution dip that recovers to a
// net-positive window is NOT flagged (the account value was never truly negative).
func TestMwrNetCapitalNonPositive(t *testing.T) {
	f := func(d int64, a float64) returns.Flow { return returns.Flow{Day: d, Amount: a} }
	cases := []struct {
		name  string
		v0    float64
		flows []returns.Flow
		want  bool
	}{
		{"net positive", 100, []returns.Flow{f(1, 50), f(2, -120), f(3, 30)}, false},
		{"net-withdrew below base", 100, []returns.Flow{f(1, -150)}, true},
		{"interim dip but net positive", 100, []returns.Flow{f(1, -120), f(2, 200)}, false},
		{"zero base funded by deposits is fine", 0, []returns.Flow{f(1, 50)}, false},
		{"net exactly wipes the base", 100, []returns.Flow{f(1, -100)}, true},
	}
	for _, c := range cases {
		if got := mwrNetCapitalNonPositive(c.v0, c.flows); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestRunReturnsMwrNegativeNetCapital drives it end to end: a withdrawal that
// exceeds the invested capital drives running capital negative, so MWR is n/a +
// mwr_negative_net_capital while TWR is still reported.
func TestRunReturnsMwrNegativeNetCapital(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, tW, t1 := dy(2023, time.January, 2), dy(2023, time.June, 1), dy(2023, time.December, 29)
	// Base 1000; withdraw 1500 (more than the invested capital) -> capital < 0.
	seedAcct(t, db, ctx, "ubs", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {tW, 1000}, {t1, 800}},
		[]txn{{tW, canonical.TxKindWithdrawal, -1500}})

	rows, err := RunReturns(ctx, db, params("accounts", 0, eod(2023, time.December, 29)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	r, ok := summaryFor(rows, "A1")
	if !ok {
		t.Fatal("no A1 row")
	}
	if r.MWR != nil {
		t.Errorf("MWR should be n/a, got %v", r.MWR)
	}
	if !qualityHas(r, "mwr_negative_net_capital") {
		t.Errorf("want mwr_negative_net_capital, got quality %v", r.Quality)
	}
	if r.TWR == nil {
		t.Error("TWR should still be reported when MWR is n/a")
	}
}
