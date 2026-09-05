package gold

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

func seedReturnsSource(t *testing.T, db *sql.DB, ctx context.Context, id, kind string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
			high_watermark, first_loaded_at, last_loaded_at)
		VALUES (?, ?, ?, -1, 0, 0)`, id, kind, "/tmp/"+id+".db"); err != nil {
		t.Fatalf("seed source %s: %v", id, err)
	}
}

func qualityHas(r ReturnRow, flag string) bool {
	for _, q := range r.Quality {
		if q == flag {
			return true
		}
	}
	return false
}

func summaryFor(rows []ReturnRow, entityID string) (ReturnRow, bool) {
	for _, r := range rows {
		if r.IsSummary && r.EntityID == entityID {
			return r, true
		}
	}
	return ReturnRow{}, false
}

// TestRunReturnsSyntheticFixture exercises the engine end-to-end on a synthetic
// USD gold fixture: a flow-complete brokerage account, a mortgage liability, and
// a NAV-only (manual) holding.
func TestRunReturnsSyntheticFixture(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "schwab", "schwab")
	seedReturnsSource(t, db, ctx, "manualre", "manual")

	usd := "USD"
	day := func(y int, m time.Month, d int) int64 {
		return time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Unix()
	}
	t0, tMid, t1 := day(2024, time.January, 2), day(2024, time.April, 1), day(2024, time.July, 2)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "schwab", AccountExternalID: "BROK1", AccountKind: canonical.AccountKindBrokerage,
				DisplayName: ptr("Brokerage"), BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
			{SilverSourceID: "schwab", AccountExternalID: "MORT1", AccountKind: canonical.AccountKindMortgage,
				DisplayName: ptr("Mortgage"), BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
			{SilverSourceID: "manualre", AccountExternalID: "RE1", AccountKind: canonical.AccountKindOther,
				DisplayName: ptr("Real estate"), BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
		})
	})

	d := func(n int64) *canonical.Decimal { v := canonical.NewDecimalFromInt(n); return &v }
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			// Brokerage: 1000 -> 1200 (post-deposit + growth).
			{SilverSourceID: "schwab", SnapshotAt: t0, AccountExternalID: "BROK1", PositionKey: "AAA",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(1000)},
			{SilverSourceID: "schwab", SnapshotAt: t1, AccountExternalID: "BROK1", PositionKey: "AAA",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(1200)},
			// Mortgage: negative principal.
			{SilverSourceID: "schwab", SnapshotAt: t0, AccountExternalID: "MORT1", PositionKey: "M",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(-500)},
			{SilverSourceID: "schwab", SnapshotAt: t1, AccountExternalID: "MORT1", PositionKey: "M",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(-480)},
			// NAV-only real estate: 2000 -> 2100, no transactions.
			{SilverSourceID: "manualre", SnapshotAt: t0, AccountExternalID: "RE1", PositionKey: "RE",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(2000)},
			{SilverSourceID: "manualre", SnapshotAt: t1, AccountExternalID: "RE1", PositionKey: "RE",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(2100)},
		}); err != nil {
			return err
		}
		// A +100 external deposit into the brokerage between the two snapshots.
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: "schwab", TransactionExternalID: "TX1", OccurredAt: tMid,
			AccountExternalID: "BROK1", Kind: canonical.TxKindDeposit, Currency: "USD", NetAmount: d(100),
		}})
	})

	end := time.Date(2024, time.July, 2, 23, 59, 59, 0, time.UTC).Unix()
	rows, err := RunReturns(ctx, db, ReturnParams{
		Level: "accounts", FromEpoch: 0, ToEpoch: end, OutCcy: "USD",
		Method: "both", Period: "total", Annualize: "auto", Netting: true, Inception: "full",
	})
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}

	// Brokerage: a real, positive TWR and a defined MWR; honest provenance flags.
	brok, ok := summaryFor(rows, "BROK1")
	if !ok {
		t.Fatal("no BROK1 summary row")
	}
	if brok.TWR == nil || *brok.TWR <= 0 || *brok.TWR > 0.5 {
		t.Errorf("BROK1 TWR = %v, want a small positive return", brok.TWR)
	}
	if brok.MWR == nil {
		t.Errorf("BROK1 MWR should be defined (a real deposit exists)")
	}
	if !qualityHas(brok, "since_data_inception") || !qualityHas(brok, "after_tax") {
		t.Errorf("BROK1 quality missing provenance flags: %v", brok.Quality)
	}

	// Mortgage: excluded from returns, n/a + nonpositive_base.
	mort, ok := summaryFor(rows, "MORT1")
	if !ok {
		t.Fatal("no MORT1 row")
	}
	if mort.TWR != nil || !qualityHas(mort, "nonpositive_base") {
		t.Errorf("MORT1 should be n/a + nonpositive_base; got TWR=%v q=%v", mort.TWR, mort.Quality)
	}

	// NAV-only: TWR from the value series, MWR not available.
	re, ok := summaryFor(rows, "RE1")
	if !ok {
		t.Fatal("no RE1 row")
	}
	if re.TWR == nil {
		t.Errorf("RE1 should have a value-growth TWR")
	}
	if re.MWR != nil || !qualityHas(re, "mwr_no_flows") || !qualityHas(re, "nav_only") {
		t.Errorf("RE1 should be nav_only + mwr_no_flows with MWR n/a; got MWR=%v q=%v", re.MWR, re.Quality)
	}
}

// TestRunReturnsGlobalExcludesLiabilities confirms the global rollup builds a
// single summary row and excludes the mortgage from the return.
func TestRunReturnsGlobalExcludesLiabilities(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "schwab", "schwab")

	usd := "USD"
	t0 := time.Date(2024, time.January, 2, 12, 0, 0, 0, time.UTC).Unix()
	t1 := time.Date(2024, time.July, 2, 12, 0, 0, 0, time.UTC).Unix()
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "schwab", AccountExternalID: "BROK1", AccountKind: canonical.AccountKindBrokerage,
				BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
			{SilverSourceID: "schwab", AccountExternalID: "MORT1", AccountKind: canonical.AccountKindMortgage,
				BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
		})
	})
	d := func(n int64) *canonical.Decimal { v := canonical.NewDecimalFromInt(n); return &v }
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "schwab", SnapshotAt: t0, AccountExternalID: "BROK1", PositionKey: "AAA",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(1000)},
			{SilverSourceID: "schwab", SnapshotAt: t1, AccountExternalID: "BROK1", PositionKey: "AAA",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(1100)},
			{SilverSourceID: "schwab", SnapshotAt: t0, AccountExternalID: "MORT1", PositionKey: "M",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(-500)},
			{SilverSourceID: "schwab", SnapshotAt: t1, AccountExternalID: "MORT1", PositionKey: "M",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: d(-480)},
		})
	})

	end := time.Date(2024, time.July, 2, 23, 59, 59, 0, time.UTC).Unix()
	rows, err := RunReturns(ctx, db, ReturnParams{
		Level: "global", FromEpoch: 0, ToEpoch: end, OutCcy: "USD",
		Method: "twr", Period: "total", Annualize: "auto", Netting: true, Inception: "full",
	})
	if err != nil {
		t.Fatalf("RunReturns global: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("global rows = %d, want 1", len(rows))
	}
	g := rows[0]
	// 1000 -> 1100, no flows ⇒ ~10% TWR (mortgage excluded; the -480 must not drag it).
	if g.TWR == nil || *g.TWR < 0.05 || *g.TWR > 0.15 {
		t.Errorf("global TWR = %v, want ~0.10 (mortgage excluded)", g.TWR)
	}
	if strings.Contains(strings.Join(g.Quality, ";"), "nonpositive") {
		t.Errorf("global should not be nonpositive once the mortgage is excluded: %v", g.Quality)
	}
}

// TestEpochDayFloors pins the floor. Truncating instead would band a
// pre-1970 timestamp with the day after the one it belongs to, and the
// helper is what every day-banded window in the tree computes from.
func TestEpochDayFloors(t *testing.T) {
	cases := map[int64]int64{
		0:      0,
		86399:  0,
		86400:  1,
		-1:     -1,
		-86400: -1,
		-86401: -2,
	}
	for sec, want := range cases {
		if got := EpochDay(sec); got != want {
			t.Errorf("EpochDay(%d) = %d, want %d", sec, got, want)
		}
	}
}
