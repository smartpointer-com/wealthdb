package gold

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestBuildFreshAndSwapAbortsOnBadBuild is the load-bearing safety
// check: if build() produces a file that fails verification (here, a
// valid DuckDB that lacks the gold schema), BuildFreshAndSwap must
// return an error, leave the live gold DB byte-for-byte untouched, and
// remove the temp — it must NEVER swap an unverified file into place.
func TestBuildFreshAndSwapAbortsOnBadBuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "gold.db")

	// A real, migrated live gold DB (has positions/accounts).
	db, err := OpenFresh(goldPath)
	if err != nil {
		t.Fatalf("open live gold: %v", err)
	}
	db.Close()
	liveBefore, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("read live gold: %v", err)
	}

	// build() writes a valid DuckDB file with none of the gold tables,
	// so verifyGold's smoke query fails.
	_, err = BuildFreshAndSwap(context.Background(), goldPath, func(tmp string) error {
		d, e := sql.Open("duckdb", tmp)
		if e != nil {
			return e
		}
		defer d.Close()
		_, e = d.Exec("CREATE TABLE unrelated (x INTEGER)")
		return e
	})
	if err == nil {
		t.Fatal("expected BuildFreshAndSwap to fail verification and abort the swap")
	}

	liveAfter, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("re-read live gold: %v", err)
	}
	if !bytes.Equal(liveBefore, liveAfter) {
		t.Errorf("live gold changed after an aborted swap (before=%d, after=%d bytes)",
			len(liveBefore), len(liveAfter))
	}
	if matches, _ := filepath.Glob(goldPath + ".rebuild-*"); len(matches) != 0 {
		t.Errorf("aborted swap left temp files: %v", matches)
	}
}

// stubChmod replaces carryMode's chmod with one that fails with
// refusals errors of kind, then works, and counts its calls. The waits
// between tries are cut to nothing.
func stubChmod(t *testing.T, refusals int, kind error) *int {
	t.Helper()
	calls := 0
	realChmod, realBackoff := chmod, chmodBackoff
	chmod = func(path string, mode os.FileMode) error {
		calls++
		if calls <= refusals {
			return &os.PathError{Op: "chmod", Path: path, Err: kind}
		}
		return realChmod(path, mode)
	}
	chmodBackoff = 0
	t.Cleanup(func() { chmod, chmodBackoff = realChmod, realBackoff })
	return &calls
}

// A rebuilt file that has the live file's mode is not changed at all: a
// virtual machine's file share may refuse the owner's chmod of a file
// just written.
func TestCarryModeLeavesAMatchingModeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gold.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := stubChmod(t, 1000, syscall.EPERM)
	if err := carryMode(path, 0o600); err != nil {
		t.Fatalf("a matching mode: %v", err)
	}
	if *calls != 0 {
		t.Errorf("chmod ran %d time(s) on a file that had the mode", *calls)
	}
}

// A chmod refused for want of permission is tried again, and gives up
// after its bound. Any other failure is returned at once.
func TestCarryModeTriesARefusedChmodAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gold.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := stubChmod(t, 3, syscall.EPERM)
	if err := carryMode(path, 0o640); err != nil {
		t.Fatalf("three refusals, then a chmod that works: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v (%v), want 0640", fi.Mode().Perm(), err)
	}
	if *calls != 4 {
		t.Errorf("chmod ran %d time(s), want 4", *calls)
	}

	calls = stubChmod(t, 1000, syscall.EPERM)
	if err := carryMode(path, 0o600); !errors.Is(err, os.ErrPermission) {
		t.Errorf("a chmod refused every time: %v", err)
	}
	if *calls != chmodTries+1 {
		t.Errorf("chmod ran %d time(s), want %d", *calls, chmodTries+1)
	}

	calls = stubChmod(t, 1000, syscall.EROFS)
	if err := carryMode(path, 0o600); !errors.Is(err, syscall.EROFS) {
		t.Errorf("a read-only file system: %v", err)
	}
	if *calls != 1 {
		t.Errorf("chmod ran %d time(s) on a failure that waiting cannot mend, want 1", *calls)
	}
}
