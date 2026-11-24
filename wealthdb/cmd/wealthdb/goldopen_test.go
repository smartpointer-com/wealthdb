package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// holdGoldEnv makes this test binary a helper process that holds a
// read-only gold handle: it opens the named file, prints "held", and
// closes it after holdGoldForEnv. DuckDB's file lock is per process,
// so a second process is the only honest way to meet it.
const (
	holdGoldEnv    = "WEALTHDB_TEST_HOLD_GOLD"
	holdGoldForEnv = "WEALTHDB_TEST_HOLD_GOLD_FOR"
)

func TestMain(m *testing.M) {
	if path := os.Getenv(holdGoldEnv); path != "" {
		os.Exit(holdGoldReadOnly(path, os.Getenv(holdGoldForEnv)))
	}
	if os.Getenv(holdLockEnv) != "" {
		os.Exit(runAsEngine())
	}
	os.Exit(m.Run())
}

func holdGoldReadOnly(path, hold string) int {
	d, err := time.ParseDuration(hold)
	if err != nil {
		return 2
	}
	db, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		return 3
	}
	os.Stdout.WriteString("held\n")
	time.Sleep(d)
	_ = db.Close()
	return 0
}

// startGoldReader runs the helper against goldPath and returns once it
// holds its handle.
func startGoldReader(t *testing.T, goldPath string, hold time.Duration) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holdGoldEnv+"="+goldPath, holdGoldForEnv+"="+hold.String())
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("reader helper did not take the handle: %q %v", line, err)
	}
	return cmd
}

// freshGoldFor writes the gold the config at cfgPath names: migrated
// and empty, as init leaves it, but copied from the package's one image
// instead of built by the whole migration chain. A test that checks
// init itself runs init.
func freshGoldFor(t *testing.T, cfgPath string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	db, err := gold.OpenFresh(cfg.GoldDB)
	if err != nil {
		t.Fatalf("create gold: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func freshGold(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wealthdb.db")
	db, err := gold.OpenFresh(path)
	if err != nil {
		t.Fatalf("create gold: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWriteOpenWaitsOutAReader is the write-side half of the live-gold
// contract: a read-write open that meets another process's read-only
// handle retries until the reader lets go, rather than failing the
// write. The ladder is shortened so the test runs in about a second;
// the reader holds past the first retry, so the success is the retry's.
func TestWriteOpenWaitsOutAReader(t *testing.T) {
	path := freshGold(t)
	defer func(l []time.Duration) { goldLockLadder = l }(goldLockLadder)
	goldLockLadder = []time.Duration{0, 300 * time.Millisecond, 600 * time.Millisecond, time.Second}

	startGoldReader(t, path, 700*time.Millisecond)

	// The bare open meets the lock: that is the error the ladder is for.
	if _, err := gold.Open(path, gold.ModeReadWrite); !isGoldLockConflict(err) {
		t.Fatalf("bare read-write open beside a reader: err = %v, want DuckDB's lock conflict", err)
	}

	attempts := 0
	db, err := retryGoldLock(context.Background(), func() (*sql.DB, error) {
		attempts++
		return gold.Open(path, gold.ModeReadWrite)
	})
	if err != nil {
		t.Fatalf("retried open: %v", err)
	}
	defer db.Close()
	if attempts < 2 {
		t.Errorf("succeeded on attempt %d; the reader should have forced a retry", attempts)
	}
}

// TestWriteOpenGivesUpOnALongReader bounds the wait: a reader that
// outlasts the ladder fails the open with DuckDB's own error.
func TestWriteOpenGivesUpOnALongReader(t *testing.T) {
	path := freshGold(t)
	defer func(l []time.Duration) { goldLockLadder = l }(goldLockLadder)
	goldLockLadder = []time.Duration{0, 50 * time.Millisecond, 50 * time.Millisecond}

	startGoldReader(t, path, 2*time.Second)

	attempts := 0
	_, err := retryGoldLock(context.Background(), func() (*sql.DB, error) {
		attempts++
		return gold.Open(path, gold.ModeReadWrite)
	})
	if !isGoldLockConflict(err) {
		t.Fatalf("err = %v, want the lock conflict after the ladder ran out", err)
	}
	if attempts != len(goldLockLadder) {
		t.Errorf("attempts = %d, want one per rung (%d)", attempts, len(goldLockLadder))
	}
}

// TestRetryGoldLockReturnsOtherErrorsAtOnce keeps the ladder to the one
// error it is for: anything else is not going to clear by waiting.
func TestRetryGoldLockReturnsOtherErrorsAtOnce(t *testing.T) {
	t.Parallel()
	boom := errors.New("not a lock")
	attempts := 0
	_, err := retryGoldLock(context.Background(), func() (*sql.DB, error) {
		attempts++
		return nil, boom
	})
	if !errors.Is(err, boom) || attempts != 1 {
		t.Errorf("err = %v after %d attempts, want %v after 1", err, attempts, boom)
	}
}

// pendingGold is a fresh gold whose derived set carries another build's
// hash: this binary has a derived set the file has not applied.
func pendingGold(t *testing.T) string {
	t.Helper()
	path := freshGold(t)
	db, err := gold.Open(path, gold.ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE derived_state SET hash = 'another build'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// goldPending reports whether the gold at path has a migration or a
// derived set this binary carries still to apply.
func goldPending(t *testing.T, path string) bool {
	t.Helper()
	db, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pending, err := gold.MigrationsPending(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// TestReadOpenStaysReadOnlyBesideAReader: a read of a gold at this
// binary's schema opens read-only, so it attaches beside another
// process's reader at once, without the ladder, and cannot write.
func TestReadOpenStaysReadOnlyBesideAReader(t *testing.T) {
	path := freshGold(t)
	defer func(l []time.Duration) { goldLockLadder = l }(goldLockLadder)
	goldLockLadder = []time.Duration{0}

	startGoldReader(t, path, time.Second)

	db, err := openGoldForRead(globalFlags{}, &config.Config{GoldDB: path})
	if err != nil {
		t.Fatalf("read open beside a reader: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE read_open_probe (x INTEGER)`); err == nil {
		t.Error("the read handle took a write; want a read-only handle")
	}
}

// TestReadOpenAppliesAPendingSet: a read of a gold with a derived set
// still to apply opens read-write and applies it, as a write would.
func TestReadOpenAppliesAPendingSet(t *testing.T) {
	t.Parallel()
	path := pendingGold(t)
	db, err := openGoldForRead(globalFlags{}, &config.Config{GoldDB: path})
	if err != nil {
		t.Fatalf("read open of a pending gold: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if goldPending(t, path) {
		t.Error("the read left the derived set pending; want it applied")
	}
}

// TestReadOnlyReadLeavesAPendingSet: with -r a read opens read-only
// whatever is pending, and reads the file as it is.
func TestReadOnlyReadLeavesAPendingSet(t *testing.T) {
	t.Parallel()
	path := pendingGold(t)
	db, err := openGoldForRead(globalFlags{ForceReadOnly: true}, &config.Config{GoldDB: path})
	if err != nil {
		t.Fatalf("read-only read of a pending gold: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !goldPending(t, path) {
		t.Error("a read-only read applied the derived set")
	}
}
