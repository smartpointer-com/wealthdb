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
