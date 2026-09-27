package gold

import (
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestAnInKindExitPairsAcrossSources pins the equity-transfer ledger's
// treatment of a private-market exit paid in shares (docs/DESIGN.md
// §13.10): the vehicle's source books a transfer_out and the receiving
// brokerage a transfer_in, same day, same value. The vehicle realizes the
// gain from its last mark to that value, the brokerage receives it as
// capital, and global sees the pair as no flow at all.
func TestAnInKindExitPairsAcrossSources(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "al", "angellist")
	seedReturnsSource(t, db, ctx, "sw", "schwab")

	a, x, k := dy(2024, time.January, 2), dy(2024, time.June, 3), dy(2024, time.December, 30)
	// The vehicle's account holds the stake at cost 1000 beside 19000 of
	// other stakes, until its year-end statement leaves a residual 100; the
	// shares land at the broker on x worth 5000.
	seedAcct(t, db, ctx, "al", "LP", canonical.AccountKindCustody, nil,
		[]snap{{a, 20000}, {k, 19100}},
		[]txn{{x, canonical.TxKindTransferOut, -5000}})
	seedAcct(t, db, ctx, "sw", "BRK", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 2000}, {x, 7000}, {k, 7000}},
		[]txn{{x, canonical.TxKindTransferIn, 5000}})
	end := eod(2024, time.December, 30)

	acct, err := RunReturns(ctx, db, params("accounts", 0, end))
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	lp, ok := summaryFor(acct, "LP")
	if !ok {
		t.Fatal("no LP row")
	}
	if nf := netFlowOf(t, acct, "LP"); nf != -5000 {
		t.Errorf("vehicle net_flow = %.2f, want -5000 (the shares left)", nf)
	}
	// 5000 out and 100 left of a stake carried at 1000: a gain of 4100 on
	// the account's 20000, less the capital that left mid-year.
	if v, ok := parseFloatPtr(lp.EndValue); !ok || v != 19100 {
		t.Errorf("vehicle end = %v, want 19100", lp.EndValue)
	}
	if lp.TWR == nil || *lp.TWR < 0.20 || *lp.TWR > 0.25 {
		t.Errorf("vehicle TWR = %v (quality %v), want between +20%% and +25%%", lp.TWR, lp.Quality)
	}
	if nf := netFlowOf(t, acct, "BRK"); nf != 5000 {
		t.Errorf("broker net_flow = %.2f, want +5000 (capital received)", nf)
	}
	brk, _ := summaryFor(acct, "BRK")
	if brk.TWR == nil || *brk.TWR != 0 {
		t.Errorf("broker TWR = %v, want 0 (the shares arrived at their value)", brk.TWR)
	}

	glob, err := RunReturns(ctx, db, params("global", 0, end))
	if err != nil {
		t.Fatalf("RunReturns global: %v", err)
	}
	g, ok := summaryFor(glob, "")
	if !ok {
		t.Fatal("no global row")
	}
	if nf := netFlowOf(t, glob, ""); nf != 0 {
		t.Errorf("global net_flow = %.2f, want 0 (the pair nets)", nf)
	}
	if v, ok := parseFloatPtr(g.StartValue); !ok || v != 22000 {
		t.Errorf("global start = %v, want 22000", g.StartValue)
	}
	if v, ok := parseFloatPtr(g.EndValue); !ok || v != 26100 {
		t.Errorf("global end = %v, want 26100", g.EndValue)
	}
	for _, q := range g.Quality {
		if strings.HasPrefix(q, "unmatched_transfers") {
			t.Errorf("global quality %v: the ledger pair must net, not count as unmatched", g.Quality)
		}
	}
}
