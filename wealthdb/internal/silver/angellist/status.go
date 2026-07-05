package angellist

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// contentExtrema yields the MIN/MAX content date — the position event
// dates (position_snapshots.as_of_date) unioned with the funding-ledger
// dates (funding_transactions.occurred_at) — plus MAX(dump_runs). The
// window must span both, since a funding cash flow can predate the first
// investment (an opening deposit) or postdate the last valuation.
const contentExtrema = `
SELECT
    (SELECT MIN(t) FROM (SELECT MIN(as_of_date) t FROM position_snapshots
                          UNION ALL SELECT MIN(occurred_at) FROM funding_transactions)),
    (SELECT MAX(t) FROM (SELECT MAX(as_of_date) t FROM position_snapshots
                          UNION ALL SELECT MAX(occurred_at) FROM funding_transactions)),
    (SELECT MAX(snapshot_at) FROM dump_runs)`

// Status reports the content date range and pins LatestChangeNumber to
// MAX(dump_runs.snapshot_at) — a new download is the only thing that
// advances the watermark, so idle reloads stay no-ops. Transaction extrema
// come from the funding-account cash ledger.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	var oldest, latest, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, contentExtrema).
		Scan(&oldest, &latest, &latestRun); err != nil {
		return s, fmt.Errorf("angellist Status: %w", err)
	}
	if oldest.Valid {
		s.OldestSnapshotAt = oldest.Int64
	}
	if latest.Valid {
		s.LatestSnapshotAt = latest.Int64
	}
	if latestRun.Valid {
		s.LatestChangeNumber = latestRun.Int64
	}

	// transaction extrema: the funding-account cash ledger.
	var txMin, txMax sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(occurred_at), MAX(occurred_at) FROM funding_transactions`).
		Scan(&txMin, &txMax); err != nil {
		return s, fmt.Errorf("angellist Status (tx): %w", err)
	}
	if txMin.Valid {
		s.OldestTransactionAt = txMin.Int64
	}
	if txMax.Valid {
		s.LatestTransactionAt = txMax.Int64
	}
	return s, nil
}

// ChangeWindow triggers on a new download — any dump_run past `since`.
// When triggered it re-emits the FULL event history: Start is the
// earliest position event date (an inception / early K-1 year), End the
// latest. Re-emit-all is the natural unit here because a new download can
// revise any position's marks at any past event date (a K-1 finalises, a
// statement restates), and the adapter forward-fills complete per-event
// portfolios; gold's deleteWindow over [Start,End] makes the re-emit
// idempotent. NewChangeNumber advances to the latest dump_run so an idle
// reload doesn't re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	var oldest, latest, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, contentExtrema).
		Scan(&oldest, &latest, &latestRun); err != nil {
		return w, fmt.Errorf("angellist ChangeWindow: %w", err)
	}
	if !latestRun.Valid || latestRun.Int64 <= since {
		return w, nil // no new download since the watermark
	}
	w.HasChanges = true
	w.NewChangeNumber = latestRun.Int64
	if oldest.Valid {
		w.Start = oldest.Int64
	}
	if latest.Valid {
		w.End = latest.Int64
	}
	return w, nil
}
