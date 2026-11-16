package gold

import (
	"context"
	"database/sql"
	"math"
	"strings"
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
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "man", "manual")

	a, m, b := dy(2024, time.January, 2), dy(2024, time.June, 3), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "man", "CLAIM", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1000}, {m, 1500}, {b, 1500}},
		[]txn{
			{m, canonical.TxKindTransferIn, 500}, // the ledger leg: funds the step-up
			{m, canonical.TxKindDeposit, 300},    // a collected kind: never counted here
		})
	ledgerLeg(t, db, ctx, "man", "CLAIM-tx0", "claim")

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

// ledgerLeg gives a seeded transaction the ledger's id, as the
// equity-transfer ledger states its legs.
func ledgerLeg(t *testing.T, db *sql.DB, ctx context.Context, src, seeded, id string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `UPDATE transactions SET transaction_external_id = ?
		WHERE silver_source_id = ? AND transaction_external_id = ?`,
		canonical.LedgerTransferIDPrefix+id, src, seeded); err != nil {
		t.Fatal(err)
	}
}

// TestALedgerLegNetsAgainstItsPartner: a manual ledger leg is a nettable
// kind like any other, so where a grain holds both halves of a ledger move
// the netter pairs them and leaves no unmatched leg.
func TestALedgerLegNetsAgainstItsPartner(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "man", "manual")
	seedReturnsSource(t, db, ctx, "brk", "schwab")
	a, m, b := dy(2024, time.January, 2), dy(2024, time.June, 3), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "man", "CLAIM", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1500}, {m, 1000}, {b, 1000}}, []txn{{m, canonical.TxKindTransferOut, -500}})
	seedAcct(t, db, ctx, "brk", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 2000}, {m, 2500}, {b, 2500}}, []txn{{m, canonical.TxKindTransferIn, 500}})
	ledgerLeg(t, db, ctx, "man", "CLAIM-tx0", "out")
	ledgerLeg(t, db, ctx, "brk", "B1-tx0", "in")

	rows, err := RunReturns(ctx, db, params("global", 0, eod(2024, time.December, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	g, ok := summaryFor(rows, "")
	if !ok {
		t.Fatal("no global row")
	}
	for _, q := range g.Quality {
		if strings.HasPrefix(q, "unmatched_transfers") {
			t.Errorf("the ledger pair must net at global: %v", g.Quality)
		}
	}
	if nf := netFlowOf(t, rows, ""); nf != 0 {
		t.Errorf("global net_flow = %.2f, want 0", nf)
	}
}

// TestALedgerLegCountsOnAPrivateMarketSource: carta's own ledger emits no
// transfer, but a leg the equity-transfer ledger states on its account is
// owner capital, as on every flow-complete source.
func TestALedgerLegCountsOnAPrivateMarketSource(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ct", "carta")
	a, m, b := dy(2024, time.January, 2), dy(2024, time.June, 3), dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "ct", "C1", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1000}, {m, 1500}, {b, 1500}}, []txn{{m, canonical.TxKindTransferIn, 500}})
	ledgerLeg(t, db, ctx, "ct", "C1-tx0", "in")

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.December, 30)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if nf := netFlowOf(t, rows, "ct"); nf != 500 {
		t.Errorf("net_flow = %.2f, want 500 (the ledger leg)", nf)
	}
}
