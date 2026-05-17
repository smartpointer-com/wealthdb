// Package swissquote projects the swissquote-dump silver SQLite
// into canonical change records. See docs/adapters/swissquote.md
// for the mapping contract.
package swissquote

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/ptu/wealthdb/internal/silver"
)

const kindName = "swissquote"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, path string) (silver.Connection, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(true)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open swissquote silver %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping swissquote silver %q: %w", path, err)
	}
	return &Connection{db: db, path: path}, nil
}

type Connection struct {
	db   *sql.DB
	path string
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
