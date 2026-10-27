package loader

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A dated wrapper override (config account_overrides[].tax_wrapper_from)
// gives an account its override's wrapper from a day on. Before that day
// the account keeps the wrapper it has without the override: the
// adapter's, as the source-wide and portfolio statements leave it. The
// load records that earlier wrapper and the day in
// account_wrapper_history (gold migration 0162); accounts.tax_wrapper
// holds the override's, the current one. The cash flow statement's
// boundary reads the wrapper as of each row's day.

// earlierWrappers collects, per account with a dated override, the
// wrapper the override replaces. A later batch's stated wrapper wins and
// an unstated one keeps what an earlier batch stated, as the dimension
// fold's per-column upsert does. Nil is a wrapper no batch states.
type earlierWrappers map[string]*canonical.TaxWrapper

// observe records the wrapper each dated-override account carries before
// applyAccountOverrides replaces it.
func (e earlierWrappers) observe(accounts []canonical.AccountChange, overrides map[string]AccountOverride) {
	for _, a := range accounts {
		ov, ok := overrides[a.AccountExternalID]
		if !ok || ov.TaxWrapper == "" || ov.TaxWrapperFrom == 0 {
			continue
		}
		if _, seen := e[a.AccountExternalID]; !seen || a.TaxWrapper != nil {
			e[a.AccountExternalID] = a.TaxWrapper
		}
	}
}

// write brings the source's account_wrapper_history in line with its
// dated overrides. An account this load saw gets its earlier wrapper and
// day; one it did not see keeps the wrapper an earlier load recorded,
// with the day the config now states; a row whose account no longer has
// a dated override goes.
func (e earlierWrappers) write(ctx context.Context, tx *sql.Tx, sourceID string, overrides map[string]AccountOverride) error {
	dated := map[string]int64{}
	for id, ov := range overrides {
		if ov.TaxWrapper != "" && ov.TaxWrapperFrom != 0 {
			dated[id] = ov.TaxWrapperFrom
		}
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT account_external_id FROM account_wrapper_history WHERE silver_source_id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("wrapper history: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, ok := dated[id]; !ok {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_wrapper_history
		    WHERE silver_source_id = ? AND account_external_id = ?`, sourceID, id); err != nil {
			return fmt.Errorf("wrapper history: %w", err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(dated)) {
		w, seen := e[id]
		if !seen {
			if _, err := tx.ExecContext(ctx, `UPDATE account_wrapper_history SET until_at = ?
			    WHERE silver_source_id = ? AND account_external_id = ?`, dated[id], sourceID, id); err != nil {
				return fmt.Errorf("wrapper history: %w", err)
			}
			continue
		}
		var wrapper any
		if w != nil {
			wrapper = string(*w)
		}
		// A load that states no wrapper keeps the one an earlier load
		// recorded.
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_wrapper_history
		    (silver_source_id, account_external_id, tax_wrapper, until_at) VALUES (?, ?, ?, ?)
		    ON CONFLICT (silver_source_id, account_external_id) DO UPDATE SET
		        tax_wrapper = COALESCE(excluded.tax_wrapper, account_wrapper_history.tax_wrapper),
		        until_at    = excluded.until_at`,
			sourceID, id, wrapper, dated[id]); err != nil {
			return fmt.Errorf("wrapper history: %w", err)
		}
	}
	return nil
}
