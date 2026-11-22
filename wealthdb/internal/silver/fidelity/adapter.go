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
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "fidelity"

// collectors are the collectors whose silver the kind reads
// (silver.Collector).
var collectors = []silver.Collector{
	{
		Name:    "fidelity-web",
		Display: "Fidelity",
		Group:   silver.GroupUnitedStates,
		Covers:  "brokerage, retirement and donor-advised accounts",
		Files:   []string{"fidelity-web.db"},
	},
	{
		Name:    "svb",
		Display: "SVB statement archive",
		Group:   silver.GroupUnitedStates,
		Covers:  "past SVB statements, read once from PDF files",
		Files:   []string{"svb.db", "svb-deposit.db", "svb-mortgage.db"},
	},
}

// init registers the kind (silver.Source).
//
// Returns: flow-complete bank/pension (every capital kind counts),
// forward knobs defaulted (no-op). The statements
// and the API state a corporate action's cash signed, and the adapter
// passes it through (canonical.CashSignedKinds); the svb families load
// as this kind.
func init() {
	rp := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	silver.Register(silver.Source{
		Kind:       kindName,
		Open:       (&Adapter{}).Open,
		Collectors: collectors,
		Returns:    &rp,
		Lots:       lotPolicy(),
		CashSigned: []canonical.TxKind{canonical.TxKindCorporateAction},
	})
}

type Adapter struct{}

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "fidelity silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	note, err := silver.ProbeOptional(ctx, c.db, filepath.Base(spec.Path), c.schema.optional()...)
	if err != nil {
		c.db.Close()
		return nil, err
	}
	c.Note(note)
	return c, nil
}

type Connection struct {
	db     *sql.DB
	schema schema
	silver.Advice
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }

// schema is which optional parts of the silver schema a file holds,
// probed once at open.
type schema struct {
	historical      bool // the statement corpus (silver migration 0004)
	historicalCost  bool // a statement's cost basis (silver migration 0009)
	managementStyle bool // accounts.management_style
	openLots        bool // the open_lots table
	openLotCUSIP    bool // open_lots.cusip
	closedLotNames  bool // closed_lots.security_name, with disposed_date and the closed_positions kind (silver migration 0012)
	stated          bool // the stated_periods table
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "historical_position_snapshots", Version: 4, Has: &s.historical},
		{Table: "historical_position_snapshots", Columns: []string{"cost_basis"}, Version: 9, Has: &s.historicalCost},
		{Table: "accounts", Columns: []string{"management_style"}, Version: 3, Has: &s.managementStyle},
		{Table: "open_lots", Version: 11, Has: &s.openLots},
		{Table: "open_lots", Columns: []string{"cusip"}, Version: 11, Has: &s.openLotCUSIP},
		{Table: "closed_lots", Columns: []string{"security_name"}, Version: 12, Has: &s.closedLotNames},
		{Table: statedPeriodsTable, Version: 15, Has: &s.stated},
	}
}
