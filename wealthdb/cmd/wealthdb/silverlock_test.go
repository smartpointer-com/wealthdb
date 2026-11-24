package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdLockEnv marks the run of the test binary that plays `wealthdb
// hold-lock` (TestMain): it runs the command line after "--" through
// Run.
const holdLockEnv = "WEALTHDB_TEST_HOLD_LOCK"

// runAsEngine is that run.
func runAsEngine() int {
	for i, a := range os.Args {
		if a == "--" {
			return Run(os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr)
		}
	}
	return 2
}

// startHoldLock runs `hold-lock lock -- sh -c script args...` as a
// process of its own and returns it started. The script reads args as
// $0, $1, and so on.
func startHoldLock(t *testing.T, lock, script string, args ...string) *exec.Cmd {
	t.Helper()
	argv := append([]string{"--", "hold-lock", lock, "--", "sh", "-c", script}, args...)
	cmd := exec.Command(os.Args[0], argv...)
	cmd.Env = append(os.Environ(), holdLockEnv+"=1")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The command hold-lock becomes holds the lock for as long as it runs,
// and its exit status is the command's.
func TestHoldLockHoldsTheLockWhileItsCommandRuns(t *testing.T) {
	dir := t.TempDir()
	lock, started, release := filepath.Join(dir, silverLockName), filepath.Join(dir, "started"), filepath.Join(dir, "release")
	cmd := startHoldLock(t, lock, `touch "$0"; while [ ! -e "$1" ]; do sleep 0.05; done; exit 7`, started, release)
	waitFor(t, "the command to start", func() bool { _, err := os.Stat(started); return err == nil })

	f, err := os.Open(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("the lock is free while the command runs: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Errorf("hold-lock ended with %v, want the command's status 7", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Errorf("the lock is still held after the command ended: %v", err)
	}
}

func TestHoldLockUsage(t *testing.T) {
	for _, args := range [][]string{{"hold-lock"}, {"hold-lock", "f"}, {"hold-lock", "f", "x", "y"}, {"hold-lock", "f", "--"}} {
		if _, se, code := run(t, args...); code == 0 || !strings.Contains(se, "usage: wealthdb hold-lock") {
			t.Errorf("%v: exit %d, stderr %q", args, code, se)
		}
	}
	if _, se, code := run(t, "hold-lock", filepath.Join(t.TempDir(), "l"), "--", "no-such-command-zz"); code == 0 ||
		!strings.Contains(se, "no-such-command-zz") {
		t.Errorf("a command that does not exist: exit %d, stderr %q", code, se)
	}
	if _, se, code := run(t, "hold-lock", filepath.Join(t.TempDir(), "no-folder", "l"), "--", "true"); code == 0 ||
		!strings.Contains(se, "open") {
		t.Errorf("a lock file in a folder that does not exist: exit %d, stderr %q", code, se)
	}
}

// A collector that finds a load reading waits for it, and runs once the
// load is done.
func TestHoldLockWaitsForALoadThatReads(t *testing.T) {
	dir := t.TempDir()
	lock, started := filepath.Join(dir, silverLockName), filepath.Join(dir, "started")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	cmd := startHoldLock(t, lock, `touch "$0"`, started)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the collector's command ran while a load held the lock")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the collector's command after the load: %v", err)
	}
	if _, err := os.Stat(started); err != nil {
		t.Errorf("the collector's command did not run once the load was done: %v", err)
	}
}

// A load by the native engine waits for a collector that writes, skips
// the source when the collector is not done in time, and reads a
// source at once when no collector locks it. Another kind of engine
// locks nothing.
func TestALoadTakesTheCollectorsLock(t *testing.T) {
	dir := t.TempDir()
	silver := filepath.Join(dir, "bank.db")
	lock, started := filepath.Join(dir, silverLockName), filepath.Join(dir, "started")
	ctx := context.Background()
	realWait := silverLockWait
	silverLockWait = 300 * time.Millisecond
	t.Cleanup(func() { silverLockWait = realWait })

	t.Setenv(engineKindEnv, engineNative)
	unlock, err := lockSilverForRead(ctx, []string{silver, "", silver}, silverLockWait)
	if err != nil {
		t.Fatalf("a source with no lock file: %v", err)
	}
	unlock()
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the engine created the lock file in the collector's folder (%v)", err)
	}

	startHoldLock(t, lock, `touch "$0"; exec sleep 30`, started)
	waitFor(t, "the collector to start", func() bool { _, err := os.Stat(started); return err == nil })
	if _, err := lockSilverForRead(ctx, []string{silver}, silverLockWait); !errors.Is(err, errSilverBusy) {
		t.Fatalf("a source whose collector writes: %v, want it skipped as busy", err)
	}

	// A source of two folders, the second one busy: the first folder's
	// lock is given back.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, silverLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lockSilverForRead(ctx, []string{filepath.Join(other, "a.db"), silver}, 0); !errors.Is(err, errSilverBusy) {
		t.Fatalf("a source with one busy folder of two: %v", err)
	}
	free, err := os.OpenFile(filepath.Join(other, silverLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer free.Close()
	if err := unix.Flock(int(free.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Errorf("the lock of the folder that was free is still held after the busy one failed: %v", err)
	}

	// A wait that is cancelled ends at once.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	began := time.Now()
	if _, err := lockSilverForRead(cancelled, []string{silver}, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait: %v", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Errorf("a cancelled wait took %v", time.Since(began))
	}

	t.Setenv(engineKindEnv, engineContainer)
	unlock, err = lockSilverForRead(ctx, []string{silver}, silverLockWait)
	if err != nil {
		t.Fatalf("the container engine: %v", err)
	}
	unlock()
}

// A load waits for a collector that finishes within its wait, then
// reads.
func TestALoadWaitsForACollectorThatFinishes(t *testing.T) {
	dir := t.TempDir()
	silver := filepath.Join(dir, "bank.db")
	lock, started, release := filepath.Join(dir, silverLockName), filepath.Join(dir, "started"), filepath.Join(dir, "release")
	startHoldLock(t, lock, `touch "$0"; while [ ! -e "$1" ]; do sleep 0.05; done`, started, release)
	waitFor(t, "the collector to start", func() bool { _, err := os.Stat(started); return err == nil })
	t.Setenv(engineKindEnv, engineNative)
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(release, nil, 0o600)
	}()
	began := time.Now()
	unlock, err := lockSilverForRead(context.Background(), []string{silver}, 30*time.Second)
	if err != nil {
		t.Fatalf("a collector that finished within the wait: %v", err)
	}
	unlock()
	if waited := time.Since(began); waited < 300*time.Millisecond {
		t.Errorf("the load took the lock after %v, while the collector still held it", waited)
	}
}

// Two loads read one source side by side, and a collector waits for
// both.
func TestLoadsShareTheCollectorsLock(t *testing.T) {
	dir := t.TempDir()
	silver, lock := filepath.Join(dir, "bank.db"), filepath.Join(dir, silverLockName)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(engineKindEnv, engineNative)
	ctx := context.Background()
	first, err := lockSilverForRead(ctx, []string{silver}, silverLockWait)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lockSilverForRead(ctx, []string{silver}, silverLockWait)
	if err != nil {
		t.Fatalf("a second load beside the first: %v", err)
	}
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Errorf("a collector took the lock while two loads read: %v", err)
	}
	first()
	second()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Errorf("the lock is held after both loads ended: %v", err)
	}
}

// twoSourceCfg is setupCLITest's config with a second source, a copy of
// the first's silver in a folder of its own, and both loaded by the
// native engine. It returns the config and the second source's folder.
func twoSourceCfg(t *testing.T) (cfg, otherDir string) {
	t.Helper()
	cfg = setupCLITest(t)
	dir := filepath.Dir(cfg)
	otherDir = filepath.Join(dir, "other")
	if err := os.Mkdir(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	silver, err := os.ReadFile(filepath.Join(dir, "schwab.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "schwab.db"), silver, 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": [
  {"id": "schwab-test", "kind": "schwab", "path": %q},
  {"id": "schwab-two", "kind": "schwab", "path": %q}]}`,
		goldPathFromCfg(cfg), filepath.Join(dir, "schwab.db"), filepath.Join(otherDir, "schwab.db"))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(engineKindEnv, engineNative)
	freshGoldFor(t, cfg)
	if so, se, code := run(t, "-c", cfg, "load", "-a"); code != 0 {
		t.Fatalf("load -a: exit %d\n%s%s", code, so, se)
	}
	return cfg, otherDir
}

// A load and a reload go through the collector's lock: the source whose
// collector writes is skipped and named, the others load, the exit
// status says so, and a reload leaves the skipped source's rows in gold.
func TestLoadAndReloadSkipASourceWhoseCollectorWrites(t *testing.T) {
	cfg, otherDir := twoSourceCfg(t)
	realWait := silverLockWait
	silverLockWait = 200 * time.Millisecond
	t.Cleanup(func() { silverLockWait = realWait })
	snapshots := func() string {
		so, se, code := run(t, "-c", cfg, "-r", "snapshots", "schwab-two")
		if code != 0 {
			t.Fatalf("snapshots: exit %d: %s", code, se)
		}
		return so
	}
	before := snapshots()
	if !strings.Contains(before, "schwab-two") {
		t.Fatalf("the second source is not loaded:\n%s", before)
	}

	started := filepath.Join(otherDir, "started")
	startHoldLock(t, filepath.Join(otherDir, silverLockName), `touch "$0"; exec sleep 60`, started)
	waitFor(t, "the collector to start", func() bool { _, err := os.Stat(started); return err == nil })
	const busy = "schwab-two: its collector is writing its data right now; skipped"

	so, se, code := run(t, "-c", cfg, "load", "-a")
	if code == 0 || !strings.Contains(se, "load: "+busy) {
		t.Errorf("load -a: exit %d, stderr:\n%s", code, se)
	}
	if !strings.Contains(so, "load: schwab-test:") {
		t.Errorf("load -a did not load the source that was free:\n%s", so)
	}

	_, se, code = run(t, "-c", cfg, "reload", "schwab-two")
	if code == 0 || !strings.Contains(se, "reload: "+busy) {
		t.Errorf("reload schwab-two: exit %d, stderr:\n%s", code, se)
	}
	if after := snapshots(); after != before {
		t.Errorf("a reload that skipped the source changed its rows in gold:\n%s\nwas:\n%s", after, before)
	}

	_, se, code = run(t, "-c", cfg, "reload", "-a")
	if code == 0 || !strings.Contains(se, busy) {
		t.Errorf("reload -a: exit %d, stderr:\n%s", code, se)
	}
	if after := snapshots(); after != before {
		t.Errorf("a full reload with a busy source replaced gold:\n%s\nwas:\n%s", after, before)
	}

	// status reads no silver that a collector writes, and still answers.
	if _, se, code := run(t, "-c", cfg, "-r", "status"); code != 0 {
		t.Errorf("status with a busy source: exit %d: %s", code, se)
	}
}
