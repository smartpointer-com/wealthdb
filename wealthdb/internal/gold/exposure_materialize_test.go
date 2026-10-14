package gold

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"
)

// The materialized look-through at every instant is the live one, in
// every reporting currency: its rows sum to holdings global and split by
// class exactly as the classes view does. The run reaches past the
// fixture's month end, so it writes several instants.
func TestMaterializeExposureMatchesTheLiveViews(t *testing.T) {
	db, ctx := openExposureFixture(t)
	to := exAsOf + 45*86400
	n, err := MaterializeExposure(ctx, db, to, 42)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no rows materialized")
	}
	first, _, err := FirstHoldingsAt(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	instants := ExposureInstants(first, to)
	if len(instants) < 3 {
		t.Fatalf("instants %v: the test needs several", instants)
	}
	var stored int
	if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT as_of) FROM report_exposure`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != len(instants) {
		t.Errorf("%d instants stored, want %d", stored, len(instants))
	}
	for _, at := range instants {
		for _, ccy := range MaterializedCurrencies() {
			checkMaterializedInstant(t, ctx, db, at, ccy)
		}
		// web_exposure's shares of an instant add up to the whole.
		var shares float64
		if err := db.QueryRowContext(ctx, `SELECT SUM(share_pct) FROM web_exposure WHERE currency = 'USD'
		    AND as_of_day = CAST(epoch_ms(? * 1000) AS DATE)`, at).Scan(&shares); err != nil {
			t.Fatal(err)
		}
		if math.Abs(shares-100) > 1e-6 {
			t.Errorf("at %d: shares add to %.6f", at, shares)
		}
	}
	// A second run rewrites the table, it does not add to it.
	m, err := MaterializeExposure(ctx, db, to, 43)
	if err != nil || m != n {
		t.Errorf("second run: %d rows, %v; want %d", m, err, n)
	}
}

// checkMaterializedInstant compares one instant and currency of
// report_exposure with holdings global and the classes view.
func checkMaterializedInstant(t *testing.T, ctx context.Context, db *sql.DB, at int64, ccy string) {
	t.Helper()
	g, err := GlobalAsOf(ctx, db, at, ccy)
	if err != nil {
		t.Fatal(err)
	}
	var total sql.NullFloat64
	if err := db.QueryRowContext(ctx, `SELECT SUM(value) FROM report_exposure WHERE currency = ? AND as_of = ?`,
		ccy, at).Scan(&total); err != nil {
		t.Fatal(err)
	}
	// A currency no rate reaches has no net worth and no rows.
	if g.TotalValueOutCcy == nil {
		if total.Valid {
			t.Errorf("%s at %d: materialized %.4f with no holdings global", ccy, at, total.Float64)
		}
		return
	}
	if want := parse(t, g.TotalValueOutCcy); math.Abs(total.Float64-want) > 0.01 {
		t.Errorf("%s at %d: materialized %.4f, holdings global %.4f", ccy, at, total.Float64, want)
	}
	classes, err := ExposureClasses(ctx, db, at, ccy)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range classes {
		var v float64
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(value), 0) FROM report_exposure
		    WHERE currency = ? AND as_of = ? AND asset_class = ?`, ccy, at, c.AssetClass).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if want := parse(t, c.Value); math.Abs(v-want) > 0.01 {
			t.Errorf("%s %s at %d: materialized %.4f, classes view %.4f", ccy, c.AssetClass, at, v, want)
		}
	}
}

func TestExposureInstantsAreMonthEndsAndTheEnd(t *testing.T) {
	day := func(y int, m time.Month, d, h, mi, s int) int64 {
		return time.Date(y, m, d, h, mi, s, 0, time.UTC).Unix()
	}
	got := ExposureInstants(day(2026, 1, 15, 0, 0, 0), day(2026, 3, 10, 23, 59, 59))
	want := []int64{day(2026, 1, 31, 23, 59, 59), day(2026, 2, 28, 23, 59, 59), day(2026, 3, 10, 23, 59, 59)}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("instant %d = %d, want %d", i, got[i], want[i])
		}
	}
	// A month end that is the end itself counts once.
	if got := ExposureInstants(day(2026, 6, 1, 0, 0, 0), day(2026, 6, 30, 23, 59, 59)); len(got) != 1 {
		t.Errorf("one month: %v", got)
	}
}

// An empty gold materializes nothing.
func TestMaterializeExposureOnAnEmptyGold(t *testing.T) {
	db, ctx := openMigrated(t)
	n, err := MaterializeExposure(ctx, db, exAsOf, 1)
	if err != nil || n != 0 {
		t.Errorf("empty gold: %d rows, %v", n, err)
	}
}

// Cash counts as a holding: a gold that holds cash alone starts its
// history at the cash's first snapshot.
func TestFirstHoldingsAtCountsCash(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, ok, err := FirstHoldingsAt(ctx, db); err != nil || ok {
		t.Fatalf("empty gold: ok %v, %v", ok, err)
	}
	cashAt := time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC).Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
	    currency, balance_kind, amount) VALUES ('s', ?, 'a', 'USD', 'ledger', 10)`, cashAt); err != nil {
		t.Fatal(err)
	}
	if at, ok, err := FirstHoldingsAt(ctx, db); err != nil || !ok || at != cashAt {
		t.Errorf("cash only: %d, ok %v, %v; want %d", at, ok, err, cashAt)
	}
}

func TestMigration0128DDLIsRerunnable(t *testing.T) {
	db, ctx := openExposureFixture(t)
	if _, err := MaterializeExposure(ctx, db, exAsOf, 1); err != nil {
		t.Fatal(err)
	}
	rerunMigrationDDL(t, db, ctx, "0128_exposure_dashboard.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_exposure`).Scan(&n); err != nil || n == 0 {
		t.Errorf("after replay: %d rows, err %v", n, err)
	}
}
