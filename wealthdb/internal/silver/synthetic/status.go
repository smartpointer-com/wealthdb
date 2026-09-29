package synthetic

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// snapshotExtrema is the MIN/MAX snapshot_at over the three snapshot-grain
// tables.
const snapshotExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM positions
    UNION ALL SELECT snapshot_at FROM cash_balances
    UNION ALL SELECT snapshot_at FROM fx_rates)`

// Status reports the snapshot and transaction ranges and pins
// LatestChangeNumber to MAX(dump_runs.change_number). Every field is the
// "no observable state" sentinel until something is there to observe.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}

	var oldS, newS sql.NullInt64
	if err := c.db.QueryRowContext(ctx, snapshotExtrema).Scan(&oldS, &newS); err != nil {
		return s, fmt.Errorf("synthetic Status snapshot: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}

	var oldT, newT sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(occurred_at), MAX(occurred_at) FROM transactions`).Scan(&oldT, &newT); err != nil {
		return s, fmt.Errorf("synthetic Status transactions: %w", err)
	}
	if oldT.Valid {
		s.OldestTransactionAt = oldT.Int64
	}
	if newT.Valid {
		s.LatestTransactionAt = newT.Int64
	}

	var latest sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(change_number) FROM dump_runs`).Scan(&latest); err != nil {
		return s, fmt.Errorf("synthetic Status change number: %w", err)
	}
	if latest.Valid {
		s.LatestChangeNumber = latest.Int64
	}
	return s, nil
}

// ChangeWindow spans the runs appended since `since`, and only those.
//
// The silver is append-only: a run adds the days after the previous run's
// as-of, records itself in dump_runs, and rewrites nothing older. Its
// [window_start, window_end] bounds every snapshot_at and occurred_at it
// added. So the window is the union of the new runs' own windows, and
// NewChangeNumber is the newest of them. A fresh gold (watermark -1) takes
// every run at once; a later load takes only what was appended since, and
// gold's windowed delete touches only those days. An append run therefore
// leaves everything older exactly as the previous load wrote it.
//
// Snapshots and Transactions read by time, not by run, so the window stays
// correct even where two runs' windows overlap: every row inside
// [Start, End] is re-emitted, whichever run added it. With no run past the
// watermark there is nothing to apply.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var start, end, latest sql.NullInt64
	if err := c.db.QueryRowContext(ctx, `
SELECT MIN(window_start), MAX(window_end), MAX(change_number)
  FROM dump_runs
 WHERE change_number > ?`, since).Scan(&start, &end, &latest); err != nil {
		return w, fmt.Errorf("synthetic ChangeWindow: %w", err)
	}
	if !latest.Valid {
		return w, nil // no run appended since the watermark
	}
	w.Start = start.Int64
	w.End = end.Int64
	w.NewChangeNumber = latest.Int64
	w.HasChanges = true
	return w, nil
}
