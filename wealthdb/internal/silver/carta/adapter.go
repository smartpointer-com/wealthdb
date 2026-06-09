// Package carta projects the carta silver SQLite (private-market
// holdings on carta.com) into canonical change records.
//
// Single-source, snapshot-only adapter (no transactions surfaced —
// the carta silver has no transaction table; exercises ride inside
// the securities payload and fund cash-flows are captured as
// documents, not structured events).
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
//   - One ACCOUNT per entity. account_external_id = the Carta
//     entity (corporation_id). AccountKind 'custody' (Carta
//     safekeeps/administers private securities — it is not a
//     trading brokerage). TaxWrapper 'taxable_personal'.
//     ManagementStyle 'discretionary' for fund entities (GP-managed),
//     'self_directed' for cap-table entities (the holder controls
//     exercise/sale). All overridable via config account_overrides.
//
//   - One INSTRUMENT per entity (the issuer company / the fund),
//     keyed by the entity id. Carta private securities have no
//     ISIN/CUSIP/symbol, so the instrument is adapter-scoped.
//
//   - POSITIONS: one per cap-table security row (share lot / option
//     grant / …) with Quantity + BookValue=cost and a NULL
//     MarketValue (no current private valuation is exposed by the
//     captured endpoints — including an exited holding's realization
//     value); plus one position per fund entity with
//     MarketValue=NAV and BookValue=capital_contributed.
//
// Vesting schedules, documents, and cap-call rows stay silver-only —
// gold has no canonical home for them today.
package carta

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/ptu/wealthdb/internal/silver"
)

const kindName = "carta"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(true)", spec.Path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open carta silver %q: %w", spec.Path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping carta silver %q: %w", spec.Path, err)
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
