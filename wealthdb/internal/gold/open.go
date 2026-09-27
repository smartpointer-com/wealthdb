// Package gold owns the DuckDB gold database: the schema, the
// migrations, the writer that ingests canonical *Change records,
// and the queries that back the read-side subcommands.
//
// gold deliberately does not import internal/silver. Adapters
// produce canonical records; cmd/wealthdb orchestrates the
// silver→gold bridge. See docs/DESIGN.md §6.1 and §11.
package gold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/version"
)

// ErrStaleBinary is returned from Open when the binary's VCS
// commit timestamp is older than the latest binary timestamp
// recorded in the database's binary_versions table. The caller
// (cmd/wealthdb) prints the wrapped message and exits non-zero —
// the database has been written by a newer wealthdb, so the
// current binary may misread its schema.
var ErrStaleBinary = errors.New("binary is older than the database's last writer")

// Mode controls whether the gold DB is opened read-write or
// read-only. The pathmode package chooses one based on
// filesystem permissions and the -r flag.
type Mode int

const (
	// ModeReadWrite opens DuckDB with default access; the file
	// is locked for exclusive write by this process.
	ModeReadWrite Mode = iota
	// ModeReadOnly opens DuckDB with access_mode='read_only'; no
	// write attempts can succeed, and multiple readers can attach
	// concurrently.
	ModeReadOnly
)

// Open opens the gold DuckDB file at the given path. Path
// ":memory:" or "" opens an in-memory database (tests use this).
// On-disk databases respect mode; in-memory always opens RW.
//
// The returned *sql.DB is the standard database/sql handle; close
// it with db.Close() when done.
func Open(path string, mode Mode) (*sql.DB, error) {
	return open(path, mode, true)
}

// ReopenReadWrite opens an already-open-once gold file read-write
// WITHOUT stamping binary_versions.
//
// The commands that round-trip a model release the handle between
// calls so readers are not locked out for the length of a run, and
// re-take it only to flush what the model answered. Those re-opens
// are the same process continuing the same command, so recording each
// one would append a row per batch to the staleness ledger and drown
// the signal it exists for: which binary last wrote this database.
// The first open of the command records that.
func ReopenReadWrite(path string) (*sql.DB, error) {
	return open(path, ModeReadWrite, false)
}

// open is Open's body; audit says whether an RW open stamps
// binary_versions.
func open(path string, mode Mode, audit bool) (*sql.DB, error) {
	dsn := path
	if path == "" {
		dsn = ":memory:"
	}

	// Only on-disk databases honour access_mode; in-memory always
	// opens read-write (there's nothing to share).
	if mode == ModeReadOnly && dsn != ":memory:" {
		// DuckDB takes options as `?key=value`-style query params
		// on the DSN.
		dsn = dsn + "?access_mode=" + url.QueryEscape("read_only")
	}

	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}
	// sql.Open is lazy; Ping confirms the driver can actually
	// attach the file. Failures here are the user-visible "the
	// file is missing / corrupt / locked" cases.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping duckdb %q: %w", path, err)
	}
	// Apply outstanding migrations on every RW open so the schema
	// always matches the binary's expectations. Idempotent: a DB
	// already at the latest version is a fast MAX(version) scan
	// and zero ExecContexts. Skipped for read-only opens (can't
	// write DDL) — RO callers rely on prior RW callers (init,
	// load, reset) having brought the schema up to date.
	if mode == ModeReadWrite {
		if err := Migrate(context.Background(), db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate gold %q: %w", path, err)
		}
	}
	// Staleness check — runs on every Open (RO and RW). After
	// migration so a brand-new DB has the binary_versions table
	// in place; RO callers tolerate older databases that haven't
	// yet been migrated (no table → skip).
	if err := checkBinaryNotStale(context.Background(), db, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	// RW opens record themselves on the way in, so the next RW or
	// RO Open knows what the most-recent writer was. Best-effort:
	// a failure to log shouldn't block the load itself.
	if mode == ModeReadWrite && audit {
		_ = recordBinaryOpen(context.Background(), db)
	}
	return db, nil
}

// checkBinaryNotStale compares the binary's VCS-derived commit
// timestamp against MAX(binary_commit_at) in the database. Returns
// nil (no check) when either:
//
//   - the binary was built without VCS info (CommitAt == 0) — Docker
//     builds without .git in the build context, raw `go install`
//     with -buildvcs=false, etc.; we can't compare so we don't
//     block.
//   - the binary_versions table is empty or doesn't exist — DB
//     hasn't been opened since this feature was introduced, or
//     the schema is older than migration 0012.
//
// Otherwise: if the binary's commit_at is strictly less than the
// DB's recorded max, returns a wrapped ErrStaleBinary with a
// human-readable rebuild instruction.
func checkBinaryNotStale(ctx context.Context, db *sql.DB, path string) error {
	bi := version.Build()
	if bi.CommitAt == 0 {
		return nil
	}
	var maxAt sql.NullInt64
	var maxCommit sql.NullString
	err := db.QueryRowContext(ctx, `
SELECT MAX(binary_commit_at), arg_max(binary_commit, binary_commit_at)
  FROM binary_versions`).Scan(&maxAt, &maxCommit)
	if err != nil {
		// Most likely the table doesn't exist (RO open of a DB
		// older than migration 0012). Tolerate.
		return nil
	}
	if !maxAt.Valid || maxAt.Int64 <= bi.CommitAt {
		return nil
	}
	binDate := time.Unix(bi.CommitAt, 0).UTC().Format("2006-01-02")
	dbDate := time.Unix(maxAt.Int64, 0).UTC().Format("2006-01-02")
	return fmt.Errorf(`gold %q: %w

  binary commit:    %s (%s)
  database latest:  %s (%s)

The database has been written by a newer wealthdb binary. The
current binary may misread newer schema or silently drop fields
it doesn't know about. Rebuild from source:

  cd <wealthdb-repo>/wealthdb && go install ./cmd/wealthdb`,
		path, ErrStaleBinary,
		shortSHA(bi.Commit), binDate,
		shortSHA(maxCommit.String), dbDate)
}

// recordBinaryOpen appends a row to binary_versions stamping the
// current binary's identity. No-op when the binary has no VCS
// info (CommitAt == 0) — we'd just be polluting the audit log.
//
// It is a variable so a test can observe that Open reaches it and
// ReopenReadWrite does not. Counting the rows it writes cannot answer
// that: a test binary carries no VCS stamp, so the write is a no-op
// and both paths leave the same row count behind whichever one ran.
var recordBinaryOpen = recordBinaryOpenImpl

func recordBinaryOpenImpl(ctx context.Context, db *sql.DB) error {
	bi := version.Build()
	if bi.CommitAt == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO binary_versions
       (opened_at, binary_commit, binary_commit_at, binary_version)
VALUES (?, ?, ?, ?)`,
		time.Now().Unix(), bi.Commit, bi.CommitAt, version.String())
	return err
}

// shortSHA renders a git revision in the typical 7-char abbrev.
// Pass-through for shorter strings; empty input renders as "?".
func shortSHA(s string) string {
	if s == "" {
		return "?"
	}
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
