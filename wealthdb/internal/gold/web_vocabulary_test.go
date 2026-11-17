package gold

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The columns the web views derive from a vocabulary the engine states,
// each held to the Go that states it.

// yieldKinds are the yield report's kinds.
var yieldKinds = []yield.Kind{yield.KindDeposit, yield.KindTermDeposit, yield.KindMoneyMarket, yield.KindBond,
	yield.KindBill, yield.KindFloating, yield.KindNote, yield.KindBondFund, yield.KindEquityFund,
	yield.KindMultiAsset, yield.KindStock, yield.KindLiability, yield.KindOther}

// web_yield's is_cash holds the kinds `wealthdb yield cash` lists, and
// rate_sensitive names only kinds the yield report has.
func TestWebYieldKindFlags(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	for _, k := range yieldKinds {
		if _, err := db.ExecContext(ctx, `INSERT INTO report_yield_positions BY NAME
            SELECT 0 AS computed_at, 0 AS as_of, 'USD' AS currency, 's' AS silver_source_id,
                   'a' AS account_external_id, ? AS position_key, ? AS kind,
                   0.0 AS value, FALSE AS weighs`, string(k), string(k)); err != nil {
			t.Fatal(err)
		}
	}
	var cash, sensitive []string
	if err := scanAll(ctx, db, `SELECT kind, is_cash, rate_sensitive FROM web_yield ORDER BY kind`,
		func(rows *sql.Rows) error {
			var k string
			var c, s bool
			if err := rows.Scan(&k, &c, &s); err != nil {
				return err
			}
			if c {
				cash = append(cash, k)
			}
			if s {
				sensitive = append(sensitive, k)
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, k := range yieldKinds {
		b := &yield.Book{In: &yield.Inputs{}, Positions: []yield.Position{{Measures: yield.Measures{Kind: k}}}}
		if len(b.Cash()) > 0 {
			want = append(want, string(k))
		}
	}
	slices.Sort(want)
	if !slices.Equal(cash, want) {
		t.Errorf("is_cash holds %v; Book.Cash lists %v", cash, want)
	}
	if len(sensitive) == 0 {
		t.Error("rate_sensitive holds no kind")
	}
}

// income_line_class is the class cashflow_txn_nodes reads a receipt's
// from (spend_categories, stamped from canonical's roles), the projected
// types aside, and income_projected_types the types incomeTypeSQL gives
// a line of each detailed type, whatever its holding.
func TestIncomeMacrosAreTheVocabulary(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM spend_categories`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("spend_categories: %d rows, %v", n, err)
	}
	if err := scanAll(ctx, db, `
SELECT spend_detailed, income_line_class(spend_detailed),
       CASE WHEN projected_income THEN NULL ELSE COALESCE(flow_class_in, 'other_receipts') END
  FROM spend_categories
 WHERE income_line_class(spend_detailed) IS DISTINCT FROM
       CASE WHEN projected_income THEN NULL ELSE COALESCE(flow_class_in, 'other_receipts') END`,
		func(rows *sql.Rows) error {
			var d string
			var got, want sql.NullString
			if err := rows.Scan(&d, &got, &want); err != nil {
				return err
			}
			t.Errorf("%s: income_line_class %q, the vocabulary %q", d, got.String, want.String)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	var none string
	if err := db.QueryRowContext(ctx, `SELECT income_line_class(NULL)`).Scan(&none); err != nil ||
		none != planUncategorized {
		t.Errorf("a line of no type reads %q, %v; want %q", none, err, planUncategorized)
	}
	if err := scanAll(ctx, db, `
WITH l AS (SELECT spend_detailed AS income_detailed FROM spend_categories),
i AS (SELECT * FROM (VALUES ('bond'), ('etf'), ('fund'), ('stock'), (NULL)) i(vehicle)),
tm AS (SELECT * FROM (VALUES (NULL, NULL), ('fixed', 5.0), ('fixed', 0.0), (?, NULL)) tm(coupon_kind, coupon_pct)),
typed AS (
    SELECT l.income_detailed, COALESCE(list_sort(list(DISTINCT `+incomeTypeSQL+`) FILTER (WHERE `+incomeTypeSQL+` IS NOT NULL)), [])
                   AS types
      FROM l, i, tm GROUP BY 1)
SELECT income_detailed, array_to_string(types, ','),
       array_to_string(list_sort(COALESCE(income_projected_types(income_detailed), [])), ',')
  FROM typed
 WHERE types <> list_sort(COALESCE(income_projected_types(income_detailed), []))`,
		func(rows *sql.Rows) error {
			var d, want, got string
			if err := rows.Scan(&d, &want, &got); err != nil {
				return err
			}
			t.Errorf("%s: income_projected_types [%s], incomeTypeSQL [%s]", d, got, want)
			return nil
		}, string(canonical.CouponZero)); err != nil {
		t.Fatal(err)
	}
	var projected []string
	if err := scanAll(ctx, db, `SELECT spend_detailed FROM spend_categories
         WHERE income_projected_types(spend_detailed) IS NOT NULL ORDER BY 1`, func(rows *sql.Rows) error {
		var d string
		projected = append(projected, d)
		return rows.Scan(&projected[len(projected)-1])
	}); err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(slices.Values(projectedIncomeDetailed))
	if !slices.Equal(projected, want) {
		t.Errorf("income_projected_types types %v; the projection projects %v", projected, want)
	}
}

// web_sources_observed reads a source's last read as source_observed
// gives it, never before its snapshot, and is_feed leaves the declared
// accounts' source out.
func TestWebSourcesObserved(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
             high_watermark, first_loaded_at, last_loaded_at, observed_snapshots_through) VALUES
             ('quiet', 'x', '/q', -1, 0, 0, 500), ('behind', 'x', '/b', -1, 0, 0, 50), ('unsaid', 'x', '/u', -1, 0, 0, 0)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO serving_sources_latest BY NAME
             SELECT * FROM (VALUES ('quiet', 100), ('behind', 100), ('unsaid', 100), (?, 0))
                    v(silver_source_id, snapshot_at)`, canonical.DeclaredSourceID); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]any{}
	if err := scanAll(ctx, db, `SELECT silver_source_id, observed_at, is_feed FROM web_sources_observed`,
		func(rows *sql.Rows) error {
			var s string
			var at int64
			var feed bool
			if err := rows.Scan(&s, &at, &feed); err != nil {
				return err
			}
			got[s] = [2]any{at, feed}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]any{"quiet": {int64(500), true}, "behind": {int64(100), true},
		"unsaid": {int64(100), true}, canonical.DeclaredSourceID: {int64(0), false}}
	for s, w := range want {
		if got[s] != w {
			t.Errorf("%s: observed_at and is_feed %v, want %v", s, got[s], w)
		}
	}
}
