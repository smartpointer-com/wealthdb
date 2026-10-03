package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStalePinFailsBeforeGoldIsOpened pins the order every command that
// runs the enrichment pass keeps: the ledgers are read before gold is
// opened read-write.
//
// The open applies any outstanding migration. A migration that retires
// taxonomy values leaves every pin naming one invalid, so a stale pin
// is the first thing such an upgrade meets. Read after the open, the
// ledger would fail the command only once the file had been migrated
// and every source loaded: a half-done run behind an error about a CSV
// line. Read first, it fails with nothing touched, and the error names
// what to write instead.
//
// The gold file is put one migration behind first, so an open that
// migrated it would show in the version: the assertion is about the
// open, not only about the load. `reload -a` opens the live file read
// only under either order, so for it the assertion is that nothing was
// built: the build reports each source it loaded on stdout.
func TestStalePinFailsBeforeGoldIsOpened(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"load", "schwab-test"},
		{"reload", "schwab-test"},
		{"reload", "-a"},
		{"categorize"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			cfg := writeStalePinConfig(t)
			if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
				t.Fatalf("init failed: %s", se)
			}
			goldPath := goldPathFromCfg(cfg)
			behind := putGoldOneMigrationBehind(t, goldPath)

			so, se, code := run(t, append([]string{"-c", cfg}, args...)...)
			if code == 0 {
				t.Fatal("the command succeeded with a pin naming a retired value")
			}
			// The ledger, the line and the successor: everything the fix
			// needs, from the error alone.
			for _, want := range []string{"income.pins: line 2",
				`"INCOME_WAGES"`, "use INCOME_SALARY, or INCOME_GIG_ECONOMY for gig-platform pay"} {
				if !strings.Contains(se, want) {
					t.Errorf("stderr is missing %q:\n%s", want, se)
				}
			}

			version, loads, sources := goldState(t, goldPath)
			if version != behind {
				t.Errorf("schema version = %d, want %d: gold was opened read-write and migrated", version, behind)
			}
			if loads != 0 || sources != 0 {
				t.Errorf("gold holds %d load_audit and %d silver_sources row(s), want none", loads, sources)
			}
			if strings.Contains(so, "snapshot row(s)") {
				t.Errorf("a source was loaded before the ledger failed:\n%s", so)
			}
		})
	}
}

// writeStalePinConfig writes the CLI fixture with an income pins ledger
// whose one pin names INCOME_WAGES, a value the taxonomy retired, and a
// model block so that `categorize` gets as far as the ledgers. The
// endpoint is unreachable on purpose: nothing here may reach a model.
func writeStalePinConfig(t *testing.T) string {
	t.Helper()
	cfgPath := setupCLITest(t)
	dir := filepath.Dir(cfgPath)
	pins := filepath.Join(dir, "income_pins.csv")
	if err := os.WriteFile(pins, []byte(
		"silver_source_id,account,occurred_at,amount,currency,income_detailed\n"+
			"schwab-test,ACC1,2026-01-05,100.00,USD,INCOME_WAGES\n"), 0o600); err != nil {
		t.Fatalf("write pins: %v", err)
	}
	body := fmt.Sprintf(`{
        "gold_db": %q,
        "default_currency": "USD",
        "silver_sources": [{"id":"schwab-test","kind":"schwab","path":%q}],
        "spending": {"categorization": {"model":
            {"name": "test-model", "baseUrl": "http://127.0.0.1:1/v1", "api": "openai-completions"}}},
        "income": {"pins": %q}
    }`, goldPathFromCfg(cfgPath), filepath.Join(dir, "schwab.db"), pins)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfgPath
}

// putGoldOneMigrationBehind removes the newest schema_meta stamp, so the
// file reads as one migration behind this build and the next read-write
// open re-applies that migration. Returns the version left behind.
func putGoldOneMigrationBehind(t *testing.T, goldPath string) int {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM schema_meta
         WHERE gold_schema_version = (SELECT MAX(gold_schema_version) FROM schema_meta)`); err != nil {
		t.Fatalf("unstamp the newest migration: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(gold_schema_version) FROM schema_meta`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}

// goldState reads what a command that opened gold read-write would have
// changed: the schema version, the load audit and the source registry.
func goldState(t *testing.T, goldPath string) (version, loads, sources int) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(`
        SELECT (SELECT MAX(gold_schema_version) FROM schema_meta),
               (SELECT COUNT(*) FROM load_audit),
               (SELECT COUNT(*) FROM silver_sources)`).Scan(&version, &loads, &sources); err != nil {
		t.Fatalf("read gold state: %v", err)
	}
	return version, loads, sources
}
