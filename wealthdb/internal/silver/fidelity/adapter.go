// Package fidelity projects the fidelity-web silver SQLite
// into canonical change records.
//
// Single-source web-only adapter (Fidelity retired ofx.fidelity.com
// in 2026-05; the scraper is the only path). Mirrors the
// swissquote adapter's single-path / USD-only shape, with three
// fidelity-specific quirks:
//
//   - Money-market core positions (silver flag
//     `is_core_position=1`) emit as CashBalanceChange rather than
//     PositionChange + InstrumentChange. Fidelity surfaces these
//     as ordinary positions rows with a "**" suffix on the symbol;
//     silver strips the suffix and promotes the channel signal to
//     the flag. Gold convention is to route them into
//     cash_balances so `wealthdb positions --with-cash` and the
//     cash_balance aggregate column populate uniformly across
//     sources.
//
//   - Portfolios. Fidelity's selector groups accounts under
//     labelled sections; silver promotes the label as
//     portfolio_external_id and a kind classifier ('529' /
//     'trust_managed' / 'daf' / 'other'). We emit one
//     PortfolioChange per silver portfolio so `wealthdb
//     portfolios` rolls up each kind separately.
//
//   - Historical snapshots. Fidelity's positions UI is
//     point-in-time, so earlier periods come from statement PDFs
//     parsed into silver's `historical_position_snapshots` table
//     (migration 0004): fidelity-web's 529 and supplied
//     statements, and the svb statement archives, which write this
//     schema for this adapter to project. See historical.go.
package fidelity

import (
	"context"
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "fidelity"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "fidelity silver")
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
