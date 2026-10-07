// Package manual projects the manual collector's silver (hand-maintained
// private holdings with no source UI) into canonical change records. See
// collectors/manual/DESIGN.md for the bronze/silver schema and the gold
// mapping this implements.
//
// Hand-maintained CSV-backed silver tables back this source — accounts,
// positions, valuations, cost_basis — for illiquid private holdings with no
// bank or portal: real estate, direct private-company equity, convertible
// notes, fund LP interests, single-deal SPVs, and other positions (escrow
// receivables, private loans, …). accounts and cost_basis are optional; a
// book without accounts projects the single default account.
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
//     forward-filled valuation. book_value is the forward-filled cost_basis
//     entry for a position the optional paid-in series covers, else the
//     valuation dated at acquired_at.
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
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "manual"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "manual silver")
	if err != nil {
		return nil, err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
	        WHERE type = 'table' AND name = 'cost_basis'`).Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("manual silver: %w", err)
	}
	return &Connection{db: db, costBasis: n > 0}, nil
}

// Connection reads one manual silver. costBasis says whether it holds the
// paid-in series table: a silver last loaded before that table existed lacks
// it until the next load, and reads as a book with no series.
type Connection struct {
	db        *sql.DB
	costBasis bool
}

// eventDates is the SQL that lists every holding event date, as unix
// seconds in column t: each position's acquired_at and closed_at, each
// valuation's as_of_date, and each cost_basis entry's as_of_date.
func (c *Connection) eventDates() string {
	q := `
    SELECT CAST(strftime('%s', acquired_at) AS INTEGER) AS t FROM positions
    UNION ALL SELECT CAST(strftime('%s', closed_at)  AS INTEGER) FROM positions WHERE closed_at IS NOT NULL
    UNION ALL SELECT CAST(strftime('%s', as_of_date) AS INTEGER) FROM valuations`
	if c.costBasis {
		q += `
    UNION ALL SELECT CAST(strftime('%s', as_of_date) AS INTEGER) FROM cost_basis`
	}
	return q
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
