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
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY)`); err != nil {
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
	// SQLite file without the expected dump_runs table.
	path := filepath.Join(t.TempDir(), "notsilver.db")
	db, _ := sql.Open("sqlite", "file:"+path)
	db.Exec(`CREATE TABLE foo (x INTEGER)`)
	db.Close()
	good := newSilverFixture(t, "good.db")

	stdin := scriptedInput(
		"/tmp/gold.db", "USD",
		"x", "schwab",
		path, // bad: SQLite but no dump_runs
		good,
		"n",
	)
	var stdout bytes.Buffer
	if _, err := Run(stdin, &stdout, "/tmp/wealthdb.cfg", Defaults{}); err != nil {
		t.Fatalf("Run: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "missing the `dump_runs`") {
		t.Errorf("expected dump_runs guidance: %s", stdout.String())
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
