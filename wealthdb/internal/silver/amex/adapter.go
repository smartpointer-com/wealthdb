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

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "amex"

// init registers the kind (silver.Source).
//
// Every account this source emits is a card. See returns.CardIssuerPolicy
// for why a card contributes nothing to returns at any grain, and
// gold.returnsInvisibleKind for where they are dropped.
func init() {
	rp := returns.CardIssuerPolicy()
	silver.Register(silver.Source{
		Kind:    kindName,
		Open:    (&Adapter{}).Open,
		Returns: &rp,
	})
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

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
