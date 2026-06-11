package viac

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Status reports the observable snapshot + transaction extrema
// and pins LatestChangeNumber to MAX(dump_runs.snapshot_at) so
// idle reloads remain no-ops.
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
    COALESCE((SELECT MIN(occurred_at) FROM transactions), -1),
    COALESCE((SELECT MAX(occurred_at) FROM transactions), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs),  -1)
`
	var oldS, newS, oldT, newT, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT, &latestRun); err != nil {
		return s, fmt.Errorf("viac Status: %w", err)
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

// ChangeWindow trigger: any new dump_run or transaction past
// `since`. NewChangeNumber stays a live-time concept
// (MAX(dump_runs.snapshot_at)) so an idle reload doesn't
// re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL SELECT MIN(occurred_at) AS t FROM transactions WHERE occurred_at > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL SELECT MAX(occurred_at) AS t FROM transactions WHERE occurred_at > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?)
`
	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since, since, since).
		Scan(&start, &end, &newCN); err != nil {
		return w, fmt.Errorf("viac ChangeWindow: %w", err)
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
