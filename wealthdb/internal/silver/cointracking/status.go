package cointracking

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Status reports the observable snapshot + transaction extrema
// and pins LatestChangeNumber to MAX(dump_runs.snapshot_at) so
// idle reloads remain no-ops. Same shape as VIAC's status; the
// silver's `transactions.occurred_at` is a TIMESTAMP column, so
// EXTRACT(epoch FROM …) coerces it to the int64 the canonical
// Status expects.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	const q = `
SELECT
    COALESCE((SELECT MIN(snapshot_at) FROM dump_runs), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), -1),
    COALESCE((SELECT CAST(EXTRACT(epoch FROM MIN(occurred_at)) AS BIGINT) FROM transactions), -1),
    COALESCE((SELECT CAST(EXTRACT(epoch FROM MAX(occurred_at)) AS BIGINT) FROM transactions), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), -1)
`
	var oldS, newS, oldT, newT, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT, &latestRun); err != nil {
		return s, fmt.Errorf("cointracking Status: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}
	if oldT.Valid {
		s.OldestTransactionAt = oldT.Int64
	}
	if newT.Valid {
		s.LatestTransactionAt = newT.Int64
	}
	if latestRun.Valid {
		s.LatestChangeNumber = latestRun.Int64
	}
	return s, nil
}

// ChangeWindow triggers on any new dump_run past `since`. We pin
// the new change number to MAX(dump_runs.snapshot_at) — the silver
// loader stamps that on every fresh download → load cycle, and an
// idle reload that surfaces no new dump_run is a no-op.
//
// Unlike VIAC, we don't union transactions.occurred_at: every CT
// transaction is replayed from the full trade history on each
// silver load, so a new trade observation surfaces as a new
// dump_run regardless. Tracking occurred_at separately would
// re-trigger on every backfilled trade.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	const q = `
SELECT
    COALESCE((SELECT MIN(snapshot_at) FROM dump_runs WHERE snapshot_at > ?), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs WHERE snapshot_at > ?), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?)
`
	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since).
		Scan(&start, &end, &newCN); err != nil {
		return w, fmt.Errorf("cointracking ChangeWindow: %w", err)
	}
	if start.Valid && start.Int64 >= 0 && end.Valid && end.Int64 >= 0 {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
