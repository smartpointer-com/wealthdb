// Package silver defines the contract between per-bank silver
// adapters and the load orchestration in cmd/wealthdb. See
// docs/DESIGN.md §6 for the full design.
//
// Adapters never touch the gold database directly; they read silver
// SQLite files and yield canonical change records via the streams
// defined here. The main app owns all gold writes, transaction
// boundaries, and watermark bookkeeping.
package silver

import (
	"context"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Adapter is the per-bank entry point. Each backend package
// (internal/silver/{schwab,ubs,swissquote,...}) registers one
// Adapter from its init() function via Register.
type Adapter interface {
	// Kind returns the silver-kind discriminator that appears in
	// the wealthdb config file's silver_sources[].kind field
	// (e.g. "schwab", "ubs", "swissquote").
	Kind() string

	// Open attaches to the silver SQLite at `path` and returns a
	// Connection. The Connection is single-use; callers Close()
	// when done. Open is read-only — adapters never write to
	// silver.
	Open(ctx context.Context, path string) (Connection, error)
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
	// facts (positions, cash balances, fx rates, account and
	// instrument updates) whose snapshot_at falls inside the
	// given window. Pull-model batched iterator: caller loops
	// Next() until ok=false.
	Snapshots(ctx context.Context, window canonical.Window) (SnapshotStream, error)

	// Transactions iterates event-grain facts whose occurred_at
	// falls inside the given window. Pull-model batched iterator.
	Transactions(ctx context.Context, window canonical.Window) (TransactionStream, error)
}

// SnapshotStream yields batches of canonical snapshot-grain
// records. Each Next call returns one batch and whether more
// batches follow. An ok=false return still delivers a valid (but
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
