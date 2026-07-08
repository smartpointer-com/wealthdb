package gold

import (
	"context"
	"fmt"
	"os"
	"strconv"
)

// BuildFreshAndSwap builds a new gold DB via build(tmpPath) — which
// must fully create AND cleanly close the file at tmpPath — then
// verifies it and atomically renames it over goldPath. Concurrent
// readers keep the old inode (POSIX): the OS frees its storage when
// the last reader closes. Returns the reclaimed byte delta
// (old size - new size).
//
// goldPath is never touched until the final rename, so a failed build
// or a crash leaves the live database intact. The temp file lives in
// goldPath's directory so the rename is a same-filesystem atomic
// replace; on any error before the swap the temp file and its stray
// WAL are removed best-effort.
func BuildFreshAndSwap(ctx context.Context, goldPath string, build func(tmpPath string) error) (reclaimed int64, err error) {
	// Same directory as goldPath so the final rename is an atomic
	// same-filesystem os.Rename (a cross-filesystem rename is not
	// atomic and would copy). The pid keeps two concurrent runs from
	// colliding on the temp name.
	tmp := goldPath + ".rebuild-" + strconv.Itoa(os.Getpid())

	// Until the swap succeeds, clean up the temp file and any WAL it
	// left behind. goldPath itself is only ever touched by the rename
	// below, so this never endangers the live DB.
	swapped := false
	defer func() {
		if !swapped {
			_ = os.Remove(tmp)
			_ = os.Remove(tmp + ".wal")
		}
	}()

	// Clear any orphan left by a crashed prior run that shared this pid,
	// so build() always starts from a clean slate (a pre-existing file
	// could otherwise skew an append-style build; the live DB is never
	// at risk either way).
	_ = os.Remove(tmp)
	_ = os.Remove(tmp + ".wal")

	if err := build(tmp); err != nil {
		return 0, err
	}

	// Verify the freshly built file before swapping: it must open
	// read-only (which also runs the staleness check) and answer a
	// smoke query. A temp that fails to verify is never renamed over
	// the live DB.
	if err := verifyGold(ctx, tmp); err != nil {
		return 0, fmt.Errorf("verify rebuilt gold %q: %w", tmp, err)
	}

	oldSize, err := fileSize(goldPath)
	if err != nil {
		return 0, err
	}
	newSize, err := fileSize(tmp)
	if err != nil {
		return 0, err
	}

	// Carry the live file's mode (0600 in normal deployments) onto the
	// replacement before it takes over the path.
	if fi, statErr := os.Stat(goldPath); statErr == nil {
		if chmodErr := os.Chmod(tmp, fi.Mode().Perm()); chmodErr != nil {
			return 0, fmt.Errorf("preserve mode on rebuilt gold %q: %w", tmp, chmodErr)
		}
	}

	// Atomic replace. The old path's inode is unlinked but stays alive
	// for any reader that still has it open.
	if err := os.Rename(tmp, goldPath); err != nil {
		return 0, fmt.Errorf("swap rebuilt gold over %q: %w", goldPath, err)
	}
	swapped = true

	// A stale WAL left by a prior crashed in-place writer would be
	// mis-replayed against the fresh inode on the next open. The fresh
	// build already holds all committed data, so dropping it is
	// correct; normal operation leaves no WAL. Best-effort — a failure
	// here doesn't undo the successful swap.
	_ = os.Remove(goldPath + ".wal")

	return oldSize - newSize, nil
}

// verifyGold opens the freshly built DB read-only (which runs the
// staleness check) and confirms the core tables answer a smoke query.
// A brand-new build over empty silver may legitimately hold zero rows,
// so this asserts the query runs, not that any rows exist.
func verifyGold(ctx context.Context, path string) error {
	db, err := Open(path, ModeReadOnly)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, tbl := range []string{"positions", "accounts"} {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil {
			return fmt.Errorf("smoke query %s: %w", tbl, err)
		}
	}
	return nil
}

// fileSize returns the byte size of the file at path.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return fi.Size(), nil
}
