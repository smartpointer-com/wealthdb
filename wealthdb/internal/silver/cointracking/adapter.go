// Package cointracking projects the cointracking silver DuckDB — the
// aggregator-of-record for crypto holdings across exchanges and on-chain
// wallets, its trade history replayed into daily holdings per portfolio —
// into canonical change records. docs/adapters/cointracking.md documents the
// identifiers, the account taxonomy, the position and FX mapping and the
// transaction rules; policy.go registers the returns policy.
package cointracking

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "cointracking"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	// DuckDB takes options as `?key=value` query params on the DSN
	// (cf. internal/gold/open.go).
	dsn := spec.Path + "?access_mode=" + url.QueryEscape("read_only")
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cointracking silver %q: %w", spec.Path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping cointracking silver %q: %w", spec.Path, err)
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
