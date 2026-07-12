package loader

import (
	"context"
	"fmt"
)

// Reset removes everything gold has for the given silver source.
// Implements docs/DESIGN.md §9: per-source delete in FK-safe
// order, inside a single transaction. The silver SQLite file is
// untouched.
//
// A follow-up Load for the same source starts fresh (watermark
// returns to -1).
func (l *Loader) Reset(ctx context.Context, sourceID string) error {
	tx, err := l.gold.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("Reset(%s): begin: %w", sourceID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// FK order: facts → dimensions → audit → registration.
	// symbol_resolutions is per-source LLM-derived data that
	// references instrument_external_id / transaction.description
	// in the source; clear it alongside so the next load+resolve
	// cycle starts from a clean slate.
	for _, stmt := range []string{
		`DELETE FROM symbol_resolutions WHERE silver_source_id = ?`,
		`DELETE FROM transactions   WHERE silver_source_id = ?`,
		`DELETE FROM fx_rates       WHERE silver_source_id = ?`,
		`DELETE FROM cash_balances  WHERE silver_source_id = ?`,
		`DELETE FROM positions      WHERE silver_source_id = ?`,
		`DELETE FROM instruments    WHERE silver_source_id = ?`,
		`DELETE FROM accounts       WHERE silver_source_id = ?`,
		`DELETE FROM portfolios     WHERE silver_source_id = ?`,
		`DELETE FROM load_audit     WHERE silver_source_id = ?`,
		`DELETE FROM silver_sources WHERE silver_source_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, sourceID); err != nil {
			return fmt.Errorf("Reset(%s): %w", sourceID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("Reset(%s): commit: %w", sourceID, err)
	}
	committed = true
	return nil
}

// ListSourceIDs returns every silver_source_id currently
// registered in gold. Used by `wealthdb reset -a` and
// `wealthdb load -a`.
func (l *Loader) ListSourceIDs(ctx context.Context) ([]string, error) {
	rows, err := l.gold.QueryContext(ctx,
		`SELECT silver_source_id FROM silver_sources ORDER BY silver_source_id`)
	if err != nil {
		return nil, err
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
