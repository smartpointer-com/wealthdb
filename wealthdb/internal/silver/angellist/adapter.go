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
//     never merged). asset_class from offerings.kind: single-company
//     SPVs/RUVs -> spv, multi-company funds -> private_fund (both
//     illiquid, non-quotable). Instrument name = the underlying company;
//     the SPV legal name + EIN ride in offerings. No ISIN / symbol.
//
//   - Positions are forward-filled from the event-sourced
//     `position_snapshots` (silver migration 0005): for each event date
//     the adapter emits each position's latest snapshot on/before it,
//     dropping the is_open=0 (exited) ones — a COMPLETE portfolio per
//     date, which is what gold's as-of query reads. market_value =
//     `market_value_minor` (the collector's mark: current FMV, else the
//     annual K-1 tax-basis NAV, else cost — never blended within a
//     snapshot). Money is minor units (cents); the adapter scales by
//     10^-2. quantity = NULL (LP interests have no unit quantity).
//
//   - book_value is the capital paid in (docs/DESIGN.md §7.4): the
//     portal's `contributed_minor`, gross of cash paid back, stamped
//     stated / paid_in / included. A K-1 that states a Line 19(c)
//     property distribution (`k1_capital_accounts`, silver migration
//     0008) moves that basis out with the asset: from the K-1's period
//     end (Dec 31 of its tax year) the book value is the paid-in figure
//     less every such distribution so far, never below zero, stamped
//     derived / paid_in / included (basis.go). The K-1 reaches its
//     position through the fund name the collector's pairing stamps on
//     the offering.
//
//   - Transactions + cash: the funding-account ledger
//     (silver `funding_transactions`) is the dated cash flow. Each row maps
//     to a canonical kind by its source type — deposit/withdrawal (external
//     bank ↔ account), investment→contribution and refund→contribution
//     (a positive reversal), disbursement→distribution, transfer→withdrawal
//     — using the source-signed amount (it reconciles to the balance). The
//     account's current uninvested cash is one CashBalanceChange
//     (BalanceKind=current), so account value = positions + cash.
package angellist

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "angellist"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "angellist silver")
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
