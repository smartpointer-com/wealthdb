// Package relevate projects the relevate silver SQLite
// (Swiss Pillar-2 vested-benefits foundation: Pensexpert /
// pens-expert.ch) into canonical change records.
//
// Single-source, CHF-only adapter. Two structural quirks worth
// flagging up front:
//
//   - Silver's `positions` table carries per-fund TARGET
//     ALLOCATIONS (positions[].allocation, 0..1), not held
//     quantities or per-fund market values. Relevate's
//     pre-defined-strategy products rebalance to those targets
//     periodically. The adapter derives per-position market
//     value as `securities_balance * allocation` so gold's
//     positions table can carry meaningful per-fund values
//     even though silver doesn't store them. The derivation
//     is correct in steady state (between rebalances drift
//     introduces error, but that's the nature of allocation
//     models). The aggregate `securities_balance` from silver
//     equals the sum of these derived per-fund values exactly.
//
//   - Silver's `cash_balances` carries seven balance kinds
//     (cash / current / invested / investment / saving /
//     securities / virtual). Only `cash` is liquid cash the
//     account holder hasn't yet invested; the others are
//     computed aggregates over the same underlying money.
//     Gold's cash_balances gets one row per account from the
//     `cash` kind only — the rest would double-count against
//     the per-fund position values derived above.
//
// Account taxonomy:
//
//   - AccountKind:      brokerage (Pillar-2 vested benefits
//     holds investible securities, not just
//     deposit cash).
//   - TaxWrapper:       vested_benefits (the Swiss canonical
//     enum value for Pillar-2 in transit /
//     Freizügigkeit).
//   - ManagementStyle:  automated. Relevate's robo-style menu
//     maps to canonical 'automated': the
//     holder picks a strategy from a fixed
//     list, then an algorithm allocates and
//     rebalances with no human in the loop.
package relevate

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "relevate"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "relevate silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	if err := silver.ProbeOptional(ctx, c.db, c.schema.optional()...); err != nil {
		c.db.Close()
		return nil, err
	}
	return c, nil
}

type Connection struct {
	db     *sql.DB
	schema schema
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }

// schema is which optional parts of the silver schema a file holds,
// probed once at open.
type schema struct {
	historicalPositions bool // the Quartalsbericht snapshots (silver migration 0002)
	historicalCash      bool // their cash balances (silver migration 0002)
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "historical_position_snapshots", Has: &s.historicalPositions},
		{Table: "historical_cash_balances", Has: &s.historicalCash},
	}
}
