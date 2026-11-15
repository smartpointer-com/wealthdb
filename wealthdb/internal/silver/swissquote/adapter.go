// Package swissquote projects the swissquote silver SQLite
// into canonical change records. See docs/adapters/swissquote.md
// for the mapping contract.
package swissquote

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "swissquote"

// init registers the kind (silver.Source).
//
// Returns: flow-complete bank/pension, the standard bank external and
// transfer-like sets, forward knobs defaulted (no-op).
// Lots: shadow at the average method. The source states an average cost
// for its positions and no lots, so the replay fills nothing; `gains
// check` compares its ledger with the stated cost (docs/LOTS.md §6, §9).
func init() {
	rp, lp := returns.DefaultReturnsPolicy(returns.BankFlowPolicy()), lots.ShadowPolicy()
	silver.Register(silver.Source{
		Kind:    kindName,
		Open:    (&Adapter{}).Open,
		Returns: &rp,
		Lots:    &lp,
	})
}

type Adapter struct{}

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "swissquote silver")
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
	accountProduct  bool // accounts.account_product (silver v5)
	accountType     bool // accounts.account_type, its name before v5
	positionName    bool // positions.name
	positionISIN    bool // positions.isin
	averageCost     bool // positions.average_cost
	priceQuote      bool // positions.price_quote
	accruedInterest bool // positions.accrued_interest_chf
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		// A silver from before v5 states the product as account_type, which
		// v5 renames: the adapter reads either, so only a silver with
		// neither lacks it.
		{Table: "accounts", Columns: []string{"account_product"}, Has: &s.accountProduct},
		{Table: "accounts", Columns: []string{"account_type"}, Version: 2, Has: &s.accountType},
		{Table: "positions", Columns: []string{"name"}, Version: 3, Has: &s.positionName},
		{Table: "positions", Columns: []string{"isin"}, Version: 3, Has: &s.positionISIN},
		{Table: "positions", Columns: []string{"average_cost"}, Version: 6, Has: &s.averageCost},
		{Table: "positions", Columns: []string{"price_quote"}, Version: 6, Has: &s.priceQuote},
		{Table: "positions", Columns: []string{"accrued_interest_chf"}, Version: 7, Has: &s.accruedInterest},
	}
}
