package gold

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
)

func TestPositionsAsOfSingleSource(t *testing.T) {
	db, ctx := openMigrated(t)

	// Two snapshots for one silver source; older has 1 position,
	// newer has 2. Asking as-of newer's snapshot returns 2.
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC", AccountKind: canonical.AccountKindBrokerage,
			FirstSeenAt: 1, LastSeenAt: 1,
		}}); err != nil {
			return err
		}
		qty10 := canonical.NewDecimalFromInt(10)
		mv1500, _ := canonical.NewDecimalFromString("1500.00")
		qty5 := canonical.NewDecimalFromInt(5)
		mv250, _ := canonical.NewDecimalFromString("250.00")
		return w.InsertPositions(ctx, []canonical.PositionChange{
			// snapshot 1000: just AAPL
			{
				SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC",
				PositionKey: "AAPL", AssetClass: canonical.AssetClassEquity, Currency: "USD",
				Quantity: &qty10, MarketValue: &mv1500,
			},
			// snapshot 2000: AAPL + MSFT
			{
				SilverSourceID: "test-src", SnapshotAt: 2000, AccountExternalID: "ACC",
				PositionKey: "AAPL", AssetClass: canonical.AssetClassEquity, Currency: "USD",
				Quantity: &qty10, MarketValue: &mv1500,
			},
			{
				SilverSourceID: "test-src", SnapshotAt: 2000, AccountExternalID: "ACC",
				PositionKey: "MSFT", AssetClass: canonical.AssetClassEquity, Currency: "USD",
				Quantity: &qty5, MarketValue: &mv250,
			},
		})
	})

	// As-of 2500 → newest snapshot picked (2000) → 2 rows.
	rows, err := PositionsAsOf(ctx, db, 2500)
	if err != nil {
		t.Fatalf("PositionsAsOf: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].PositionKey != "AAPL" || rows[1].PositionKey != "MSFT" {
		t.Errorf("ordering = %s, %s; want AAPL, MSFT", rows[0].PositionKey, rows[1].PositionKey)
	}

	// As-of 1500 → snapshot 1000 picked → 1 row.
	rows, err = PositionsAsOf(ctx, db, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PositionKey != "AAPL" || rows[0].SnapshotAt != 1000 {
		t.Errorf("got %+v, want one AAPL row at snapshot 1000", rows)
	}

	// As-of 500 → no snapshots ≤ that → 0 rows.
	rows, err = PositionsAsOf(ctx, db, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestPositionsAsOfMultiSourceIndependentLatest(t *testing.T) {
	db, ctx := openMigrated(t)

	// Seed a second silver source.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(
            silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at
        ) VALUES ('other-src', 'ubs', '/tmp/other.db', -1, 0, 0)
    `); err != nil {
		t.Fatal(err)
	}

	qty := canonical.NewDecimalFromInt(1)
	mv, _ := canonical.NewDecimalFromString("100.00")
	inTx(t, db, ctx, func(w *Writer) error {
		// Source A: snapshots at 1000 and 3000
		// Source B: snapshots at 1500 and 2500
		// Asking as-of 2700 should pick A=3000? No — 3000 > 2700.
		// Pick A=1000 (latest ≤ 2700), B=2500.
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "test-src", AccountExternalID: "A", AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1, LastSeenAt: 1},
			{SilverSourceID: "other-src", AccountExternalID: "B", AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1, LastSeenAt: 1},
		}); err != nil {
			return err
		}
		return w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "A", PositionKey: "AAPL", AssetClass: canonical.AssetClassEquity, Currency: "USD", Quantity: &qty, MarketValue: &mv},
			{SilverSourceID: "test-src", SnapshotAt: 3000, AccountExternalID: "A", PositionKey: "AAPL", AssetClass: canonical.AssetClassEquity, Currency: "USD", Quantity: &qty, MarketValue: &mv},
			{SilverSourceID: "other-src", SnapshotAt: 1500, AccountExternalID: "B", PositionKey: "MSFT", AssetClass: canonical.AssetClassEquity, Currency: "USD", Quantity: &qty, MarketValue: &mv},
			{SilverSourceID: "other-src", SnapshotAt: 2500, AccountExternalID: "B", PositionKey: "MSFT", AssetClass: canonical.AssetClassEquity, Currency: "USD", Quantity: &qty, MarketValue: &mv},
		})
	})

	rows, err := PositionsAsOf(ctx, db, 2700)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Expect: other-src @ 2500 and test-src @ 1000
	wantSnap := map[string]int64{"other-src": 2500, "test-src": 1000}
	for _, r := range rows {
		if want, ok := wantSnap[r.SilverSourceID]; !ok || r.SnapshotAt != want {
			t.Errorf("source %q snapshot_at = %d, want %d", r.SilverSourceID, r.SnapshotAt, want)
		}
	}
}

