package fred

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// fx_rates.snapshot_at is the observation date (Unix s @ 00:00 UTC).
// dump_runs.snapshot_at is the bronze fetch-run timestamp; we use
// MAX(dump_runs.snapshot_at) as the logical change number, so each new
// `fred download` + `load` bumps it and a subsequent `wealthdb load`
// re-emits, while an idle reload is a no-op.

// Status reports the FX observation-date range and pins LatestChangeNumber
// to the latest loaded fetch run. fred records no transactions, so the
// transaction range is the "no observable state" sentinel.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	var oldS, newS sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM fx_rates`).
		Scan(&oldS, &newS); err != nil {
		return s, fmt.Errorf("fred Status snapshot: %w", err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}

	var latest sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latest); err != nil {
		return s, fmt.Errorf("fred Status load: %w", err)
	}
	if latest.Valid {
		s.LatestChangeNumber = latest.Int64
	}
	return s, nil
}

// ChangeWindow triggers on a new fetch run (any dump_runs row past `since`).
// When triggered it re-emits the FULL fx_rates range: gold's deleteWindow
// over [Start,End] makes the re-emit idempotent (rates upsert by date in
// silver, so the latest fetch's revisions win). NewChangeNumber advances to
// the latest fetch so an idle reload doesn't re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var latest sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latest); err != nil {
		return w, fmt.Errorf("fred ChangeWindow load: %w", err)
	}
	if !latest.Valid || latest.Int64 <= since {
		return w, nil // no new fetch since the watermark
	}

	var start, end sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM fx_rates`).
		Scan(&start, &end); err != nil {
		return w, fmt.Errorf("fred ChangeWindow span: %w", err)
	}
	w.NewChangeNumber = latest.Int64
	if start.Valid && end.Valid {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	return w, nil
}
