package firstcitizens

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at) and the transaction ledger (posted_at) —
// the latter carries both the per-day cash marks and the transaction events.
// It bounds the snapshot range in Status and the re-emit window in ChangeWindow
// (gold's deleteWindow over [Start,End] must cover every record it re-applies).
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions)`

// Status reports the content ranges and pins LatestChangeNumber to
// MAX(dump_runs.snapshot_at) — the load clock. Each bronze dump loaded bumps
// it and a subsequent `wealthdb load` re-emits; an idle reload is a no-op.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}

	var oldS, newS sql.NullInt64
	if err := c.db.QueryRowContext(ctx, spanExtrema).Scan(&oldS, &newS); err != nil {
		return s, fmt.Errorf("firstcitizens Status snapshot: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}

	var oldT, newT sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(posted_at), MAX(posted_at) FROM transactions`).Scan(&oldT, &newT); err != nil {
		return s, fmt.Errorf("firstcitizens Status transactions: %w", err)
	}
	if oldT.Valid {
		s.OldestTransactionAt = oldT.Int64
	}
	if newT.Valid {
		s.LatestTransactionAt = newT.Int64
	}

	var latestLoad sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latestLoad); err != nil {
		return s, fmt.Errorf("firstcitizens Status load: %w", err)
	}
	if latestLoad.Valid {
		s.LatestChangeNumber = latestLoad.Int64
	}
	return s, nil
}

// ChangeWindow triggers on a new load — any dump_run past `since`. When
// triggered it re-emits the FULL history: Start/End span every snapshot and
// transaction, so gold's deleteWindow over [Start,End] makes the re-emit
// idempotent. NewChangeNumber advances to the latest load so an idle reload
// doesn't re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var latestLoad sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latestLoad); err != nil {
		return w, fmt.Errorf("firstcitizens ChangeWindow load: %w", err)
	}
	if !latestLoad.Valid || latestLoad.Int64 <= since {
		return w, nil // no new load since the watermark
	}

	var start, end sql.NullInt64
	if err := c.db.QueryRowContext(ctx, spanExtrema).Scan(&start, &end); err != nil {
		return w, fmt.Errorf("firstcitizens ChangeWindow span: %w", err)
	}
	w.NewChangeNumber = latestLoad.Int64
	if start.Valid && end.Valid {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	return w, nil
}
