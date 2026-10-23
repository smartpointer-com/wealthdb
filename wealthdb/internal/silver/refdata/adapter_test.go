package refdata

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// testdata/silver_schema.sql is collectors/refdata/migrations/0001_initial.sql
// and 0002_yield_facts.sql, one after the other; the collector's tests
// check that it is the migrated schema.
//
//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// v2Header opens 0002's part of the schema.
const v2Header = "-- refdata silver schema v2"

const (
	run1  = int64(1_790_000_000) // fetch runs
	run2  = int64(1_790_100_000)
	load1 = int64(1_790_000_500) // the loads that wrote them
	load2 = int64(1_790_100_500)
	d1    = int64(1_767_139_200) // 2025-12-31
	d2    = int64(1_769_817_600) // 2026-01-31
	d3    = int64(1_772_236_800) // 2026-02-28
	d0    = int64(1_609_372_800) // 2020-12-31, a backfilled day
)

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

// newV1Silver is a silver its collector last loaded before migration
// 0002: the schema's part before v2Header.
func newV1Silver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	v1, _, found := strings.Cut(silverSchemaSQL, v2Header)
	if !found {
		t.Fatalf("testdata/silver_schema.sql has no %q", v2Header)
	}
	return silvertest.NewDB(t, v1)
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{}, path)
}

// seed writes two runs of an invented fund: run1 serves 2025-12-31 and
// 2026-01-31, run2 serves 2026-02-28 and a backfilled 2020-12-31, plus a
// security looked up in run2.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?), (?, 1, '/x/2', ?)`, run1, load1, run2, load2)
	for _, c := range []struct {
		asOf, run int64
	}{{d1, run1}, {d2, run1}, {d3, run2}, {d0, run2}} {
		silvertest.Exec(t, db, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, fund_name,
		    currency, net_assets, line_count, weight_sum, run_at)
		    VALUES ('isin:XX0000000001', ?, 'holdings', 'inbox', 'inbox', 'Example Index Fund', 'USD', '1000', 2, '99.5', ?)`,
			c.asOf, c.run)
		silvertest.Exec(t, db, `INSERT INTO lines (fund_ref, as_of, basis, line_no, isin, name, asset_type, weight, currency, country, sector)
		    VALUES ('isin:XX0000000001', ?, 'holdings', 1, 'xx0000000002', 'Example Corp', 'Equity', '60.5', 'USD', 'United States', 'Information Technology'),
		           ('isin:XX0000000001', ?, 'holdings', 2, NULL, 'USD CASH', 'Cash', '39', 'USD', NULL, NULL)`,
			c.asOf, c.asOf)
	}
	silvertest.Exec(t, db, `INSERT INTO attributes (fund_ref, as_of, domicile, ucits, hedged_currency, run_at)
	    VALUES ('isin:XX0000000001', ?, 'IE', 1, 'CHF', ?)`, d3, run2)
	silvertest.Exec(t, db, `INSERT INTO counterparties (fund_ref, as_of, role, name, share, run_at)
	    VALUES ('isin:XX0000000001', ?, 'swap', 'Example Bank', '12.5', ?)`, d3, run2)
	silvertest.Exec(t, db, `INSERT INTO securities (security_key, as_of, provider, isin, figi, ticker, name, run_at)
	    VALUES ('symbol:EXMP', ?, 'openfigi', 'XX0000000002', 'BBG000000002', 'EXMP', 'Example Corp', ?)`, d3, run2)
	silvertest.Exec(t, db, `INSERT INTO security_facts (security_key, field, as_of, provider, value, scheme, run_at)
	    VALUES ('symbol:EXMP', 'sic', ?, 'sec-edgar', '3571', 'sic', ?)`, d3, run2)
}

func TestKindIsRefdata(t *testing.T) {
	t.Parallel()
	if got := (&Adapter{}).Kind(); got != "refdata" {
		t.Fatalf("Kind() = %q", got)
	}
}

func TestStatusEmpty(t *testing.T) {
	t.Parallel()
	path, _ := newFixtureSilver(t)
	st, err := openAdapter(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.LatestChangeNumber != -1 || st.OldestSnapshotAt != -1 || st.ObservedSnapshotsThrough != -1 {
		t.Errorf("empty silver status = %+v, want the -1 sentinels", st)
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	path, db := newFixtureSilver(t)
	seed(t, db)
	st, err := openAdapter(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != d0 || st.LatestSnapshotAt != d3 {
		t.Errorf("snapshot range = [%d,%d], want [%d,%d]", st.OldestSnapshotAt, st.LatestSnapshotAt, d0, d3)
	}
	if st.LatestChangeNumber != load2 || st.ObservedSnapshotsThrough != run2 || st.ObservedTransactionsThrough != run2 {
		t.Errorf("status = %+v, want the change number at the latest load and observed at the latest fetch", st)
	}
	if st.OldestTransactionAt != -1 || st.LatestTransactionAt != -1 {
		t.Errorf("transactions should be the -1 sentinel, got %+v", st)
	}
}

// The first load spans every as-of; a later one spans only the days the
// runs loaded since wrote, and re-emits every row inside that span.
func TestChangeWindowSpansTheNewRunsDays(t *testing.T) {
	t.Parallel()
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != d0 || w.End != d3 || w.NewChangeNumber != load2 {
		t.Fatalf("first window = %+v", w)
	}

	w, err = conn.ChangeWindow(context.Background(), load1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != d0 || w.End != d3 {
		t.Fatalf("window since load1 = %+v, want [d0, d3]", w)
	}
	// Each kind of record spans the days run2 wrote of it.
	if want := map[string]canonical.Span{canonical.RefFundDays: {Start: d0, End: d3},
		canonical.RefSecurities: {Start: d3, End: d3}, canonical.RefSecurityFacts: {Start: d3, End: d3}}; !maps.Equal(w.Spans, want) {
		t.Errorf("spans since load1 = %+v, want %+v", w.Spans, want)
	}
	got := silvertest.Merged(t, conn, w)
	// d0..d3 holds all four fund-days, d1 and d2 from run1 among them.
	if len(got.FundCompositions) != 4 || len(got.FundLines) != 8 {
		t.Errorf("compositions = %d, lines = %d; want 4 and 8", len(got.FundCompositions), len(got.FundLines))
	}

	w, err = conn.ChangeWindow(context.Background(), load2)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasChanges || w.NewChangeNumber != load2 {
		t.Errorf("idle reload window = %+v, want no changes", w)
	}
}

// A run fetched before the watermark's run but loaded after it, such as
// an import from another machine, still reaches gold.
func TestChangeWindowTakesALateLoadedRun(t *testing.T) {
	t.Parallel()
	path, db := newFixtureSilver(t)
	seed(t, db)
	early, dLate := run1-50_000, d1-86400
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/0', ?)`, early, load2+100)
	silvertest.Exec(t, db, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
	    VALUES ('symbol:OTHER', ?, 'holdings', 'inbox', 'inbox', 0, ?)`, dLate, early)
	w, err := openAdapter(t, path).ChangeWindow(context.Background(), load2)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != dLate || w.End != dLate || w.NewChangeNumber != load2+100 {
		t.Errorf("window = %+v, want the late run's day", w)
	}
}

// A run that wrote nothing advances the watermark without a window.
func TestChangeWindowEmptyRun(t *testing.T) {
	t.Parallel()
	path, db := newFixtureSilver(t)
	seed(t, db)
	load3 := load2 + 1000
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/3', ?)`, run2+1000, load3)
	w, err := openAdapter(t, path).ChangeWindow(context.Background(), load2)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasChanges || w.NewChangeNumber != load3 {
		t.Errorf("window = %+v, want no changes and the watermark at load3", w)
	}
}

func TestSnapshotsMapRecords(t *testing.T) {
	t.Parallel()
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)
	got := silvertest.Merged(t, conn, canonical.Window{Start: d3, End: d3, HasChanges: true})

	if len(got.FundCompositions) != 1 {
		t.Fatalf("compositions = %d", len(got.FundCompositions))
	}
	c := got.FundCompositions[0]
	if c.FundRef != "isin:XX0000000001" || c.AsOf != d3 || c.Basis != canonical.CompositionHoldings ||
		c.LineCount != 2 || c.WeightSum == nil || c.WeightSum.String() != "99.5" || c.FetchedAt != run2 {
		t.Errorf("composition = %+v", c)
	}
	if len(got.FundLines) != 2 {
		t.Fatalf("lines = %d", len(got.FundLines))
	}
	l := got.FundLines[0]
	// The line's key comes from its ISIN, normalised.
	if l.SecurityKey != "isin:XX0000000002" || l.Weight == nil || l.Weight.String() != "60.5" ||
		l.Sector == nil || *l.Sector != "Information Technology" {
		t.Errorf("line = %+v", l)
	}
	// A line without an identifier keys by its name.
	if k := got.FundLines[1].SecurityKey; len(k) < 5 || k[:5] != "name:" {
		t.Errorf("cash line key = %q, want a name key", k)
	}
	if len(got.FundAttributes) != 1 || got.FundAttributes[0].UCITS == nil || !*got.FundAttributes[0].UCITS ||
		got.FundAttributes[0].HedgedCurrency == nil || *got.FundAttributes[0].HedgedCurrency != "CHF" ||
		got.FundAttributes[0].Lending != nil {
		t.Errorf("attributes = %+v", got.FundAttributes)
	}
	if len(got.FundCounterparties) != 1 || got.FundCounterparties[0].Role != "swap" {
		t.Errorf("counterparties = %+v", got.FundCounterparties)
	}
	if len(got.Securities) != 1 || got.Securities[0].FIGI == nil || *got.Securities[0].FIGI != "BBG000000002" {
		t.Errorf("securities = %+v", got.Securities)
	}
	if len(got.SecurityFacts) != 1 || got.SecurityFacts[0].Value != "3571" {
		t.Errorf("facts = %+v", got.SecurityFacts)
	}
}

// lowerBudgets shrinks the reference stream's batch budgets for one
// test, so a few rows span several batches. The budgets are package
// state, so a test that lowers them does not run in parallel.
func lowerBudgets(t *testing.T, records, tail int) {
	t.Helper()
	r, s := silver.ReferenceRecordBudget, silver.ReferenceTailBudget
	silver.ReferenceRecordBudget, silver.ReferenceTailBudget = records, tail
	t.Cleanup(func() { silver.ReferenceRecordBudget, silver.ReferenceTailBudget = r, s })
}

// A window wider than one batch's record budget splits between fund-days
// and loses none.
func TestSnapshotsBatchLargeWindows(t *testing.T) {
	lowerBudgets(t, 20, 5)
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, load1)
	const days, perDay = 3, 12
	for d := int64(0); d < days; d++ {
		asOf := d1 + d*86400
		silvertest.Exec(t, db, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
		    VALUES ('symbol:BIG', ?, 'holdings', 'inbox', 'inbox', ?, ?)`, asOf, perDay, run1)
		for i := 0; i < perDay; i++ {
			silvertest.Exec(t, db, `INSERT INTO lines (fund_ref, as_of, basis, line_no, ticker, weight)
			    VALUES ('symbol:BIG', ?, 'holdings', ?, 'T', '0.001')`, asOf, i)
		}
	}
	stream, err := openAdapter(t, path).Snapshots(context.Background(),
		canonical.Window{Start: d1, End: d1 + 10*86400, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var batches []int
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(b.FundLines) > 0 {
			batches = append(batches, len(b.FundLines))
		}
		if !more {
			break
		}
	}
	// Two fund-days of a composition and its lines each fill the first
	// batch past its budget; the third rides in the second.
	if len(batches) != 2 || batches[0] != 2*perDay || batches[1] != perDay {
		t.Errorf("lines per batch = %v, want [%d %d]", batches, 2*perDay, perDay)
	}
}

// Fund-days without a line count against the budget too: a run of days
// that state only characteristics splits like one of compositions.
func TestSnapshotsBatchLinelessFundDays(t *testing.T) {
	lowerBudgets(t, 20, 5)
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, load1)
	const days = 30
	for d := int64(0); d < days; d++ {
		silvertest.Exec(t, db, `INSERT INTO characteristics (fund_ref, as_of, provider, field, value, run_at)
		    VALUES ('symbol:BND1', ?, 'ishares', 'sec_yield', '4.4', ?)`, d1+d*86400, run1)
	}
	var batches []int
	for _, b := range silvertest.Snapshots(t, openAdapter(t, path),
		canonical.Window{Start: d1, End: d1 + days*86400, HasChanges: true}) {
		if len(b.FundCharacteristics) > 0 {
			batches = append(batches, len(b.FundCharacteristics))
		}
	}
	if !slices.Equal(batches, []int{20, 10}) {
		t.Errorf("characteristics per batch = %v, want [20 10]", batches)
	}
}

func TestTransactionsEmpty(t *testing.T) {
	t.Parallel()
	path, _ := newFixtureSilver(t)
	stream, err := openAdapter(t, path).Transactions(context.Background(), canonical.Window{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	b, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Transactions) != 0 {
		t.Errorf("refdata emits no transactions, got %d", len(b.Transactions))
	}
}

// Security identities and facts come a page at a time, and the keyset
// cursor neither skips nor repeats a row: the identities share keys and
// days, so a page can end inside a key.
func TestSnapshotsPageSecurities(t *testing.T) {
	lowerBudgets(t, 20, 5)
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, load1)
	for _, key := range []string{"symbol:AAA", "symbol:BBB", "symbol:CCC"} {
		for _, asOf := range []int64{d1, d2} {
			for _, provider := range []string{"openfigi", "sec-edgar"} {
				silvertest.Exec(t, db, `INSERT INTO securities (security_key, as_of, provider, run_at) VALUES (?, ?, ?, ?)`,
					key, asOf, provider, run1)
			}
		}
	}
	const n = 12
	for i := 0; i < n; i++ {
		silvertest.Exec(t, db, `INSERT INTO security_facts (security_key, field, as_of, provider, value, run_at)
		    VALUES (?, 'sic', ?, 'sec-edgar', '3571', ?)`, fmt.Sprintf("symbol:S%02d", i), d1, run1)
	}
	// Period facts share a key, a field and a period end, and differ by
	// period start, the fourth of five key columns.
	for i := 0; i < n; i++ {
		silvertest.Exec(t, db, `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value, run_at)
		    VALUES ('symbol:AAA', 'net_income', ?, ?, 'sec-edgar', '1', ?)`, d2, fmt.Sprintf("2025-%02d-01", i+1), run1)
	}
	stream, err := openAdapter(t, path).Snapshots(context.Background(), canonical.Window{Start: d1, End: d2, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	secs, facts, periods := map[string]int{}, map[string]int{}, map[string]int{}
	secPages, factPages, periodPages := 0, 0, 0
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Securities) > 0 {
			secPages++
		}
		if len(b.SecurityFacts) > 0 {
			factPages++
		}
		if len(b.SecurityPeriodFacts) > 0 {
			periodPages++
		}
		for _, s := range b.Securities {
			secs[fmt.Sprintf("%s|%d|%s", s.SecurityKey, s.AsOf, s.Provider)]++
		}
		for _, f := range b.SecurityFacts {
			facts[f.SecurityKey]++
		}
		for _, f := range b.SecurityPeriodFacts {
			periods[f.PeriodStart.Format(time.DateOnly)]++
		}
		if !more {
			break
		}
	}
	if len(secs) != 12 || secPages != 3 {
		t.Errorf("identities = %v in %d pages, want 12 in 3", secs, secPages)
	}
	if len(facts) != n || factPages != 3 {
		t.Errorf("facts = %v in %d pages, want %d in 3", facts, factPages, n)
	}
	if len(periods) != n || periodPages != 3 {
		t.Errorf("period facts = %v in %d pages, want %d in 3", periods, periodPages, n)
	}
	for _, read := range []map[string]int{secs, periods} {
		for k, c := range read {
			if c != 1 {
				t.Errorf("%s read %d times", k, c)
			}
		}
	}
}

// seedYield writes, in run2, one record of every kind schema v2 adds for
// the fund-day d3: a debt line with every v2 column, an interest-rate and
// a credit-spread risk row, a single figure and a breakdown part, and a
// company's figure for the quarter that ends on d3.
func seedYield(t *testing.T, db *sql.DB) {
	t.Helper()
	silvertest.Exec(t, db, `INSERT INTO lines (fund_ref, as_of, basis, line_no, cusip, name, asset_type, weight,
	        coupon_kind, is_default, in_arrears, paid_in_kind, exchange_rate, duration, modified_duration,
	        yield_to_maturity, yield_to_worst, yield_to_call, real_duration, real_yield, accrual_date)
	    VALUES ('isin:XX0000000001', ?, 'holdings', 3, '000000001', 'Example Treasury 2036', 'Fixed Income', '0.5',
	        'Fixed', 0, 1, 1, '1.085', '6.25', '6.1', '4.35', '4.2', '4.1', '5.5', '1.85', '2025-09-15')`, d3)
	silvertest.Exec(t, db, `INSERT INTO fund_risk (fund_ref, as_of, currency, tenor, dv01, dv100, credit_ig_dv01,
	        credit_hy_dv01, run_at, payload)
	    VALUES ('isin:XX0000000001', ?, 'USD', '10Y', '125.5', '12400', NULL, NULL, ?, '{"r":1}'),
	           ('isin:XX0000000001', ?, 'XXX', '5Y', NULL, NULL, '3.25', '1.5', ?, '{}')`, d3, run2, d3, run2)
	silvertest.Exec(t, db, `INSERT INTO characteristics (fund_ref, as_of, provider, field, label, value, run_at, payload)
	    VALUES ('isin:XX0000000001', ?, 'ishares', 'effective_duration', '', '6.31', ?, '{"c":1}'),
	           ('isin:XX0000000001', ?, 'ishares', 'credit_quality', 'AAA', '42.5', ?, '{}')`, d3, run2, d3, run2)
	silvertest.Exec(t, db, `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value,
	        scheme, unit, fiscal_year, fiscal_period, form, filed, run_at, payload)
	    VALUES ('symbol:EXMP', 'eps_diluted', ?, '2025-12-01', 'sec-edgar', '1.23',
	        'us-gaap:EarningsPerShareDiluted', 'USD/shares', 2026, 'Q1', '10-Q', '2026-04-10', ?, '{"f":1}')`, d3, run2)
}

// fields renders optional values for a field-by-field comparison.
func fields(vs ...any) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		switch v := v.(type) {
		case *string:
			out[i] = "<nil>"
			if v != nil {
				out[i] = *v
			}
		case *bool:
			out[i] = "<nil>"
			if v != nil {
				out[i] = fmt.Sprint(*v)
			}
		case *int64:
			out[i] = "<nil>"
			if v != nil {
				out[i] = fmt.Sprint(*v)
			}
		case *canonical.Decimal:
			out[i] = "<nil>"
			if v != nil {
				out[i] = v.String()
			}
		case *time.Time:
			out[i] = "<nil>"
			if v != nil {
				out[i] = v.Format(time.DateOnly)
			}
		default:
			out[i] = fmt.Sprint(v)
		}
	}
	return out
}

func checkFields(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// Every v2 column and table maps into its canonical field.
func TestSnapshotsMapYieldRecords(t *testing.T) {
	t.Parallel()
	path, db := newFixtureSilver(t)
	seed(t, db)
	seedYield(t, db)
	got := silvertest.Merged(t, openAdapter(t, path), canonical.Window{Start: d3, End: d3, HasChanges: true})

	if len(got.FundLines) != 3 {
		t.Fatalf("lines = %d, want 3", len(got.FundLines))
	}
	l := got.FundLines[2]
	checkFields(t, "debt line", fields(l.SecurityKey, l.CouponKind, l.IsDefault, l.InArrears, l.PaidInKind,
		l.ExchangeRate, l.Duration, l.ModifiedDuration, l.YieldToMaturity, l.YieldToWorst, l.YieldToCall,
		l.RealDuration, l.RealYield, l.AccrualDate),
		[]string{"cusip:000000001", "Fixed", "false", "true", "true", "1.085",
			"6.25", "6.1", "4.35", "4.2", "4.1", "5.5", "1.85", "2025-09-15"})
	// An equity line states none of them.
	e := got.FundLines[0]
	checkFields(t, "equity line", fields(e.CouponKind, e.IsDefault, e.InArrears, e.PaidInKind, e.ExchangeRate,
		e.Duration, e.AccrualDate), []string{"<nil>", "<nil>", "<nil>", "<nil>", "<nil>", "<nil>", "<nil>"})

	if len(got.FundRisk) != 2 {
		t.Fatalf("fund risk = %+v, want 2 rows", got.FundRisk)
	}
	for i, want := range [][]string{
		{"isin:XX0000000001", fmt.Sprint(d3), "USD", "10Y", "125.5", "12400", "<nil>", "<nil>", `{"r":1}`},
		{"isin:XX0000000001", fmt.Sprint(d3), canonical.NoCurrency, "5Y", "<nil>", "<nil>", "3.25", "1.5", `{}`},
	} {
		r := got.FundRisk[i]
		checkFields(t, "fund risk", fields(r.FundRef, r.AsOf, r.Currency, r.Tenor, r.DV01, r.DV100,
			r.CreditIGDV01, r.CreditHYDV01, string(r.Payload)), want)
	}

	if len(got.FundCharacteristics) != 2 {
		t.Fatalf("characteristics = %+v, want 2", got.FundCharacteristics)
	}
	for i, want := range [][]string{
		{"isin:XX0000000001", fmt.Sprint(d3), "ishares", "credit_quality", "AAA", "42.5", `{}`},
		{"isin:XX0000000001", fmt.Sprint(d3), "ishares", "effective_duration", "", "6.31", `{"c":1}`},
	} {
		c := got.FundCharacteristics[i]
		checkFields(t, "characteristic", fields(c.FundRef, c.AsOf, c.Provider, c.Field, c.Label,
			c.Value.String(), string(c.Payload)), want)
	}

	if len(got.SecurityPeriodFacts) != 1 {
		t.Fatalf("period facts = %+v, want 1", got.SecurityPeriodFacts)
	}
	f := got.SecurityPeriodFacts[0]
	checkFields(t, "period fact", fields(f.SecurityKey, f.Field, f.AsOf, &f.PeriodStart, f.Provider,
		f.Value.String(), f.Scheme, f.Unit, f.FiscalYear, f.FiscalPeriod, f.Form, f.Filed, string(f.Payload)),
		[]string{"symbol:EXMP", "eps_diluted", fmt.Sprint(d3), "2025-12-01", "sec-edgar",
			"1.23", "us-gaap:EarningsPerShareDiluted", "USD/shares", "2026", "Q1", "10-Q", "2026-04-10", `{"f":1}`})
}

// A silver still at schema v1 has none of the v2 tables or lines columns.
// It loads as before, with every v2 field absent.
func TestSnapshotsReadAV1Silver(t *testing.T) {
	t.Parallel()
	path, db := newV1Silver(t)
	if ok, err := silver.HasTables(context.Background(), db, "fund_risk"); err != nil || ok {
		t.Fatalf("the v1 fixture holds fund_risk (%v)", err)
	}
	seed(t, db)
	conn := openAdapter(t, path)
	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != d0 || st.LatestChangeNumber != load2 {
		t.Errorf("status = %+v", st)
	}
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != d0 || w.End != d3 {
		t.Fatalf("window = %+v, want [d0, d3]", w)
	}
	got := silvertest.Merged(t, conn, w)
	if len(got.FundCompositions) != 4 || len(got.FundLines) != 8 || len(got.Securities) != 1 ||
		len(got.SecurityFacts) != 1 {
		t.Errorf("v1 records = %d compositions, %d lines, %d securities, %d facts",
			len(got.FundCompositions), len(got.FundLines), len(got.Securities), len(got.SecurityFacts))
	}
	if n := len(got.FundRisk) + len(got.FundCharacteristics) + len(got.SecurityPeriodFacts); n != 0 {
		t.Errorf("a v1 silver states %d v2 records", n)
	}
	for _, l := range got.FundLines {
		if l.CouponKind != nil || l.IsDefault != nil || l.InArrears != nil || l.PaidInKind != nil || l.ExchangeRate != nil ||
			l.Duration != nil || l.ModifiedDuration != nil || l.YieldToMaturity != nil || l.YieldToWorst != nil ||
			l.YieldToCall != nil || l.RealDuration != nil || l.RealYield != nil || l.AccrualDate != nil {
			t.Errorf("v1 line %+v carries a v2 field", l)
		}
	}
}

// A run that wrote one row of a v2 table alone opens a window on that
// row's day, and the row comes back inside it: a risk row or a
// characteristic as a fund-day of its own, a period fact by its period's
// last day.
func TestChangeWindowSpansAYieldOnlyRun(t *testing.T) {
	t.Parallel()
	const dX = int64(1_772_323_200) // 2026-03-01
	for _, c := range []struct {
		table, insert string
		count         func(canonical.SnapshotBatch) int
	}{
		{"fund_risk", `INSERT INTO fund_risk (fund_ref, as_of, currency, tenor, dv01, run_at)
		    VALUES ('isin:XX0000000009', ?, 'USD', '1Y', '1', ?)`,
			func(b canonical.SnapshotBatch) int { return len(b.FundRisk) }},
		{"characteristics", `INSERT INTO characteristics (fund_ref, as_of, provider, field, value, run_at)
		    VALUES ('isin:XX0000000009', ?, 'ishares', 'sec_yield', '4.1', ?)`,
			func(b canonical.SnapshotBatch) int { return len(b.FundCharacteristics) }},
		{"period_facts", `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value, run_at)
		    VALUES ('symbol:EXMP', 'cash', ?, '2026-03-01', 'sec-edgar', '100', ?)`,
			func(b canonical.SnapshotBatch) int { return len(b.SecurityPeriodFacts) }},
	} {
		t.Run(c.table, func(t *testing.T) {
			t.Parallel()
			path, db := newFixtureSilver(t)
			seed(t, db)
			run3, load3 := run2+1000, load2+1000
			silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 2, '/x/3', ?)`, run3, load3)
			silvertest.Exec(t, db, c.insert, dX, run3)
			conn := openAdapter(t, path)
			w, err := conn.ChangeWindow(context.Background(), load2)
			if err != nil {
				t.Fatal(err)
			}
			if !w.HasChanges || w.Start != dX || w.End != dX || w.NewChangeNumber != load3 {
				t.Fatalf("window = %+v, want [%d, %d] at load3", w, dX, dX)
			}
			got := silvertest.Merged(t, conn, w)
			if n := c.count(got); n != 1 {
				t.Errorf("%s rows in the window = %d, want 1", c.table, n)
			}
			if len(got.FundCompositions) != 0 {
				t.Errorf("compositions = %+v, want none on that day", got.FundCompositions)
			}
		})
	}
}

// A characteristic or a period fact whose value is no decimal, or a period
// fact whose start is no date, fails the load with an error that names the
// row.
func TestSnapshotsRefuseABadYieldFigure(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, insert string
		want         []string
	}{
		{"characteristic value", `INSERT INTO characteristics (fund_ref, as_of, provider, field, label, value, run_at)
		    VALUES ('isin:XX0000000001', ?, 'ishares', 'credit_quality', 'BBB', 'n/a', ?)`,
			[]string{"isin:XX0000000001", "credit_quality", "BBB", "n/a"}},
		{"period fact value", `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value, run_at)
		    VALUES ('symbol:EXMP', 'revenue', ?, '2025-12-01', 'sec-edgar', '12,5', ?)`,
			[]string{"symbol:EXMP", "revenue", "2025-12-01", "12,5"}},
		{"period fact start", `INSERT INTO period_facts (security_key, field, as_of, period_start, provider, value, run_at)
		    VALUES ('symbol:EXMP', 'revenue', ?, 'Q4', 'sec-edgar', '12', ?)`,
			[]string{"symbol:EXMP", "revenue", "period_start", "Q4"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path, db := newFixtureSilver(t)
			seed(t, db)
			silvertest.Exec(t, db, c.insert, d3, run2)
			stream, err := openAdapter(t, path).Snapshots(context.Background(),
				canonical.Window{Start: d3, End: d3, HasChanges: true})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			for {
				_, more, err := stream.Next(context.Background())
				if err != nil {
					for _, w := range c.want {
						if !strings.Contains(err.Error(), w) {
							t.Errorf("error %q does not name %q", err, w)
						}
					}
					return
				}
				if !more {
					t.Fatal("the stream ended without an error")
				}
			}
		})
	}
}
