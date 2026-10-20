// Package fred projects the fred collector's silver — daily USD-centric
// FX reference rates from the US Federal Reserve H.10 release and the
// Treasury yields of the H.15 release (FRED) — into canonical FxRateChange
// and RateObservationChange records. See collectors/fred/ for the
// bronze/silver schema and the series maps.
//
// Gold projection: FX rates and published interest rates. The fred silver
// has two fact tables. fx_rates(snapshot_at, base_currency_iso,
// quote_currency_iso, mid, payload) is already in the canonical
// convention — (base, quote, mid) means "1 quote = mid base" — so each row
// maps straight to one FxRateChange. rates(as_of, series_id, value,
// payload) holds the yield series (silver migration 0002); each row maps
// to one RateObservationChange, and a file without the table states none.
// fred emits no accounts, positions, instruments, or transactions; it is a
// pure reference-data source that fills the historic FX gap left by the
// account collectors (ubs-psn supplies fresh daily rates but no deep
// history) and serves the public curves the yield report reads.
//
// fred is a FALLBACK FX source: where another source (e.g. ubs-psn) has a
// rate for a date, gold prefers that one; fred fills the rest. That
// precedence is data-driven — the gold fx_daily view orders by the
// silver_sources.fx_priority column (stamped from config by
// gold.SetFxPriorities), which ranks reference sources below account
// sources.
package fred

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "fred"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "fred silver")
	if err != nil {
		return nil, err
	}
	rates, err := silver.HasTables(ctx, db, silver.RatesTable)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("fred silver: %w", err)
	}
	return &Connection{db: db, rates: rates}, nil
}

// Connection reads one fred silver. rates says whether it holds the rates
// table (silver migration 0002): a silver last loaded before that
// migration lacks it and reads as stating no rate series.
type Connection struct {
	db    *sql.DB
	rates bool
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
