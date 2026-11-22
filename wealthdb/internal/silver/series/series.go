// Package series projects the silver of the public series sources into
// canonical records. Two kinds register here, one descriptor each:
//
//   - fred: daily USD-centric FX reference rates from the US Federal
//     Reserve H.10 release and the Treasury yields of the H.15 release
//     (collectors/fred).
//   - curves: the public government yield curves of the euro (the
//     ECB's) and the franc (the SNB's), one rate series per tenor
//     (collectors/curves).
//
// A silver holds up to two fact tables, and Open probes for each:
//
//   - fx_rates(snapshot_at, base_currency_iso, quote_currency_iso, mid,
//     payload) is already in the canonical convention: (base, quote,
//     mid) means "1 quote = mid base". Each row maps to one
//     FxRateChange (silver.NewFxRateStream). fred writes it.
//   - rates(as_of, series_id, value, payload) holds the rate series.
//     Each row maps to one RateObservationChange (silver.NewRateStream).
//     Its payload states the series' currency, tenor, basis and
//     compounding, which the yield report reads to build a curve. curves
//     writes it, and fred from its silver migration 0002 on. A fred file
//     without the table states no rate series.
//
// Neither kind emits accounts, positions, instruments or transactions.
//
// fred is a FALLBACK FX source: where another source (e.g. ubs-psn) has a
// rate for a date, gold prefers that one; fred fills the rest. That
// precedence is data-driven — the gold fx_daily view orders by the
// silver_sources.fx_priority column (stamped from config by
// gold.SetFxPriorities), which ranks reference sources below account
// sources.
package series

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// kinds are the source kinds this package reads, each with the
// collector that writes its silver (silver.Collector).
var kinds = map[string]silver.Collector{
	"fred": {
		Name:    "fred",
		Display: "FRED exchange and interest rates",
		Group:   silver.GroupMarketData,
		Covers:  "exchange rates and US interest rates; needs a free API key",
		Files:   []string{"fred.db"},
		FX:      true,
	},
	"curves": {
		Name:    "curves",
		Display: "Government yield curves",
		Group:   silver.GroupMarketData,
		Covers:  "EUR and CHF yield curves, for the yield report",
		Files:   []string{"curves.db"},
	},
}

// init registers each kind (silver.Source).
func init() {
	for kind, c := range kinds {
		silver.Register(silver.Source{
			Kind:       kind,
			Open:       (&Adapter{kind: kind}).Open,
			Collectors: []silver.Collector{c},
		})
	}
}

// Adapter opens the silver of one kind.
type Adapter struct{ kind string }

func (a *Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, a.kind+" silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{kind: a.kind, db: db}
	note, err := silver.ProbeOptional(ctx, db, filepath.Base(spec.Path), c.schema.optional(a.kind)...)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("%s silver: %w", a.kind, err)
	}
	c.Note(note)
	return c, nil
}

// Connection reads one silver of the kind.
type Connection struct {
	kind   string
	db     *sql.DB
	schema schema
	silver.Advice
}

// schema is which fact tables a file holds, probed once at open.
type schema struct {
	fx, rates bool
}

// optional lists the fact tables, each with the version of the kind's
// collector migration that adds it: fred's FX rates from its first and its
// rates from its second; curves states no FX rates, and its rates from its
// first.
func (s *schema) optional(kind string) []silver.Optional {
	fx, rates := 1, 2
	if kind == "curves" {
		fx, rates = 0, 1
	}
	return []silver.Optional{
		{Table: silver.FxRatesTable, Version: fx, Has: &s.fx},
		{Table: silver.RatesTable, Version: rates, Has: &s.rates},
	}
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
