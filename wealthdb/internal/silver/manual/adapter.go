// Package manual projects the manual collector's silver (hand-maintained
// private holdings with no source UI) into canonical change records. See
// collectors/manual/DESIGN.md for the bronze/silver schema and the gold
// mapping this implements.
//
// Three hand-maintained CSV-backed silver tables back this source —
// accounts, positions, valuations — for illiquid private holdings with no
// bank or portal: real estate, direct private-company equity, convertible
// notes, fund LP interests, single-deal SPVs, and other positions (escrow
// receivables, private loans, …). accounts is optional; a book without one
// projects the single default account this source had before accounts
// existed.
//
// Gold projection:
//
//   - One ACCOUNT per declared sleeve, plus the default account
//     (account_external_id = "manual") for every position naming none:
//     AccountKind 'other' — directly-held assets with no institutional
//     container — TaxWrapper 'taxable_personal', ManagementStyle
//     'self_directed'. A declared account carries its own three values
//     instead, which is what lets one book span tax sleeves. All still
//     overridable via account_overrides. An account is emitted at a date
//     only while it holds something then.
//
//   - One INSTRUMENT + one POSITION per held asset, keyed on the position id.
//     The bronze `kind` IS the canonical asset_class (identity classmap,
//     classmap.go). Manual holdings have no ISIN/CUSIP/symbol, so the
//     instrument is adapter-scoped.
//
//   - POSITIONS are per-date forward-filled snapshots reconstructed from the
//     valuations time series (snapshots.go): for every event date a COMPLETE
//     portfolio is emitted (each position's latest valuation on/before that
//     date), so gold's as-of query is correct at any historical date; a
//     position drops out exactly at its closed_at. market_value is the
//     forward-filled valuation; book_value is the valuation dated at
//     acquired_at (the cost basis).
//
//   - NO TRANSACTIONS. The wires that fund a purchase, pay a fee, or return a
//     distribution are real movements in the bank accounts, already
//     captured by the bank collectors; recording them here too would only
//     duplicate them (transactions.go returns an empty stream). See
//     collectors/manual/DESIGN.md §6.
package manual

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "manual"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "manual silver")
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
