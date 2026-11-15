// Package silver defines the contract between per-bank silver
// adapters and the load sequence in internal/loader. See
// docs/DESIGN.md §6 for the full design.
//
// Adapters never touch the gold database directly; they read silver
// SQLite (or, for cointracking, DuckDB) files and yield canonical
// change records via the streams defined here. The main app owns all
// gold writes, transaction boundaries, and watermark bookkeeping.
package silver

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Adapter is the per-bank entry point. Each backend package
// (internal/silver/{schwab,ubs,swissquote,...}) registers its Open in
// a Source from its init() function via Register.
type Adapter interface {
	// Open attaches to the silver source(s) described by spec and
	// returns a Connection. The Connection is single-use; callers
	// Close() when done. Open is read-only — adapters never write
	// to silver.
	Open(ctx context.Context, spec OpenSpec) (Connection, error)
}

// OpenSpec is everything Adapter.Open needs to know about the
// configured silver source. Single-file adapters read Path. The merged
// adapters (schwab = api + web, UBS = web + PSN) read Subsources and
// Settings. UBS also reads ID.
type OpenSpec struct {
	// ID is the source's id in gold (silver_source_id), for an adapter
	// whose keys are scoped by the source, such as a contract's
	// (canonical.ContractKey).
	ID string

	// Path is the silver SQLite path for single-file adapters.
	// Empty when Subsources is used.
	Path string

	// Subsources lists the backing silvers when one logical source
	// is composed from several (UBS = ubs-web + ubs-psn). Each
	// entry's Kind is adapter-specific; the adapter dispatches.
	// At least one Subsource must be present when Subsources is
	// set.
	Subsources []Subsource

	// Settings is the source's own settings as its kind's
	// Source.Settings parsed them from the config, nil where the
	// config states none.
	Settings any
}

// Subsource is one backing silver inside a merged Adapter.
type Subsource struct {
	Kind string
	Path string
}

// Connection is one attached silver database, ready to answer
// Status / ChangeWindow / Snapshots / Transactions calls. Not
// goroutine-safe; use one Connection per goroutine if you need
// concurrency.
type Connection interface {
	// Close releases the underlying *sql.DB handle. Calling Close
	// more than once is safe; further calls are no-ops.
	Close() error

	// Status is a read-only snapshot of where the silver DB stands.
	// Cheap to call (a few aggregate queries). The main app uses
	// it on every `wealthdb load` and `wealthdb status`. All
	// timestamps are Unix seconds UTC; -1 is the "no observable
	// state" sentinel.
	Status(ctx context.Context) (canonical.Status, error)

	// ChangeWindow returns the time window of changes in silver
	// since the given logical change number, plus the new change
	// number to advance the gold watermark to upon successful
	// load. If `Window.HasChanges` is false, the caller is done
	// and should skip the Snapshots / Transactions calls (but
	// may still advance the watermark to track progress).
	ChangeWindow(ctx context.Context, sinceChangeNumber int64) (canonical.Window, error)

	// Snapshots iterates dimension upserts and snapshot-grain
	// facts (positions, open lots, cash balances, fx rates,
	// reference records, and portfolio, account and instrument
	// updates) whose snapshot_at falls inside the given window.
	// Pull-model batched iterator: caller loops Next() until
	// more=false.
	Snapshots(ctx context.Context, window canonical.Window) (SnapshotStream, error)

	// Transactions iterates event-grain facts whose occurred_at
	// falls inside the given window. Pull-model batched iterator.
	Transactions(ctx context.Context, window canonical.Window) (TransactionStream, error)
}

// SnapshotStream yields batches of canonical snapshot-grain
// records. Each Next call returns one batch and whether more
// batches follow. A more=false return still delivers a valid (but
// possibly empty) final batch; callers should apply it before
// stopping.
type SnapshotStream interface {
	Next(ctx context.Context) (batch canonical.SnapshotBatch, more bool, err error)
	Close() error
}

// TransactionStream is the event-grain counterpart of
// SnapshotStream.
type TransactionStream interface {
	Next(ctx context.Context) (batch canonical.TransactionBatch, more bool, err error)
	Close() error
}
