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

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// kinds are the source kinds this package reads.
var kinds = []string{"fred", "curves"}

// init registers each kind (silver.Source).
func init() {
	for _, kind := range kinds {
		silver.Register(silver.Source{
			Kind: kind,
			Open: (&Adapter{kind: kind}).Open,
		})
	}
}

// Adapter opens the silver of one kind.
type Adapter struct{ kind string }

func (a *Adapter) Kind() string { return a.kind }

func (a *Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, a.kind+" silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{kind: a.kind, db: db}
	if err := silver.ProbeOptional(ctx, db, c.schema.optional()...); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s silver: %w", a.kind, err)
	}
	return c, nil
}

// Connection reads one silver of the kind.
type Connection struct {
	kind   string
	db     *sql.DB
	schema schema
}

// schema is which fact tables a file holds, probed once at open.
type schema struct {
	fx, rates bool
}

func (s *schema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: silver.FxRatesTable, Has: &s.fx},
		{Table: silver.RatesTable, Has: &s.rates},
	}
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
