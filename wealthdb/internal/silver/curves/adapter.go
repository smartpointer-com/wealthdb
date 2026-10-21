// Package curves projects the curves collector's silver, the public
// government yield curves of the euro (the ECB's) and the franc (the
// SNB's), into canonical RateObservationChange records. See
// collectors/curves/ for the bronze/silver schema and the series maps.
//
// Gold projection: published interest rates only. The silver's one fact
// table, rates(as_of, series_id, value, payload), is the fred collector's
// table column for column, so silver.NewRateStream reads it and each row
// maps to one RateObservationChange. Each row's payload states the
// series' currency, tenor, basis and compounding, which the yield report
// reads to build the curve. curves emits no accounts, positions,
// instruments, transactions or FX rates.
package curves

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "curves"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "curves silver")
	if err != nil {
		return nil, err
	}
	return &Connection{db: db}, nil
}

// Connection reads one curves silver.
type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
