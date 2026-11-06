package gold

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedMarkPositions writes one position per snapshot, with its own mark day.
func seedMarkPositions(t *testing.T, db *sql.DB, ctx context.Context, src, acct string, kind canonical.AccountKind,
	class canonical.AssetClass, rows []canonical.PositionChange) {
	t.Helper()
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: src, AccountExternalID: acct,
			AccountKind: kind, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 1}})
	})
	for i := range rows {
		rows[i].SilverSourceID, rows[i].AccountExternalID = src, acct
		rows[i].AssetClass, rows[i].Vehicle, rows[i].Currency = class, canonical.VehicleStock, "USD"
		if rows[i].PositionKey == "" {
			rows[i].PositionKey = "P"
		}
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertPositions(ctx, rows) })
}

func marks(t *testing.T, db *sql.DB, ctx context.Context, ccy string) map[string]MarkRow {
	t.Helper()
	rows, err := MarkAges(ctx, db, 0, dy(2024, time.April, 30)+43199, ccy)
	if err != nil {
		t.Fatalf("MarkAges: %v", err)
	}
	out := map[string]MarkRow{}
	for _, r := range rows {
		out[time.Unix(r.MonthEnd, 0).UTC().Format("2006-01-02")] = r
	}
	return out
}

func TestMarkAge(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "m", "schwab")
	// The history opens with cash alone, so January's month end has no line.
	seedCashOnlyAcct(t, db, ctx, "m", "CASH", canonical.AccountKindCash,
		[]snap{{dy(2024, time.January, 10), 5000}, {dy(2024, time.April, 30), 5000}}, nil)
	feb29, mar31 := dy(2024, time.February, 29), dy(2024, time.March, 31)
	seedMarkPositions(t, db, ctx, "m", "FRESH", canonical.AccountKindBrokerage, canonical.AssetClassPublicEquity,
		[]canonical.PositionChange{
			// No valued_at: the snapshot's own day, at the month end.
			{SnapshotAt: feb29, MarketValue: decp(600)},
			{SnapshotAt: mar31, MarketValue: decp(600)},
		})
	seedMarkPositions(t, db, ctx, "m", "EDGE", canonical.AccountKindBrokerage, canonical.AssetClassPublicEquity,
		[]canonical.PositionChange{
			// Five days old at February's end counts as marked.
			{SnapshotAt: feb29, ValuedAt: dy(2024, time.February, 24), MarketValue: decp(300)},
			// Six days old at March's end does not.
			{SnapshotAt: mar31, ValuedAt: dy(2024, time.March, 25), MarketValue: decp(300)},
		})
	seedMarkPositions(t, db, ctx, "m", "OLD", canonical.AccountKindBrokerage, canonical.AssetClassPrivateEquity,
		[]canonical.PositionChange{{SnapshotAt: feb29, ValuedAt: dy(2023, time.December, 31), MarketValue: decp(100)},
			{SnapshotAt: mar31, ValuedAt: dy(2023, time.December, 31), MarketValue: decp(100)}})
	// A liability counts for nothing, a line above zero on one too: an
	// escrow balance on a mortgage, an overpaid card.
	seedMarkPositions(t, db, ctx, "m", "MORT", canonical.AccountKindMortgage, canonical.AssetClassPublicEquity,
		[]canonical.PositionChange{{SnapshotAt: feb29, MarketValue: decp(-9000)}, {SnapshotAt: mar31, MarketValue: decp(-9000)},
			{SnapshotAt: feb29, PositionKey: "ESCROW", ValuedAt: dy(2023, time.January, 1), MarketValue: decp(4000)},
			{SnapshotAt: mar31, PositionKey: "ESCROW", ValuedAt: dy(2023, time.January, 1), MarketValue: decp(4000)}})
	seedMarkPositions(t, db, ctx, "m", "CARD", canonical.AccountKindCard, canonical.AssetClassPublicEquity,
		[]canonical.PositionChange{{SnapshotAt: feb29, ValuedAt: dy(2023, time.January, 1), MarketValue: decp(2000)},
			{SnapshotAt: mar31, ValuedAt: dy(2023, time.January, 1), MarketValue: decp(2000)}})
	seedFX(t, db, dy(2024, time.January, 1), "USD", "CHF", "0.50")

	usd := marks(t, db, ctx, "USD")
	if r, ok := usd["2024-01-31"]; !ok || r.MarkedPct != nil || r.Quality != "no_positions" {
		t.Errorf("a month with no line: %+v, present %v", r, ok)
	}
	feb, mar := usd["2024-02-29"], usd["2024-03-31"]
	if feb.MarkedPct == nil || math.Abs(*feb.MarkedPct-90) > 1e-9 {
		t.Errorf("February %v, want 90: the five-day edge is marked", feb.MarkedPct)
	}
	if mar.MarkedPct == nil || math.Abs(*mar.MarkedPct-60) > 1e-9 {
		t.Errorf("March %v, want 60: a sixth day is not", mar.MarkedPct)
	}
	if cents(t, feb.ValueTotal) != 100000 || cents(t, feb.ValueMarked) != 90000 {
		t.Errorf("February values %s, %s", *feb.ValueTotal, *feb.ValueMarked)
	}
	// marked_pct does not move with the currency; the money does.
	chf := marks(t, db, ctx, "CHF")["2024-03-31"]
	if chf.MarkedPct == nil || *chf.MarkedPct != *mar.MarkedPct {
		t.Errorf("CHF %v, USD %v", chf.MarkedPct, mar.MarkedPct)
	}
	if cents(t, chf.ValueTotal) != 200000 {
		t.Errorf("CHF total %s", *chf.ValueTotal)
	}
}

func TestMarkAgeLeavesOutCashClassPositions(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "m", "schwab")
	at := dy(2024, time.January, 31)
	seedMarkPositions(t, db, ctx, "m", "A", canonical.AccountKindBrokerage, canonical.AssetClassPublicEquity,
		[]canonical.PositionChange{{SnapshotAt: at, ValuedAt: dy(2023, time.January, 1), MarketValue: decp(100)}})
	if _, err := db.ExecContext(ctx, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
		position_key, asset_class, vehicle, currency, market_value)
		VALUES ('m', ?, 'A', 'C', 'cash', 'money_market_fund', 'USD', 900)`, at); err != nil {
		t.Fatalf("seed cash position: %v", err)
	}
	r := marks(t, db, ctx, "USD")["2024-01-31"]
	if r.MarkedPct == nil || *r.MarkedPct != 0 || cents(t, r.ValueTotal) != 10000 {
		t.Errorf("row = %+v", r)
	}
}
