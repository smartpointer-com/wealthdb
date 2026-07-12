package gold

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

func eod(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 23, 59, 59, 0, time.UTC).Unix()
}

// TestRunReturnsDisappearingAccountReconciles guards review #1: an account that
// vanishes from a later same-source snapshot must read 0 thereafter (matching the
// macros), so the aggregate reconciles with GlobalAsOf and the dropped account is
// flagged rather than carried forward at its last value.
func TestRunReturnsDisappearingAccountReconciles(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, t1 := dy(2024, time.January, 2), dy(2024, time.July, 2)
	// A present at both snapshots; B present only at t0 (the t1 snapshot omits it).
	seedAcct(t, db, ctx, "ubs", "A", canonical.AccountKindBrokerage, nil, []snap{{t0, 1000}, {t1, 1100}}, nil)
	seedAcct(t, db, ctx, "ubs", "B", canonical.AccountKindBrokerage, nil, []snap{{t0, 1700}}, nil)

	end := eod(2024, time.July, 2)
	g, err := RunReturns(ctx, db, params("global", 0, end))
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if len(g) != 1 {
		t.Fatalf("global rows = %d, want 1", len(g))
	}
	// Reconcile the engine's terminal value against the macro-based GlobalAsOf.
	ga, err := GlobalAsOf(ctx, db, end, "USD")
	if err != nil {
		t.Fatalf("GlobalAsOf: %v", err)
	}
	got, gok := parseFloatPtr(g[0].EndValue)
	want, wok := parseFloatPtr(ga.TotalValueOutCcy)
	if !gok || !wok || math.Abs(got-want) > 1e-6 {
		t.Errorf("global == Σ accounts broken: returns end=%v, GlobalAsOf total=%v (B should be 0, both 1100)",
			g[0].EndValue, ga.TotalValueOutCcy)
	}

	// B (accounts grain) dropped out while still holding value ⇒ flagged.
	rows, err := RunReturns(ctx, db, params("accounts", 0, end))
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	b, ok := summaryFor(rows, "B")
	if !ok || !qualityHas(b, "dropped_while_nonzero") {
		t.Errorf("B should carry dropped_while_nonzero; got %+v", b)
	}
}

// TestRunReturnsClosureDedup guards review #2: a real "withdraw everything"
// closure must not be double-counted by the synthetic closure outflow.
func TestRunReturnsClosureDedup(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, t1 := dy(2024, time.January, 2), dy(2024, time.July, 2)
	seedAcct(t, db, ctx, "ubs", "A", canonical.AccountKindBrokerage, nil, []snap{{t0, 1000}, {t1, 1000}}, nil)
	// C closes: 1000 → 0 driven by a real withdrawal of the full balance.
	seedAcct(t, db, ctx, "ubs", "C", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, 0}}, []txn{{t1, canonical.TxKindWithdrawal, -1000}})

	g, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.July, 2)))
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	s, ok := summaryFor(g, "ubs")
	if !ok || s.TWR == nil {
		t.Fatalf("no ubs source TWR: %+v", s)
	}
	// The real -1000 withdrawal is counted once: capital left, ~0 return. Without
	// the dedup the synthetic -1000 doubles it into a spurious ~+50% gain.
	if math.Abs(*s.TWR) > 0.1 {
		t.Errorf("source TWR = %.4f, want ~0 (closure double-count would inflate it)", *s.TWR)
	}
}

// TestRunReturnsCoarseNetting guards the netting wiring: an internal transfer
// pair nets, a lone leg stays unmatched (with the =N count), and --netting off
// keeps all legs.
func TestRunReturnsCoarseNetting(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)
	flat := []snap{{t0, 1000}, {t1, 1000}}
	seedAcct(t, db, ctx, "ubs", "X", canonical.AccountKindBrokerage, nil, flat, []txn{{tMid, canonical.TxKindTransferOut, -500}})
	seedAcct(t, db, ctx, "ubs", "Y", canonical.AccountKindBrokerage, nil, flat, []txn{{tMid, canonical.TxKindTransferIn, 500}})
	seedAcct(t, db, ctx, "ubs", "Z", canonical.AccountKindBrokerage, nil, flat, []txn{{tMid, canonical.TxKindTransferOut, -300}})

	end := eod(2024, time.July, 2)
	on, err := RunReturns(ctx, db, params("sources", 0, end))
	if err != nil {
		t.Fatalf("netting on: %v", err)
	}
	s, _ := summaryFor(on, "ubs")
	if !qualityHas(s, "unmatched_transfers=1") {
		t.Errorf("X↔Y should net and Z stay unmatched (=1); quality=%v", s.Quality)
	}

	p := params("sources", 0, end)
	p.Netting = false
	off, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("netting off: %v", err)
	}
	so, _ := summaryFor(off, "ubs")
	for _, q := range so.Quality {
		if strings.HasPrefix(q, "unmatched_transfers") {
			t.Errorf("--netting off must not flag unmatched; quality=%v", so.Quality)
		}
	}
}

// TestRunReturnsMWRErrorFlags guards the XIRR-error → quality-flag mapping
// through RunReturns.
func TestRunReturnsMWRErrorFlags(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "schwab", "schwab")
	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)
	// Went net-negative with only capital-in ⇒ investor vector never flips sign.
	seedAcct(t, db, ctx, "schwab", "NEG", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, -50}}, []txn{{tMid, canonical.TxKindDeposit, 100}})
	// Near-total loss with an interior deposit ⇒ true root below the -100% floor.
	seedAcct(t, db, ctx, "schwab", "WIPE", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, 1}}, []txn{{tMid, canonical.TxKindDeposit, 100}})

	rows, err := RunReturns(ctx, db, params("accounts", 0, eod(2024, time.July, 2)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if neg, _ := summaryFor(rows, "NEG"); !qualityHas(neg, "mwr_no_sign_change") {
		t.Errorf("NEG should be mwr_no_sign_change; got %+v", neg)
	}
	if wipe, _ := summaryFor(rows, "WIPE"); !qualityHas(wipe, "mwr_no_converge") {
		t.Errorf("WIPE should be mwr_no_converge; got %+v", wipe)
	}
}

// TestRunReturnsFxClampFlags guards review #3: a cross-currency window/flow valued
// before the FX history (off the migration-0023 day-0 clamp) is flagged.
func TestRunReturnsFxClampFlags(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, tDep, t1 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.July, 2)
	fxDay := dy(2024, time.April, 1) // USD/CHF history starts only in April
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		VALUES ('ubs', ?, 'USD', 'CHF', CAST('0.9' AS DECIMAL(20,10)))`, fxDay); err != nil {
		t.Fatalf("seed fx: %v", err)
	}
	chf := "CHF"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "ubs", AccountExternalID: "CH1", AccountKind: canonical.AccountKindBrokerage,
			BaseCurrency: &chf, FirstSeenAt: t0, LastSeenAt: t1}})
	})
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "ubs", SnapshotAt: t0, AccountExternalID: "CH1", PositionKey: "P", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "CHF", MarketValue: decp(1000)},
			{SilverSourceID: "ubs", SnapshotAt: t1, AccountExternalID: "CH1", PositionKey: "P", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "CHF", MarketValue: decp(1100)},
		}); err != nil {
			return err
		}
		// A CHF deposit before the FX history ⇒ valued off the clamped rate.
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: "ubs", TransactionExternalID: "CH1-d", OccurredAt: tDep, AccountExternalID: "CH1",
			Kind: canonical.TxKindDeposit, Currency: "CHF", NetAmount: decp(100)}})
	})

	rows, err := RunReturns(ctx, db, params("accounts", 0, eod(2024, time.July, 2)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	r, ok := summaryFor(rows, "CH1")
	if !ok || !qualityHas(r, "pre_fx_history") || !qualityHas(r, "fx_clamped_flow") {
		t.Errorf("CH1 should carry pre_fx_history + fx_clamped_flow; got %v", r.Quality)
	}
}

// TestRunReturnsMWRWiring checks a gold-level MWR against the value independently
// derived from the seeded inputs — verifying the engine assembles (v0, v1,
// window, flows) correctly and renders the period (de-annualized) figure.
func TestRunReturnsMWRWiring(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "schwab", "schwab")
	t0, tMid := dy(2024, time.January, 1), dy(2024, time.July, 1)
	end := eod(2024, time.December, 31)
	seedAcct(t, db, ctx, "schwab", "ACC", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {dy(2024, time.December, 31), 1700}}, []txn{{tMid, canonical.TxKindDeposit, 500}})

	rows, err := RunReturns(ctx, db, params("accounts", 0, end))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	acc, ok := summaryFor(rows, "ACC")
	if !ok || acc.MWR == nil {
		t.Fatalf("no ACC MWR: %+v", acc)
	}
	winFrom, winTo := t0/86400, end/86400
	rate, err := returns.XIRR(1000, 1700, winFrom, winTo, []returns.Flow{{Day: tMid / 86400, Amount: 500}})
	if err != nil {
		t.Fatalf("reference XIRR: %v", err)
	}
	want := returns.DeAnnualize(rate, float64(winTo-winFrom))
	if math.Abs(*acc.MWR-want) > 1e-6 {
		t.Errorf("ACC MWR (period) = %.6f, want %.6f (de-annualized XIRR of the seeded flows)", *acc.MWR, want)
	}
}

// TestRunReturnsSparseSnapshotNoChainCollapse guards the snapshot-aligned headline
// chain: a large deposit lands in a snapshot gap that straddles a month boundary
// and its value only shows at the next snapshot. Fixed monthly buckets would split
// the flow (its month has no value move ⇒ a sub-(-100%) Dietz) from the value jump
// (the next month), and chaining the poisoned factor collapses the headline (the
// real-data symptom was a since-inception TWR orders of magnitude below -100%).
// Snapshot-aligned sub-periods keep both in one [snap,snap] bucket.
func TestRunReturnsSparseSnapshotNoChainCollapse(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "sprx", "ubs")
	jan7 := dy(2024, time.January, 7)
	jan15 := dy(2024, time.January, 15)
	feb20 := dy(2024, time.February, 20)
	// Snapshots only at Jan 7 (1000) and Feb 20 (51500); a +50000 deposit on Jan 15
	// sits in the gap and is reflected only at the Feb 20 valuation.
	seedAcct(t, db, ctx, "sprx", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{jan7, 1000}, {feb20, 51500}},
		[]txn{{jan15, canonical.TxKindDeposit, 50000}})

	rows, err := RunReturns(ctx, db, params("sources", 0, eod(2024, time.March, 1)))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	s, ok := summaryFor(rows, "sprx")
	if !ok || s.TWR == nil {
		t.Fatalf("no sprx TWR: %+v", s)
	}
	// The canonical chain is the single snapshot-aligned bucket [Jan7, Feb20]: the
	// deposit explains the jump, leaving only the small real move. Independent oracle.
	want, dok := returns.ModifiedDietz(1000, 51500, jan7/86400, feb20/86400,
		[]returns.Flow{{Day: jan15 / 86400, Amount: 50000}})
	if !dok {
		t.Fatal("reference Dietz degenerate")
	}
	if math.Abs(*s.TWR-want) > 1e-9 {
		t.Errorf("summary TWR = %.4f, want %.4f (single snapshot-aligned bucket; monthly buckets would collapse)", *s.TWR, want)
	}
	if *s.TWR <= -1.0 {
		t.Errorf("summary TWR = %.2f collapsed below -100%% (monthly-bucket regression)", *s.TWR)
	}
}
