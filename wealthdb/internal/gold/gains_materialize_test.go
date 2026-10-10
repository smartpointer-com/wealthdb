package gold

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// The materialized rows summed by month are the gains report's monthly
// buckets, in every reporting currency.
func TestMaterializeGainsMatchesTheMonthlyBuckets(t *testing.T) {
	db, ctx := openGainsFixture(t)
	n, err := MaterializeGains(ctx, db, gainsTo, 42)
	if err != nil {
		t.Fatal(err)
	}
	var total, stamped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE computed_at = 42) FROM report_gains`).Scan(&total, &stamped); err != nil {
		t.Fatal(err)
	}
	if n == 0 || total != n || stamped != n {
		t.Fatalf("materialized %d rows, table holds %d, %d stamped", n, total, stamped)
	}
	matchesBuckets(t, ctx, db)

	// A rerun replaces the table rather than adding to it.
	if n2, err := MaterializeGains(ctx, db, gainsTo, 43); err != nil || n2 != n {
		t.Fatalf("rerun: %d rows, err %v; want %d", n2, err, n)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM report_gains`).Scan(&total); err != nil || total != n {
		t.Errorf("after the rerun the table holds %d rows, want %d", total, n)
	}
}

// The realized cost basis counts the lots whose gain is known: BBB's
// derived gain of 150 on 400, and not the undated AAA lot, which states
// a gain and no cost basis.
func TestMaterializedRealizedCostBasis(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := MaterializeGains(ctx, db, gainsTo, 1); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `
        SELECT instrument_key, realized, realized_cost_basis
          FROM web_gains WHERE currency = 'USD' AND realized_lots > 0 ORDER BY instrument_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]*float64{}
	for rows.Next() {
		var key string
		var realized, cost *float64
		if err := rows.Scan(&key, &realized, &cost); err != nil {
			t.Fatal(err)
		}
		got[key] = [2]*float64{realized, cost}
	}
	if r := got["BBB"]; r[0] == nil || r[1] == nil || !near(*r[0], 150) || !near(*r[1], 400) {
		t.Errorf("BBB = %v", r)
	}
	if r := got["AAA"]; r[0] == nil || !near(*r[0], 5) || r[1] != nil {
		t.Errorf("AAA = %v", r)
	}
	if r := got["ZZZ"]; r[0] != nil || r[1] != nil {
		t.Errorf("ZZZ = %v", r)
	}
}

// web_gains labels the account and carries its tax wrapper, and its
// period is the month's start, the first bucket's too, which opens at
// the first snapshot.
func TestWebGainsLabelsTheAccount(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := MaterializeGains(ctx, db, gainsTo, 1); err != nil {
		t.Fatal(err)
	}
	var account, wrapper, period string
	if err := db.QueryRowContext(ctx, `
        SELECT account, tax_wrapper, CAST(period AS VARCHAR) FROM web_gains
         WHERE currency = 'USD' AND symbol = 'DDD' AND at_end ORDER BY period LIMIT 1`).Scan(&account, &wrapper, &period); err != nil {
		t.Fatal(err)
	}
	if account != "Brokerage (test-src brokerage)" || wrapper != "taxable_personal" || period != "1970-01-01 00:00:00" {
		t.Errorf("DDD's first row: %q %q %q", account, wrapper, period)
	}
}

func TestMigration0117DDLIsRerunnable(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := MaterializeGains(ctx, db, gainsTo, 1); err != nil {
		t.Fatal(err)
	}
	rerunMigrationDDL(t, db, ctx, "0117_gains_dashboard.sql")
	rerunMigrationDDL(t, db, ctx, "0118_lot_engine.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_gains`).Scan(&n); err != nil || n == 0 {
		t.Errorf("after replay: %d rows, err %v", n, err)
	}
}

// matchesBuckets checks report_gains, summed by month, against the
// gains report's monthly buckets in every currency and reading.
func matchesBuckets(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, ccy := range MaterializedCurrencies() {
		for _, missing := range []lots.MissingBasis{lots.MissingIgnore, lots.MissingZero} {
			want, err := GainsBuckets(ctx, db, 0, gainsTo, ccy, "month", GainsAll, missing)
			if err != nil {
				t.Fatal(err)
			}
			got, err := db.QueryContext(ctx, `
            SELECT period_start, SUM(realized_x), SUM(unrealized_start_x), SUM(unrealized_end_x),
                   SUM(unrealized_change_x), SUM(proceeds_x),
                   SUM(n_lots), SUM(n_sells), COUNT(*) FILTER (WHERE at_end)
              FROM report_gains WHERE currency = ? AND missing_basis = ? GROUP BY 1 ORDER BY 1`, ccy, string(missing))
			if err != nil {
				t.Fatal(err)
			}
			i := 0
			for got.Next() {
				var start, lots, sells, positions int64
				var sums [5]*float64
				if err := got.Scan(&start, &sums[0], &sums[1], &sums[2], &sums[3], &sums[4], &lots, &sells, &positions); err != nil {
					t.Fatal(err)
				}
				if i >= len(want) {
					t.Fatalf("%s: more months materialized than the report has", ccy)
				}
				w := want[i]
				ok := start == *w.PeriodStart && lots == w.RealizedLots && sells == w.Sells && positions == w.Positions
				for j, r := range []*string{w.Realized, w.UnrealizedStart, w.UnrealizedEnd, w.UnrealizedChange, w.Proceeds} {
					ok = ok && (sums[j] == nil) == (r == nil) && (r == nil || near(*sums[j], num(t, r)))
				}
				if !ok {
					t.Errorf("%s %s month %d: materialized %d %v %d %d %d, report %+v",
						ccy, missing, i, start, sums, lots, sells, positions, w)
				}
				i++
			}
			if err := got.Err(); err != nil {
				t.Fatal(err)
			}
			got.Close()
			if i != len(want) {
				t.Errorf("%s %s: %d months materialized, the report has %d", ccy, missing, i, len(want))
			}
		}
	}
}

// Where no cost basis is missing the zero reading is a copy of the
// ignore reading, and still the report's buckets.
func TestMaterializeGainsCopiesAReadingThatChangesNothing(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := db.ExecContext(ctx, `
        UPDATE positions SET book_value = 0 WHERE basis_missing(asset_class, vehicle, book_value);
        UPDATE realized_lots SET book_value = 0 WHERE gain_needs_cost(realized_gain_loss, proceeds, book_value);
        UPDATE lot_realized SET book_value = 0 WHERE gain_needs_cost(NULL, proceeds, book_value)`); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeGains(ctx, db, gainsTo, 42); err != nil {
		t.Fatal(err)
	}
	var ignore, zero int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE missing_basis = 'ignore'),
	        COUNT(*) FILTER (WHERE missing_basis = 'zero') FROM report_gains`).Scan(&ignore, &zero); err != nil {
		t.Fatal(err)
	}
	if ignore == 0 || zero != ignore {
		t.Fatalf("ignore rows %d, zero rows %d", ignore, zero)
	}
	matchesBuckets(t, ctx, db)
}

func TestMigration0119DDLIsRerunnable(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rerunMigrationDDL(t, db, ctx, "0119_gains_one_pass.sql")
	if _, err := MaterializeGains(ctx, db, gainsTo, 42); err != nil {
		t.Fatal(err)
	}
	matchesBuckets(t, ctx, db)
}
