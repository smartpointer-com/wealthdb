package gold

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

func decp(n int64) *canonical.Decimal { v := canonical.NewDecimalFromInt(n); return &v }

func dy(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Unix()
}

type snap struct{ at, val int64 }
type txn struct {
	at   int64
	kind canonical.TxKind
	amt  int64
}

// seedAcct upserts an account (USD, optional portfolio) plus its position
// snapshots and transactions. All values synthetic.
func seedAcct(t *testing.T, db *sql.DB, ctx context.Context, src, acct string, kind canonical.AccountKind, pf *string, snaps []snap, txns []txn) {
	t.Helper()
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: src, AccountExternalID: acct, AccountKind: kind,
			BaseCurrency: &usd, PortfolioExternalID: pf,
			FirstSeenAt: snaps[0].at, LastSeenAt: snaps[len(snaps)-1].at,
		}})
	})
	inTx(t, db, ctx, func(w *Writer) error {
		var pos []canonical.PositionChange
		for _, s := range snaps {
			pos = append(pos, canonical.PositionChange{
				SilverSourceID: src, SnapshotAt: s.at, AccountExternalID: acct,
				PositionKey: "P", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: "USD", MarketValue: decp(s.val),
			})
		}
		if err := w.InsertPositions(ctx, pos); err != nil {
			return err
		}
		if len(txns) == 0 {
			return nil
		}
		var tx []canonical.TransactionChange
		for i, x := range txns {
			tx = append(tx, canonical.TransactionChange{
				SilverSourceID: src, TransactionExternalID: fmt.Sprintf("%s-tx%d", acct, i),
				OccurredAt: x.at, AccountExternalID: acct, Kind: x.kind,
				Currency: "USD", NetAmount: decp(x.amt),
			})
		}
		return w.InsertTransactions(ctx, tx)
	})
}

func params(level string, from, to int64) ReturnParams {
	return ReturnParams{Level: level, FromEpoch: from, ToEpoch: to, OutCcy: "USD",
		Method: "both", Period: "total", Annualize: "auto", Netting: true, Inception: "full"}
}

// TestNetTransfers covers the heuristic internal-transfer matching directly
// against netOwnedTransfers (the ownership-free case: untagged ownedFlow inputs).
func TestNetTransfers(t *testing.T) {
	net := func(fs ...returns.Flow) (int, int) {
		owned := make([]ownedFlow, len(fs))
		for i, f := range fs {
			owned[i] = ownedFlow{Flow: f}
		}
		kept, unmatched := netOwnedTransfers(owned)
		return len(kept), unmatched
	}
	// Opposite pair, same magnitude, within ±3 days ⇒ netted (internal move).
	if kept, n := net(returns.Flow{Day: 10, Amount: 1000}, returns.Flow{Day: 12, Amount: -1000}); kept != 0 || n != 0 {
		t.Errorf("matched pair: kept=%d unmatched=%d, want 0/0", kept, n)
	}
	// Same pair but outside the ±3-day window ⇒ both kept, both unmatched.
	if kept, n := net(returns.Flow{Day: 10, Amount: 1000}, returns.Flow{Day: 20, Amount: -1000}); kept != 2 || n != 2 {
		t.Errorf("out-of-window: kept=%d unmatched=%d, want 2/2", kept, n)
	}
	// Relative-eps branch: a 0.4% leg difference on a large transfer (within the
	// 0.5% tolerance) still nets.
	if kept, _ := net(returns.Flow{Day: 10, Amount: 100000}, returns.Flow{Day: 11, Amount: -100400}); kept != 0 {
		t.Errorf("rel-eps pair should net: kept=%d", kept)
	}
	// A lone leg can't match ⇒ kept + counted unmatched.
	if kept, n := net(returns.Flow{Day: 10, Amount: -500}); kept != 1 || n != 1 {
		t.Errorf("lone leg: kept=%d unmatched=%d, want 1/1", kept, n)
	}
}

// TestRunReturnsPortfoliosGrain covers the portfolios grain end to end: a real
// portfolio bucket plus the per-source "(no portfolio)" bucket.
func TestRunReturnsPortfoliosGrain(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	pf := "PF1"
	t0, t1 := dy(2024, time.January, 2), dy(2024, time.July, 2)
	seedAcct(t, db, ctx, "ubs", "A1", canonical.AccountKindBrokerage, &pf, []snap{{t0, 1000}, {t1, 1100}}, nil)
	seedAcct(t, db, ctx, "ubs", "A2", canonical.AccountKindBrokerage, &pf, []snap{{t0, 500}, {t1, 560}}, nil)
	seedAcct(t, db, ctx, "ubs", "ORPH", canonical.AccountKindSafekeeping, nil, []snap{{t0, 200}, {t1, 200}}, nil)

	rows, err := RunReturns(ctx, db, params("portfolios", 0, t1))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	if pfRow, ok := summaryFor(rows, "PF1"); !ok {
		t.Error("missing PF1 portfolio row")
	} else if pfRow.TWR == nil || pfRow.StartValue == nil || *pfRow.StartValue != "1500.00" {
		t.Errorf("PF1 start=%v twr=%v, want start 1500 + a TWR", pfRow.StartValue, pfRow.TWR)
	}
	if orph, ok := summaryFor(rows, ""); !ok || orph.EntityLabel != "(no portfolio)" {
		t.Errorf("missing/mislabelled no-portfolio bucket: %+v", orph)
	}
}

// TestRunReturnsPortfolioNameResolved verifies the portfolios grain reports the
// portfolio's display_name as the entity label while EntityID stays the stable
// external id (so the friendly name shows in the `entity` column like holdings).
func TestRunReturnsPortfolioNameResolved(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	pid, name := "PF9", "my-nickname"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertPortfolios(ctx, []canonical.PortfolioChange{{
			SilverSourceID: "ubs", PortfolioExternalID: pid, DisplayName: &name,
			FirstSeenAt: 1000, LastSeenAt: 1000,
		}})
	})
	t0, t1 := dy(2024, time.January, 2), dy(2024, time.July, 2)
	seedAcct(t, db, ctx, "ubs", "A1", canonical.AccountKindBrokerage, &pid,
		[]snap{{t0, 1000}, {t1, 1100}}, nil)

	rows, err := RunReturns(ctx, db, params("portfolios", 0, t1))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	r, ok := summaryFor(rows, pid) // EntityID stays the external id
	if !ok {
		t.Fatalf("missing %s portfolio row", pid)
	}
	if r.EntityLabel != name {
		t.Errorf("EntityLabel = %q, want display_name %q", r.EntityLabel, name)
	}
}

// TestRunReturnsStaggeredOnboarding covers the aggregate path: synthetic
// onboarding (the late constituent's full debut value, with its own debut-region
// deposit subsumed — see flowSubsumed) and the staggered_inception flag.
func TestRunReturnsStaggeredOnboarding(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)
	// A: present from t0, flat. B: debuts at tMid worth 500, with a real deposit of
	// 200 ON the debut day (subsumed, so onboarding books B's full 500 once — the
	// deposit is NOT also counted on top).
	seedAcct(t, db, ctx, "ubs", "A", canonical.AccountKindBrokerage, nil, []snap{{t0, 1000}, {t1, 1000}}, nil)
	seedAcct(t, db, ctx, "ubs", "B", canonical.AccountKindBrokerage, nil, []snap{{tMid, 500}, {t1, 520}},
		[]txn{{tMid, canonical.TxKindDeposit, 200}})

	rows, err := RunReturns(ctx, db, params("sources", 0, t1))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	g, ok := summaryFor(rows, "ubs")
	if !ok {
		t.Fatal("missing ubs source row")
	}
	if !qualityHas(g, "staggered_inception") {
		t.Errorf("expected staggered_inception; quality=%v", g.Quality)
	}
	// B's debut value must not be booked as performance: the aggregate TWR stays
	// small/sane (A flat + B's ~4%% growth on its slice), never a ~50%% step-up.
	if g.TWR == nil || *g.TWR < -0.1 || *g.TWR > 0.2 {
		t.Errorf("staggered TWR = %v, want a small sane value (onboarding absorbed)", g.TWR)
	}
}

// TestRunReturnsMWRFlags covers mwr_incomplete_flows (mixed regime aggregate)
// and mwr_nonunique (interleaved flows).
func TestRunReturnsMWRFlags(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "schwab", "schwab")
	seedReturnsSource(t, db, ctx, "manualre", "manual")
	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)

	// Global mixes a flow-complete brokerage (with a real deposit) and a NAV-only
	// manual holding ⇒ MWR is computed but flagged incomplete.
	seedAcct(t, db, ctx, "schwab", "BROK", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, 1200}}, []txn{{tMid, canonical.TxKindDeposit, 100}})
	seedAcct(t, db, ctx, "manualre", "RE", canonical.AccountKindOther, nil, []snap{{t0, 2000}, {t1, 2100}}, nil)

	g, err := RunReturns(ctx, db, params("global", 0, t1))
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if len(g) != 1 || g[0].MWR == nil || !qualityHas(g[0], "mwr_incomplete_flows") {
		t.Errorf("global MWR should be defined + mwr_incomplete_flows; got %+v", g[0])
	}
	// The global row spans multiple sources, so silver_source must be empty —
	// never a non-deterministic single-source pick.
	if g[0].SilverSourceID != "" {
		t.Errorf("global silver_source = %q, want empty (cross-source)", g[0].SilverSourceID)
	}

	// An account with a withdrawal then a later deposit gives >1 sign change in
	// the investor vector ⇒ mwr_nonunique (a root is still reported).
	seedAcct(t, db, ctx, "schwab", "NONUNI", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, 1100}},
		[]txn{{dy(2024, time.February, 1), canonical.TxKindWithdrawal, -200},
			{dy(2024, time.May, 1), canonical.TxKindDeposit, 300}})
	rows, err := RunReturns(ctx, db, params("accounts", 0, t1))
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	nu, ok := summaryFor(rows, "NONUNI")
	if !ok || !qualityHas(nu, "mwr_nonunique") {
		t.Errorf("NONUNI should be mwr_nonunique; got %+v", nu)
	}
}

// TestRunReturnsRegimeFlags covers journal_present and
// crypto_unclassified_transfers. (unknown_adapter_policy is unreachable through
// the real pipeline — the silver_sources.silver_kind CHECK constraint admits
// only known adapter kinds — so the policy default is unit-tested at the
// ReturnsPolicyFor level instead; see RETURNS-NOTES.)
func TestRunReturnsRegimeFlags(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fid", "fidelity")
	seedReturnsSource(t, db, ctx, "ct", "cointracking")
	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)

	seedAcct(t, db, ctx, "fid", "F", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 1000}, {t1, 1100}}, []txn{{tMid, canonical.TxKindJournal, 50}})
	seedAcct(t, db, ctx, "ct", "C", canonical.AccountKindCryptoSelfCustody, nil,
		[]snap{{t0, 1000}, {t1, 1100}}, []txn{{tMid, canonical.TxKindTransferIn, 999}})

	rows, err := RunReturns(ctx, db, params("accounts", 0, t1))
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	check := func(entity, flag string) {
		r, ok := summaryFor(rows, entity)
		if !ok || !qualityHas(r, flag) {
			t.Errorf("%s should carry %s; got %+v", entity, flag, r)
		}
	}
	check("F", "journal_present")
	check("C", "crypto_unclassified_transfers")
}

// TestRunReturnsPartialWindowAndStrict covers the explicit-window clamp flag and
// the strict-inception aggregate window start.
func TestRunReturnsPartialWindowAndStrict(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	t0, tMid, t1 := dy(2024, time.March, 1), dy(2024, time.June, 1), dy(2024, time.September, 1)
	seedAcct(t, db, ctx, "ubs", "A", canonical.AccountKindBrokerage, nil, []snap{{t0, 1000}, {t1, 1100}}, nil)
	seedAcct(t, db, ctx, "ubs", "B", canonical.AccountKindBrokerage, nil, []snap{{tMid, 500}, {t1, 520}}, nil)

	// Explicit FROM before A's inception ⇒ clamped + partial_window (accounts).
	early := dy(2024, time.January, 1)
	rows, err := RunReturns(ctx, db, params("accounts", early, t1))
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	a, ok := summaryFor(rows, "A")
	if !ok || !qualityHas(a, "partial_window") {
		t.Errorf("A should carry partial_window; got %+v", a)
	}
	if a.StartDay != t0/86400 {
		t.Errorf("A window start = %d, want clamped to inception %d", a.StartDay, t0/86400)
	}

	// strict inception on the source aggregate ⇒ window starts at the LATEST
	// constituent's first snapshot (B's tMid), not the earliest (A's t0).
	p := params("sources", 0, t1)
	p.Inception = "strict"
	srows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	s, ok := summaryFor(srows, "ubs")
	if !ok || s.StartDay != tMid/86400 {
		t.Errorf("strict window start = %d, want %d (max constituent inception)", s.StartDay, tMid/86400)
	}
}

// TestRunReturnsEmptyBucket covers the per-bucket display row and the
// empty_bucket / carried_forward flags on a month with no fresh snapshot.
func TestRunReturnsEmptyBucket(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	// Snapshots in Jan and Apr only ⇒ Feb and Mar buckets are carried-forward.
	t0, t1 := dy(2024, time.January, 15), dy(2024, time.April, 15)
	seedAcct(t, db, ctx, "ubs", "A", canonical.AccountKindBrokerage, nil, []snap{{t0, 1000}, {t1, 1200}}, nil)

	p := params("accounts", 0, t1)
	p.Period = "monthly"
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	var sawEmpty, sawBoundary bool
	for _, r := range rows {
		if r.IsSummary {
			continue
		}
		if qualityHas(r, "empty_bucket") && qualityHas(r, "carried_forward") {
			sawEmpty = true
		}
		// The receiving bucket (a fresh snapshot after empty months) over-attributes
		// the accumulated move and must carry boundary_same_snapshot — distinct from
		// the donor empty_bucket.
		if qualityHas(r, "boundary_same_snapshot") {
			sawBoundary = true
		}
	}
	if !sawEmpty {
		t.Errorf("expected at least one empty_bucket/carried_forward month; rows=%d", len(rows))
	}
	if !sawBoundary {
		t.Errorf("expected a boundary_same_snapshot receiving bucket after the gap; rows=%d", len(rows))
	}
}
