package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// seedMerchantStore writes one merchant verdict and one per-source
// enrichment row straight into the gold file, standing in for what an
// LLM categorisation pass would have left behind.
func seedMerchantStore(t *testing.T, goldPath string) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold to seed merchant store: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-market', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test-model');

        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('schwab-test', 'T1', 'sig-market', 1, NULL, 'signature-only', 100);
    `); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}
}

// readMerchantStore returns the row count of the merchant store and of
// the per-transaction enrichment overlay.
func readMerchantStore(t *testing.T, goldPath string) (merchants, enrichments int, name, detailed string) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&merchants); err != nil {
		t.Fatalf("count merchant store: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM spend_txn_enrichment`).Scan(&enrichments); err != nil {
		t.Fatalf("count enrichment overlay: %v", err)
	}
	if merchants > 0 {
		if err := db.QueryRow(`SELECT merchant_name, spend_detailed
                                 FROM spend_merchant_categories`).Scan(&name, &detailed); err != nil {
			t.Fatalf("read merchant row: %v", err)
		}
	}
	return merchants, enrichments, name, detailed
}

// merchantRow reads the single seeded merchant verdict as a
// column-name → rendered-value map. The columns are enumerated from
// the result set rather than named here, so every column the table
// carries is compared — including one a future migration adds, which
// is precisely the value a hand-maintained carry-across list drops.
func merchantRow(t *testing.T, goldPath string) map[string]string {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT * FROM spend_merchant_categories`)
	if err != nil {
		t.Fatalf("read merchant store: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("merchant store columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("merchant store is empty; want the seeded verdict")
	}
	cells := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range cells {
		ptrs[i] = &cells[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan merchant row: %v", err)
	}
	out := make(map[string]string, len(cols))
	for i, c := range cols {
		out[c] = fmt.Sprintf("%v", cells[i])
	}
	if rows.Next() {
		t.Fatalf("merchant store holds more than the seeded row")
	}
	return out
}

// TestReloadFreshCarriesMerchantStore is the regression guard for the
// one thing a fresh-file rebuild cannot regenerate. 'reload -a' builds
// an empty temp file and swaps it over the live path, so without an
// explicit carry-across every routine compaction-by-reload would wipe
// the LLM verdicts — which, unlike FX priorities or the config
// overrides, have no config file to be re-stamped from. The
// per-transaction enrichment is deliberately NOT carried: it is
// derived from the transactions the rebuild re-projects.
//
// The row is compared column by column, whole: the carry-across
// derives its column list from the schema, and a comparison naming
// only a couple of columns would pass while the rest went missing.
func TestReloadFreshCarriesMerchantStore(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	seedMerchantStore(t, goldPath)
	before := merchantRow(t, goldPath)

	so, se, code := run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	// The carry-across is otherwise invisible: without a line of its
	// own, a rebuild that carried nothing reads exactly like one that
	// carried the whole store.
	if !strings.Contains(so, "carried 1 merchant verdict(s) across the rebuild") {
		t.Errorf("reload -a did not report the carry-across:\n%s", so)
	}

	after := merchantRow(t, goldPath)
	for col, want := range before {
		if got, ok := after[col]; !ok {
			t.Errorf("column %q did not survive reload -a", col)
		} else if got != want {
			t.Errorf("column %q after reload -a = %q, want %q", col, got, want)
		}
	}
	if len(after) != len(before) {
		t.Errorf("merchant row columns after reload -a = %d, want %d", len(after), len(before))
	}

	_, enrichments, _, _ := readMerchantStore(t, goldPath)
	if enrichments != 0 {
		t.Errorf("enrichment rows after reload -a = %d, want 0 (derived, recomputed by the next pass)", enrichments)
	}
}

// TestReloadInPlaceKeepsMerchantStore confirms the other direct-loader
// path needs no carry-across: it resets and re-loads the live file,
// which Reset leaves the merchant store alone in.
func TestReloadInPlaceKeepsMerchantStore(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	seedMerchantStore(t, goldPath)

	if _, se, code := run(t, "-c", cfg, "reload", "-a", "--in-place"); code != 0 {
		t.Fatalf("reload -a --in-place failed: code=%d stderr=%s", code, se)
	}

	merchants, enrichments, _, _ := readMerchantStore(t, goldPath)
	if merchants != 1 {
		t.Errorf("merchant store rows after in-place reload = %d, want 1", merchants)
	}
	if enrichments != 0 {
		t.Errorf("enrichment rows after in-place reload = %d, want 0 (Reset clears them)", enrichments)
	}
}

// TestCompactPreservesSpendOverlay checks the assumption that the
// narrower compaction path needs no special handling: COPY FROM
// DATABASE copies every table, so both overlay tables cross the swap
// untouched — including the enrichment rows, which compaction has no
// reason to drop because it never re-projects anything.
func TestCompactPreservesSpendOverlay(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	seedMerchantStore(t, goldPath)

	if _, se, code := run(t, "-c", cfg, "compact"); code != 0 {
		t.Fatalf("compact failed: code=%d stderr=%s", code, se)
	}

	merchants, enrichments, name, _ := readMerchantStore(t, goldPath)
	if merchants != 1 || name != "Corner Market" {
		t.Errorf("merchant store after compact = %d row(s), name %q; want 1, Corner Market", merchants, name)
	}
	if enrichments != 1 {
		t.Errorf("enrichment rows after compact = %d, want 1 (a faithful copy)", enrichments)
	}
}

// TestSpendingPassRunsOnEveryLoadPath is the hook's regression guard.
// The pass is wired into three places — load, the fresh-file rebuild,
// and the in-place reload — and a path that quietly loses it leaves
// gold with stale verdicts and no error to say so. Each path is
// exercised for the one line the pass always prints.
func TestSpendingPassRunsOnEveryLoadPath(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"load", []string{"load", "schwab-test"}},
		{"reload -a (fresh swap)", []string{"reload", "-a"}},
		{"reload -a --in-place", []string{"reload", "-a", "--in-place"}},
	} {
		so, se, code := run(t, append([]string{"-c", cfg}, tc.args...)...)
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", tc.name, code, se)
		}
		if !strings.Contains(so, "spending: ") {
			t.Errorf("%s: no spending-pass line on stdout; the hook is missing from this path\n%s",
				tc.name, so)
		}
	}
}

// dropMerchantColumn removes one column from the live gold's merchant
// store, standing in for the state a live file is left in by an
// additive migration nothing has applied to it yet: 'reload -a' never
// opens the live file read-write, so it never migrates it.
func dropMerchantColumn(t *testing.T, goldPath, column string) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold to drop %s: %v", column, err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE spend_merchant_categories DROP COLUMN ` + column); err != nil {
		t.Fatalf("drop %s from the merchant store: %v", column, err)
	}
}

// TestReloadFreshNamesTheColumnTheLiveFileLacks covers the drift
// direction the derived column list makes silent rather than safe.
//
// The intersection drops a column the outgoing file has not got, and
// every column of the merchant store is NOT NULL — so the INSERT would
// die on the target table's constraint, mid-rebuild, with a message
// naming no file, no command and no way forward. The refusal has to
// come first and name the column.
func TestReloadFreshNamesTheColumnTheLiveFileLacks(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	seedMerchantStore(t, goldPath)
	dropMerchantColumn(t, goldPath, "model_name")

	_, se, code := run(t, "-c", cfg, "reload", "-a")
	if code == 0 {
		t.Fatal("reload -a succeeded against a live merchant store missing a required column")
	}
	if !strings.Contains(se, "model_name") {
		t.Errorf("the refusal does not name the missing column:\n%s", se)
	}
	if !strings.Contains(se, "wealthdb load") {
		t.Errorf("the refusal does not name the command that migrates the live file:\n%s", se)
	}
	if strings.Contains(se, "NOT NULL constraint failed") {
		t.Errorf("the raw constraint error surfaced instead of the named-column refusal:\n%s", se)
	}

	// The live file is untouched: a refused rebuild swaps nothing.
	merchants, _, _, _ := readMerchantStore(t, goldPath)
	if merchants != 1 {
		t.Errorf("merchant store rows after the refused reload = %d, want 1", merchants)
	}
}

// TestReloadFreshDistinguishesAnEmptyStoreFromNoStore pins the two
// nothing-carried cases apart. A file with an empty merchant store and
// a file predating the store entirely are different states, and one
// line for both reads as the schema being absent when it is not.
func TestReloadFreshDistinguishesAnEmptyStoreFromNoStore(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	so, se, code := run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "merchant store is empty") {
		t.Errorf("reload -a over an empty (but present) merchant store did not say so:\n%s", so)
	}
}
