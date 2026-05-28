package ubs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Status mirrors Schwab's adapter: -1 sentinels for "no rows".
// Snapshot extrema span both dump_runs and the content tables —
// PSN promotes snapshot_at on content rows to business-date
// midnight, which can be earlier than the dump's wall-clock run
// time. LatestChangeNumber = MAX(dump_runs.snapshot_at) so an
// idle reload stays a no-op.
func (c *psnReader) Status(ctx context.Context) (canonical.Status, error) {
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
        UNION ALL SELECT MIN(snapshot_at) FROM cash_accounts
        UNION ALL SELECT MIN(snapshot_at) FROM safekeeping_accounts
        UNION ALL SELECT MIN(snapshot_at) FROM portfolios
        UNION ALL SELECT MIN(snapshot_at) FROM holdings
        UNION ALL SELECT MIN(snapshot_at) FROM cash_balances
        UNION ALL SELECT MIN(snapshot_at) FROM instruments
        UNION ALL SELECT MIN(snapshot_at) FROM fx_rates
        UNION ALL SELECT MIN(snapshot_at) FROM forward_contracts
    )), -1),
    COALESCE((SELECT MAX(t) FROM (
        SELECT MAX(snapshot_at) AS t FROM dump_runs
        UNION ALL SELECT MAX(snapshot_at) FROM cash_accounts
        UNION ALL SELECT MAX(snapshot_at) FROM safekeeping_accounts
        UNION ALL SELECT MAX(snapshot_at) FROM portfolios
        UNION ALL SELECT MAX(snapshot_at) FROM holdings
        UNION ALL SELECT MAX(snapshot_at) FROM cash_balances
        UNION ALL SELECT MAX(snapshot_at) FROM instruments
        UNION ALL SELECT MAX(snapshot_at) FROM fx_rates
        UNION ALL SELECT MAX(snapshot_at) FROM forward_contracts
    )), -1),
    COALESCE((SELECT MIN(timestamp)   FROM events),    -1),
    COALESCE((SELECT MAX(timestamp)   FROM events),    -1),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), -1)
`
	var oldS, newS, oldT, newT, latestRun sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q).Scan(&oldS, &newS, &oldT, &newT, &latestRun); err != nil {
		return s, fmt.Errorf("ubs Status: %w", err)
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
// the next snapshot pass needs to see. Trigger is still "is there
// anything new in dump_runs or events past since" (so an idle
// reload remains a no-op), but the window bounds are computed
// across the content tables too — they carry business-date
// midnight snapshot_at values that can fall either side of the
// triggering dump_runs row, so a bounds query restricted to
// dump_runs alone would clip them out of the byTime dispatch.
//
// NewChangeNumber stays a live-time concept
// (MAX(dump_runs.snapshot_at)) so subsequent loads with no new
// dump don't re-trigger.
func (c *psnReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	const q = `
SELECT
    COALESCE(
        (SELECT MIN(t) FROM (
            SELECT MIN(snapshot_at) AS t FROM dump_runs WHERE snapshot_at > ?
            UNION ALL
            SELECT MIN(timestamp)   AS t FROM events    WHERE timestamp   > ?
        )),
        -1
    ),
    COALESCE(
        (SELECT MAX(t) FROM (
            SELECT MAX(snapshot_at) AS t FROM dump_runs WHERE snapshot_at > ?
            UNION ALL
            SELECT MAX(timestamp)   AS t FROM events    WHERE timestamp   > ?
        )),
        -1
    ),
    COALESCE((SELECT MAX(snapshot_at) FROM dump_runs), ?),
    COALESCE((SELECT MIN(t) FROM (
        SELECT MIN(snapshot_at) AS t FROM cash_accounts        WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM safekeeping_accounts WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM portfolios           WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM holdings             WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM cash_balances        WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM instruments          WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM fx_rates             WHERE snapshot_at > ?
        UNION ALL SELECT MIN(snapshot_at) FROM forward_contracts    WHERE snapshot_at > ?
    )), -1),
    COALESCE((SELECT MAX(t) FROM (
        SELECT MAX(snapshot_at) AS t FROM cash_accounts        WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM safekeeping_accounts WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM portfolios           WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM holdings             WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM cash_balances        WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM instruments          WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM fx_rates             WHERE snapshot_at > ?
        UNION ALL SELECT MAX(snapshot_at) FROM forward_contracts    WHERE snapshot_at > ?
    )), -1)
`
	args := []any{since, since, since, since, since}
	for i := 0; i < 16; i++ {
		args = append(args, since)
	}
	var start, end, newCN sql.NullInt64
	var contentLo, contentHi sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, args...).
		Scan(&start, &end, &newCN, &contentLo, &contentHi); err != nil {
		return w, fmt.Errorf("ubs ChangeWindow: %w", err)
	}
	if start.Valid && start.Int64 >= 0 && end.Valid && end.Int64 >= 0 {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	if w.HasChanges {
		if contentLo.Valid && contentLo.Int64 >= 0 && contentLo.Int64 < w.Start {
			w.Start = contentLo.Int64
		}
		if contentHi.Valid && contentHi.Int64 > w.End {
			w.End = contentHi.Int64
		}
	}
	if newCN.Valid {
		w.NewChangeNumber = newCN.Int64
	}
	return w, nil
}
