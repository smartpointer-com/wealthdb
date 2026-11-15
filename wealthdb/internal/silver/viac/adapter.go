// Package viac projects the viac silver SQLite (VIAC /
// Terzo / WIR Group Swiss Pillar-3a + vested-benefits app) into
// canonical change records.
//
// Single-source CHF-only adapter. Notable shapes:
//
//   - Accounts split by silver `product_code`:
//     '3' → Pillar 3a            → tax_wrapper = pillar_3a
//     '2' → Vested benefits      → tax_wrapper = vested_benefits
//     '1' → Free investment      → tax_wrapper = taxable_personal
//     (Other codes fall through to taxable_personal.)
//
//   - Management style is robo-managed across every VIAC product
//     today — the account holder picks a strategy from a fixed
//     menu, an algorithm allocates and rebalances, no human in
//     the loop. The adapter stamps ManagementStyleAutomated by
//     default; if silver ever gains a per-account
//     `management_style` column the adapter reads it via a
//     hasColumn-gated SELECT and the silver value takes
//     precedence.
//
//   - Positions carry the per-fund CHF market value (silver
//     positions.market_value_chf). The book value is quantity ×
//     acquisition_price, VIAC's average cost per unit in CHF,
//     stamped derived / average / none (basis.go,
//     docs/DESIGN.md §7.4). VIAC states no lots and no
//     acquisition date.
//
//   - Transactions are already canonicalised at silver: the
//     `kind` column carries 'buy' / 'sell' / 'fee' / 'interest'
//     / 'dividend' / 'corporate_action' / 'deposit'. The adapter
//     maps those strings to the canonical TxKind enum without
//     re-classification.
//
//   - Cash balances: silver carries one per account per snapshot
//     with kind='cash'. The adapter emits one
//     CashBalanceChange per row with BalanceKindCurrent.
package viac

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "viac"

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

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "viac silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	if err := silver.ProbeOptional(ctx, db, c.schema.optional()...); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

type Connection struct {
	db     *sql.DB
	schema schema
}

// schema is which optional parts of the silver schema a file holds,
// probed once at open.
type schema struct {
	managementStyle bool // accounts.management_style
	dryRun          bool // dump_runs.dry_run
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "accounts", Columns: []string{"management_style"}, Has: &s.managementStyle},
		{Table: "dump_runs", Columns: []string{"dry_run"}, Has: &s.dryRun},
	}
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
