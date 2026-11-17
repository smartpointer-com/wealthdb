// Package angellist projects the angellist silver SQLite (AngelList
// venture LP portal) into canonical change records.
//
// Single-source, USD adapter for a limited-partner book of SPVs and
// fund deals. The collector does the valuation + lifecycle work and
// stores a per-position EVENT timeline; this adapter forward-fills it,
// and takes the basis that left in kind off the book value. Notable
// shapes:
//
//   - One account for the whole LP book (silver
//     `dump_runs.invest_account_slug`). The holder owns many SPV stakes
//     inside that one account, so each stake maps to an instrument +
//     position, not to an account. Stamped account_kind = custody (LP
//     interests held in custody, not a brokerage), tax_wrapper =
//     taxable_personal, management_style = self_directed (the holder
//     chooses which deals to back; the GP's management inside each vehicle
//     is not modeled) — same as carta / equityzen; config overrides win.
//
//   - One instrument + position per SPV stake (silver `offerings`, one
//     row per AngelList position id — the SPV, not the company; SPVs are
//     never merged). The taxonomy pair from offerings.kind: exposure
//     private_equity for both, vehicle spv for a single-company SPV/RUV and
//     fund for a multi-company fund (both illiquid, non-quotable).
//     Instrument name = the underlying company;
//     the SPV legal name + EIN ride in offerings. No ISIN / symbol.
//
//   - Positions are forward-filled from the event-sourced
//     `position_snapshots` (silver migration 0005): for each event date
//     the adapter emits each position's latest snapshot on/before it,
//     dropping the is_open=0 (exited) ones — a COMPLETE portfolio per
//     date, which is what gold's as-of query reads. market_value =
//     `market_value_minor` where the latest mark states it (current FMV,
//     else a fund statement's fair value, else the annual K-1 tax-basis
//     NAV — never blended within a snapshot), plus what the funding
//     ledger moved into the position since at cost; before any mark the
//     position is at that cost alone (carry.go). Each ledger movement is
//     a snapshot day. A position is held from its first payment where
//     that comes first, and a deal only the ledger names while what the
//     ledger moved into it is above zero. The position is valued at its
//     last mark's day, or with none at its first payment's, and its
//     payload's valuation_basis says `cost` or `mark_plus_cost`.
//     Money is minor units (cents); the adapter scales by 10^-2.
//     quantity = NULL (LP interests have no unit quantity).
//
//   - book_value is the capital paid in (docs/DESIGN.md §7.4): the
//     portal's `contributed_minor`, gross of cash paid back, stamped
//     stated / paid_in / included. Where the portal states none, it is
//     the capital the funding ledger paid in less its refunds, stamped
//     derived / paid_in / included. A K-1 that states a Line 19(c)
//     property distribution (`k1_capital_accounts`, silver migration
//     0008) moves that basis out with the asset: from the K-1's period
//     end (Dec 31 of its tax year) the book value is the paid-in figure
//     less every such distribution so far, never below zero, stamped
//     derived / paid_in / included (silver.InKind). The period end is a
//     snapshot day, so the cut lands on its date. The K-1 reaches its
//     position through the fund name the collector's pairing stamps on
//     the offering; a fund name no single offering carries reduces
//     nothing, and the load names it (basis.go).
//
//   - Transactions + cash: the funding-account ledger
//     (silver `funding_transactions`) is the dated cash flow. Each row maps
//     to a canonical kind by its source type — deposit/withdrawal (external
//     bank ↔ account), investment→contribution and refund→contribution
//     (a positive reversal), disbursement→sell on an SPV and distribution
//     on a fund, transfer→withdrawal — using the source-signed amount (it
//     reconciles to the balance). The wallet's uninvested cash is a
//     CashBalanceChange (BalanceKind=current) at the end of every ledger
//     day, read from the ledger (cash.go), so account value = positions +
//     cash on every day.
package angellist

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "angellist"

// init registers the kind (silver.Source).
//
// Lots: the cost basis of its SPV stakes and fund interests is what
// the source states, capital paid in, and no lot book describes it
// (docs/LOTS.md §6). The engine builds none and realizes a sale against
// the stated basis. An account id is the holder's portal slug.
func init() {
	lp := lots.StatedPolicy()
	silver.Register(silver.Source{
		Kind:    kindName,
		Open:    (&Adapter{}).Open,
		Returns: returnsPolicy(),
		Lots:    &lp,
		Labels:  silver.LabelTraits{TypedAccountIDs: true},
	})
}

type Adapter struct{}

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "angellist silver")
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
	propertyMinor       bool // k1_capital_accounts.property_distributions_minor
	fundingAccounts     bool // the funding_accounts table
	fundingTransactions bool // the funding_transactions table
	fundingLinks        bool // funding_transactions.position_external_id
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "k1_capital_accounts", Columns: []string{"property_distributions_minor"}, Version: 8, Has: &s.propertyMinor},
		{Table: "funding_accounts", Version: 6, Has: &s.fundingAccounts},
		{Table: "funding_transactions", Version: 6, Has: &s.fundingTransactions},
		{Table: "funding_transactions", Columns: []string{"position_external_id"}, Version: 7, Has: &s.fundingLinks},
	}
}
