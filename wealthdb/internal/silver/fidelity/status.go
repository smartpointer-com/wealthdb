package fidelity

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// Status reports the observable snapshot/transaction range and
// the latest change number. Snapshot extrema span both dump_runs
// and the content tables (positions); LatestChangeNumber stays
// pinned to MAX(dump_runs.snapshot_at) so an idle reload is a
// no-op even when historical content is present.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	hasHist, err := c.hasHistoricalTable(ctx)
	if err != nil {
		return s, err
	}
	histClause := "SELECT NULL AS t WHERE 0"
	if hasHist {
		histClause = "SELECT as_of_date AS t FROM historical_position_snapshots"
	}
	q := fmt.Sprintf(`
SELECT
    COALESCE((SELECT MIN(t) FROM (
        SELECT MIN(snapshot_at) AS t FROM dump_runs
        UNION ALL SELECT MIN(snapshot_at) FROM positions
        UNION ALL %s
    )), -1),
    COALESCE((SELECT MAX(t) FROM (
        SELECT MAX(snapshot_at) AS t FROM dump_runs
        UNION ALL SELECT MAX(snapshot_at) FROM positions
        UNION ALL %s
    )), -1),
    COALESCE((SELECT MIN(timestamp) FROM transactions), -1),
    COALESCE((SELECT MAX(timestamp) FROM transactions), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs),  -1)
`, histClause, histClause)
	var oldS, newS, oldT, newT, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT, &latestRun); err != nil {
		return s, fmt.Errorf("fidelity Status: %w", err)
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

// ChangeWindow returns a window that brackets every silver row
// the next snapshot pass needs to see. Trigger is "is there
// anything new in dump_runs or transactions past since" (so an
// idle reload remains a no-op); bounds widen to cover any
// position snapshot whose snapshot_at — or any historical
// position snapshot whose as_of_date — falls outside the trigger
// range so the byTime dispatch in Snapshots() doesn't drop rows.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	hasHist, err := c.hasHistoricalTable(ctx)
	if err != nil {
		return w, err
	}
	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL SELECT MIN(timestamp) AS t FROM transactions WHERE timestamp > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL SELECT MAX(timestamp) AS t FROM transactions WHERE timestamp > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?),
    COALESCE((SELECT MIN(snapshot_at) FROM positions), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM positions), -1)
`
	var start, end, newCN sql.NullInt64
	var posLo, posHi sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since, since, since).
		Scan(&start, &end, &newCN, &posLo, &posHi); err != nil {
		return w, fmt.Errorf("fidelity ChangeWindow: %w", err)
	}
	if start.Valid && start.Int64 >= 0 && end.Valid && end.Int64 >= 0 {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	if w.HasChanges {
		if posLo.Valid && posLo.Int64 >= 0 && posLo.Int64 < w.Start {
			w.Start = posLo.Int64
		}
		if posHi.Valid && posHi.Int64 > w.End {
			w.End = posHi.Int64
		}
		if hasHist {
			var histLo, histHi sql.NullInt64
			if err := c.db.QueryRowContext(ctx, `
SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`).
				Scan(&histLo, &histHi); err != nil {
				return w, fmt.Errorf("fidelity ChangeWindow historical: %w", err)
			}
			if histLo.Valid && histLo.Int64 >= 0 && histLo.Int64 < w.Start {
				w.Start = histLo.Int64
			}
			if histHi.Valid && histHi.Int64 > w.End {
				w.End = histHi.Int64
			}
		}
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
