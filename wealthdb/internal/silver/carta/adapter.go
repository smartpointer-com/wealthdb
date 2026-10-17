// Package carta projects the carta silver SQLite (private-market holdings
// on carta.com: cap-table equity and LP fund interests) into canonical
// change records: one custody account for the portfolio, one instrument and
// one forward-filled position per held company, a position lot per held
// share certificate, and the cash-flow ledger as balanced double-entry
// pairs. See docs/adapters/carta.md for the mapping contract.
package carta

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "carta"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "carta silver")
	if err != nil {
		return nil, err
	}
	exercise, err := silver.HasColumn(ctx, db, "securities", "exercise_fmv")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("carta silver: %w", err)
	}
	return &Connection{db: db, exercise: exercise}, nil
}

// Connection reads one carta silver. exercise says whether its securities
// carry the exercise facts (silver migration 0004): a silver last loaded
// before that migration lacks them and reads as stating none.
type Connection struct {
	db       *sql.DB
	exercise bool
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
