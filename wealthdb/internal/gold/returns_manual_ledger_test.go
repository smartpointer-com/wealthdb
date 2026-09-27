package gold

import (
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestAManualLedgerLegFundsTheMarkItArrivesWith runs the positions-only
// source's policy end to end: a ledger leg (transfer_in) on that source
// is a counted flow, a collected deposit on it still is not, and a mark
// that steps up by exactly the leg's amount on the same day is funded
// rather than earned — net flow equals the leg, the return is flat, and
// the source keeps its NAV-only honesty tags with MWR n/a.
func TestAManualLedgerLegFundsTheMarkItArrivesWith(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "man", "manual")

	a, m, b := dy(2024, time.January, 2), dy(2024, time.June, 3), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "man", "CLAIM", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1000}, {m, 1500}, {b, 1500}},
		[]txn{
			{m, canonical.TxKindTransferIn, 500}, // the ledger leg: funds the step-up
			{m, canonical.TxKindDeposit, 300},    // a collected kind: never counted here
		})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.December, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "man"); nf != 500 {
		t.Errorf("net_flow = %.2f, want 500 (the ledger leg alone; the deposit must not count)", nf)
	}
	s, ok := summaryFor(rows, "man")
	if !ok {
		t.Fatal("no summary row for the positions-only source")
	}
	if s.TWR == nil || math.Abs(*s.TWR) > 0.5 {
		t.Errorf("TWR = %v, want ~0: the mark rose by exactly what arrived", s.TWR)
	}
	if !qualityHas(s, "nav_only") || s.MWR != nil {
		t.Errorf("the source must stay nav_only with MWR n/a (q=%v MWR=%v)", s.Quality, s.MWR)
	}
}
