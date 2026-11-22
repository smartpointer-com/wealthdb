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
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "carta"

// collectors are the collectors whose silver the kind reads
// (silver.Collector).
var collectors = []silver.Collector{
	{
		Name:    "carta",
		Display: "Carta",
		Group:   silver.GroupUnitedStates,
		Covers:  "shares and funds held through Carta",
		Files:   []string{"carta.db"},
	},
}

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
		Kind:       kindName,
		Open:       (&Adapter{}).Open,
		Collectors: collectors,
		Returns:    &rp,
		Lots:       &lp,
	})
}

type Adapter struct{}

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "carta silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	note, err := silver.ProbeOptional(ctx, db, filepath.Base(spec.Path), c.schema.optional()...)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("carta silver: %w", err)
	}
	c.Note(note)
	return c, nil
}

// Connection reads one carta silver.
type Connection struct {
	memo   memo
	db     *sql.DB
	schema schema
	silver.Advice
}

// schema is which optional parts of the silver schema a file holds,
// probed once at open. A silver last loaded before a part's migration
// lacks it and reads as stating none of it.
type schema struct {
	exercise    bool // securities.exercise_fmv: the exercise facts (silver migration 0004)
	k1          bool // the k1_capital_accounts table
	k1PeriodEnd bool // k1_capital_accounts.period_end
	flowAt      bool // cash_flows.flow_at: the cash-flow ledger's days (silver migration 0007)
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "securities", Columns: []string{"exercise_fmv"}, Version: 4, Has: &s.exercise},
		{Table: "k1_capital_accounts", Version: 4, Has: &s.k1},
		{Table: "k1_capital_accounts", Columns: []string{"period_end"}, Version: 5, Has: &s.k1PeriodEnd},
		{Table: "cash_flows", Columns: []string{"flow_at"}, Version: 7, Has: &s.flowAt},
	}
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
