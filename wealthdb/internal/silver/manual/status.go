package manual

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Silver dates are source ISO TEXT (calendar dates); strftime('%s', …)
// converts them to the canonical unix seconds the gold contract uses.

// snapshotExtrema is the MIN/MAX position event date — the union of every
// position's acquired_at + closed_at and every valuation's as_of_date. The
// timeline runs from the earliest acquisition (which can be years before the
// latest load) to the latest valuation. The manual collector records no
// transactions, so the window spans only this snapshot stream.
const snapshotExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT CAST(strftime('%s', acquired_at) AS INTEGER) AS t FROM positions
    UNION ALL SELECT CAST(strftime('%s', closed_at)  AS INTEGER) FROM positions WHERE closed_at IS NOT NULL
    UNION ALL SELECT CAST(strftime('%s', as_of_date) AS INTEGER) FROM valuations)`

// Status reports the content snapshot range and pins LatestChangeNumber to
// MAX(load_runs.load_at). The manual collector rebuilds silver from the CSVs
// on every load, so each load bumps load_at and a subsequent `wealthdb load`
// re-emits; an idle reload (no new load) is a no-op. The transaction range is
// always the "no observable state" sentinel — manual projects no transactions.
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
		return s, fmt.Errorf("manual Status snapshot: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}

	var latestLoad sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(load_at) FROM load_runs`).Scan(&latestLoad); err != nil {
		return s, fmt.Errorf("manual Status load: %w", err)
	}
	if latestLoad.Valid {
		s.LatestChangeNumber = latestLoad.Int64
	}
	return s, nil
}

// ChangeWindow triggers on a new load — any load_run past `since`. When
// triggered it re-emits the FULL history: Start/End span the snapshot stream,
// so the per-event forward-filled snapshots all reach gold (gold's
// deleteWindow over [Start,End] makes the re-emit idempotent). NewChangeNumber
// advances to the latest load so an idle reload doesn't re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var latestLoad sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(load_at) FROM load_runs`).Scan(&latestLoad); err != nil {
		return w, fmt.Errorf("manual ChangeWindow load: %w", err)
	}
	if !latestLoad.Valid || latestLoad.Int64 <= since {
		return w, nil // no new load since the watermark
	}

	var start, end sql.NullInt64
	if err := c.db.QueryRowContext(ctx, snapshotExtrema).Scan(&start, &end); err != nil {
		return w, fmt.Errorf("manual ChangeWindow span: %w", err)
	}
	w.NewChangeNumber = latestLoad.Int64
	if start.Valid && end.Valid {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	return w, nil
}
