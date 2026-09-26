package main

import (
	"database/sql"
	"strings"
	"testing"
)

// seedBothStores puts one verdict in each store at the SAME signature —
// one counterparty that is both a merchant and a payer, which is the
// case the family selector and the two-store --forget exist for.
func seedBothStores(t *testing.T, goldPath string) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
        INSERT OR REPLACE INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
        VALUES ('EXAMPLE TRADING AG', 'Example Trading AG', 'GENERAL_SERVICES_CONSULTING_AND_LEGAL',
                1, 100, 'test-model');
        INSERT OR REPLACE INTO income_payer_categories (payer_signature, payer_name, income_detailed,
                                             signature_version, assigned_at, model_name)
        VALUES ('EXAMPLE TRADING AG', 'Example Trading AG', 'INCOME_SELF_EMPLOYMENT',
                1, 100, 'test-model');`); err != nil {
		t.Fatalf("seed both stores: %v", err)
	}
}

// TestCategorizationsNamesOneFamily pins the positional selector on the
// dump: absent lists both stores with a family column, a name lists
// that one alone.
func TestCategorizationsNamesOneFamily(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	seedBothStores(t, goldPathFromCfg(cfg))

	both, se, code := run(t, "-c", cfg, "categorizations")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(both, "spending") || !strings.Contains(both, "income") {
		t.Errorf("the bare dump does not carry both families:\n%s", both)
	}
	if !strings.Contains(both, "GENERAL_SERVICES_CONSULTING_AND_LEGAL") ||
		!strings.Contains(both, "INCOME_SELF_EMPLOYMENT") {
		t.Errorf("the bare dump is missing a store's verdict:\n%s", both)
	}

	only, se, code := run(t, "-c", cfg, "categorizations", "income")
	if code != 0 {
		t.Fatalf("categorizations income exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(only, "INCOME_SELF_EMPLOYMENT") {
		t.Errorf("the income dump lost its own verdict:\n%s", only)
	}
	if strings.Contains(only, "GENERAL_SERVICES_CONSULTING_AND_LEGAL") {
		t.Errorf("the income dump carries a spending verdict:\n%s", only)
	}

	if _, _, code := run(t, "-c", cfg, "categorizations", "payers"); code != 2 {
		t.Errorf("an unknown family exit=%d, want 2", code)
	}
	if _, _, code := run(t, "-c", cfg, "categorizations", "income", "extra"); code != 2 {
		t.Errorf("a second positional exit=%d, want 2", code)
	}
}

// TestCategorizationsForgetSpansBothStores pins the decision that a
// signature names a COUNTERPARTY: with no family it is forgotten
// everywhere, with one only there. Naming a family is the only way to
// retire one wrong verdict without destroying the other, which was paid
// for separately.
func TestCategorizationsForgetSpansBothStores(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	goldPath := goldPathFromCfg(cfg)
	seedBothStores(t, goldPath)

	count := func(table string) int {
		t.Helper()
		db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
		if err != nil {
			t.Fatalf("open gold: %v", err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// One family named: only that store loses the row.
	so, se, code := run(t, "-c", cfg, "categorizations", "income",
		"--forget", "EXAMPLE TRADING AG")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "(income)") {
		t.Errorf("the removal does not say which store it came from:\n%s", so)
	}
	if got := count("income_payer_categories"); got != 0 {
		t.Errorf("the payer verdict survived a named --forget (%d rows)", got)
	}
	if got := count("spend_merchant_categories"); got != 1 {
		t.Errorf("a named --forget removed the OTHER family's verdict (%d rows left)", got)
	}

	// No family: both stores, and each removal names its own.
	seedBothStores(t, goldPath)
	so, se, code = run(t, "-c", cfg, "categorizations", "--forget", "EXAMPLE TRADING AG")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "(spending)") || !strings.Contains(so, "(income)") {
		t.Errorf("a bare --forget did not report per store:\n%s", so)
	}
	if got := count("income_payer_categories"); got != 0 {
		t.Errorf("payer store not cleared (%d rows)", got)
	}

	// A signature in neither store says so once, not once per store.
	so, _, code = run(t, "-c", cfg, "categorizations", "--forget", "NOT A SIGNATURE")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if n := strings.Count(so, "no verdict stored at"); n != 1 {
		t.Errorf("an absent signature reported %d times, want 1:\n%s", n, so)
	}
}
