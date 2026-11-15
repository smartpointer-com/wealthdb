// Package plaid projects the silver of one Plaid Item into canonical change
// records. See docs/adapters/plaid.md for the mapping this implements.
//
// One silver is one Item: one login at one institution, read through the
// Plaid aggregator (collectors/plaid). A bank, a broker and a card issuer
// all arrive in the same shape, so the adapter decides each account's kind
// from Plaid's own account type and subtype, and each instrument's pair
// from Plaid's security type.
//
// Every snapshot fact of one run carries the run's start, the silver's
// dump_runs.snapshot_at: gold reads a source's current state from its
// single latest snapshot time. Each run restates every account, and each
// run that read the holdings restates every holding. Only a run that read
// the holdings, or found none to read, carries snapshots (snapshots.go),
// so no fact goes missing from that instant.
//
// The source has no change feed. A loaded run is the trigger, and a full
// re-emit of the whole history is the response (status.go).
package plaid

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "plaid"

// init registers the kind (silver.Source).
func init() {
	silver.Register(silver.Source{
		Kind:    kindName,
		Open:    (&Adapter{}).Open,
		Returns: returnsPolicy(),
	})
}

type Adapter struct{}

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "plaid silver")
	if err != nil {
		return nil, err
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db     *sql.DB
	latest silver.Lazy[map[string]account]
	secs   silver.Lazy[map[string]security]
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
