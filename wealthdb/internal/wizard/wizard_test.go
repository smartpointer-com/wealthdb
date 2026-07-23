package wizard

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newSilverFixture writes a minimal SQLite with a `dump_runs`
// table so probeSilverDB succeeds against it.
func newSilverFixture(t *testing.T, name string) string {
	return newSilverFixtureTable(t, name, "dump_runs")
}

// newSilverFixtureTable writes a minimal SQLite carrying the named
// run-tracking table (`dump_runs` for collector silvers, `load_runs`
// for load-only silvers such as manual).
func newSilverFixtureTable(t *testing.T, name, table string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ` + table + ` (snapshot_at INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path
}

// scriptedInput returns an io.Reader that yields the given lines.
func scriptedInput(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func TestRunHappyPath(t *testing.T) {
	// Real adapters auto-register via blank imports in
	// production; for tests we don't import them, so kind
	// validation is skipped (per wizard.go's "no registered
	// adapters → skip" branch). Use one with no kind validation.
	silverPath := newSilverFixture(t, "test.db")
	configPath := filepath.Join(t.TempDir(), "wealthdb.cfg")

	stdin := scriptedInput(
		"",          // gold_db: accept default
		"",          // default_currency: accept default
		"my-source", // id
		"schwab",    // kind (anything is fine since no registry)
		silverPath,  // path
		"n",         // no more sources
	)
	var stdout bytes.Buffer

	def := Defaults{
		GoldDB:          filepath.Join(t.TempDir(), "wealthdb.db"),
		DefaultCurrency: "USD",
	}
	res, err := Run(stdin, &stdout, configPath, def)
	if err != nil {
		t.Fatalf("Run: %v\nstdout: %s", err, stdout.String())
	}

	if res.Config.GoldDB != def.GoldDB {
		t.Errorf("GoldDB = %q, want default %q", res.Config.GoldDB, def.GoldDB)
	}
	if res.Config.DefaultCurrency != "USD" {
		t.Errorf("DefaultCurrency = %q, want USD", res.Config.DefaultCurrency)
	}
	if len(res.Config.SilverSources) != 1 {
		t.Fatalf("silver_sources len = %d, want 1", len(res.Config.SilverSources))
	}
	s := res.Config.SilverSources[0]
	if s.ID != "my-source" || s.Kind != "schwab" || s.Path != silverPath {
		t.Errorf("silver source = %+v", s)
	}
	if !strings.Contains(stdout.String(), "Setup complete") {
		t.Errorf("missing 'Setup complete' summary: %s", stdout.String())
	}
}

func TestRunMultipleSources(t *testing.T) {
	sp1 := newSilverFixture(t, "s1.db")
	sp2 := newSilverFixture(t, "s2.db")

	stdin := scriptedInput(
		"/tmp/gold.db",
		"CHF",
		"source-a", "ubs", sp1,
		"y", // add another
		"source-b", "schwab", sp2,
		"n", // done
	)
	var stdout bytes.Buffer
	res, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{})
	if err != nil {
		t.Fatalf("Run: %v\nstdout: %s", err, stdout.String())
	}
	if len(res.Config.SilverSources) != 2 {
		t.Fatalf("want 2 sources, got %d", len(res.Config.SilverSources))
	}
	if res.Config.SilverSources[0].ID != "source-a" || res.Config.SilverSources[1].ID != "source-b" {
		t.Errorf("source order = %+v", res.Config.SilverSources)
	}
}

func TestRunRejectsBadCurrency(t *testing.T) {
	sp := newSilverFixture(t, "x.db")
	// Type a bad currency, then a good one, then continue normally.
	stdin := scriptedInput(
		"/tmp/gold.db",
		"usd",    // bad: not uppercase
		"DOLLAR", // bad: too long
		"USD",    // good
		"x", "schwab", sp, "n",
	)
	var stdout bytes.Buffer
	if _, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{}); err != nil {
		t.Fatalf("Run: %v\nstdout: %s", err, stdout.String())
	}
	// The output should show two rejections before acceptance.
	if c := strings.Count(stdout.String(), "not a 3-letter uppercase code"); c != 2 {
		t.Errorf("expected 2 rejection lines, got %d", c)
	}
}

func TestRunRejectsBadSilverPath(t *testing.T) {
	// Point at a file that doesn't exist.
	missing := filepath.Join(t.TempDir(), "missing.db")
	good := newSilverFixture(t, "good.db")

	stdin := scriptedInput(
		"/tmp/gold.db", "USD",
		"x", "schwab",
		missing, // bad path
		good,    // retry: good path
		"n",
	)
	var stdout bytes.Buffer
	res, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{})
	if err != nil {
		t.Fatalf("Run: %v\nstdout: %s", err, stdout.String())
	}
	if res.Config.SilverSources[0].Path != good {
		t.Errorf("expected retry to use good path, got %q", res.Config.SilverSources[0].Path)
	}
	if !strings.Contains(stdout.String(), "does not exist") {
		t.Errorf("missing 'does not exist' rejection: %s", stdout.String())
	}
}

func TestRunRejectsNotSilverDB(t *testing.T) {
	// SQLite file without a dump_runs or load_runs table.
	path := filepath.Join(t.TempDir(), "notsilver.db")
	db, _ := sql.Open("sqlite", "file:"+path)
	db.Exec(`CREATE TABLE foo (x INTEGER)`)
	db.Close()
	good := newSilverFixture(t, "good.db")

	stdin := scriptedInput(
		"/tmp/gold.db", "USD",
		"x", "schwab",
		path, // bad: SQLite but no run-tracking table
		good,
		"n",
	)
	var stdout bytes.Buffer
	if _, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{}); err != nil {
		t.Fatalf("Run: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "neither a `dump_runs` nor a `load_runs`") {
		t.Errorf("expected run-table guidance: %s", stdout.String())
	}
}

func TestRunAcceptsLoadRunsSilver(t *testing.T) {
	// Load-only silvers (e.g. manual) have load_runs instead of
	// dump_runs; the probe must accept them.
	silverPath := newSilverFixtureTable(t, "manual.db", "load_runs")
	stdin := scriptedInput(
		"/tmp/gold.db", "USD",
		"m", "manual", silverPath, "n",
	)
	var stdout bytes.Buffer
	res, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, stdout.String())
	}
	if res.Config.SilverSources[0].Path != silverPath {
		t.Errorf("expected load_runs silver accepted, got %+v", res.Config.SilverSources)
	}
}

func TestRunRejectsDuplicateID(t *testing.T) {
	sp := newSilverFixture(t, "x.db")
	stdin := scriptedInput(
		"/tmp/gold.db", "USD",
		"id-a", "schwab", sp, "y",
		"id-a", // dup
		"id-b", "schwab", sp, "n",
	)
	var stdout bytes.Buffer
	res, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "already in use") {
		t.Errorf("expected dup-id rejection: %s", stdout.String())
	}
	if res.Config.SilverSources[1].ID != "id-b" {
		t.Errorf("second source should be id-b, got %q", res.Config.SilverSources[1].ID)
	}
}
