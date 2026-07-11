package gold

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// legacyToNewAllowlist maps each legacy asset_class value to the set
// of new (asset_class_new, vehicle) pairs it may become — the
// reviewable "expected diffs" (docs/TAXONOMY-PLAN.md). Legacy `other`
// is intentionally absent: those rows were unclassified, so any valid
// pair is an improvement and is accepted. Every other legacy value is
// tightly enumerated, so an unexpected reclassification (a bug) is
// caught.
var legacyToNewAllowlist = map[canonical.AssetClass][]taxPair{
	"equity":   {{"public_equity", "stock"}},
	"etf":      {{"public_equity", "etf"}},
	"bond_etf": {{"fixed_income", "etf"}},
	"fund": {
		{"public_equity", "fund"}, {"fixed_income", "fund"},
		{"real_estate", "fund"}, {"multi_asset", "fund"},
		{"cash", "fund"}, {"hedge_fund", "fund"}, {"infrastructure", "fund"},
	},
	"bond":             {{"fixed_income", "bond"}},
	"option":           {{"public_equity", "option"}, {"private_equity", "option"}, {"foreign_exchange", "option"}},
	"future":           {{"public_equity", "future"}},
	"fx_forward":       {{"foreign_exchange", "forward"}},
	"fx_option":        {{"foreign_exchange", "option"}},
	"money_market":     {{"cash", "fund"}, {"cash", "time_deposit"}},
	"otc_derivative":   {{"foreign_exchange", "forward"}, {"other", "other"}},
	"metal":            {{"metal", "physical"}, {"metal", "etf"}, {"metal", "fund"}},
	"crypto":           {{"crypto", "physical"}, {"crypto", "etf"}},
	"private_equity":   {{"private_equity", "stock"}, {"private_equity", "option"}},
	"spv":              {{"private_equity", "spv"}},
	"private_fund":     {{"private_equity", "fund"}, {"infrastructure", "fund"}, {"hedge_fund", "fund"}},
	"real_estate":      {{"real_estate", "physical"}, {"real_estate", "fund"}},
	"convertible_note": {{"private_debt", "convertible_note"}},
	"mortgage":         {{"real_estate", "mortgage"}},
}

type taxPair struct {
	class   canonical.AssetClass
	vehicle canonical.Vehicle
}

// TestAllowlistPairsAreAdmitted keeps the harness's legacy→new
// allowlist consistent with the canonical taxonomy: every pair it
// permits must be one the writer would accept (ValidTaxonomyPair).
// Runs always (not env-gated) so drift between the two is caught in
// CI, not only against a live DB.
func TestAllowlistPairsAreAdmitted(t *testing.T) {
	for legacy, pairs := range legacyToNewAllowlist {
		for _, p := range pairs {
			if !canonical.ValidTaxonomyPair(p.class, p.vehicle) {
				t.Errorf("allowlist[%q] permits (%q, %q), which is not an admitted taxonomy pair", legacy, p.class, p.vehicle)
			}
		}
	}
}

// TestTaxonomyMigrationInvariants is the migration verification
// harness (docs/TAXONOMY-PLAN.md). It runs against a REAL gold DB —
// set WEALTHDB_GOLD_VERIFY to its path — and is skipped otherwise, so
// it never runs in CI against a fixture.
//
// For every migrated (asset_class_new NOT NULL) row in positions and
// instruments it asserts (1) pair validity and (2) the legacy→new
// allowlist, and prints the full distribution + per-source
// market_value totals for human review and the external before/after
// value-preservation diff.
func TestTaxonomyMigrationInvariants(t *testing.T) {
	path := os.Getenv("WEALTHDB_GOLD_VERIFY")
	if path == "" {
		t.Skip("set WEALTHDB_GOLD_VERIFY=<gold.db> to run the taxonomy migration harness")
	}
	db, err := Open(path, ModeReadOnly)
	if err != nil {
		t.Fatalf("open gold %q: %v", path, err)
	}
	defer db.Close()
	ctx := context.Background()

	printPerSourceValueTotals(ctx, t, db)
	if !hasTaxonomyColumns(ctx, db) {
		t.Skip("gold DB predates migration 0028 (no asset_class_new column); reload with the current binary first")
	}
	for _, tbl := range []string{"positions", "instruments"} {
		t.Run(tbl, func(t *testing.T) { checkTaxonomyTable(ctx, t, db, tbl) })
	}
}

// hasTaxonomyColumns reports whether the gold DB has been migrated to
// schema 0028 (the 2-D columns). Lets the harness run at any stage and
// skip cleanly before the columns exist.
func hasTaxonomyColumns(ctx context.Context, db *sql.DB) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.columns
          WHERE table_name = 'positions' AND column_name = 'asset_class_new'`).Scan(&n)
	return err == nil && n == 1
}

func checkTaxonomyTable(ctx context.Context, t *testing.T, db *sql.DB, tbl string) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
SELECT silver_source_id, asset_class,
       COALESCE(asset_class_new, ''), COALESCE(vehicle, ''), count(*)
  FROM %s
 GROUP BY 1, 2, 3, 4
 ORDER BY 1, 2, 3, 4`, tbl))
	if err != nil {
		t.Fatalf("query %s: %v", tbl, err)
	}
	defer rows.Close()

	var migrated, pending int64
	var lines []string
	for rows.Next() {
		var src, legacy, newClass, vehicle string
		var n int64
		if err := rows.Scan(&src, &legacy, &newClass, &vehicle, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if newClass == "" && vehicle == "" {
			pending += n
			lines = append(lines, fmt.Sprintf("  %-12s %-16s → (not migrated)            %6d", src, legacy, n))
			continue
		}
		migrated += n
		lines = append(lines, fmt.Sprintf("  %-12s %-16s → (%s, %s)  %6d", src, legacy, newClass, vehicle, n))

		ac, veh := canonical.AssetClass(newClass), canonical.Vehicle(vehicle)
		if !canonical.ValidTaxonomyPair(ac, veh) {
			t.Errorf("%s [%s]: legacy %q → (%q, %q) is not an admitted taxonomy pair (%d rows)", tbl, src, legacy, newClass, vehicle, n)
			continue
		}
		if canonical.AssetClass(legacy) == canonical.AssetClassOther {
			continue // `other` was unclassified; any valid pair is fine
		}
		allowed, ok := legacyToNewAllowlist[canonical.AssetClass(legacy)]
		if !ok {
			t.Errorf("%s [%s]: legacy value %q has no allowlist entry (%d rows → (%q,%q))", tbl, src, legacy, n, newClass, vehicle)
			continue
		}
		if !containsPair(allowed, taxPair{ac, veh}) {
			t.Errorf("%s [%s]: legacy %q → (%q, %q) is OFF the allowlist (%d rows) — reconcile adapter or allowlist", tbl, src, legacy, newClass, vehicle, n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	t.Logf("%s distribution (migrated=%d, pending=%d):\n%s", tbl, migrated, pending, join(lines))
}

func containsPair(set []taxPair, p taxPair) bool {
	for _, q := range set {
		if q == p {
			return true
		}
	}
	return false
}

// printPerSourceValueTotals dumps per-source market_value sums so an
// external before/after diff can confirm classification changed no
// values (invariant 1). Reclassification must never move money.
func printPerSourceValueTotals(ctx context.Context, t *testing.T, db *sql.DB) {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, count(*), COALESCE(CAST(sum(market_value) AS VARCHAR), '')
  FROM positions GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatalf("value totals: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var src, total string
		var n int64
		if err := rows.Scan(&src, &n, &total); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("  %-12s rows=%-7d sum(market_value)=%s", src, n, total))
	}
	t.Logf("per-source position totals (control for value preservation):\n%s", join(lines))
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
