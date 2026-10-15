package refdata

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// testdata/silver_schema.sql is a copy of
// collectors/refdata/migrations/0001_initial.sql; the collector's tests
// check that the two agree.
//
//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

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
	path := t.TempDir() + "/refdata.db"
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path, db
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seed writes two runs of an invented fund: run1 serves 2025-12-31 and
// 2026-01-31, run2 serves 2026-02-28 and a backfilled 2020-12-31, plus a
// security looked up in run2.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?), (?, 1, '/x/2', ?)`, run1, load1, run2, load2)
	for _, c := range []struct {
		asOf, run int64
	}{{d1, run1}, {d2, run1}, {d3, run2}, {d0, run2}} {
		exec(t, db, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, fund_name,
		    currency, net_assets, line_count, weight_sum, run_at)
		    VALUES ('isin:XX0000000001', ?, 'holdings', 'inbox', 'inbox', 'Example Index Fund', 'USD', '1000', 2, '99.5', ?)`,
			c.asOf, c.run)
		exec(t, db, `INSERT INTO lines (fund_ref, as_of, basis, line_no, isin, name, asset_type, weight, currency, country, sector)
		    VALUES ('isin:XX0000000001', ?, 'holdings', 1, 'xx0000000002', 'Example Corp', 'Equity', '60.5', 'USD', 'United States', 'Information Technology'),
		           ('isin:XX0000000001', ?, 'holdings', 2, NULL, 'USD CASH', 'Cash', '39', 'USD', NULL, NULL)`,
			c.asOf, c.asOf)
	}
	exec(t, db, `INSERT INTO attributes (fund_ref, as_of, domicile, ucits, hedged_currency, run_at)
	    VALUES ('isin:XX0000000001', ?, 'IE', 1, 'CHF', ?)`, d3, run2)
	exec(t, db, `INSERT INTO counterparties (fund_ref, as_of, role, name, share, run_at)
	    VALUES ('isin:XX0000000001', ?, 'swap', 'Example Bank', '12.5', ?)`, d3, run2)
	exec(t, db, `INSERT INTO securities (security_key, as_of, provider, isin, figi, ticker, name, run_at)
	    VALUES ('symbol:EXMP', ?, 'openfigi', 'XX0000000002', 'BBG000000002', 'EXMP', 'Example Corp', ?)`, d3, run2)
	exec(t, db, `INSERT INTO security_facts (security_key, field, as_of, provider, value, scheme, run_at)
	    VALUES ('symbol:EXMP', 'sic', ?, 'sec-edgar', '3571', 'sic', ?)`, d3, run2)
}

func collect(t *testing.T, conn silver.Connection, w canonical.Window) canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	var all canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		all.FundCompositions = append(all.FundCompositions, b.FundCompositions...)
		all.FundLines = append(all.FundLines, b.FundLines...)
		all.FundAttributes = append(all.FundAttributes, b.FundAttributes...)
		all.FundCounterparties = append(all.FundCounterparties, b.FundCounterparties...)
		all.Securities = append(all.Securities, b.Securities...)
		all.SecurityFacts = append(all.SecurityFacts, b.SecurityFacts...)
		if !more {
			return all
		}
	}
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
	got := collect(t, conn, w)
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
	exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/0', ?)`, early, load2+100)
	exec(t, db, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
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
	exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/3', ?)`, run2+1000, load3)
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
	got := collect(t, conn, canonical.Window{Start: d3, End: d3, HasChanges: true})

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
func lowerBudgets(t *testing.T, lines, tail int) {
	t.Helper()
	l, s := silver.ReferenceLineBudget, silver.ReferenceTailBudget
	silver.ReferenceLineBudget, silver.ReferenceTailBudget = lines, tail
	t.Cleanup(func() { silver.ReferenceLineBudget, silver.ReferenceTailBudget = l, s })
}

// A window wider than one batch's line budget splits between fund-days
// and loses none.
func TestSnapshotsBatchLargeWindows(t *testing.T) {
	lowerBudgets(t, 20, 5)
	path, db := newFixtureSilver(t)
	exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, load1)
	const days, perDay = 3, 12
	for d := int64(0); d < days; d++ {
		asOf := d1 + d*86400
		exec(t, db, `INSERT INTO compositions (fund_ref, as_of, basis, provider, source_kind, line_count, run_at)
		    VALUES ('symbol:BIG', ?, 'holdings', 'inbox', 'inbox', ?, ?)`, asOf, perDay, run1)
		for i := 0; i < perDay; i++ {
			exec(t, db, `INSERT INTO lines (fund_ref, as_of, basis, line_no, ticker, weight)
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
	// Two fund-days fill the first batch past its budget; the third
	// rides in the second.
	if len(batches) != 2 || batches[0] != 2*perDay || batches[1] != perDay {
		t.Errorf("lines per batch = %v, want [%d %d]", batches, 2*perDay, perDay)
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
	exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1', ?)`, run1, load1)
	for _, key := range []string{"symbol:AAA", "symbol:BBB", "symbol:CCC"} {
		for _, asOf := range []int64{d1, d2} {
			for _, provider := range []string{"openfigi", "sec-edgar"} {
				exec(t, db, `INSERT INTO securities (security_key, as_of, provider, run_at) VALUES (?, ?, ?, ?)`,
					key, asOf, provider, run1)
			}
		}
	}
	const n = 12
	for i := 0; i < n; i++ {
		exec(t, db, `INSERT INTO security_facts (security_key, field, as_of, provider, value, run_at)
		    VALUES (?, 'sic', ?, 'sec-edgar', '3571', ?)`, fmt.Sprintf("symbol:S%02d", i), d1, run1)
	}
	stream, err := openAdapter(t, path).Snapshots(context.Background(), canonical.Window{Start: d1, End: d2, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	secs, facts := map[string]int{}, map[string]int{}
	secPages, factPages := 0, 0
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
		for _, s := range b.Securities {
			secs[fmt.Sprintf("%s|%d|%s", s.SecurityKey, s.AsOf, s.Provider)]++
		}
		for _, f := range b.SecurityFacts {
			facts[f.SecurityKey]++
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
	for k, c := range secs {
		if c != 1 {
			t.Errorf("identity %s read %d times", k, c)
		}
	}
}
