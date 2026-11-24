package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// silverLockName is the lock file beside a collector's database.
//
// A collector that runs in a container writes its database inside
// Docker's virtual machine, and a native engine reads it on the
// computer. SQLite's own locks are file locks, and a file lock does not
// reach between the two. So in a tree of the native engine both sides
// lock this file on the computer instead: the collector's wrapper holds
// it for a verb that writes the database (hold-lock), and a load holds
// it shared while it reads the source. The container engine takes no
// part: it and a collector in a container are both in the virtual
// machine, where SQLite's locks work.
const silverLockName = ".wealthdb-silver.lock"

// silverLockWait is how long a load waits for a collector that writes
// before it skips the source. A collector's load may take minutes, and
// the other sources should not wait for it.
var silverLockWait = 10 * time.Second

// errSilverBusy is a source skipped because its collector writes.
var errSilverBusy = errors.New("its collector is writing its data right now; skipped. Load it again when the collector has finished")

// lockSilverForRead takes the lock of every database in paths, shared,
// for a read by the native engine, and returns what releases them. It
// waits up to wait for a collector that writes, then fails with
// errSilverBusy. A database with no lock file has no collector that
// locks, and is read as it is: the engine creates no file in a
// collector's folder. An engine of another kind, or of none, locks
// nothing.
func lockSilverForRead(ctx context.Context, paths []string, wait time.Duration) (unlock func(), err error) {
	unlock = func() {}
	if kind, err := engineKind(); err != nil || kind != engineNative {
		return unlock, err
	}
	var held []*os.File
	unlock = func() {
		for _, f := range held {
			_ = f.Close()
		}
	}
	seen := map[string]bool{}
	for _, p := range paths {
		lock := filepath.Join(filepath.Dir(p), silverLockName)
		if p == "" || seen[lock] {
			continue
		}
		seen[lock] = true
		f, err := os.Open(lock)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			unlock()
			return func() {}, fmt.Errorf("open the collector's lock %q: %w", lock, err)
		}
		held = append(held, f)
		deadline := time.Now().Add(wait)
		for {
			err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) {
				unlock()
				return func() {}, fmt.Errorf("lock %q: %w", lock, err)
			}
			if time.Now().After(deadline) {
				unlock()
				return func() {}, errSilverBusy
			}
			select {
			case <-ctx.Done():
				unlock()
				return func() {}, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return unlock, nil
}

func init() {
	register("hold-lock", cmdHoldLock)
}

const holdLockUsage = `usage: wealthdb hold-lock FILE -- COMMAND [ARG...]

Takes the lock on FILE, then runs COMMAND in place of this program. The
lock is held for as long as COMMAND runs. A collector's wrapper runs its
container through this for a verb that writes the collector's database,
so that a load by the native engine never reads the database half
written. The collectors' wrappers call it; it is not a command to type.`

// cmdHoldLock is `hold-lock FILE -- COMMAND...`: it locks FILE and
// becomes COMMAND, which inherits the open file and with it the lock.
// The kernel drops the lock when COMMAND, and every process that
// inherited the file from it, has ended. For `docker run` that is the
// client: one killed apart from its container gives the lock up early.
// It waits for a load that reads the collector's data, which takes
// seconds.
func cmdHoldLock(_ context.Context, _ globalFlags, subargs []string, _ io.Reader, _, stderr io.Writer) error {
	if len(subargs) < 3 || subargs[1] != "--" {
		fmt.Fprintln(stderr, holdLockUsage)
		return errs.Newf(errs.ExitUsage, "hold-lock: want FILE -- COMMAND")
	}
	path, argv := subargs[0], subargs[2:]
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return errs.Newf(errs.ExitUsage, "hold-lock: %v", err)
	}
	// No O_CLOEXEC: the command keeps the descriptor, and so the lock.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("hold-lock: open %q: %w", path, err))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("hold-lock: lock %q: %w", path, err))
		}
		fmt.Fprintln(stderr, "wealthdb: waiting for a load that reads this collector's data, or for another run of the collector …")
		if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
			return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("hold-lock: lock %q: %w", path, err))
		}
	}
	err = syscall.Exec(bin, argv, os.Environ())
	return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("hold-lock: run %s: %w", bin, err))
}
