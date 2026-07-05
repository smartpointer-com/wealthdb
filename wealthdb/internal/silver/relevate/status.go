package relevate

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// Status reports the observable snapshot range. Relevate silver
// has no transactions surfaced yet (silver.transactions is
// schema-ready but empty in practice — Relevate doesn't expose
// a transaction-history endpoint, so the deposits_endpoint
// source produces zero rows today). The transaction-extrema
// fields stay at the -1 sentinel until that changes.
//
// OldestSnapshotAt / LatestSnapshotAt span both live (dump_runs)
// and historical (Quartalsbericht-derived) tables — that's the
// user-facing question "what dates does relevate cover in gold?".
// LatestChangeNumber stays bound to MAX(dump_runs.snapshot_at) —
// it's the "what new bronze has gold not seen yet" watermark, and
// historical rows are deterministic outputs of the loader's PDF
// parse step, not a separate change source.
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
    COALESCE((SELECT MIN(snapshot_at) FROM dump_runs), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), -1),
    COALESCE((SELECT MIN(occurred_at) FROM transactions), -1),
    COALESCE((SELECT MAX(occurred_at) FROM transactions), -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs),  -1)
`
	var oldS, newS, oldT, newT, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT, &latestRun); err != nil {
		return s, fmt.Errorf("relevate Status: %w", err)
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

	// Fold the historical tables into the snapshot range.
	histMin, histMax, err := c.historicalRange(ctx)
	if err != nil {
		return s, err
	}
	if histMin >= 0 {
		if s.OldestSnapshotAt == -1 || histMin < s.OldestSnapshotAt {
			s.OldestSnapshotAt = histMin
		}
	}
	if histMax >= 0 {
		if s.LatestSnapshotAt == -1 || histMax > s.LatestSnapshotAt {
			s.LatestSnapshotAt = histMax
		}
	}
	return s, nil
}

// ChangeWindow shape mirrors the swissquote / fidelity
// adapters: trigger is any new dump_run or transaction past
// `since`, NewChangeNumber is MAX(dump_runs.snapshot_at) so
// an idle reload is a no-op.
//
// When historical-PDF tables exist (silver migration 0002), the
// Start of any new window is extended backwards to MIN(historical
// snapshot_at). This ensures the loader's window-DELETE step
// covers the historical rows before the re-INSERT, so freshly-
// re-parsed historical content lands in gold idempotently.
func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL SELECT MIN(occurred_at) AS t FROM transactions WHERE occurred_at > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs    WHERE snapshot_at > ?
            UNION ALL SELECT MAX(occurred_at) AS t FROM transactions WHERE occurred_at > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?)
`
	var start, end, newCN sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, since, since, since, since, since).
		Scan(&start, &end, &newCN); err != nil {
		return w, fmt.Errorf("relevate ChangeWindow: %w", err)
	}
	if start.Valid && start.Int64 >= 0 && end.Valid && end.Int64 >= 0 {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true

		// Extend Start back to cover historical rows so a
		// rebuild from the same set of Quartalsbericht PDFs
		// replaces them in place rather than appending alongside
		// stale rows from the prior gold load.
		histMin, _, err := c.historicalRange(ctx)
		if err != nil {
			return w, err
		}
		if histMin >= 0 && histMin < w.Start {
			w.Start = histMin
		}
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
