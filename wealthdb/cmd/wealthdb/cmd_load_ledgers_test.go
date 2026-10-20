package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// A declaration the build refuses stops load and reload before gold is
// opened read-write, as a stale pin does, and names its line.
func TestBadDeclarationFailsBeforeGoldIsOpened(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"load", "schwab-test"}, {"reload", "schwab-test"}, {"reload", "-a"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			cfgPath := setupCLITest(t)
			dir := filepath.Dir(cfgPath)
			ledger := filepath.Join(dir, "exposure_declarations.ndjson")
			if err := os.WriteFile(ledger, []byte(`{"silver_source_id":"schwab-test","instrument":"QPRIV","country":{"CH":100}}`+"\n"+
				`{"silver_source_id":"schwab-test","instrument":"QPRIV2","region":{"europe":60,"rest":30}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeCLIConfig(t, cfgPath, fmt.Sprintf(`, "exposure": {"declarations": %q}`, ledger))
			if _, se, code := run(t, "-c", cfgPath, "init"); code != 0 {
				t.Fatalf("init failed: %s", se)
			}
			goldPath := goldPathFromCfg(cfgPath)
			behind := putGoldOneMigrationBehind(t, goldPath)
			so, se, code := run(t, append([]string{"-c", cfgPath}, args...)...)
			if code == 0 || !strings.Contains(se, "exposure.declarations") || !strings.Contains(se, "line 2: region") {
				t.Fatalf("exit %d, stderr:\n%s", code, se)
			}
			version, loads, sources := goldState(t, goldPath)
			if version != behind || loads != 0 || sources != 0 || strings.Contains(so, "snapshot row(s)") {
				t.Errorf("gold at %d (want %d), %d loads, %d sources: opened before the ledger failed", version, behind,
					loads, sources)
			}
		})
	}
}

// A declaration whose holding no source states loads, and the load warns
// with its line: the record declares nothing. Past ten lines the warning
// cuts the list short. The load says what it stamped and what the
// exposure pass built.
func TestLoadWarnsOfADeclarationNamingNoHolding(t *testing.T) {
	t.Parallel()
	for records, lines := range map[int]string{
		1:  "1 declaration(s) name a holding no source states (exposure.declarations line 1)",
		12: "12 declaration(s) name a holding no source states (exposure.declarations line 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, …)",
	} {
		t.Run(strconv.Itoa(records), func(t *testing.T) {
			t.Parallel()
			cfgPath := setupCLITest(t)
			dir := filepath.Dir(cfgPath)
			ledger := filepath.Join(dir, "exposure_declarations.ndjson")
			var body strings.Builder
			for i := range records {
				fmt.Fprintf(&body, `{"silver_source_id":"schwab-test","instrument":"QNOPE%d","country":{"CH":100}}`+"\n", i)
			}
			if err := os.WriteFile(ledger, []byte(body.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			writeCLIConfig(t, cfgPath, fmt.Sprintf(`, "exposure": {"declarations": %q}`, ledger))
			if _, se, code := run(t, "-c", cfgPath, "init"); code != 0 {
				t.Fatalf("init failed: %s", se)
			}
			so, se, code := run(t, "-c", cfgPath, "load", "schwab-test")
			if code != 0 || !strings.Contains(se, lines) {
				t.Errorf("exit %d, stderr:\n%s", code, se)
			}
			if want := fmt.Sprintf("load: %d exposure entr(ies) stamped", records); !strings.Contains(so, want) {
				t.Errorf("stdout lacks %q:\n%s", want, so)
			}
			if !regexp.MustCompile(`(?m)^exposure: [1-9][0-9]* securities, [0-9]+ aliases, [0-9]+ unmapped label\(s\)$`).
				MatchString(so) {
				t.Errorf("stdout lacks the exposure pass's summary:\n%s", so)
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
	writeCLIConfig(t, cfgPath, fmt.Sprintf(`,
        "spending": {"categorization": {"model":
            {"name": "test-model", "baseUrl": "http://127.0.0.1:1/v1", "api": "openai-completions"}}},
        "income": {"pins": %q}`, pins))
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
