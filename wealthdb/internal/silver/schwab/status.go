package schwab

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// Status returns the silver-side snapshot/transaction extrema and
// the latest logical change number. Per docs/adapters/schwab.md §6,
// the change number is MAX(dump_runs.snapshot_at), or -1 if there
// are no dump_runs at all.
func (c *apiReader) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}

	// One query that returns all five extrema. COALESCE(...,-1)
	// gives us the canonical sentinel when a table is empty.
	const q = `
SELECT
    COALESCE((SELECT MIN(snapshot_at) FROM dump_runs),    -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs),    -1),
    COALESCE((SELECT MIN(timestamp)   FROM transactions), -1),
    COALESCE((SELECT MAX(timestamp)   FROM transactions), -1)
`
	var oldS, newS, oldT, newT sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT); err != nil {
		return s, fmt.Errorf("schwab Status: %w", err)
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
	// LatestChangeNumber == latest snapshot_at.
	s.LatestChangeNumber = s.LatestSnapshotAt
	return s, nil
}

// ChangeWindow returns the time window covering all changes in
// silver since the given change number (exclusive). See
// docs/DESIGN.md §6.2 and §8.1.
//
// Window.Start is the earliest snapshot_at or transaction
// timestamp strictly greater than `since`. Window.End is the
// latest of the two. Window.NewChangeNumber is the value to
// advance the gold watermark to upon successful load.
// HasChanges is false when nothing in silver is strictly newer
// than `since`.
func (c *apiReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL
            SELECT MIN(timestamp)   AS t FROM transactions WHERE timestamp   > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL
            SELECT MAX(timestamp)   AS t FROM transactions WHERE timestamp   > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?)
`
	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since, since, since).
		Scan(&start, &end, &newCN); err != nil {
		return w, fmt.Errorf("schwab ChangeWindow: %w", err)
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
