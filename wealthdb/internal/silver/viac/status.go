package viac

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Status reports the observable snapshot + transaction extrema and
// pins LatestChangeNumber to MAX(dump_runs.snapshot_at) so idle
// reloads remain no-ops.
//
// Oldest/Latest snapshot span the historical position + cash
// snapshots reconstructed from the Reporting PDFs (silver schema v3:
// positions/cash_balances rows tagged source='report:<docid>',
// keyed on each report's period-end date), not just the live dump
// timestamps — so `wealthdb status` reflects that viac holdings
// history reaches back to the contract's first year, well before
// live scraping began.
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
    COALESCE((SELECT MIN(t) FROM (
        SELECT MIN(snapshot_at) AS t FROM dump_runs
        UNION ALL SELECT MIN(snapshot_at) FROM positions
        UNION ALL SELECT MIN(snapshot_at) FROM cash_balances
    )), -1),
    COALESCE((SELECT MAX(t) FROM (
        SELECT MAX(snapshot_at) AS t FROM dump_runs
        UNION ALL SELECT MAX(snapshot_at) FROM positions
        UNION ALL SELECT MAX(snapshot_at) FROM cash_balances
    )), -1),
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

// ChangeWindow trigger: any new dump_run or transaction past `since`.
//
// When triggered, the window spans the ENTIRE snapshot + transaction
// history (global MIN..MAX across positions, cash_balances, dump_runs
// and transactions), not just the post-`since` delta. This is what
// lets historical backfill work: the Reporting PDFs introduce
// position/cash snapshots whose snapshot_at is a PAST period-end date
// (e.g. 2021-12-31), so a delta window of (since, now] would never
// cover them. A full-history window means gold's windowed
// delete-then-reinsert refreshes every viac snapshot on each load
// that has new work — correct, idempotent, and cheap at this data
// size. Idle reloads (no new dump/txn) still no-op via the watermark:
// NewChangeNumber stays MAX(dump_runs.snapshot_at), and the
// historical snapshots' past dates never advance it.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	const q = `
SELECT
    (SELECT COUNT(*) FROM dump_runs    WHERE snapshot_at > ?)
        + (SELECT COUNT(*) FROM transactions WHERE occurred_at > ?),
    COALESCE((SELECT MIN(t) FROM (
        SELECT MIN(snapshot_at) AS t FROM positions
        UNION ALL SELECT MIN(snapshot_at) FROM cash_balances
        UNION ALL SELECT MIN(snapshot_at) FROM dump_runs
        UNION ALL SELECT MIN(occurred_at) FROM transactions
    )), -1),
    COALESCE((SELECT MAX(t) FROM (
        SELECT MAX(snapshot_at) AS t FROM positions
        UNION ALL SELECT MAX(snapshot_at) FROM cash_balances
        UNION ALL SELECT MAX(snapshot_at) FROM dump_runs
        UNION ALL SELECT MAX(occurred_at) FROM transactions
    )), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?)
`
	var trigger sql.NullInt64
	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since).
		Scan(&trigger, &start, &end, &newCN); err != nil {
		return w, fmt.Errorf("viac ChangeWindow: %w", err)
	}
	if trigger.Valid && trigger.Int64 > 0 &&
		start.Valid && start.Int64 >= 0 && end.Valid && end.Int64 >= 0 {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
