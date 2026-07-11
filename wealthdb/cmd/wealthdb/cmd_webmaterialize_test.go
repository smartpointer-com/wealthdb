package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// TestWebMaterializeCLIEndToEnd drives the hidden web-materialize subcommand
// against the synthetic returns fixture and checks the report_returns table
// carries the full grain/granularity matrix in the currencies the fixture's
// FX rates can resolve (no EUR rate is seeded, so EUR partitions are empty).
func TestWebMaterializeCLIEndToEnd(t *testing.T) {
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

	goldPath := filepath.Join(filepath.Dir(cfg), "wealthdb.db")
	db, err := gold.Open(goldPath, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()

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
}

// TestWebMaterializeHiddenButDispatched keeps the subcommand out of the help
// listing while `wealthdb web-materialize` still dispatches (the web-config
// precedent).
func TestWebMaterializeHiddenButDispatched(t *testing.T) {
	_, se, code := run(t, "help")
	if code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	if !strings.Contains(se, "returns") {
		t.Fatalf("help listing looks wrong:\n%s", se)
	}
	if strings.Contains(se, "web-materialize") {
		t.Errorf("help should not list web-materialize:\n%s", se)
	}
	// Dispatch reaches the handler (config load fails, not "unknown subcommand").
	_, se, code = run(t, "-c", "/nonexistent/wealthdb.cfg", "web-materialize")
	if code == 0 || strings.Contains(se, "unknown subcommand") {
		t.Errorf("web-materialize did not dispatch: exit=%d stderr=%s", code, se)
	}
}

func TestWebMaterializeRequiresWritableGold(t *testing.T) {
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
