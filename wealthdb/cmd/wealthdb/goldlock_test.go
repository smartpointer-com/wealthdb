package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
)

// The write mutex is what stands between a rebuild-and-swap and a
// write into the inode the swap is about to unlink, so all four of
// its properties are pinned: it excludes a second writer, it names
// the sidecar when it does, it is released by unlock, and it lets
// readers through untouched.

// TestGoldWriteLockExcludesASecondWriter checks the refusal itself,
// its exit code and its wording. The message has to name the sidecar:
// it is the only file that explains why a command that touched
// nothing refused to run.
func TestGoldWriteLockExcludesASecondWriter(t *testing.T) {
	t.Parallel()
	goldPath := filepath.Join(t.TempDir(), "wealthdb.db")
	sidecar := goldPath + goldWriteLockSuffix

	held, err := lockGoldForWrite(goldPath, "categorize")
	if err != nil {
		t.Fatalf("first take: %v", err)
	}

	_, err = lockGoldForWrite(goldPath, "compact")
	if err == nil {
		t.Fatal("second take succeeded; the mutex excludes nothing")
	}
	var exit *errs.ExitError
	if !errors.As(err, &exit) || exit.Code != errs.ExitOpenFailed {
		t.Errorf("second take error = %v, want an ExitError with code %d", err, errs.ExitOpenFailed)
	}
	if !strings.Contains(err.Error(), sidecar) {
		t.Errorf("refusal does not name the sidecar %q:\n%s", sidecar, err)
	}
	if !strings.Contains(err.Error(), "compact") {
		t.Errorf("refusal does not name the refused command:\n%s", err)
	}

	// unlock hands the mutex on rather than poisoning the file.
	held.unlock()
	next, err := lockGoldForWrite(goldPath, "compact")
	if err != nil {
		t.Fatalf("take after unlock: %v", err)
	}
	next.unlock()
}

// TestGoldWriteLockLetsReadersThrough is the other half of the
// contract: the mutex exists to order writers, and a reader can lose
// nothing by running beside one. A read command shut out by it would
// be a worse regression than the race it prevents, because a model
// run can hold the mutex for an hour.
func TestGoldWriteLockLetsReadersThrough(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}

	held, err := lockGoldForWrite(goldPathFromCfg(cfg), "categorize")
	if err != nil {
		t.Fatalf("take the mutex: %v", err)
	}
	defer held.unlock()

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"holdings positions", []string{"holdings", "positions"}},
	} {
		if _, se, code := run(t, append([]string{"-c", cfg}, tc.args...)...); code != 0 {
			t.Errorf("%s while the write mutex is held: exit %d, stderr=%s", tc.name, code, se)
		}
	}

	// A write command, by contrast, is refused — and says why.
	_, se, code := run(t, "-c", cfg, "load", "schwab-test")
	if code != errs.ExitOpenFailed {
		t.Errorf("load while the write mutex is held: exit %d, want %d", code, errs.ExitOpenFailed)
	}
	if !strings.Contains(se, goldWriteLockSuffix) {
		t.Errorf("refusal does not name the sidecar:\n%s", se)
	}
}

// TestGoldWriteLockSidecarSurvivesTheSwap pins the sidecar's
// independence from the file it guards. 'reload -a' renames a
// rebuilt file over the live path; a mutex kept on the live inode
// would be replaced along with it, and the next command would take a
// lock on a file nothing else can see.
func TestGoldWriteLockSidecarSurvivesTheSwap(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	goldPath := goldPathFromCfg(cfg)
	sidecar := goldPath + goldWriteLockSuffix

	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	before, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("a write command left no sidecar at %q: %v", sidecar, err)
	}

	if _, se, code := run(t, "-c", cfg, "reload", "-a"); code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	after, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("the sidecar did not survive the reload -a swap: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the sidecar was replaced by the swap; the next writer would lock a different file")
	}
	held, err := lockGoldForWrite(goldPath, "load")
	if err != nil {
		t.Fatalf("take the mutex after the swap: %v", err)
	}
	held.unlock()
}
