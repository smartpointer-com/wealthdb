package loader

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
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

	// gold.SourceTables is every per-source table, in delete order; its
	// classes say what rebuilds each. A reset keeps:
	//   - the verdict stores (spend_merchant_categories,
	//     income_payer_categories): keyed by signature, not by source,
	//     and paid for;
	//   - the config stamps (the three account scopes, the cashflow
	//     wrapper sides, the holding aliases, the exposure and yield
	//     config): every load stamps them again, and the records parsed
	//     from the exposure_reports a reset deletes leave on that stamp;
	//   - the tables the exposure and yield passes derive
	//     (instrument_terms and account_rates among them) and the
	//     lot-run log: the next pass rebuilds them;
	//   - the report_* and serving_* tables, materialized from the rest.
	for _, t := range gold.SourceTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t.Name+` WHERE silver_source_id = ?`, sourceID); err != nil {
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
// registered in gold. Used by `wealthdb reset -a`.
func (l *Loader) ListSourceIDs(ctx context.Context) ([]string, error) {
	return gold.ListSilverSources(ctx, l.gold)
}
