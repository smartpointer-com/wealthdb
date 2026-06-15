// Package fred projects the fred collector's silver — daily USD-centric
// FX reference rates from the US Federal Reserve H.10 release (FRED) — into
// canonical FxRateChange records. See collectors/fred/ for the bronze/silver
// schema and the series → currency mapping.
//
// Gold projection: FX rates only. The fred silver has a single fact table,
// fx_rates(snapshot_at, base_currency_iso, quote_currency_iso, mid, payload),
// already in the canonical convention — (base, quote, mid) means
// "1 quote = mid base" — so each row maps straight to one FxRateChange.
// fred emits no accounts, positions, instruments, or transactions; it is a
// pure reference-data source that fills the historic FX gap left by the
// account collectors (ubs-psn supplies fresh daily rates but no deep
// history).
//
// fred is a FALLBACK FX source: where another source (e.g. ubs-psn) has a
// rate for a date, gold prefers that one; fred fills the rest. That
// precedence lives in the gold FX resolver (internal/gold/fx.go), which
// ranks reference sources below account sources.
package fred

import (
	"context"
	"database/sql"

	"github.com/ptu/wealthdb/internal/silver"
)

const kindName = "fred"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "fred silver")
	if err != nil {
		return nil, err
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
