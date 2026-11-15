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

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "carta"

// init registers the kind (silver.Source).
//
// Returns: see returns.PrivateMarketLedgerPolicy for the double-entry
// custody-ledger rationale (transactions.go emits the pairs on the
// custody account). Lots: the cost basis of its exercised and bought
// shares and fund interests is what the source states, capital paid in,
// and no lot book describes it (docs/LOTS.md §6). The engine builds none
// and realizes a sale against the stated basis.
func init() {
	rp, lp := returns.PrivateMarketLedgerPolicy(), lots.StatedPolicy()
	silver.Register(silver.Source{
		Kind:    kindName,
		Open:    (&Adapter{}).Open,
		Returns: &rp,
		Lots:    &lp,
	})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "carta silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	if err := silver.ProbeOptional(ctx, db, c.schema.optional()...); err != nil {
		db.Close()
		return nil, fmt.Errorf("carta silver: %w", err)
	}
	return c, nil
}

// Connection reads one carta silver.
type Connection struct {
	memo   memo
	db     *sql.DB
	schema schema
}

// schema is which optional parts of the silver schema a file holds,
// probed once at open. A silver last loaded before a part's migration
// lacks it and reads as stating none of it.
type schema struct {
	exercise    bool // securities.exercise_fmv: the exercise facts (silver migration 0004)
	k1          bool // the k1_capital_accounts table
	k1PeriodEnd bool // k1_capital_accounts.period_end
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "securities", Columns: []string{"exercise_fmv"}, Has: &s.exercise},
		{Table: "k1_capital_accounts", Has: &s.k1},
		{Table: "k1_capital_accounts", Columns: []string{"period_end"}, Has: &s.k1PeriodEnd},
	}
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
