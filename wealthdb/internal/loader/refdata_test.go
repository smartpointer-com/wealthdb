package loader_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/refdata"
)

// newSilver creates a silver SQLite from a schema file, named relative to
// this package, and returns its path and a handle for seeding it.
func newSilver(t *testing.T, schemaFile string) (string, *sql.DB) {
	t.Helper()
	schema, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	return silvertest.NewDB(t, string(schema))
}

// goldCount is the count a query yields on gold.
func goldCount(t *testing.T, g *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := g.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A refdata silver loads its reference records into gold, a later run
// replaces only the days it re-served, and a reset clears them.
func TestLoadRefdata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	silverPath, s := newSilver(t, "../silver/refdata/testdata/silver_schema.sql")
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_767_139_200, 1_769_817_600 // 2025-12-31, 2026-01-31
	)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, run1+5)
	for _, d := range []int64{d1, d2} {
		silvertest.Exec(t, s, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, weight_sum, run_at)
		    VALUES ('symbol:IDX1', ?, 'holdings', 'inbox', 'inbox', 2, '100', ?)`, d, run1)
		silvertest.Exec(t, s, `INSERT INTO lines (fund_ref, as_of, basis, line_no, ticker, weight) VALUES
		    ('symbol:IDX1', ?, 'holdings', 1, 'AAA', '70'), ('symbol:IDX1', ?, 'holdings', 2, 'BBB', '30')`, d, d)
	}
	silvertest.Exec(t, s, `INSERT INTO security_facts (security_key, field, as_of, provider, value, run_at)
	    VALUES ('symbol:AAA', 'sic', ?, 'sec-edgar', '3571', ?)`, d2, run1)

	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "ref", Kind: "refdata", Path: silverPath}
	res, err := ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.SnapshotsLoaded != 7 {
		t.Errorf("loaded %d reference rows, want 2 compositions + 4 lines + 1 fact", res.SnapshotsLoaded)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM fund_lines WHERE security_key = 'symbol:AAA' AND weight = 70`); n != 2 {
		t.Errorf("AAA lines = %d, want 2", n)
	}

	// Run 2 re-serves 2026-01-31 with one line; 2025-12-31 is untouched.
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 1, '/x/2', ?)`, run2, run2+5)
	silvertest.Exec(t, s, `DELETE FROM lines WHERE as_of = ?`, d2)
	silvertest.Exec(t, s, `UPDATE compositions SET line_count = 1, run_at = ? WHERE as_of = ?`, run2, d2)
	silvertest.Exec(t, s, `INSERT INTO lines (fund_ref, as_of, basis, line_no, ticker, weight) VALUES ('symbol:IDX1', ?, 'holdings', 1, 'CCC', '100')`, d2)
	res, err = ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if res.Window.Start != d2 || res.Window.End != d2 {
		t.Errorf("second window = %+v, want the re-served day alone", res.Window)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM fund_lines`); n != 3 {
		t.Errorf("lines after the re-serve = %d, want 2 + 1", n)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM security_facts`); n != 1 {
		t.Errorf("facts = %d, want the one inside the window re-emitted", n)
	}

	if err := ld.Reset(ctx, "ref"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, tbl := range gold.ReferenceTables {
		if n := goldCount(t, g, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s keeps %d rows after reset", tbl, n)
		}
	}
}

// The yield facts of a refdata silver at schema v2 load into their gold
// tables and the fund lines' columns, a later run that re-serves a day
// replaces that day's rows rather than adding to them, and a reset clears
// them.
func TestLoadRefdataYieldFacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	silverPath, s := newSilver(t, "../silver/refdata/testdata/silver_schema.sql")
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_767_139_200, 1_769_817_600 // 2025-12-31, 2026-01-31
	)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/x/1', ?)`, run1, run1+5)
	for _, d := range []int64{d1, d2} {
		silvertest.Exec(t, s, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
		    VALUES ('isin:XX0000000001', ?, 'holdings', 'ishares', 'daily_file', 1, ?)`, d, run1)
		silvertest.Exec(t, s, `INSERT INTO lines (fund_ref, as_of, basis, line_no, cusip, name, weight,
		        coupon_kind, is_default, in_arrears, paid_in_kind, exchange_rate, duration, modified_duration,
		        yield_to_maturity, yield_to_worst, yield_to_call, real_duration, real_yield, accrual_date)
		    VALUES ('isin:XX0000000001', ?, 'holdings', 1, '000000001', 'Example Note 2036', '100',
		        'Fixed', 0, 0, 0, '1', '6.25', '6.1', '4.35', '4.2', '4.1', '5.5', '1.85', '2025-09-15')`, d)
		silvertest.Exec(t, s, `INSERT INTO fund_risk (fund_ref, as_of, currency, tenor, dv01, dv100, credit_ig_dv01,
		        credit_hy_dv01, run_at)
		    VALUES ('isin:XX0000000001', ?, 'USD', '10Y', '125.5', '12400', NULL, NULL, ?),
		           ('isin:XX0000000001', ?, 'XXX', '10Y', NULL, NULL, '3.25', '1.5', ?)`, d, run1, d, run1)
		silvertest.Exec(t, s, `INSERT INTO characteristics (fund_ref, as_of, provider, field, label, value, run_at)
		    VALUES ('isin:XX0000000001', ?, 'ishares', 'effective_duration', '', '6.31', ?),
		           ('isin:XX0000000001', ?, 'ishares', 'credit_quality', 'AAA', '42.5', ?)`, d, run1, d, run1)
	}
	silvertest.Exec(t, s, `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value,
	        scheme, unit, fiscal_year, fiscal_period, form, filed, run_at)
	    VALUES ('symbol:EXMP', 'eps_diluted', ?, '2025-11-01', 'sec-edgar', '1.23',
	        'us-gaap:EarningsPerShareDiluted', 'USD/shares', 2026, 'Q1', '10-Q', '2026-03-10', ?)`, d2, run1)

	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "ref", Kind: "refdata", Path: silverPath}
	res, err := ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.SnapshotsLoaded != 2*(1+1+2+2)+1 {
		t.Errorf("loaded %d reference rows, want 13", res.SnapshotsLoaded)
	}
	// Each table's rows land with every column the silver filled.
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM fund_lines WHERE silver_source_id = 'ref' AND coupon_kind = 'Fixed'
		    AND NOT is_default AND NOT in_arrears AND NOT paid_in_kind AND exchange_rate = 1 AND duration = 6.25
		    AND modified_duration = 6.1 AND yield_to_maturity = 4.35 AND yield_to_worst = 4.2
		    AND yield_to_call = 4.1 AND real_duration = 5.5 AND real_yield = 1.85
		    AND accrual_date = DATE '2025-09-15'`: 2,
		`SELECT COUNT(*) FROM fund_risk WHERE silver_source_id = 'ref' AND currency = 'USD' AND tenor = '10Y'
		    AND dv01 = 125.5 AND dv100 = 12400 AND credit_ig_dv01 IS NULL AND payload IS NOT NULL`: 2,
		`SELECT COUNT(*) FROM fund_risk WHERE currency = 'XXX' AND credit_ig_dv01 = 3.25
		    AND credit_hy_dv01 = 1.5 AND dv01 IS NULL`: 2,
		`SELECT COUNT(*) FROM fund_characteristics WHERE silver_source_id = 'ref' AND provider = 'ishares'
		    AND ((field = 'effective_duration' AND label = '' AND value = 6.31)
		      OR (field = 'credit_quality' AND label = 'AAA' AND value = 42.5))`: 4,
		`SELECT COUNT(*) FROM security_period_facts WHERE silver_source_id = 'ref'
		    AND security_key = 'symbol:EXMP' AND field = 'eps_diluted' AND as_of = 1769817600
		    AND period_start = DATE '2025-11-01' AND provider = 'sec-edgar' AND value = 1.23
		    AND scheme = 'us-gaap:EarningsPerShareDiluted' AND unit = 'USD/shares' AND fiscal_year = 2026
		    AND fiscal_period = 'Q1' AND form = '10-Q' AND filed = DATE '2026-03-10'`: 1,
	} {
		if n := goldCount(t, g, q); n != want {
			t.Errorf("%s\n= %d, want %d", q, n, want)
		}
	}

	// Run 2 re-serves 2026-01-31 with a revised characteristic and period
	// fact; 2025-12-31 is untouched.
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/x/2', ?)`, run2, run2+5)
	for _, tbl := range []string{"compositions", "fund_risk", "characteristics", "period_facts"} {
		silvertest.Exec(t, s, `UPDATE `+tbl+` SET run_at = ? WHERE as_of = ?`, run2, d2)
	}
	silvertest.Exec(t, s, `UPDATE characteristics SET value = '6.4' WHERE as_of = ? AND field = 'effective_duration'`, d2)
	silvertest.Exec(t, s, `UPDATE period_facts SET value = '1.25', filed = '2026-03-20' WHERE as_of = ?`, d2)
	res, err = ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if res.Window.Start != d2 || res.Window.End != d2 {
		t.Errorf("second window = %+v, want the re-served day alone", res.Window)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM fund_lines`:            2,
		`SELECT COUNT(*) FROM fund_risk`:             4,
		`SELECT COUNT(*) FROM fund_characteristics`:  4,
		`SELECT COUNT(*) FROM security_period_facts`: 1,
		`SELECT COUNT(*) FROM fund_characteristics WHERE field = 'effective_duration' AND value = 6.4`:  1,
		`SELECT COUNT(*) FROM fund_characteristics WHERE field = 'effective_duration' AND value = 6.31`: 1,
		`SELECT COUNT(*) FROM security_period_facts WHERE value = 1.25 AND filed = DATE '2026-03-20'`:   1,
	} {
		if n := goldCount(t, g, q); n != want {
			t.Errorf("after the re-serve, %s = %d, want %d", q, n, want)
		}
	}

	if err := ld.Reset(ctx, "ref"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, tbl := range gold.ReferenceTables {
		if n := goldCount(t, g, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s keeps %d rows after reset", tbl, n)
		}
	}
}
