// Package refdata projects the refdata collector's silver — fund
// compositions, attributes, counterparties, rate sensitivities and
// characteristics, and security identities, facts and reported figures,
// as the providers served them — into canonical reference records
// (canonical/refdata.go). See collectors/refdata/ for the silver schema
// and docs/adapters/refdata.md for the mapping.
//
// refdata is a reference source like fred: it emits no accounts,
// positions or transactions, so nothing of it reaches the returns engine.
// Its records are dated facts: a composition by the day it describes, a
// company's figure by the last day of its period, a security fact by the
// day it was looked up.
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
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "refdata"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "refdata silver")
	if err != nil {
		return nil, err
	}
	yield, err := silver.HasTables(ctx, db, silver.ReferenceYieldTables...)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("refdata silver: %w", err)
	}
	return &Connection{db: db, yield: yield}, nil
}

// Connection reads one refdata silver. yield says whether it holds the
// tables of silver migration 0002 (silver.ReferenceYieldTables): a silver
// last loaded before that migration lacks them and reads as stating none.
type Connection struct {
	db    *sql.DB
	yield bool
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }

// Snapshots emits the reference records whose as_of lies in the window,
// each kind over its own span (ChangeWindow).
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	return silver.NewReferenceStream(ctx, c.db, w)
}

// Transactions: refdata has no event-grain facts.
func (c *Connection) Transactions(_ context.Context, _ canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
