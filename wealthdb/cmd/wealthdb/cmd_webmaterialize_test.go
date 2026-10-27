package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

// TestWebMaterializeCLIEndToEnd drives the hidden web-materialize subcommand
// against the synthetic returns fixture and checks the report_returns table
// carries the full grain/granularity matrix in the currencies the fixture's
// FX rates can resolve (no EUR rate is seeded, so EUR partitions are empty),
// and that report_exposure holds the history in those currencies.
func TestWebMaterializeCLIEndToEnd(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)

	so, se, code := run(t, "-c", cfg, "web-materialize")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	if so != "" {
		t.Errorf("stdout should be empty, got:\n%s", so)
	}
	if !strings.Contains(se, "materialized") {
		t.Errorf("stderr missing the materialize summary line: %s", se)
	}
	for _, phase := range []string{"key", "open", "serving", "returns", "gains", "exposure", "yield", "close"} {
		if !strings.Contains(se, "timing: "+phase+" ") {
			t.Errorf("stderr missing timing for %s: %s", phase, se)
		}
	}

	_, se2, code2 := run(t, "-c", cfg, "web-materialize")
	if code2 != 0 {
		t.Fatalf("second run exit=%d stderr=%s", code2, se2)
	}
	if !strings.Contains(se2, "timing: key ") || strings.Contains(se2, "timing: open ") {
		t.Errorf("a skipped run times its key check and opens nothing, stderr=%s", se2)
	}
	if !strings.Contains(se2, "inputs unchanged") {
		t.Errorf("second run should skip, stderr=%s", se2)
	}

	goldPath := filepath.Join(filepath.Dir(cfg), "wealthdb.db")
	db, err := gold.Open(goldPath, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()

	// The stored key hashes the config's bytes.
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := gold.HashInputs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stored, ok, err := gold.ReadMaterializeKey(context.Background(), db, gold.WebMaterializeScope); err != nil || !ok || stored.ConfigHash != wantHash {
		t.Fatalf("stored key %+v ok=%v err=%v, want config hash %s", stored, ok, err, wantHash)
	}

	// Per currency, so a missing CHF slice can't hide behind the USD one.
	for _, ccy := range []string{"USD", "CHF"} {
		var partitions, total int
		if err := db.QueryRow(`
			SELECT COUNT(DISTINCT grain || '/' || granularity), COUNT(*)
			  FROM report_returns WHERE currency = ?`, ccy).
			Scan(&partitions, &total); err != nil {
			t.Fatalf("count report_returns (%s): %v", ccy, err)
		}
		if partitions != 16 {
			t.Errorf("%s: got %d grain/granularity partitions, want 16 (4 grains x 4 periods)", ccy, partitions)
		}
		if total == 0 {
			t.Errorf("%s partitions are empty after web-materialize", ccy)
		}
	}
	var eur int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM report_returns WHERE currency = 'EUR'`).Scan(&eur); err != nil {
		t.Fatalf("count EUR rows: %v", err)
	}
	if eur != 0 {
		t.Errorf("EUR partitions carry %d rows despite the fixture seeding no EUR rate", eur)
	}

	// The exposure history reaches today, in each currency a rate resolves.
	if !strings.Contains(se, "exposure: materialized") {
		t.Errorf("stderr missing the exposure line: %s", se)
	}
	for _, ccy := range []string{"USD", "CHF"} {
		var instants int
		if err := db.QueryRow(`SELECT COUNT(DISTINCT as_of) FROM report_exposure WHERE currency = ?`, ccy).
			Scan(&instants); err != nil {
			t.Fatalf("count report_exposure (%s): %v", ccy, err)
		}
		if instants < 2 {
			t.Errorf("%s: %d exposure instants, want the month ends and today", ccy, instants)
		}
	}
}

// `wealthdb web-materialize` dispatches although the help listing leaves
// it out (TestUsageListsEverySubcommand): the config load fails, not the
// dispatch.
func TestWebMaterializeDispatches(t *testing.T) {
	t.Parallel()
	_, se, code := run(t, "-c", "/nonexistent/wealthdb.cfg", "web-materialize")
	if code == 0 || strings.Contains(se, "unknown subcommand") {
		t.Errorf("web-materialize did not dispatch: exit=%d stderr=%s", code, se)
	}
}

func TestWebMaterializeRequiresWritableGold(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	_, se, code := run(t, "-r", "-c", cfg, "web-materialize")
	if code == 0 {
		t.Fatal("web-materialize succeeded read-only")
	}
	if !strings.Contains(se, "read-only") {
		t.Errorf("stderr missing read-only explanation: %s", se)
	}
}

func TestWebMaterializeMissingDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": []}`,
		filepath.Join(dir, "missing.db"))
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, se, code := run(t, "-c", cfg, "web-materialize")
	if code != 3 {
		t.Errorf("exit=%d, want 3 (missing DB)", code)
	}
	if !strings.Contains(se, "does not exist") {
		t.Errorf("stderr missing does-not-exist message: %s", se)
	}
}

// Each phase prints its timing line, then its summary on success. A
// failed phase prints its timing line and no row count, and the phases
// after it do not run.
func TestRunMaterializePhasesStopsAtAFailure(t *testing.T) {
	t.Parallel()
	var errb strings.Builder
	summary := func(n int) string { return fmt.Sprintf("materialized %d rows", n) }
	ran := false
	err := runMaterializePhases(&errb, []materializePhase{
		{"serving", func() (int, error) { return 3, nil }, summary},
		{"returns", func() (int, error) { return 5, fmt.Errorf("boom") }, summary},
		{"gains", func() (int, error) { ran = true; return 0, nil }, summary},
	})
	if err == nil || ran {
		t.Fatalf("err=%v, later phase ran=%v", err, ran)
	}
	lines := strings.Split(strings.TrimSuffix(errb.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "timing: serving ") ||
		lines[1] != "materialized 3 rows" || !strings.HasPrefix(lines[2], "timing: returns ") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestEngineVersionMatchesThisExecutable(t *testing.T) {
	t.Parallel()
	first := engineVersion()
	if !strings.Contains(first, version.String()) {
		t.Fatalf("engine version %q does not carry %q", first, version.String())
	}
	if second := engineVersion(); second != first {
		t.Fatalf("one process produced two engine versions: %q and %q", first, second)
	}
}

// The transfer-overrides ledger is read beside the config when transfer
// matching is on, so an edit to it alone makes the next run materialize.
func TestWebMaterializeRerunsAfterALedgerEdit(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	dir := filepath.Dir(cfg)
	ledger := filepath.Join(dir, "transfer-overrides.csv")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": [],
 "returns_transfer_matching": {"enabled": true},
 "spending": {"transfer_overrides": %q}}`, filepath.Join(dir, "wealthdb.db"), ledger)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	header := "verb,silver_source_id,account,occurred_at,amount,currency,note\n"
	if err := os.WriteFile(ledger, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{false, true} {
		if _, se, code := run(t, "-c", cfg, "web-materialize"); code != 0 || strings.Contains(se, "inputs unchanged") != want {
			t.Fatalf("run %d: exit=%d skipped=%v, want skipped=%v: %s", i, code, strings.Contains(se, "inputs unchanged"), want, se)
		}
	}
	edited := header + "unmatch,schwab,BROK1,2024-01-02,100.00,USD,not a transfer\n"
	if err := os.WriteFile(ledger, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "web-materialize"); code != 0 || strings.Contains(se, "inputs unchanged") {
		t.Fatalf("after a ledger edit: exit=%d, want a materialize: %s", code, se)
	}
}

// A gold with a migration still to apply is materialized, never
// skipped: the read-write open applies the migration. The next run
// skips.
func TestWebMaterializeWithAMigrationPending(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	db, err := gold.Open(filepath.Join(filepath.Dir(cfg), "wealthdb.db"), gold.ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_meta WHERE gold_schema_version = (SELECT max(gold_schema_version) FROM schema_meta)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i, want := range []bool{false, true} {
		if _, se, code := run(t, "-c", cfg, "web-materialize"); code != 0 || strings.Contains(se, "inputs unchanged") != want {
			t.Fatalf("run %d: exit=%d skipped=%v, want skipped=%v: %s", i, code, strings.Contains(se, "inputs unchanged"), want, se)
		}
	}
}

// A run that fails partway leaves no key: the phases before the
// failure have committed, so a later run with the earlier inputs
// materializes again rather than match half-rewritten tables.
func TestWebMaterializeAfterARunThatFailedPartway(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	goldPath := filepath.Join(filepath.Dir(cfg), "wealthdb.db")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(stmt string) {
		t.Helper()
		db, err := gold.Open(goldPath, gold.ModeReadWrite)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, se, code := run(t, "-c", cfg, "web-materialize"); code != 0 {
		t.Fatalf("first run: exit=%d %s", code, se)
	}
	// Other inputs, and a run that fails in the exposure phase.
	if err := os.WriteFile(cfg, append(append([]byte{}, body...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	exec(`ALTER TABLE report_exposure RENAME TO report_exposure_aside`)
	if _, se, code := run(t, "-c", cfg, "web-materialize"); code == 0 {
		t.Fatalf("the run without report_exposure succeeded: %s", se)
	}
	exec(`ALTER TABLE report_exposure_aside RENAME TO report_exposure`)
	// Back to the first run's inputs.
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "web-materialize"); code != 0 || strings.Contains(se, "inputs unchanged") {
		t.Fatalf("after a failed run: exit=%d, want a materialize: %s", code, se)
	}
}
