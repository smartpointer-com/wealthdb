// Package equityzen projects the equityzen silver SQLite (EquityZen
// pre-IPO secondary marketplace — a buyer's own holdings) into canonical
// change records.
//
// Single-source, USD adapter for a buyer's book of membership interests in
// single-company SPVs and multi-company funds. The collector does the
// valuation + lifecycle work and stores an event-sourced position history
// plus a purchase/distribution cash ledger; this adapter forward-fills the
// positions and maps the ledger to transactions. Notable shapes:
//
//   - One account, a constant (the silver has no buyer-id column and the
//     holder has one relationship). "equityzen" is the custody account holding
//     the positions AND the double-entry transaction pairs (see
//     transactions.go): account_kind = custody (EquityZen administers the
//     interests; the buyer places no trades — cf. carta / angellist),
//     tax_wrapper = taxable_personal, management_style = self_directed (the
//     holder chooses which interests to hold; the GP management inside each
//     vehicle is not modelled; config overrides win).
//
//   - One instrument + position per offering (silver `offerings`, keyed by
//     deal_external_id — the SPV/fund interest, never merged). asset_class
//     from offerings.kind: single-company -> spv, multi-company fund ->
//     private_fund. Instrument name = the underlying company / fund label;
//     a single-company SPV also carries EquityZen's per-company symbol (an
//     EZ-internal ticker, surfaced so positions show a symbol like public
//     equities) — funds have none. No ISIN/CUSIP (private, non-quotable).
//
//   - Positions forward-filled from the event-sourced `positions` table
//     (silver migrations 0001/0002): for each event date the adapter emits
//     each deal's latest event on/before it (by event_seq), dropping the
//     is_open=0 (exited) ones — a COMPLETE portfolio per date, which is what
//     gold's as-of query reads. market_value is the collector's mark
//     (CLOSED-deal prices for SPVs; parsed capital-account-statement NAVs
//     for funds). quantity = shares_held for SPVs, NULL for funds (units
//     are not a share count). Silver dates are source ISO TEXT; the
//     adapter converts to unix seconds (strftime).
//
//   - book_value includes the execution fee charged on the purchase
//     (basis.go, docs/DESIGN.md §7.4). An SPV's is `cost_basis_remaining`,
//     the stake still held at the price paid, plus fee × shares held ÷
//     shares bought (at most the whole fee): derived / average /
//     included. A fund's is the capital paid in, gross (`offerings.basis`),
//     plus the whole fee: derived / paid_in / included. A purchase that
//     states no fee leaves it out, fees unknown.
//
//   - Transactions from the `cash_flows` ledger, as balanced double-entry
//     pairs on the custody account: a purchase -> deposit + buy (spv) or
//     deposit + contribution (fund); a distribution -> sell + withdrawal for
//     an SPV (a tax-transparent single-stock vehicle realizes the
//     underlying), or distribution + withdrawal for a multi-company fund
//     (which reinvests, so a payout is not a sale). A $0 distribution (an
//     exit with no proceeds) omits the $0 withdrawal. See transactions.go.
package equityzen

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "equityzen"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "equityzen silver")
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
