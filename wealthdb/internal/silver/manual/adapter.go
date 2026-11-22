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
//     The bronze `kind` sets the exposure and silver's `vehicle` column
//     the vehicle, with a kind-derived default (classmap.go). Manual
//     holdings have no ISIN/CUSIP/symbol, so the instrument is
//     adapter-scoped.
//
//   - POSITIONS are per-date forward-filled snapshots reconstructed from the
//     valuations time series (snapshots.go): for every event date a COMPLETE
//     portfolio is emitted (each position's latest valuation on/before that
//     date), so gold's as-of query is correct at any historical date; a
//     position drops out exactly at its closed_at. market_value is the
//     forward-filled valuation. book_value is the forward-filled cost_basis
//     entry for a position the optional paid-in series covers, stamped
//     stated / paid_in / unknown, else the valuation dated at acquired_at,
//     stamped stated / acquisition_value / unknown (docs/DESIGN.md §7.4):
//     a figure entered by hand states nothing about fees.
//
//   - ONE TRANSACTION per exit: a `sell` for what a position brought on the
//     day it left the book (exits.csv), which the lot engine realizes
//     against its book value. The wires that fund a purchase, pay a fee,
//     or return a distribution are real movements in the bank accounts,
//     already captured by the bank collectors, so nothing else is
//     recorded here (transactions.go). See collectors/manual/DESIGN.md §6.
package manual

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "manual"

// collectors are the collectors whose silver the kind reads
// (silver.Collector).
var collectors = []silver.Collector{
	{
		Name:    "manual",
		Display: "Manual holdings (CSV files)",
		Group:   silver.GroupWorldwide,
		Covers:  "property, loans and other holdings recorded by hand",
		Files:   []string{"manual.db"},
	},
}

// init registers the kind (silver.Source).
//
// Lots: the cost basis of the holdings it is typed from is what the
// source states, capital paid in, and no lot book describes it
// (docs/LOTS.md §6). The engine builds none and realizes a sale against
// the stated basis. Its account labels and holdings are named by hand:
// the holder's own words.
func init() {
	lp := lots.StatedPolicy()
	silver.Register(silver.Source{
		Kind:       kindName,
		Open:       (&Adapter{}).Open,
		Collectors: collectors,
		Returns:    returnsPolicy(),
		Lots:       &lp,
		Labels:     silver.LabelTraits{TypedAccountLabels: true, TypedHoldings: true},
	})
}

type Adapter struct{}

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "manual silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	note, err := silver.ProbeOptional(ctx, db, filepath.Base(spec.Path), c.schema.optional()...)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("manual silver: %w", err)
	}
	c.Note(note)
	return c, nil
}

// Connection reads one manual silver.
type Connection struct {
	db     *sql.DB
	schema schema
	silver.Advice
}

// schema is which optional parts of the silver schema a file holds,
// probed once at open. costBasis and exits say whether it holds the
// paid-in series and the exits tables: a silver last loaded before a
// table existed lacks it until the next load, and reads as a book
// without that series.
type schema struct {
	costBasis, exits bool
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "cost_basis", Version: 4, Has: &s.costBasis},
		{Table: "exits", Version: 5, Has: &s.exits},
	}
}

// eventDates is the SQL that lists every holding event date, as unix
// seconds in column t: each position's acquired_at and closed_at, each
// valuation's as_of_date, and each cost_basis entry's as_of_date.
func (c *Connection) eventDates() string {
	q := `
    SELECT CAST(strftime('%s', acquired_at) AS INTEGER) AS t FROM positions
    UNION ALL SELECT CAST(strftime('%s', closed_at)  AS INTEGER) FROM positions WHERE closed_at IS NOT NULL
    UNION ALL SELECT CAST(strftime('%s', as_of_date) AS INTEGER) FROM valuations`
	if c.schema.costBasis {
		q += `
    UNION ALL SELECT CAST(strftime('%s', as_of_date) AS INTEGER) FROM cost_basis`
	}
	return q
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
