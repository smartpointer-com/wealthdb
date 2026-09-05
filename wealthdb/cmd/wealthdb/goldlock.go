package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
)

// goldWriteLockSuffix names the sidecar file the write mutex is taken
// on. It sits beside the gold database rather than being the database
// itself, because two of the write commands never open the live file
// at all: compact and 'reload -a' build a replacement and rename it
// over the live path, so a lock on the live inode would not exclude
// them from each other.
const goldWriteLockSuffix = ".wealthdb.lock"

// goldWriteLock is the whole-command mutex over the gold database.
//
// DuckDB's own file lock covers only the window a handle is open, and
// the rebuild-and-swap commands hold no handle across their rebuild:
// they read the outgoing file, spend minutes building a replacement,
// and rename it over the live path. A verdict or a load written into
// the outgoing file in that window lands in an inode the rename then
// unlinks, and both commands report success. The merchant store is
// the one table with no other source of truth, so that loss is
// unrecoverable.
//
// The mutex is an advisory flock on a sidecar file, which the kernel
// releases when the process dies — an O_EXCL lockfile would survive a
// SIGKILL and block every later write with no recovery path. It is
// taken NON-blocking: a write command that finds the mutex held fails
// immediately saying so, rather than queueing behind a model run that
// may take an hour.
//
// Read-only commands never take it. Concurrent readers are the case
// the whole read-only-share contract exists for, and a reader cannot
// lose a write.
type goldWriteLock struct {
	f *os.File
}

// lockGoldForWrite takes the write mutex for the gold database at
// goldPath, or fails with ExitOpenFailed naming the sidecar when
// another wealthdb write command holds it. cmdName names the caller
// in that message. The returned lock is released by unlock, which
// tolerates a nil receiver so callers can defer it unconditionally.
func lockGoldForWrite(goldPath, cmdName string) (*goldWriteLock, error) {
	path := goldPath + goldWriteLockSuffix
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed,
			fmt.Errorf("open the gold write lock %q: %w", path, err))
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errs.Newf(errs.ExitOpenFailed,
			"'%s' cannot write %q: another wealthdb write command (load / reset / reload / compact / "+
				"categorize / resolve-symbols / web-materialize) is running and holds %q. "+
				"Wait for it to finish and re-run.", cmdName, goldPath, path)
	}
	return &goldWriteLock{f: f}, nil
}

// unlock releases the mutex. Closing the descriptor drops the flock,
// so the sidecar file is left in place for the next command to take.
func (l *goldWriteLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Close()
}
