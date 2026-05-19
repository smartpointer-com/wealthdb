package swissquote

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Status reports the observable snapshot/transaction range and
// the latest change number. Historical positions (silver
// migration 0004, source='pp:<doc_id>') extend OldestSnapshotAt
// back to the earliest reconstructed PDF date — they're stored
// in `positions` with snapshot_at = PDF as-of date, not in
// dump_runs. LatestChangeNumber stays a live-time concept: it
// tracks MAX(dump_runs.snapshot_at) so that an idle silver with
// no new dump produces an empty window even when historical
// positions are present.
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
        UNION ALL
        SELECT MIN(snapshot_at) AS t FROM positions
        UNION ALL
        SELECT MIN(snapshot_at) AS t FROM currency_balances
    )), -1),
    COALESCE((SELECT MAX(t) FROM (
        SELECT MAX(snapshot_at) AS t FROM dump_runs
        UNION ALL
        SELECT MAX(snapshot_at) AS t FROM positions
        UNION ALL
        SELECT MAX(snapshot_at) AS t FROM currency_balances
    )), -1),
    COALESCE((SELECT MIN(occurred_at) FROM transactions), -1),
    COALESCE((SELECT MAX(occurred_at) FROM transactions), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs),    -1)
`
	var oldS, newS, oldT, newT, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT, &latestRun); err != nil {
		return s, fmt.Errorf("swissquote Status: %w", err)
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

// ChangeWindow returns a window covering any new live content
// (dump_run or transaction past `since`), with Start extended
// back to MIN(positions.snapshot_at) so the loader's window-
// DELETE covers any existing historical gold rows before they're
// re-inserted. Identical pattern to the UBS web reader's
// ChangeWindow — see internal/silver/ubs/web_reader.go.
//
// NewChangeNumber stays a live-time concept (MAX over
// dump_runs.snapshot_at) so an idle reload is a no-op even with
// historical positions present.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL
            SELECT MIN(occurred_at) AS t FROM transactions WHERE occurred_at > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL
            SELECT MAX(occurred_at) AS t FROM transactions WHERE occurred_at > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?),
    COALESCE((SELECT MIN(snapshot_at) FROM positions),         -1),
    COALESCE((SELECT MAX(snapshot_at) FROM positions),         -1)
`
	var start, end, newCN sql.NullInt64
	var histLo, histHi sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since, since, since).
		Scan(&start, &end, &newCN, &histLo, &histHi); err != nil {
		return w, fmt.Errorf("swissquote ChangeWindow: %w", err)
	}
	if start.Valid && start.Int64 >= 0 && end.Valid && end.Int64 >= 0 {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	if w.HasChanges {
		if histLo.Valid && histLo.Int64 >= 0 && histLo.Int64 < w.Start {
			w.Start = histLo.Int64
		}
		if histHi.Valid && histHi.Int64 > w.End {
			w.End = histHi.Int64
		}
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
