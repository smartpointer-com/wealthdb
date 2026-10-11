package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// SyncHoldingAliases makes holding_aliases hold exactly the config's
// aliases (config `holding_aliases`): the table is replaced whole, so an
// alias taken out of the config leaves on the next load. Returns how
// many were stamped.
func SyncHoldingAliases(ctx context.Context, db *sql.DB, aliases []canonical.HoldingAlias) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("SyncHoldingAliases: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM holding_aliases`); err != nil {
		return 0, fmt.Errorf("SyncHoldingAliases: clear: %w", err)
	}
	for _, a := range aliases {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key) VALUES (?, ?, ?, ?)`,
			a.SilverSourceID, a.AccountExternalID, a.FromKey, a.ToKey); err != nil {
			return 0, fmt.Errorf("SyncHoldingAliases: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("SyncHoldingAliases: commit: %w", err)
	}
	return len(aliases), nil
}
