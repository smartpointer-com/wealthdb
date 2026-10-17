// Package refdata projects the refdata collector's silver — fund
// compositions, fund attributes and counterparties, and security
// identities and facts, as the providers served them — into canonical
// reference records (canonical/refdata.go). See collectors/refdata/ for
// the silver schema and docs/adapters/refdata.md for the mapping.
//
// refdata is a reference source like fred: it emits no accounts,
// positions or transactions, so nothing of it reaches the returns engine.
// Its records are dated facts: a composition by the day it describes, a
// security fact by the day it was looked up.
//
// The change clock is the silver load of a run (status.go), and the
// window is not the whole history: it spans the as-of days the runs
// loaded since the watermark wrote, so a composition for an old day that
// arrives late loads like any other and the rest of gold stays as it
// was.
package refdata

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "refdata"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "refdata silver")
	if err != nil {
		return nil, err
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }

// Snapshots emits the reference records whose as_of lies in the window.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	return silver.NewReferenceStream(ctx, c.db, w)
}

// Transactions: refdata has no event-grain facts.
func (c *Connection) Transactions(_ context.Context, _ canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
