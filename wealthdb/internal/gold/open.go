// Package gold owns the DuckDB gold database: the schema, the
// migrations, the writer that ingests canonical *Change records,
// and the queries that back the read-side subcommands.
//
// gold deliberately does not import internal/silver. Adapters
// produce canonical records; cmd/wealthdb orchestrates the
// silver→gold bridge. See docs/DESIGN.md §6.1 and §11.
package gold

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// Mode controls whether the gold DB is opened read-write or
// read-only. The pathmode package (milestone 6) chooses one based
// on filesystem permissions and the -r flag.
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
	return db, nil
}
