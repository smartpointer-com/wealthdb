package swissquote

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// clock: the snapshot range spans the dumps, the positions and the
// currency balances. The holdings reconstructed from statement PDFs
// (silver migration 0004, source 'pp:<doc_id>') sit in positions at their
// as-of date, before the first dump. A dump or a transaction past the
// watermark opens a window over the new rows, widened to every position
// snapshot, so the loader's windowed delete covers the historical gold
// rows before they are re-inserted. The change number is the latest dump,
// so an idle reload is a no-op even with historical positions present.
//
// A dump's export restates its span of the ledger, which can reach back
// past every position snapshot. Each row names the dump that wrote it
// (transactions.run_snapshot_at, silver v9), so the window also spans
// the rows a dump past the watermark wrote. A row written before v9
// names none and opens no window; it was in silver before every dump
// the stamp names.
func clock(runStamp bool) silver.Clock {
	newRows := []string{
		silver.RangePast("dump_runs", "snapshot_at"),
		silver.RangePast("transactions", "occurred_at"),
	}
	if runStamp {
		newRows = append(newRows, silver.RangeStampedPast("transactions", "occurred_at", "run_snapshot_at"))
	}
	return silver.Clock{
		Kind: "swissquote",
		Runs: silver.LatestDump,
		Snapshot: []string{
			silver.Range("dump_runs", "snapshot_at"),
			silver.Range("positions", "snapshot_at"),
			silver.Range("currency_balances", "snapshot_at"),
		},
		Ledger: []string{silver.Range("transactions", "occurred_at")},
		Pin:    true,
		Mode: silver.Incremental{
			New:   newRows,
			Widen: []string{silver.Range("positions", "snapshot_at")},
		},
	}
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock(c.schema.runStamp).Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock(c.schema.runStamp).ChangeWindow(ctx, c.db, since)
}
