package gold

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/version"
)

// binaryVersionRows counts the staleness ledger through a read-only
// open, which never stamps, so counting cannot disturb what is being
// counted.
func binaryVersionRows(t *testing.T, path string) int {
	t.Helper()
	db, err := Open(path, ModeReadOnly)
	if err != nil {
		t.Fatalf("open %q read-only: %v", path, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM binary_versions`).Scan(&n); err != nil {
		t.Fatalf("count binary_versions: %v", err)
	}
	return n
}

// TestReopenReadWriteDoesNotStampBinaryVersions pins the whole reason
// ReopenReadWrite exists beside Open.
//
// binary_versions answers one question — which binary last wrote this
// database — and the commands that round-trip a model re-open the file
// once per flush. Stamping those would append a row per batch and bury
// the answer under the same command's own re-entries.
func TestReopenReadWriteDoesNotStampBinaryVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gold.db")

	db, err := Open(path, ModeReadWrite)
	if err != nil {
		t.Fatalf("Open (create): %v", err)
	}
	db.Close()

	base := binaryVersionRows(t, path)

	rw, err := ReopenReadWrite(path)
	if err != nil {
		t.Fatalf("ReopenReadWrite: %v", err)
	}
	// Skipping the audit row is the only thing it skips: the handle is
	// still read-write, which is the whole point of re-taking it.
	if _, err := rw.Exec(`CREATE TABLE reopen_probe (n INTEGER)`); err != nil {
		t.Errorf("ReopenReadWrite handle rejects a write: %v", err)
	} else if _, err := rw.Exec(`DROP TABLE reopen_probe`); err != nil {
		t.Errorf("drop the probe table: %v", err)
	}
	rw.Close()
	if got := binaryVersionRows(t, path); got != base {
		t.Errorf("binary_versions rows after ReopenReadWrite = %d, want %d (a re-open is the same command continuing)",
			got, base)
	}

	full, err := Open(path, ModeReadWrite)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	full.Close()

	// The audit row is a no-op for a binary with no VCS stamp, which
	// is the normal state of a test binary — so the increment is
	// asserted only where it can happen at all. The invariance above
	// holds either way, and is the property under test. Which is why
	// the row count is not the whole test: TestOpenStampsAndReopenDoesNot
	// watches the call rather than its effect.
	want := base
	if version.Build().CommitAt == 0 {
		t.Log("binary carries no VCS stamp; Open's audit row is a no-op here")
	} else {
		want = base + 1
	}
	if got := binaryVersionRows(t, path); got != want {
		t.Errorf("binary_versions rows after Open = %d, want %d", got, want)
	}
}

// TestOpenStampsAndReopenDoesNot pins the audit stamp where a row
// count cannot reach it.
//
// recordBinaryOpen writes nothing when the binary carries no VCS
// stamp, which is the normal state of a test binary — so counting
// binary_versions rows says the same thing whether Open stamps or
// not, and the invariance half of the test above is vacuous under the
// runner that actually runs it. Swapping the recorder for a counter
// asks the question directly: a read-write Open records once, a
// read-only Open never, and ReopenReadWrite — the re-take a model
// command makes once per flush, the whole reason it exists beside
// Open — never either.
func TestOpenStampsAndReopenDoesNot(t *testing.T) {
	calls := 0
	restore := recordBinaryOpen
	recordBinaryOpen = func(context.Context, *sql.DB) error {
		calls++
		return nil
	}
	t.Cleanup(func() { recordBinaryOpen = restore })

	path := filepath.Join(t.TempDir(), "gold.db")
	db, err := Open(path, ModeReadWrite)
	if err != nil {
		t.Fatalf("Open (create): %v", err)
	}
	db.Close()
	if calls != 1 {
		t.Fatalf("a read-write Open recorded %d time(s), want 1", calls)
	}

	ro, err := Open(path, ModeReadOnly)
	if err != nil {
		t.Fatalf("Open read-only: %v", err)
	}
	ro.Close()
	if calls != 1 {
		t.Errorf("a read-only Open recorded; calls = %d, want 1", calls)
	}

	for i := 0; i < 3; i++ {
		rw, err := ReopenReadWrite(path)
		if err != nil {
			t.Fatalf("ReopenReadWrite %d: %v", i, err)
		}
		rw.Close()
	}
	if calls != 1 {
		t.Errorf("ReopenReadWrite recorded; calls = %d, want 1 — a re-open is the same command continuing", calls)
	}
}
