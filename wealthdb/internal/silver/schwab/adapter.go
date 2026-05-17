// Package schwab projects the schwab-dump silver SQLite into
// canonical change records. See docs/adapters/schwab.md for the
// mapping contract.
package schwab

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // SQLite driver registration

	"github.com/ptu/wealthdb/internal/silver"
)

// kindName is the silver-kind discriminator that appears in the
// wealthdb config file's silver_sources[].kind field.
const kindName = "schwab"

func init() {
	silver.Register(&Adapter{})
}

// Adapter implements silver.Adapter.
type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

// Open opens the silver SQLite read-only and returns a Connection.
// Read-only opening is enforced via the DSN; the underlying file
// is never written by this adapter.
func (*Adapter) Open(_ context.Context, path string) (silver.Connection, error) {
	// `?mode=ro` is honoured by modernc.org/sqlite via the file:
	// URI form; `_pragma=query_only(true)` is a belt-and-braces
	// safeguard in case the URI's mode flag is stripped.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(true)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open schwab silver %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping schwab silver %q: %w", path, err)
	}
	return &Connection{db: db, path: path}, nil
}

// Connection is one attached schwab silver SQLite.
type Connection struct {
	db   *sql.DB
	path string
}

// Close releases the underlying *sql.DB. Safe to call more than
// once.
func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
