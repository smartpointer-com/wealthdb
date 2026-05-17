package ubs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Status mirrors Schwab's adapter: -1 sentinels for "no rows",
// LatestChangeNumber = MAX(dump_runs.snapshot_at).
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
    COALESCE((SELECT MIN(timestamp)   FROM events),    -1),
    COALESCE((SELECT MAX(timestamp)   FROM events),    -1)
`
	var oldS, newS, oldT, newT sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT); err != nil {
		return s, fmt.Errorf("ubs Status: %w", err)
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
	s.LatestChangeNumber = s.LatestSnapshotAt
	return s, nil
}

// ChangeWindow follows the same shape as the Schwab adapter's:
// the change window starts at MIN of "newer snapshot_at" and
// "newer event timestamp", ends at MAX of the two.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs WHERE snapshot_at > ?
            UNION ALL
            SELECT MIN(timestamp)   AS t FROM events    WHERE timestamp   > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs WHERE snapshot_at > ?
            UNION ALL
            SELECT MAX(timestamp)   AS t FROM events    WHERE timestamp   > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?)
`
	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since, since, since).
		Scan(&start, &end, &newCN); err != nil {
		return w, fmt.Errorf("ubs ChangeWindow: %w", err)
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
