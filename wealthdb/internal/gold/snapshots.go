package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// ListSnapshotTimes returns the distinct snapshot_at epochs gold
// has data for under the given silver source, sorted oldest first.
// Pulls from positions, cash_balances, and fx_rates — any of
// those tables having a row for a snapshot counts as that
// snapshot being "loaded". Returns an empty slice (no error)
// when the source has no data at all.
func ListSnapshotTimes(ctx context.Context, db *sql.DB, silverSourceID string) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM positions      WHERE silver_source_id = ?
    UNION ALL
    SELECT snapshot_at FROM cash_balances  WHERE silver_source_id = ?
    UNION ALL
    SELECT snapshot_at FROM fx_rates       WHERE silver_source_id = ?
)
ORDER BY snapshot_at`
	rows, err := db.QueryContext(ctx, q, silverSourceID, silverSourceID, silverSourceID)
	if err != nil {
		return nil, fmt.Errorf("ListSnapshotTimes(%s): %w", silverSourceID, err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("ListSnapshotTimes(%s) scan: %w", silverSourceID, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListSilverSources returns every silver_source_id currently
// registered in gold, sorted alphabetically. Convenience wrapper
// (loader.ListSourceIDs does the same, but having it on the gold
// side too keeps cmd subcommands from importing loader just for
// this).
func ListSilverSources(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT silver_source_id FROM silver_sources ORDER BY silver_source_id`)
	if err != nil {
		return nil, fmt.Errorf("ListSilverSources: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
