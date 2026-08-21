package gold

// Tests for the ClosureScope policy knob against the exit-day zero snapshot
// the adapters now emit (silver.ClosureMarkerBatch): a ledger-exact source's
// zero-tail flows are the real exit proceeds and are kept — the realized-vs-
// mark delta shows as return — while the default treats them as strays and
// subsumes them, leaving the zeroing value drop as the exit signal. Windows
// run to today (far-future end, clamped) so the closure machinery, which keys
// off the zero-carry tail's end, is engaged.

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

func assertTWR(t *testing.T, r ReturnRow, want float64, label string) {
	t.Helper()
	if r.TWR == nil {
		t.Fatalf("%s: TWR = n/a (quality %v), want %.4f", label, r.Quality, want)
	}
	if math.Abs(*r.TWR-want) > 1e-9 {
		t.Errorf("%s: TWR = %.6f, want %.4f", label, *r.TWR, want)
	}
}

// closureFixture seeds one custody account marked 2000 that fully exits on the
// zero day, with the given exit-day withdrawal (0 = flow-less).
func closureFixture(t *testing.T, db *sql.DB, ctx context.Context, src string, proceeds int64) {
	t.Helper()
	a, e := dy(2024, time.January, 2), dy(2024, time.June, 3)
	txns := []txn(nil)
	if proceeds != 0 {
		txns = []txn{{e, canonical.TxKindWithdrawal, -proceeds}}
	}
	seedAcct(t, db, ctx, src, "PORT", canonical.AccountKindCustody, nil,
		[]snap{{a, 2000}, {e, 0}}, txns)
}

// TestClosureLedgerExactRealizedDelta pins both directions of the realized-vs-
// mark delta at a coarse grain: the exit-day withdrawal is kept and no
// synthetic is booked, so an exit below the last mark is a loss and one above
// it a gain.
func TestClosureLedgerExactRealizedDelta(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "below", "carta")
	seedReturnsSource(t, db, ctx, "above", "carta")
	closureFixture(t, db, ctx, "below", 1800) // marked 2000, sold for 1800: −10%
	closureFixture(t, db, ctx, "above", 2500) // marked 2000, sold for 2500: +25%

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2027, time.January, 1)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "below"); nf != -1800 {
		t.Errorf("below net_flow = %.2f, want the real proceeds -1800 (kept, no synthetic)", nf)
	}
	if nf := netFlowOf(t, rows, "above"); nf != -2500 {
		t.Errorf("above net_flow = %.2f, want the real proceeds -2500 (kept, no synthetic)", nf)
	}
	b, _ := summaryFor(rows, "below")
	assertTWR(t, b, -0.10, "below-mark exit")
	ab, _ := summaryFor(rows, "above")
	assertTWR(t, ab, 0.25, "above-mark exit")
	if b.MWR == nil {
		t.Errorf("below-mark exit must compute a real MWR (quality %v)", b.Quality)
	}
	// A profitable full exit repays more than the committed capital — net
	// invested capital goes negative — but the fully-realized single-sign-change
	// shape has a unique IRR and must NOT be blanked as mwr_negative_net_capital.
	if ab.MWR == nil {
		t.Errorf("above-mark exit must compute a real MWR (quality %v)", ab.Quality)
	}
	if qualityHas(ab, "mwr_negative_net_capital") {
		t.Errorf("above-mark exit must not carry mwr_negative_net_capital: %v", ab.Quality)
	}
}

// TestClosureLedgerExactFlowlessReadsAsLoss pins the no-ledger shape: a
// positions-only silver's full exit has no proceeds to keep, so the zeroing
// value drop reads as a full loss — flagged by the flow-less capital-call tag,
// and identical to what the default scope yields for the same data.
func TestClosureLedgerExactFlowlessReadsAsLoss(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "crt", "carta")
	closureFixture(t, db, ctx, "crt", 0)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2027, time.January, 1)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "crt"); nf != 0 {
		t.Errorf("net_flow = %.2f, want 0 (nothing observed, nothing synthesized)", nf)
	}
	s, _ := summaryFor(rows, "crt")
	assertTWR(t, s, -1, "flow-less full exit")
	if !qualityHas(s, "nav_only_capital_call_risk") {
		t.Errorf("flow-less window must keep the capital-call tag: %v", s.Quality)
	}
}

// TestClosureDefaultSubsumesZeroTailDrains pins the default scope on the same
// fixture: the exit-day withdrawal lands in the zero-carry tail, is treated
// as a stray, and is subsumed — the value drop reads as a full loss and the
// proceeds vanish from the flow series. This contrast is exactly why the
// complete-ledger sources opt into ClosureLedgerExact.
func TestClosureDefaultSubsumesZeroTailDrains(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "sq", "swissquote")
	closureFixture(t, db, ctx, "sq", 1800)

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2027, time.January, 1)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "sq"); nf != 0 {
		t.Errorf("net_flow = %.2f, want 0 (zero-tail drain subsumed)", nf)
	}
	s, _ := summaryFor(rows, "sq")
	assertTWR(t, s, -1, "default zero-tail closure")
}
