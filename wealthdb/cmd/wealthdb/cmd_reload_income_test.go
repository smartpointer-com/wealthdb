package main

import (
	"database/sql"
	"strings"
	"testing"
)

// TestReloadFreshCarriesThePayerStore is the merchant-store carry read
// for the second family. `reload -a` builds an empty file and swaps it
// over the live one, and a verdict store is the one thing in gold a
// rebuild cannot regenerate: it was paid for and has no config backup.
// A store left out of the carry list is lost silently, so the carry is
// pinned per store rather than once.
func TestReloadFreshCarriesThePayerStore(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)

	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	if _, err := db.Exec(`
        INSERT INTO income_payer_categories (payer_signature, payer_name, income_detailed,
                                             signature_version, assigned_at, model_name)
        VALUES ('sig-payroll', 'Blue Harbour Payroll', 'INCOME_WAGES', 1, 100, 'test-model')`); err != nil {
		t.Fatalf("seed payer store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	so, se, code := run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	// A line of its own, per store: without it a rebuild that carried
	// nothing reads exactly like one that carried everything.
	if !strings.Contains(so, "carried 1 payer verdict(s) across the rebuild") {
		t.Errorf("reload -a did not report the payer carry-across:\n%s", so)
	}

	ro, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("reopen gold: %v", err)
	}
	defer ro.Close()
	var name, detailed, model string
	var version int
	if err := ro.QueryRow(`
        SELECT payer_name, income_detailed, signature_version, model_name
          FROM income_payer_categories WHERE payer_signature = 'sig-payroll'`).
		Scan(&name, &detailed, &version, &model); err != nil {
		t.Fatalf("the payer verdict did not survive the rebuild: %v", err)
	}
	if name != "Blue Harbour Payroll" || detailed != "INCOME_WAGES" {
		t.Errorf("carried verdict = (%q, %q)", name, detailed)
	}
	// model_name and the signature version travel verbatim: a carried
	// verdict must still say which model produced it and under which
	// fold, rather than claiming to be new work.
	if model != "test-model" || version != 1 {
		t.Errorf("carried verdict lost its provenance: model=%q version=%d", model, version)
	}
}
