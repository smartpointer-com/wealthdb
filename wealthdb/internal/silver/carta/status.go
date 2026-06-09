package carta

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// contentSpan is the MIN/MAX snapshot_at across the position-bearing content
// tables. carta reconstructs one download into event-dated deltas (an
// exercise in 2024, an acquisition in 2026, a quarterly NAV), which sit years
// before the download. The observable snapshot range — and the load window —
// must track THIS span, not dump_runs (the download time), so the historical
// deltas fall in-window and reach gold's as-of query.
const contentSpan = `
SELECT MIN(snapshot_at), MAX(snapshot_at) FROM (
    SELECT snapshot_at FROM entities
    UNION ALL SELECT snapshot_at FROM securities
    UNION ALL SELECT snapshot_at FROM fund_metrics
)`

// Status reports the observable snapshot range. carta is snapshot-only — no
// transactions — so the transaction-extrema fields stay at the -1 sentinel.
// The range spans the event-dated content; LatestChangeNumber is the newest
// dump (the live-time signal, one row per ingested bronze run, so an idle
// reload is a no-op).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	var oldS, newS, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, contentSpan).Scan(&oldS, &newS); err != nil {
		return s, fmt.Errorf("carta Status span: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latestRun); err != nil {
		return s, fmt.Errorf("carta Status dump: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}
	if latestRun.Valid {
		s.LatestChangeNumber = latestRun.Int64
	}
	return s, nil
}

// ChangeWindow triggers on any dump_run past `since` (a new download), but the
// window spans the full event-dated content range so every reconstructed
// delta is re-emitted and reaches gold. NewChangeNumber is
// MAX(dump_runs.snapshot_at) so an idle reload (no new dump) is a no-op.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var hasChanges bool
	if err := c.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM dump_runs WHERE snapshot_at > ?)`, since).
		Scan(&hasChanges); err != nil {
		return w, fmt.Errorf("carta ChangeWindow trigger: %w", err)
	}

	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, contentSpan).Scan(&start, &end); err != nil {
		return w, fmt.Errorf("carta ChangeWindow span: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&newCN); err != nil {
		return w, fmt.Errorf("carta ChangeWindow dump: %w", err)
	}

	if hasChanges && start.Valid && end.Valid {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
