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
//
// The carry also respells. A file no read-write open has migrated
// since a retirement still holds the retired spellings in its store.
// The rebuilt file was migrated before the rows arrived, so nothing
// else would move them to their successors, and a verdict on a retired
// value is one no report can show.
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

	// Written straight into the store, past the migration that would
	// have respelled them, the way a file from before it holds them.
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	if _, err := db.Exec(`
        INSERT INTO income_payer_categories (payer_signature, payer_name, income_detailed,
                                             signature_version, assigned_at, model_name) VALUES
            ('sig-payroll',     'Blue Harbour Payroll',  'INCOME_WAGES',                     1, 100, 'test-model'),
            ('sig-maintenance', 'Example Maintenance',   'INCOME_ALIMONY_AND_CHILD_SUPPORT', 1, 200, 'test-model'),
            ('sig-letting',     'Example Letting Agent', 'INCOME_RENTAL',                    1, 300, 'test-model')`); err != nil {
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
	if !strings.Contains(so, "carried 3 payer verdict(s) across the rebuild") {
		t.Errorf("reload -a did not report the payer carry-across:\n%s", so)
	}
	if !strings.Contains(so, "respelled 2 carried payer verdict(s)") {
		t.Errorf("reload -a did not report the respelled verdicts:\n%s", so)
	}

	ro, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("reopen gold: %v", err)
	}
	defer ro.Close()
	for _, want := range []struct {
		sig, name, detailed string
		assignedAt          int64
	}{
		// A rename moves to the one value that replaced it.
		{"sig-payroll", "Blue Harbour Payroll", "INCOME_SALARY", 100},
		// A split moves to the successor stored data takes. A stored
		// verdict cannot say which half it meant; a rule or a pin
		// places a payer at the other one.
		{"sig-maintenance", "Example Maintenance", "INCOME_CHILD_SUPPORT", 200},
		// A current value is carried verbatim.
		{"sig-letting", "Example Letting Agent", "INCOME_RENTAL", 300},
	} {
		var name, detailed, model string
		var version int
		var assignedAt int64
		if err := ro.QueryRow(`
            SELECT payer_name, income_detailed, signature_version, assigned_at, model_name
              FROM income_payer_categories WHERE payer_signature = ?`, want.sig).
			Scan(&name, &detailed, &version, &assignedAt, &model); err != nil {
			t.Fatalf("the %s verdict did not survive the rebuild: %v", want.sig, err)
		}
		if name != want.name || detailed != want.detailed {
			t.Errorf("%s: carried verdict = (%q, %q), want (%q, %q)",
				want.sig, name, detailed, want.name, want.detailed)
		}
		// Every other column travels verbatim: a carried verdict must
		// still say which model produced it, when, and under which
		// fold, rather than claiming to be new work. The respell
		// changes the value and nothing else.
		if model != "test-model" || version != 1 || assignedAt != want.assignedAt {
			t.Errorf("%s: carried verdict lost its provenance: model=%q version=%d assigned_at=%d",
				want.sig, model, version, assignedAt)
		}
	}
}
