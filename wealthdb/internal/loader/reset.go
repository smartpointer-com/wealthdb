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

	// gold.SourceTables is every per-source table, in delete order.
	// symbol_resolutions is per-source LLM-derived data that
	// references instrument_external_id / transaction.description
	// in the source; clear it alongside so the next load+resolve
	// cycle starts from a clean slate.
	//
	// The two enrichment overlays go the same way: both are derived
	// from the transactions being deleted and are recomputed by the
	// next enrichment pass, which writes them in one transaction. So
	// does the lot engine's ledger (gold.LotLedgerTables), which the
	// next lot pass rewrites. The reference records the source loaded
	// (gold.ReferenceTables) go with its other facts.
	//
	// Six tables deliberately survive a reset, two per family. The
	// verdict stores — spend_merchant_categories and
	// income_payer_categories — are global knowledge keyed by
	// signature rather than by source, since the same counterparty
	// reappears under every account and those verdicts were paid for.
	// The three account scopes and the cashflow wrapper boundary are
	// configuration stamped into gold (the fx_priority precedent), not
	// source data. The exposure and yield config tables survive too:
	// every load re-stamps them from the config (gold.SyncExposure,
	// gold.SyncYield), and the next exposure and yield passes rebuild the
	// tables they derive (instrument_terms and account_rates among them).
	// Their parsed records, built from the exposure_reports a reset
	// deletes, leave on that same re-stamp.
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
