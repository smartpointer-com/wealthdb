package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
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

// web_gains labels the account and carries its tax wrapper, an account
// that states none reading taxable_personal, and its period is the
// month's start, the first bucket's too, which opens at the first
// snapshot.
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
	var unwrapped string
	if err := db.QueryRowContext(ctx, `
        SELECT string_agg(DISTINCT tax_wrapper, ',') FROM web_gains WHERE account_external_id = 'BRK2'`).Scan(&unwrapped); err != nil {
		t.Fatal(err)
	}
	if unwrapped != "taxable_personal" {
		t.Errorf("an account with no wrapper reads %q", unwrapped)
	}
}

// web_gains reads a row with no asset class as `unknown` (0170): BBB, a
// sale within a month no snapshot shows it at the start or the end of,
// and ZZZ, sold with no snapshot at all, leave report_gains' class blank;
// every other row, DDD's among them, keeps its class. So the gains of
// every class picked add up to the whole, realized and unrealized, as
// the Gains dashboard's Asset class picker sums them: a picked class is
// never blank. Replaying 0170 changes nothing.
func TestWebGainsReadsAMissingClassAsUnknown(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := MaterializeGains(ctx, db, gainsTo, 1); err != nil {
		t.Fatal(err)
	}
	blank := scanStrings(t, db, `SELECT DISTINCT k FROM report_gains WHERE asset_class IS NULL ORDER BY 1`)
	if strings.Join(blank, ",") != "BBB,ZZZ" {
		t.Fatalf("report_gains leaves %v unclassed, want BBB and ZZZ", blank)
	}
	check := func() {
		t.Helper()
		var nulls, unknown, dddOther int
		var whole, picked, wholeU, pickedU float64
		if err := db.QueryRowContext(ctx, `
            SELECT count(*) FILTER (WHERE asset_class IS NULL),
                   count(*) FILTER (WHERE asset_class = 'unknown' AND instrument_key IN ('BBB', 'ZZZ')),
                   count(*) FILTER (WHERE instrument_key = 'DDD' AND asset_class IS DISTINCT FROM 'public_equity'),
                   coalesce(sum(realized), 0), coalesce(sum(realized) FILTER (WHERE asset_class IS NOT NULL), 0),
                   coalesce(sum(unrealized_change), 0),
                   coalesce(sum(unrealized_change) FILTER (WHERE asset_class IS NOT NULL), 0)
              FROM web_gains WHERE currency = 'USD' AND missing_basis = 'ignore'`).
			Scan(&nulls, &unknown, &dddOther, &whole, &picked, &wholeU, &pickedU); err != nil {
			t.Fatal(err)
		}
		if nulls != 0 || unknown == 0 || dddOther != 0 || whole == 0 ||
			math.Abs(whole-picked) > 1e-9 || math.Abs(wholeU-pickedU) > 1e-9 {
			t.Errorf("web_gains: %d blank, %d unknown, %d DDD rows not public_equity; realized %v picked %v; "+
				"unrealized %v picked %v", nulls, unknown, dddOther, whole, picked, wholeU, pickedU)
		}
	}
	check()
	rerunMigrationDDL(t, db, ctx, "0170_gains_unknown_class.sql")
	check()
}

func TestMigration0117DDLIsRerunnable(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := MaterializeGains(ctx, db, gainsTo, 1); err != nil {
		t.Fatal(err)
	}
	rerunMigrationDDL(t, db, ctx, "0117_gains_dashboard.sql")
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

func TestMigrations0119To0157DDLAreRerunnable(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rerunMigrationDDL(t, db, ctx, "0119_gains_one_pass.sql")
	rerunMigrationDDL(t, db, ctx, "0120_gains_held_sold.sql")
	rerunMigrationDDL(t, db, ctx, "0121_private_exits.sql")
	rerunMigrationDDL(t, db, ctx, "0133_deterministic_labels.sql")
	rerunMigrationDDL(t, db, ctx, "0157_gains_start_rate.sql")
	if _, err := MaterializeGains(ctx, db, gainsTo, 42); err != nil {
		t.Fatal(err)
	}
	matchesBuckets(t, ctx, db)
}

// The held and sold split of February, on an account of its own: a
// January snapshot, a February one, and sales between them, every lot
// bought in 1969.
//
//	GGG  5 @ 500, cost 400 → gone; 5 sold for 550: the start gain of 100
//	     leaves the held change, and the sale gained 50
//	EEE  4 @ 400, cost 200 → 2 @ 260, cost 100; 2 sold for 300, cost
//	     100, on a document that names it by its CUSIP: the 2 kept gain
//	     60, the 2 sold gain 100 over their start value of 200
//	FFF  1 @ 100, cost 50 → gone; 3 sold for 360, cost 150: only the
//	     unit held at the start releases its gain of 50
//	HHH  valued by amount, no quantity: 500, cost 100 → gone; sold for
//	     700 against its whole basis: the sale gained 200 over its start
//	     value, and the start gain of 400 leaves the held change
//
// The fixture's own sales fall in the first bucket, which has no start
// to release from: BBB's whole gain of 150 is sold gain there. Every
// row keeps held + sold = realized + unrealized change.
func TestMaterializedGainSplitsHeldAndSold(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name,
                              first_seen_at, last_seen_at) VALUES
            ('test-src', 'BRK3', 'brokerage', 'Third', 1, 1);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, cusip, name,
                                 currency, first_seen_at, last_seen_at) VALUES
            ('test-src', 'EEE',       'public_equity', 'EEE', 'C0000000X', 'Epsilon', 'USD', 1, 1),
            ('test-src', 'C0000000X', 'public_equity', 'EEE', 'C0000000X', 'Epsilon', 'USD', 1, 1),
            ('test-src', 'FFF',       'public_equity', 'FFF', NULL,        'Phi',     'USD', 1, 1),
            ('test-src', 'GGG',       'public_equity', 'GGG', NULL,        'Eta',     'USD', 1, 1),
            ('test-src', 'HHH',       'private_equity', 'HHH', NULL,       'Theta',   'USD', 1, 1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity,
                               market_value, book_value, basis_origin, basis_method, basis_fees) VALUES
            ('test-src', 200000,  'BRK3', 'EEE', 'EEE', 'public_equity', 'stock', 'USD', 4, 400, 200, 'stated', 'lots', 'included'),
            ('test-src', 200000,  'BRK3', 'FFF', 'FFF', 'public_equity', 'stock', 'USD', 1, 100, 50,  'stated', 'lots', 'included'),
            ('test-src', 200000,  'BRK3', 'GGG', 'GGG', 'public_equity', 'stock', 'USD', 5, 500, 400, 'stated', 'lots', 'included'),
            ('test-src', 200000,  'BRK3', 'HHH', 'HHH', 'private_equity', 'stock', 'USD', NULL, 500, 100, 'stated', 'paid_in', 'included'),
            ('test-src', 3974400, 'BRK3', 'EEE', 'EEE', 'public_equity', 'stock', 'USD', 2, 260, 100, 'stated', 'lots', 'included');
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
                                   instrument_external_id, document_kind, tax_year, acquisition_date,
                                   acquired_various, disposal_date, currency, quantity, proceeds,
                                   book_value, term, is_primary) VALUES
            ('test-src', 'R-EEE', 'BRK3', 'C0000000X', 'form_1099b', 1970, DATE '1969-06-01', FALSE,
             DATE '1970-02-10', 'USD', 2, 300, 100, 'short', TRUE),
            ('test-src', 'R-FFF', 'BRK3', 'FFF',       'form_1099b', 1970, DATE '1969-06-01', FALSE,
             DATE '1970-02-10', 'USD', 3, 360, 150, 'short', TRUE),
            ('test-src', 'R-GGG', 'BRK3', 'GGG',       'form_1099b', 1970, DATE '1969-06-01', FALSE,
             DATE '1970-02-10', 'USD', 5, 550, 400, 'short', TRUE),
            ('test-src', 'R-HHH', 'BRK3', 'HHH',       'engine',     1970, DATE '1969-06-01', FALSE,
             DATE '1970-02-10', 'USD', 1, 700, 100, 'short', TRUE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeGains(ctx, db, gainsTo, 1); err != nil {
		t.Fatal(err)
	}
	var off int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM report_gains
         WHERE abs(COALESCE(held_change_x, 0) + COALESCE(sold_gain_x, 0)
                   - COALESCE(realized_x, 0) - COALESCE(unrealized_change_x, 0)) > 1e-9`).Scan(&off); err != nil || off != 0 {
		t.Errorf("%d rows break held + sold = realized + unrealized change (err %v)", off, err)
	}
	for _, c := range []struct {
		ccy, month, key string
		held, sold      float64
	}{
		{"USD", "1970-02-01", "GGG", 0, 50},
		{"USD", "1970-02-01", "EEE", 60, 100},
		{"USD", "1970-02-01", "FFF", 0, 160},
		{"USD", "1970-02-01", "HHH", 0, 200},
		{"CHF", "1970-02-01", "GGG", 0, 25}, // at 2 USD a franc
		{"USD", "1970-01-01", "BBB", math.NaN(), 150},
	} {
		var held, sold *float64
		if err := db.QueryRowContext(ctx, `
            SELECT SUM(g.held_change), SUM(g.sold_gain) FROM web_gains g
              LEFT JOIN instruments i ON i.silver_source_id = g.silver_source_id AND i.instrument_external_id = g.instrument_key
             WHERE g.currency = ? AND g.period = CAST(? AS TIMESTAMP) AND g.missing_basis = 'ignore'
               AND COALESCE(i.symbol, g.instrument_key) = ?`,
			c.ccy, c.month, c.key).Scan(&held, &sold); err != nil {
			t.Fatal(err)
		}
		heldOK := held == nil && math.IsNaN(c.held) || held != nil && near(*held, c.held)
		if !heldOK || sold == nil || !near(*sold, c.sold) {
			t.Errorf("%s %s %s: held %s sold %s, want %v %v", c.ccy, c.month, c.key, floatText(held), floatText(sold), c.held, c.sold)
		}
	}
}

// A source holds GGG in two accounts whose latest snapshots before
// February fall on days with different rates: BRK3's on 3 January at 2
// dollars a franc, BRK4's on 24 January at 4. BRK4 sells its 5 in
// February, 150 dollars over their cost. The start price converts at
// the newest observation's rate, BRK4's own, so BRK4's start gain of
// 100 dollars releases as 25 francs: its held change is 0, and the sale
// gained 12.5 francs (37.5 at the sale's rate, less the 25). BRK3 keeps
// its 5, and the rate alone moves its gain from 50 francs to 25.
func TestMaterializedSplitTakesTheNewestStartRate(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('test-src', 1728000, 'CHF', 'USD', 0.25),
            ('test-src', 1728000, 'USD', 'CHF', 4.0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name,
                              first_seen_at, last_seen_at) VALUES
            ('test-src', 'BRK3', 'brokerage', 'Third', 1, 1),
            ('test-src', 'BRK4', 'brokerage', 'Fourth', 1, 1);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, name,
                                 currency, first_seen_at, last_seen_at) VALUES
            ('test-src', 'GGG', 'public_equity', 'GGG', 'Eta', 'USD', 1, 1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity,
                               market_value, book_value, basis_origin, basis_method, basis_fees) VALUES
            ('test-src', 200000,  'BRK3', 'GGG', 'GGG', 'public_equity', 'stock', 'USD', 5, 500, 400, 'stated', 'lots', 'included'),
            ('test-src', 2000000, 'BRK4', 'GGG', 'GGG', 'public_equity', 'stock', 'USD', 5, 500, 400, 'stated', 'lots', 'included'),
            ('test-src', 3974400, 'BRK3', 'GGG', 'GGG', 'public_equity', 'stock', 'USD', 5, 500, 400, 'stated', 'lots', 'included'),
            ('test-src', 3974400, 'BRK4', 'DDD', 'DDD', 'public_equity', 'stock', 'USD', 1, 100, 100, 'stated', 'lots', 'included');
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
                                   instrument_external_id, document_kind, tax_year, acquisition_date,
                                   acquired_various, disposal_date, currency, quantity, proceeds,
                                   book_value, term, is_primary) VALUES
            ('test-src', 'R-GGG', 'BRK4', 'GGG', 'form_1099b', 1970, DATE '1969-06-01', FALSE,
             DATE '1970-02-10', 'USD', 5, 550, 400, 'short', TRUE)`); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if _, err := MaterializeGains(ctx, db, gainsTo, int64(run)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			ccy, acct  string
			held, sold float64
		}{
			{"CHF", "BRK4", 0, 12.5},
			{"CHF", "BRK3", -25, math.NaN()},
			{"USD", "BRK4", 0, 50},
		} {
			var held, sold *float64
			if err := db.QueryRowContext(ctx, `
                SELECT held_change, sold_gain FROM web_gains
                 WHERE currency = ? AND period = TIMESTAMP '1970-02-01' AND missing_basis = 'ignore'
                   AND account_external_id = ? AND instrument_key = 'GGG'`,
				c.ccy, c.acct).Scan(&held, &sold); err != nil {
				t.Fatalf("%s %s: %v", c.ccy, c.acct, err)
			}
			soldOK := sold == nil && math.IsNaN(c.sold) || sold != nil && near(*sold, c.sold)
			if held == nil || !near(*held, c.held) || !soldOK {
				t.Errorf("run %d, %s %s: held %s sold %s, want %v %v",
					run, c.ccy, c.acct, floatText(held), floatText(sold), c.held, c.sold)
			}
		}
	}
}

func floatText(f *float64) string {
	if f == nil {
		return "NULL"
	}
	return fmt.Sprint(*f)
}
