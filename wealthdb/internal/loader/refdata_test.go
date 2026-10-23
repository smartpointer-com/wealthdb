package loader_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// A refdata run that writes an old period and a later day reaches only
// those: the window bounds each kind on its own span, so the load neither
// deletes nor re-emits the compositions and other facts dated between
// the old period and the newest day. A kind the run does not write has
// no span, and the load leaves its tables alone: run 1's security and
// its fact stay as they are.
func TestLoadRefdataBoundsEachKindToItsSpan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	silverPath, s := newSilver(t, "../silver/refdata/testdata/silver_schema.sql")
	const (
		run1, run2   = 1_790_000_000, 1_790_100_000
		dOld, d1, d2 = 1_261_526_400, 1_767_139_200, 1_769_817_600 // 2009-12-23, 2025-12-31, 2026-01-31
	)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/x/1', ?)`, run1, run1+5)
	silvertest.Exec(t, s, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
	    VALUES ('isin:XX0000000001', ?, 'holdings', 'ishares', 'daily_file', 1, ?)`, d1, run1)
	silvertest.Exec(t, s, `INSERT INTO lines (fund_ref, as_of, basis, line_no, cusip, name, weight)
	    VALUES ('isin:XX0000000001', ?, 'holdings', 1, '000000001', 'Example Note 2036', '100')`, d1)
	silvertest.Exec(t, s, `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value, run_at)
	    VALUES ('symbol:EXMP', 'cash', ?, '2025-10-01', 'sec-edgar', '100', ?)`, d1, run1)
	silvertest.Exec(t, s, `INSERT INTO securities (security_key, as_of, provider, isin, ticker, name, run_at)
	    VALUES ('symbol:EXMP', ?, 'openfigi', 'XX0000000002', 'EXMP', 'Example Corp', ?)`, d1, run1)
	silvertest.Exec(t, s, `INSERT INTO security_facts (security_key, field, as_of, provider, value, scheme, run_at)
	    VALUES ('symbol:EXMP', 'sic', ?, 'sec-edgar', '3571', 'sic', ?)`, d1, run1)

	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "ref", Kind: "refdata", Path: silverPath}
	if _, err := ld.Load(ctx, spec); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Run 2 writes an old period fact and a later day's composition: one
	// window over both would delete and re-emit run 1's d1 composition and
	// its line and its d1 period fact too.
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/x/2', ?)`, run2, run2+5)
	silvertest.Exec(t, s, `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value, run_at)
	    VALUES ('symbol:EXMP', 'cash', ?, '2009-09-24', 'sec-edgar', '90', ?)`, dOld, run2)
	silvertest.Exec(t, s, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
	    VALUES ('isin:XX0000000001', ?, 'holdings', 'ishares', 'daily_file', 0, ?)`, d2, run2)
	res, err := ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if res.SnapshotsLoaded != 2 {
		t.Errorf("second load wrote %d reference rows, want the old period fact and the d2 composition", res.SnapshotsLoaded)
	}
	if span, ok := res.Window.SpanOf(canonical.RefFundDays); !ok || span != (canonical.Span{Start: d2, End: d2}) {
		t.Errorf("fund-days span = %+v (%v), want d2 alone", span, ok)
	}
	if span, ok := res.Window.SpanOf(canonical.RefPeriodFacts); !ok || span != (canonical.Span{Start: dOld, End: dOld}) {
		t.Errorf("period facts span = %+v (%v), want the old period alone", span, ok)
	}
	for _, kind := range []string{canonical.RefSecurities, canonical.RefSecurityFacts} {
		if res.Window.Changes(kind) {
			t.Errorf("%s changes in the second load", kind)
		}
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM fund_compositions`:                               2,
		`SELECT COUNT(*) FROM fund_compositions WHERE as_of = ` + itoa(d1):     1,
		`SELECT COUNT(*) FROM fund_lines WHERE as_of = ` + itoa(d1):            1,
		`SELECT COUNT(*) FROM security_period_facts`:                           2,
		`SELECT COUNT(*) FROM security_period_facts WHERE as_of = ` + itoa(d1): 1,
		`SELECT COUNT(*) FROM security_identities`:                             1,
		`SELECT COUNT(*) FROM security_facts`:                                  1,
	} {
		if n := goldCount(t, g, q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
}

// itoa formats an instant for a query.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A refdata silver's distributions load into dividend_calendar. A later
// run that states one again, on a later day, replaces its row: the load's
// window spans the later day alone, and the row moves to it. A reset
// clears them.
func TestLoadRefdataDividendCalendar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	silverPath, s := newSilver(t, "../silver/refdata/testdata/silver_schema.sql")
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_767_139_200, 1_769_817_600 // 2025-12-31, 2026-01-31
	)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 3, '/x/1', ?)`, run1, run1+5)
	silvertest.Exec(t, s, `INSERT INTO dividend_events (ref, provider, event_id, as_of, kind, ex_date, record_date,
	        pay_date, amount, currency, status, run_at) VALUES
	    ('isin:XX0000000001', 'ishares', 'e1', ?1, 'dividend', '2025-12-15', '2025-12-15', '2025-12-19', '0.40', 'USD',
	     'final', ?2),
	    ('isin:XX0000000001', 'ishares', 'e2', ?1, 'dividend', '2026-03-16', '2026-03-16', '2026-03-20', '0.41', 'USD',
	     'estimated', ?2)`, d1, run1)
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
	if res.SnapshotsLoaded != 2 {
		t.Errorf("loaded %d reference rows, want the two distributions", res.SnapshotsLoaded)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM dividend_calendar WHERE silver_source_id = 'ref'
	    AND ref = 'isin:XX0000000001' AND provider = 'ishares' AND event_id = 'e1' AND as_of = 1767139200
	    AND kind = 'dividend' AND ex_day = DATE '2025-12-15' AND record_day = DATE '2025-12-15'
	    AND pay_day = DATE '2025-12-19' AND declared_day IS NULL AND amount = 0.40 AND currency = 'USD'
	    AND status = 'final'`); n != 1 {
		t.Errorf("the first distribution's row = %d, want 1", n)
	}

	// Run 2 states the estimated distribution again, final, and a new one.
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 3, '/x/2', ?)`, run2, run2+5)
	silvertest.Exec(t, s, `UPDATE dividend_events SET as_of = ?, amount = '0.42', status = 'final', run_at = ?
	    WHERE event_id = 'e2'`, d2, run2)
	silvertest.Exec(t, s, `INSERT INTO dividend_events (ref, provider, event_id, as_of, kind, ex_date, run_at)
	    VALUES ('symbol:EXMP', 'six', 'e3', ?, 'dividend', '2026-01-20', ?)`, d2, run2)
	res, err = ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if span, ok := res.Window.SpanOf(canonical.RefDividendEvents); !ok || span != (canonical.Span{Start: d2, End: d2}) {
		t.Errorf("dividend events span = %+v (%v), want d2 alone", span, ok)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM dividend_calendar`:                                              3,
		`SELECT COUNT(*) FROM dividend_calendar WHERE event_id = 'e1' AND as_of = 1767139200`: 1,
		`SELECT COUNT(*) FROM dividend_calendar WHERE event_id = 'e2' AND as_of = 1769817600 AND amount = 0.42
		    AND status = 'final'`: 1,
		`SELECT COUNT(*) FROM dividend_calendar WHERE event_id = 'e3' AND record_day IS NULL`: 1,
	} {
		if n := goldCount(t, g, q); n != want {
			t.Errorf("after the restatement, %s = %d, want %d", q, n, want)
		}
	}

	if err := ld.Reset(ctx, "ref"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if n := goldCount(t, g, `SELECT COUNT(*) FROM dividend_calendar`); n != 0 {
		t.Errorf("dividend_calendar keeps %d rows after reset", n)
	}
}

// A provider that states a ref's whole history drops from silver the row
// of a distribution whose days it corrected. Gold mirrors the drop: the
// load reads every row of the ref and provider its window names and
// replaces gold's, so the stale row leaves though it was stated on a day
// outside the window. Another ref keeps its rows.
func TestLoadRefdataDividendDrop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	silverPath, s := newSilver(t, "../silver/refdata/testdata/silver_schema.sql")
	const (
		run1, run2 = 1_790_000_000, 1_790_100_000
		d1, d2     = 1_780_272_000, 1_780_876_800 // 2026-06-01, 2026-06-08
	)
	stale, corrected := "dividend|2026-06-15|2026-06-15|2026-06-19", "dividend|2026-06-15|2026-06-15|2026-06-22"
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 3, '/x/1', ?)`, run1, run1+5)
	silvertest.Exec(t, s, `INSERT INTO dividend_events (ref, provider, event_id, as_of, kind, ex_date, record_date,
	        pay_date, run_at) VALUES
	    ('isin:XX0000000001', 'ishares', ?1, ?2, 'dividend', '2026-06-15', '2026-06-15', '2026-06-19', ?3),
	    ('isin:XX0000000002', 'ishares', ?1, ?2, 'dividend', '2026-06-15', '2026-06-15', '2026-06-19', ?3)`,
		stale, d1, run1)
	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ld := loader.New(g)
	spec := loader.SourceSpec{ID: "ref", Kind: "refdata", Path: silverPath}
	if _, err := ld.Load(ctx, spec); err != nil {
		t.Fatalf("Load: %v", err)
	}

	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 3, '/x/2', ?)`, run2, run2+5)
	silvertest.Exec(t, s, `DELETE FROM dividend_events WHERE ref = 'isin:XX0000000001'`)
	silvertest.Exec(t, s, `INSERT INTO dividend_events (ref, provider, event_id, as_of, kind, ex_date, record_date,
	        pay_date, run_at) VALUES ('isin:XX0000000001', 'ishares', ?, ?, 'dividend', '2026-06-15', '2026-06-15',
	        '2026-06-22', ?)`, corrected, d2, run2)
	res, err := ld.Load(ctx, spec)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if span, ok := res.Window.SpanOf(canonical.RefDividendEvents); !ok || span != (canonical.Span{Start: d2, End: d2}) {
		t.Errorf("dividend events span = %+v (%v), want d2 alone", span, ok)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM dividend_calendar WHERE ref = 'isin:XX0000000001'`: 1,
		`SELECT COUNT(*) FROM dividend_calendar WHERE ref = 'isin:XX0000000001' AND event_id = '` + corrected +
			`' AND pay_day = DATE '2026-06-22' AND as_of = ` + itoa(d2): 1,
		`SELECT COUNT(*) FROM dividend_calendar WHERE ref = 'isin:XX0000000002' AND event_id = '` + stale + `'`: 1,
	} {
		if n := goldCount(t, g, q); n != want {
			t.Errorf("after the correction, %s = %d, want %d", q, n, want)
		}
	}
}
