// Package fidelity projects the fidelity-web silver SQLite
// into canonical change records.
//
// A single-path adapter: the web scraper is Fidelity's only feed. It
// mirrors the swissquote adapter's single-path / USD-only shape, with
// these fidelity-specific quirks:
//
//   - Money-market core positions (silver flag
//     `is_core_position=1`) emit as CashBalanceChange rather than
//     PositionChange + InstrumentChange. Fidelity surfaces these
//     as ordinary positions rows with a "**" suffix on the symbol;
//     silver strips the suffix and promotes the channel signal to
//     the flag. Gold convention is to route them into
//     cash_balances so `wealthdb holdings positions --with-cash` and
//     the cash_balance aggregate column populate uniformly across
//     sources.
//
//   - Portfolios. Fidelity's selector groups accounts under
//     labelled sections; silver promotes the label as
//     portfolio_external_id and a kind classifier ('529' /
//     'trust_managed' / 'daf' / 'other'). The adapter emits one
//     PortfolioChange per silver portfolio so `wealthdb holdings
//     portfolios` rolls up each kind separately.
//
//   - Historical snapshots. Fidelity's positions UI is
//     point-in-time, so earlier periods come from statement PDFs
//     parsed into silver's `historical_position_snapshots` table
//     (migration 0004): fidelity-web's 529 and supplied
//     statements, and the svb statement archives, which write this
//     schema for this adapter to project. See historical.go.
//
//   - Wind-up counter-legs. A closed account's out-legs are printed
//     nowhere, and the receiving account's in-legs name it; the adapter
//     mirrors them (windup.go).
//
//   - Account kinds. Every account is brokerage, except the DAF's own
//     kind and an account whose historical rows type it by what it
//     holds: a home loan's outstanding principal makes it a mortgage, a
//     deposit account's balance a cash account (applyHeldKind).
//
//   - Cost basis (docs/DESIGN.md §7.4). A live holding's book value is
//     Fidelity's cost basis total. A statement holding's is the cost
//     basis the statement prints. Both are the sum of the holding's
//     tax lots, fees included, as Fidelity states it (basis.go). The
//     svb statements print none, so their holdings carry no book value
//     here; the lot engine rebuilds one from their trades
//     (lotpolicy.go, docs/LOTS.md).
//
//   - Open lots. A holding's lots come from the latest fetch of its lot
//     table at or before the snapshot. They ride a snapshot only while
//     their quantities and costs still sum to the holding's. A lot
//     carried past its fetch states no market value, and its term only
//     while that cannot have changed (lots.go).
//
//   - Realized lots. The 1099-B lots, the closed-positions page and the
//     statements' sales each state realized lots. Per account and tax
//     year, the best-ranked of them is primary. A statement sale settled
//     in January after a December trade counts in December's year
//     (realized.go).
package fidelity

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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
