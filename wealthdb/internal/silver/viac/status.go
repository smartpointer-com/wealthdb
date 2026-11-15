package viac

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// clock: a dump past the watermark re-emits the whole history
// (silver.FullSpan), every snapshot and transaction, so the historical
// snapshots of the Reporting PDFs, keyed on past period ends, reach gold
// too. An idle reload is a no-op: the change number is the latest dump.
// The source is observed through the latest run that read it
// (observed).
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind: "viac",
		Runs: silver.LatestDump,
		Snapshot: []string{
			silver.Range("dump_runs", "snapshot_at"),
			silver.Range("positions", "snapshot_at"),
			silver.Range("cash_balances", "snapshot_at"),
		},
		Ledger:   []string{silver.Range("transactions", "occurred_at")},
		Pin:      true,
		Mode:     silver.FullSpan{},
		Observed: silver.Observed{Set: c.observed},
	}
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}

// observed sets both watermarks to the latest run. A dry run reads
// nothing; a silver from before the column counts every run.
func (c *Connection) observed(ctx context.Context, s *canonical.Status) error {
	runs := `SELECT MAX(snapshot_at) FROM dump_runs`
	if c.schema.dryRun {
		runs += ` WHERE NOT COALESCE(dry_run, 0)`
	}
	t, err := silver.LatestRun(ctx, c.db, "viac", runs)
	s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough = t, t
	return err
}
