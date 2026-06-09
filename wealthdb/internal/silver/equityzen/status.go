package equityzen

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Silver dates are source ISO TEXT (calendar dates); strftime('%s', …)
// converts them to the canonical unix seconds the gold contract uses.

// snapshotExtrema yields the MIN/MAX position event date (positions.as_of_date)
// plus MAX(dump_runs). The positions timeline runs from each deal's original
// investment (well before the first download) to the latest event date.
const snapshotExtrema = `
SELECT
    (SELECT MIN(CAST(strftime('%s', as_of_date) AS INTEGER)) FROM positions WHERE as_of_date IS NOT NULL),
    (SELECT MAX(CAST(strftime('%s', as_of_date) AS INTEGER)) FROM positions WHERE as_of_date IS NOT NULL),
    (SELECT MAX(snapshot_at) FROM dump_runs)`

// txExtrema yields the MIN/MAX cash-flow date — the buyer's purchases and
// distributions (see transactions.go).
const txExtrema = `
SELECT MIN(CAST(strftime('%s', flow_date) AS INTEGER)),
       MAX(CAST(strftime('%s', flow_date) AS INTEGER))
  FROM cash_flows WHERE flow_date IS NOT NULL`

// windowExtrema spans both fact streams (positions + cash_flows) so the
// re-emit window covers every snapshot AND every transaction. Cash-flow dates
// are a subset of position event dates today, but the union keeps the window
// correct regardless.
const windowExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT CAST(strftime('%s', as_of_date) AS INTEGER) AS t FROM positions WHERE as_of_date IS NOT NULL
    UNION ALL
    SELECT CAST(strftime('%s', flow_date) AS INTEGER) FROM cash_flows WHERE flow_date IS NOT NULL)`

// Status reports the content snapshot + transaction ranges and pins
// LatestChangeNumber to MAX(dump_runs.snapshot_at) — a new download is the
// only thing that advances the watermark, so idle reloads stay no-ops.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	var oldest, latest, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, snapshotExtrema).
		Scan(&oldest, &latest, &latestRun); err != nil {
		return s, fmt.Errorf("equityzen Status: %w", err)
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

	var oldestTx, latestTx sql.NullInt64
	if err := c.db.QueryRowContext(ctx, txExtrema).Scan(&oldestTx, &latestTx); err != nil {
		return s, fmt.Errorf("equityzen Status (tx): %w", err)
	}
	if oldestTx.Valid {
		s.OldestTransactionAt = oldestTx.Int64
	}
	if latestTx.Valid {
		s.LatestTransactionAt = latestTx.Int64
	}
	return s, nil
}

// ChangeWindow triggers on a new download — any dump_run past `since`. When
// triggered it re-emits the FULL event history: Start/End span both fact
// streams. Re-emit-all is the natural unit because a new download can revise
// any deal's marks at any past event date (a tender records, a statement NAV
// finalises), and the adapter forward-fills complete per-event portfolios;
// gold's deleteWindow over [Start,End] makes the re-emit idempotent.
// NewChangeNumber advances to the latest dump_run so an idle reload doesn't
// re-trigger.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latestRun); err != nil {
		return w, fmt.Errorf("equityzen ChangeWindow dump: %w", err)
	}
	if !latestRun.Valid || latestRun.Int64 <= since {
		return w, nil // no new download since the watermark
	}

	var start, end sql.NullInt64
	if err := c.db.QueryRowContext(ctx, windowExtrema).Scan(&start, &end); err != nil {
		return w, fmt.Errorf("equityzen ChangeWindow span: %w", err)
	}
	w.NewChangeNumber = latestRun.Int64
	if start.Valid && end.Valid {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	return w, nil
}
