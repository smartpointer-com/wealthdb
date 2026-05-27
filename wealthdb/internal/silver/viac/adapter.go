// Package viac projects the viac-dump silver SQLite (VIAC /
// Terzo / WIR Group Swiss Pillar-3a + vested-benefits app) into
// canonical change records.
//
// Single-source CHF-only adapter. Notable shapes:
//
//   - Accounts split by silver `product_code`:
//        '3' → Pillar 3a            → tax_wrapper = pillar_3a
//        '2' → Vested benefits      → tax_wrapper = vested_benefits
//        '1' → Free investment      → tax_wrapper = taxable_personal
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
//   - Positions carry actual per-fund CHF amounts (silver.
//     positions.amount). No derivation needed.
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
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/ptu/wealthdb/internal/silver"
)

const kindName = "viac"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(true)", spec.Path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open viac silver %q: %w", spec.Path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping viac silver %q: %w", spec.Path, err)
	}
	return &Connection{db: db, path: spec.Path}, nil
}

type Connection struct {
	db   *sql.DB
	path string
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
