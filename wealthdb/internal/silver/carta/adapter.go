// Package carta projects the carta silver SQLite (private-market
// holdings on carta.com) into canonical change records.
//
// This source is one Carta "individual portfolio" holding one or more
// ENTITIES, each either:
//
//   - a cap-table corporation (is_fund_investment=0): direct
//     private-company equity and equity-comp — shares, option
//     grants (with strike + vesting), RSUs/RSAs, SAFEs/notes,
//     warrants. Mapped to AssetClassPrivateEquity.
//
//   - a fund investment (is_fund_investment=1): an LP interest in a
//     venture/PE fund, with a structured capital account
//     (commitment / called / contributed / distributions / NAV).
//     Mapped to AssetClassPrivateFund.
//
// Gold projection:
//
//   - ONE custody ACCOUNT for the whole portfolio (account_external_id
//     = the Carta individual_id; AccountKind 'custody', TaxWrapper
//     'taxable_personal', ManagementStyle 'self_directed' — the
//     fund-vs-equity split rides on each position's asset_class), plus
//     a sentinel 'carta-funding' cash account carrying the transaction
//     pairs (transactions.go). All overridable via account_overrides.
//
//   - One INSTRUMENT per held company (the issuer / the fund), keyed
//     by the entity id. Carta private securities have no
//     ISIN/CUSIP/symbol, so the instrument is adapter-scoped.
//
//   - One POSITION per held company: the cap-table share / option lots
//     aggregated (Quantity = the share count, MarketValue the per-date
//     valuation, BookValue = cost; the per-lot detail in the payload),
//     or the fund's capital account (MarketValue = NAV, BookValue =
//     contributed). Forward-filled per event date (snapshots.go).
//
//   - TRANSACTIONS: the cash-flow ledger (silver migration 0003)
//     projected as balanced double-entry pairs on the funding sentinel
//     (transactions.go), so its derived balance is always 0.
//
// Vesting schedules, documents, and cap-call rows stay silver-only —
// gold has no canonical home for them today.
package carta

import (
	"context"
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "carta"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "carta silver")
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
