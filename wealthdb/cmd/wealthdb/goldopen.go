package main

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/pathmode"
)

// defaultMissingGoldMsg is the ExitMissingDB message for a gold DB
// that doesn't exist yet: openGoldForRead's default, and what most
// write commands pass as their missingDBMsg. A few commands vary it
// ("Nothing to reset", "Nothing to compact").
const defaultMissingGoldMsg = "gold database %q does not exist. Run 'wealthdb init' first (requires write access)."

// openGoldForRead opens the gold DB for a read-side subcommand. It
// picks read-write when the filesystem allows it — so an outstanding
// migration is applied on open — and read-only when -r is set or the
// path isn't writable. Returns a clear ExitMissingDB error
// when the DB doesn't exist yet. missingDBMsg optionally overrides
// the ExitMissingDB message (a format string taking the DB path)
// for commands whose wording differs. The caller owns db.Close().
func openGoldForRead(g globalFlags, cfg *config.Config, missingDBMsg ...string) (*sql.DB, error) {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		msg := defaultMissingGoldMsg
		if len(missingDBMsg) > 0 {
			msg = missingDBMsg[0]
		}
		return nil, errs.Newf(errs.ExitMissingDB, msg, cfg.GoldDB)
	}
	openMode := gold.ModeReadWrite
	if dec.Mode == pathmode.ModeReadOnly {
		openMode = gold.ModeReadOnly
	}
	// A read command opens read-write where it can, and so meets a
	// reader as a write command does: the MCP server holds a read-only
	// handle for each call. It waits on the same ladder.
	db, err := retryGoldLock(context.Background(), func() (*sql.DB, error) { return gold.Open(cfg.GoldDB, openMode) })
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	return db, nil
}

// gateGoldForWrite verifies the gold DB exists and is writable for a
// write-side subcommand and takes the gold write mutex, returning a
// typed error (ExitMissingDB, ExitRWNeeded or ExitOpenFailed) when
// any of the three fails. cmdName names the command in the read-only
// and mutex-held rejections; missingDBMsg is the ExitMissingDB
// message (a format string taking the DB path) — commands vary it
// ("Run 'wealthdb init' first" vs "Nothing to reset" vs "Nothing to
// compact"). It does not open the DB: commands that rewrite the file
// via swap machinery (compact, reload) gate without holding a handle,
// which is exactly why the mutex is needed beside DuckDB's own file
// lock. The caller holds the returned lock for the whole command and
// releases it with unlock.
func gateGoldForWrite(g globalFlags, cfg *config.Config, cmdName, missingDBMsg string) (*goldWriteLock, error) {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return nil, errs.Newf(errs.ExitMissingDB, missingDBMsg, cfg.GoldDB)
	}
	if dec.Mode != pathmode.ModeReadWrite {
		return nil, errs.Newf(errs.ExitRWNeeded,
			"'%s' requires write access to the gold database, but '%s' is read-only (detected: %s).",
			cmdName, cfg.GoldDB, dec.Reason)
	}
	return lockGoldForWrite(cfg.GoldDB, cmdName)
}

// openGoldForWrite gates as gateGoldForWrite does, then opens the gold
// DB read-write (applying any outstanding migration on open). The
// caller owns db.Close() and lock.unlock(). Used by the write commands
// that mutate the live file directly (load, reset, lots,
// web-materialize); compact and reload rewrite via a swap and only
// need gateGoldForWrite.
//
// The open waits out a reader. DuckDB refuses a read-write attach
// while any read-only handle is open, and a reader holds one for the
// length of one report: a read command, or a call to the MCP server.
// So a conflicting lock is retried on goldLockLadder before it fails.
// The write mutex above stays non-blocking: another writer may run for
// an hour, a reader for seconds.
func openGoldForWrite(g globalFlags, cfg *config.Config, cmdName, missingDBMsg string) (*sql.DB, *goldWriteLock, error) {
	lock, err := gateGoldForWrite(g, cfg, cmdName, missingDBMsg)
	if err != nil {
		return nil, nil, err
	}
	db, err := retryGoldLock(context.Background(), func() (*sql.DB, error) {
		return gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	})
	if err != nil {
		lock.unlock()
		return nil, nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	return db, lock, nil
}

// openGoldForOverlay opens gold for a command that writes an overlay
// store and has a dry run: categorize, categorizations --forget and
// resolve-symbols. A dry run is a pure read. It opens read-only and
// takes no lock, so readers keep the file while a model answers. A real
// run needs a writable gold and holds the write mutex for the whole
// command, so a rebuild-and-swap cannot land between what it reads and
// what it stores. The lock is nil on a dry run; unlock tolerates that.
// dryRunHint ends the read-only rejection: what a dry run shows instead.
func openGoldForOverlay(ctx context.Context, g globalFlags, goldPath, cmdName, missingDBMsg, dryRunHint string, dryRun bool) (*sql.DB, *goldWriteLock, error) {
	dec, err := pathmode.Detect(goldPath, g.ForceReadOnly, false)
	if err != nil {
		return nil, nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return nil, nil, errs.Newf(errs.ExitMissingDB, missingDBMsg, goldPath)
	}
	mode := gold.ModeReadOnly
	var lock *goldWriteLock
	if !dryRun {
		if dec.Mode != pathmode.ModeReadWrite {
			return nil, nil, errs.Newf(errs.ExitRWNeeded,
				"'%s' requires write access to the gold database, but '%s' is read-only (detected: %s). "+
					"Pass --dry-run if you only want to %s.", cmdName, goldPath, dec.Reason, dryRunHint)
		}
		if lock, err = lockGoldForWrite(goldPath, cmdName); err != nil {
			return nil, nil, err
		}
		mode = gold.ModeReadWrite
	}
	db, err := retryGoldLock(ctx, func() (*sql.DB, error) { return gold.Open(goldPath, mode) })
	if err != nil {
		lock.unlock()
		return nil, nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	return db, lock, nil
}

// goldLockLadder is the wait before each attempt to open gold past
// another process's DuckDB file lock: four tries over nine seconds.
// It outlasts a reader's single report and gives up well before a
// writer's run would end.
var goldLockLadder = []time.Duration{0, time.Second, 3 * time.Second, 5 * time.Second}

// retryGoldLock runs open, retrying on goldLockLadder while it fails
// with DuckDB's file-lock conflict. Any other error returns at once.
func retryGoldLock(ctx context.Context, open func() (*sql.DB, error)) (*sql.DB, error) {
	var db *sql.DB
	err := retryOn(ctx, goldLockLadder, isGoldLockConflict, func() (err error) {
		db, err = open()
		return err
	})
	if err != nil {
		return nil, err
	}
	return db, nil
}

// retryOn runs fn after each wait of ladder until it succeeds or fails
// with an error retryable refuses, and returns the last error.
func retryOn(ctx context.Context, ladder []time.Duration, retryable func(error) bool, fn func() error) error {
	var err error
	for _, wait := range ladder {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = fn(); err == nil || !retryable(err) {
			return err
		}
	}
	return err
}

// isGoldLockConflict reports whether err is DuckDB refusing to attach
// a file whose lock another process holds: a writer for a read-only
// open, any open handle for a read-write one.
func isGoldLockConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Could not set lock on file")
}

// retryFlush runs a store-what-the-model-answered step, retrying a
// failure a couple of times with a short backoff before giving up.
//
// The commands that round-trip a model release the gold handle for the
// length of the run and re-take it only to persist, and that re-open
// can lose a race: DuckDB refuses to attach a file read-write while
// another handle — including the read-only handle of a concurrent
// read command — is open on it. What is being stored has already been
// paid for, so a momentary loss of that race must not discard it. The
// backoff is bounded and short: a failure that outlives it is a real
// one, and the caller reports it rather than holding the answers
// forever.
func retryFlush(ctx context.Context, flush func() error) error {
	return retryOn(ctx, []time.Duration{0, time.Second, 3 * time.Second}, func(error) bool { return true }, flush)
}
