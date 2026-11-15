package synthetic

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// clock: the silver is append-only. A run adds the days after the
// previous run's as-of, and each company figure filed since then, whose
// as_of (its period's last day) can lie before that as-of. It records
// itself in dump_runs, numbered by change_number, and changes nothing
// already written. Its [window_start, window_end] bounds every
// snapshot_at, occurred_at and as_of it added, so its window reaches back
// to such a figure's day.
//
// The window spans the runs appended past the watermark, and only those:
// the union of their own windows, its change number the newest of them. A
// fresh gold (watermark -1) takes every run at once; a later load takes
// only what was appended since, and gold's windowed delete touches only
// those windows. An append run therefore leaves everything outside its
// window exactly as the previous load wrote it. Snapshots and
// Transactions read by time, not by run, so the window stays correct
// even where two runs' windows overlap: every row inside [Start, End] is
// re-emitted, whichever run added it.
//
// A run writes its window whole: the holdings through its end, and the
// ledger over [window_start, window_end]. Every range is the "no
// observable state" sentinel until something is there to observe.
var clock = silver.Clock{
	Kind: "synthetic",
	Runs: `SELECT MAX(change_number) FROM dump_runs`,
	Snapshot: []string{
		silver.Range("positions", "snapshot_at"),
		silver.Range("cash_balances", "snapshot_at"),
		silver.Range("fx_rates", "snapshot_at"),
	},
	Ledger: []string{silver.Range("transactions", "occurred_at")},
	Observed: silver.Observed{
		Runs:    `SELECT MAX(window_end) FROM dump_runs`,
		Windows: `SELECT window_start, window_end FROM dump_runs`,
	},
	Mode: silver.Incremental{New: []string{
		`SELECT MIN(window_start), MAX(window_end) FROM dump_runs WHERE change_number > ?`,
	}},
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return clock.Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return clock.ChangeWindow(ctx, c.db, since)
}
