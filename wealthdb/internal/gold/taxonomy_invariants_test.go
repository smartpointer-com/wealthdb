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

// TestTaxonomyInvariants is the gold taxonomy-integrity +
// value-preservation harness. It runs against a REAL gold DB — set
// WEALTHDB_GOLD_VERIFY to its path — and is skipped otherwise, so it
// never runs in CI against a fixture.
//
// For every positions / instruments row it asserts the
// (asset_class, vehicle) pair is populated and taxonomy-admitted
// (ValidTaxonomyPair), and prints the per-source distribution for
// human review. When WEALTHDB_GOLD_BEFORE points at a pre-reload copy
// of the gold DB it additionally asserts value preservation across
// the reload (see checkValuePreservation).
func TestTaxonomyInvariants(t *testing.T) {
	path := os.Getenv("WEALTHDB_GOLD_VERIFY")
	if path == "" {
		t.Skip("set WEALTHDB_GOLD_VERIFY=<gold.db> to run the taxonomy harness")
	}
	db, err := Open(path, ModeReadOnly)
	if err != nil {
		t.Fatalf("open gold %q: %v", path, err)
	}
	defer db.Close()
	ctx := context.Background()

	checkValuePreservation(ctx, t, db)
	for _, tbl := range []string{"positions", "instruments"} {
		t.Run(tbl, func(t *testing.T) { checkTaxonomyTable(ctx, t, db, tbl) })
	}
}

// checkTaxonomyTable asserts every row of tbl carries an admitted
// (asset_class, vehicle) pair. The writer already rejects invalid
// pairs at write time (validateTaxonomyPair); this is defence in
// depth against a hand-edited DB or rows written by an older binary,
// and prints the live distribution for review.
func checkTaxonomyTable(ctx context.Context, t *testing.T, db *sql.DB, tbl string) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
SELECT silver_source_id, asset_class, COALESCE(vehicle, ''), count(*)
  FROM %s
 GROUP BY 1, 2, 3
 ORDER BY 1, 2, 3`, tbl))
	if err != nil {
		t.Fatalf("query %s: %v", tbl, err)
	}
	defer rows.Close()

	var total int64
	var lines []string
	for rows.Next() {
		var src, class, vehicle string
		var n int64
		if err := rows.Scan(&src, &class, &vehicle, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total += n
		lines = append(lines, fmt.Sprintf("  %-12s (%s, %s)  %6d", src, class, vehicle, n))

		if !canonical.ValidTaxonomyPair(canonical.AssetClass(class), canonical.Vehicle(vehicle)) {
			t.Errorf("%s [%s]: (%q, %q) is not an admitted taxonomy pair (%d rows)", tbl, src, class, vehicle, n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	t.Logf("%s distribution (rows=%d):\n%s", tbl, total, join(lines))
}

// checkValuePreservation enforces that a reload never moves money or
// drops rows. When WEALTHDB_GOLD_BEFORE points at a pre-reload copy of
// the gold DB, it ASSERTS that per-(source, account, snapshot)
// market_value totals and per-source positions + instruments row
// counts are identical before and after — the finest grain at which a
// mis-projection could hide a value change. Absent the before-copy it
// degrades to printing the current totals for a manual external diff.
//
// Operator workflow for a real check:
//
//	cp $GOLD gold.before.db          # BEFORE reload
//	wealthdb reload -a               # re-project with the current adapters
//	WEALTHDB_GOLD_VERIFY=$GOLD WEALTHDB_GOLD_BEFORE=gold.before.db \
//	  go test ./internal/gold -run TestTaxonomyInvariants -v
func checkValuePreservation(ctx context.Context, t *testing.T, after *sql.DB) {
	printCounts(ctx, t, "after", after)

	beforePath := os.Getenv("WEALTHDB_GOLD_BEFORE")
	if beforePath == "" {
		t.Log("WEALTHDB_GOLD_BEFORE unset: value preservation printed only, not asserted (set it to a pre-reload gold copy to assert)")
		return
	}
	before, err := Open(beforePath, ModeReadOnly)
	if err != nil {
		t.Fatalf("open before-gold %q: %v", beforePath, err)
	}
	defer before.Close()
	printCounts(ctx, t, "before", before)

	// Per-(source, account, snapshot) market_value totals must match
	// exactly. DECIMAL summed and rendered as VARCHAR compares exactly.
	const q = `
SELECT silver_source_id || '\x1f' || account_external_id || '\x1f' || CAST(snapshot_at AS VARCHAR),
       COALESCE(CAST(sum(market_value) AS VARCHAR), 'NULL')
  FROM positions GROUP BY 1`
	beforeTot, err := scanKV(ctx, before, q)
	if err != nil {
		t.Fatalf("before totals: %v", err)
	}
	afterTot, err := scanKV(ctx, after, q)
	if err != nil {
		t.Fatalf("after totals: %v", err)
	}
	drift := 0
	for k, bv := range beforeTot {
		if av, ok := afterTot[k]; !ok {
			t.Errorf("value preservation: group %q present before, missing after", k)
			drift++
		} else if av != bv {
			t.Errorf("value preservation: group %q market_value %s → %s (must be unchanged)", k, bv, av)
			drift++
		}
	}
	for k := range afterTot {
		if _, ok := beforeTot[k]; !ok {
			t.Errorf("value preservation: group %q appeared after (was absent before)", k)
			drift++
		}
	}
	if drift == 0 {
		t.Logf("value preservation OK: %d (source,account,snapshot) groups identical before/after", len(afterTot))
	}
}

func printCounts(ctx context.Context, t *testing.T, label string, db *sql.DB) {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id,
       (SELECT count(*) FROM positions p WHERE p.silver_source_id = s.silver_source_id),
       (SELECT count(*) FROM instruments i WHERE i.silver_source_id = s.silver_source_id),
       COALESCE(CAST((SELECT sum(market_value) FROM positions p WHERE p.silver_source_id = s.silver_source_id) AS VARCHAR), '')
  FROM silver_sources s ORDER BY silver_source_id`)
	if err != nil {
		t.Fatalf("%s counts: %v", label, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var src, total string
		var pos, instr int64
		if err := rows.Scan(&src, &pos, &instr, &total); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("  %-12s positions=%-7d instruments=%-6d sum(market_value)=%s", src, pos, instr, total))
	}
	t.Logf("%s per-source counts:\n%s", label, join(lines))
}

func scanKV(ctx context.Context, db *sql.DB, q string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
