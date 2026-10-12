// Package amex projects the amex collector's silver — an American Express
// credit- and charge-card relationship — into canonical change records: one
// card account per card, its outstanding balance as negative cash, and the
// whole card ledger. Nothing reaches the returns engine; the source earns its
// keep in spending. docs/adapters/amex.md documents the mapping, the one sign
// flip (signedBalance) and the kind rules.
package amex

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "amex"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "amex silver")
	if err != nil {
		return nil, err
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
