package gold

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// TestLoadReturnsDatasetsMultiMatchesSingle is the core guarantee for the
// single-pass multi-currency loader: for every currency, the dataset it
// produces must be bit-identical to the one loadReturnsDataset(ccy) builds from
// the per-currency macros — same accounts, value series, snapshot days, flows,
// derived flags and globalMax. If they ever diverge, the materialized table
// would silently stop matching the CLI for CHF/EUR.
func TestLoadReturnsDatasetsMultiMatchesSingle(t *testing.T) {
	db, ctx := openMigrated(t)
	seedMaterializeFixture(t, db, ctx)

	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		t.Fatalf("loadFxBounds: %v", err)
	}
	multi, err := loadReturnsDatasetsMulti(ctx, db, fx)
	if err != nil {
		t.Fatalf("loadReturnsDatasetsMulti: %v", err)
	}

	for _, ccy := range materializeCurrencies {
		single, err := loadReturnsDataset(ctx, db, ccy, fx)
		if err != nil {
			t.Fatalf("loadReturnsDataset %s: %v", ccy, err)
		}
		m, ok := multi[ccy]
		if !ok {
			t.Fatalf("multi loader missing currency %s", ccy)
		}
		if m.globalMax != single.globalMax {
			t.Errorf("%s: globalMax %d, single %d", ccy, m.globalMax, single.globalMax)
		}
		if len(m.accts) != len(single.accts) {
			t.Fatalf("%s: %d accounts, single %d", ccy, len(m.accts), len(single.accts))
		}
		if len(single.accts) == 0 {
			t.Fatalf("%s: fixture produced no accounts", ccy)
		}
		for key, a := range single.accts {
			b, ok := m.accts[key]
			if !ok {
				t.Errorf("%s: multi missing account %q", ccy, key)
				continue
			}
			if a.baseCurrency != b.baseCurrency || a.label != b.label ||
				a.portfolio != b.portfolio || a.kind != b.kind {
				t.Errorf("%s/%s: metadata differs", ccy, key)
			}
			if a.cryptoExcluded != b.cryptoExcluded || a.journalPresent != b.journalPresent ||
				a.hasClampedFlow != b.hasClampedFlow || a.droppedNonzero != b.droppedNonzero {
				t.Errorf("%s/%s: flags differ (single %+v multi %+v)", ccy, key,
					[]bool{a.cryptoExcluded, a.journalPresent, a.hasClampedFlow, a.droppedNonzero},
					[]bool{b.cryptoExcluded, b.journalPresent, b.hasClampedFlow, b.droppedNonzero})
			}
			if !sameSeries(a.series, b.series) {
				t.Errorf("%s/%s: value series differ:\n single %v\n multi  %v", ccy, key, a.series, b.series)
			}
			if !sameInts(a.snapDays, b.snapDays) {
				t.Errorf("%s/%s: snapDays differ", ccy, key)
			}
			if !sameFlows(a.nonTransfer, b.nonTransfer) {
				t.Errorf("%s/%s: nonTransfer flows differ:\n single %v\n multi  %v", ccy, key, a.nonTransfer, b.nonTransfer)
			}
			if !sameFlows(a.transferLike, b.transferLike) {
				t.Errorf("%s/%s: transferLike flows differ:\n single %v\n multi  %v", ccy, key, a.transferLike, b.transferLike)
			}
		}
	}
}

func sameSeries(a, b []dayVal) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].day != b[i].day || a[i].val != b[i].val {
			return false
		}
	}
	return true
}

func sameInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameFlows(a, b []returns.Flow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Day != b[i].Day || a[i].Amount != b[i].Amount || a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

// TestReturnsInsertSQL checks the batched-INSERT builder emits one placeholder
// tuple per row with the right column count (a miscount would shift every bind
// value silently).
func TestReturnsInsertSQL(t *testing.T) {
	for _, n := range []int{1, 3, returnsInsertBatch} {
		q := returnsInsertSQL(n)
		if got := strings.Count(q, "?"); got != n*returnsInsertCols {
			t.Errorf("n=%d: %d placeholders, want %d", n, got, n*returnsInsertCols)
		}
		if got := strings.Count(q, "("); got != n+1 { // n value tuples + the column list
			t.Errorf("n=%d: %d open-parens, want %d", n, got, n+1)
		}
		if !strings.HasPrefix(q, "INSERT INTO report_returns") {
			t.Errorf("n=%d: unexpected prefix %q", n, q[:30])
		}
	}
}

// TestMaterializeReturnsBatchBoundary drives enough rows through the batched
// insert to cross the returnsInsertBatch boundary (≥1 full batch + a
// remainder) and checks nothing is lost or duplicated: the reported count
// equals the table count equals the sum of the per-partition RunReturns row
// counts, on a single computed_at.
func TestMaterializeReturnsBatchBoundary(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "src-a", "schwab")

	// A wide monthly window so the accounts/monthly partitions alone yield
	// hundreds of bucket rows (carried forward between the two sparse snaps),
	// comfortably past the 128-row batch size.
	t0, t1 := dy(2016, time.January, 4), dy(2024, time.July, 1)
	pf := "PF1"
	seedAcct(t, db, ctx, "src-a", "A1", canonical.AccountKindBrokerage, &pf,
		[]snap{{t0, 1000}, {t1, 2200}}, []txn{{dy(2020, time.March, 2), canonical.TxKindDeposit, 300}})
	seedAcct(t, db, ctx, "test-src", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 500}, {t1, 900}}, nil)
	seedFX(t, db, dy(2016, time.January, 1), "USD", "CHF", "1.10")
	seedFX(t, db, dy(2016, time.January, 1), "USD", "EUR", "1.05")
	end := time.Date(2024, time.July, 1, 23, 59, 59, 0, time.UTC).Unix()

	n, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1000})
	if err != nil {
		t.Fatalf("MaterializeReturns: %v", err)
	}
	if n <= returnsInsertBatch {
		t.Fatalf("fixture only produced %d rows; need > %d to exercise a full batch + remainder",
			n, returnsInsertBatch)
	}

	// Independent expectation: sum the row counts of a per-partition RunReturns.
	want := 0
	for _, ccy := range materializeCurrencies {
		for _, grain := range materializeGrains {
			for _, period := range materializePeriods {
				rows, err := RunReturns(ctx, db, ReturnParams{
					Level: grain, FromEpoch: 0, ToEpoch: end, OutCcy: ccy,
					Method: "both", Period: period, Annualize: "auto", Netting: true, Inception: "full",
				})
				if err != nil {
					t.Fatalf("RunReturns %s/%s/%s: %v", grain, period, ccy, err)
				}
				want += len(rows)
			}
		}
	}
	if n != want {
		t.Errorf("MaterializeReturns inserted %d rows, per-partition RunReturns totals %d", n, want)
	}

	var total, stamps int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT computed_at) FROM report_returns`).Scan(&total, &stamps); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != n {
		t.Errorf("table has %d rows, MaterializeReturns reported %d", total, n)
	}
	if stamps != 1 {
		t.Errorf("expected one computed_at, got %d distinct", stamps)
	}
}

// TestGroupAccountsMemberOrderDeterministic protects the groupAccounts member
// sort directly (the aggregate-grain determinism fix). It loads many accounts
// under one source — whose sources/global groups therefore have many members —
// and asserts each group's members come back in (src, acct) order. Without the
// sort the members would follow accts' randomized map-iteration order, which
// for this many deliberately-unsorted ids is astronomically unlikely to already
// be sorted, so the assertion fails.
func TestGroupAccountsMemberOrderDeterministic(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "src-a", "schwab")

	// Ids chosen so lexical order != insertion order; enough of them that a
	// coincidental already-sorted map iteration is negligible.
	ids := []string{"m5", "a3", "z9", "c1", "q7", "b2", "y8", "d4", "x6", "e0"}
	t0, t1 := dy(2023, time.January, 3), dy(2023, time.June, 1)
	for _, id := range ids {
		seedAcct(t, db, ctx, "src-a", id, canonical.AccountKindBrokerage, nil,
			[]snap{{t0, 1000}, {t1, 1100}}, nil)
	}

	accts, err := loadAccountData(ctx, db, "USD")
	if err != nil {
		t.Fatalf("loadAccountData: %v", err)
	}

	for _, level := range []string{"sources", "global"} {
		groups, order := groupAccounts(level, accts, nil)
		for _, k := range order {
			members := groups[k]
			if len(members) < 2 {
				continue
			}
			for i := 1; i < len(members); i++ {
				prev, cur := members[i-1], members[i]
				if prev.src > cur.src || (prev.src == cur.src && prev.acct > cur.acct) {
					var got []string
					for _, m := range members {
						got = append(got, m.src+"/"+m.acct)
					}
					t.Fatalf("%s group %q not sorted by (src, acct): %v", level, k, got)
				}
			}
		}
	}
}

// TestMaterializeReturnsDeterministic re-materializes twice and checks the row
// bodies are byte-identical (only computed_at differs). It backstops the
// groupAccounts sort at the whole-table level; the direct guarantee is
// TestGroupAccountsMemberOrderDeterministic.
func TestMaterializeReturnsDeterministic(t *testing.T) {
	db, ctx := openMigrated(t)
	seedMaterializeFixture(t, db, ctx)
	end := time.Date(2024, time.July, 2, 23, 59, 59, 0, time.UTC).Unix()

	digest := func() string {
		if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1000}); err != nil {
			t.Fatalf("MaterializeReturns: %v", err)
		}
		rows, err := db.QueryContext(ctx, `
			SELECT currency, grain, granularity, silver_source_id, entity_id, period, is_summary,
			       COALESCE(CAST(start_value AS VARCHAR), ''), COALESCE(CAST(end_value AS VARCHAR), ''),
			       COALESCE(CAST(net_flow AS VARCHAR), ''),
			       COALESCE(CAST(twr AS VARCHAR), ''), COALESCE(CAST(mwr AS VARCHAR), ''), quality
			  FROM report_returns
			 ORDER BY currency, grain, granularity, silver_source_id, entity_id, is_summary, start_day`)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			b.WriteString(fmt.Sprintf("%v\n", vals))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}

	if a, b := digest(), digest(); a != b {
		t.Error("two materializations produced different row bodies (nondeterministic)")
	}
}
