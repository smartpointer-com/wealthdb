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

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Adapter is the per-bank entry point. Each backend package
// (internal/silver/{schwab,ubs,swissquote,...}) registers one
// Adapter from its init() function via Register.
type Adapter interface {
	// Kind returns the silver-kind discriminator that appears in
	// the wealthdb config file's silver_sources[].kind field
	// (e.g. "schwab", "ubs", "swissquote").
	Kind() string

	// Open attaches to the silver source(s) described by spec and
	// returns a Connection. The Connection is single-use; callers
	// Close() when done. Open is read-only — adapters never write
	// to silver. Single-backing-file adapters use spec.Path;
	// multi-source adapters (UBS = web + PSN) use spec.Subsources
	// and may use spec.Relationships to align cross-source
	// identities.
	Open(ctx context.Context, spec OpenSpec) (Connection, error)
}

// OpenSpec is everything Adapter.Open needs to know about the
// configured silver source. Single-file adapters only read Path;
// merged adapters (UBS) read Subsources and Relationships.
type OpenSpec struct {
	// Path is the silver SQLite path for single-file adapters.
	// Empty when Subsources is used.
	Path string

	// Subsources lists the backing silvers when one logical source
	// is composed from several (UBS = ubs-web + ubs-psn). Each
	// entry's Kind is adapter-specific; the adapter dispatches.
	// At least one Subsource must be present when Subsources is
	// set.
	Subsources []Subsource

	// Relationships pairs cross-subsource entity identities under
	// a single user-chosen label. Adapters that don't merge
	// ignore this. UBS uses it to pair web banking_relationship_id
	// with PSN SFTP relationship_id.
	Relationships []RelationshipPair
}

// Subsource is one backing silver inside a merged Adapter.
type Subsource struct {
	Kind string
	Path string
}

// RelationshipPair is one entry in OpenSpec.Relationships.
// Either WebID or PSNID (or both) must be set; only the
// configured side is used.
type RelationshipPair struct {
	// Label is the canonical (user-readable) name the adapter
	// stamps on canonical records so downstream sees a single key
	// regardless of which subsource produced the record.
	Label string
	// WebID, when set, is the web silver's banking_relationship_id.
	WebID string
	// PSNID, when set, is the PSN silver's relationship_id.
	PSNID string
	// PSNStartOverride, when non-zero, overrides the auto-detected
	// PSN-start cutover date used to splice transactions between
	// web and PSN. Unix seconds UTC. Zero = auto-detect.
	PSNStartOverride int64
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
