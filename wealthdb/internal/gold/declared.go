package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// SyncDeclaredAccounts makes the accounts dimension hold exactly the
// declared accounts (config `declared_accounts`) under
// canonical.DeclaredSourceID: every row given is written, and every
// row of that source the config no longer names is dropped, so a
// removed declaration leaves on the next load rather than lingering.
//
// The rows are replaced rather than upserted. UpsertAccounts arbitrates
// conflicting observations by recency and keeps an older value where
// the newer is unset, which is right for two collectors describing one
// account and wrong for a declaration: config is the only writer, and
// a nickname taken out of it should go.
//
// A declared account carries no facts, so dropping one is dropping a
// dimension row and nothing else. Returns how many were stamped.
func SyncDeclaredAccounts(ctx context.Context, db *sql.DB, declared []canonical.AccountChange) (int, error) {
	for i := range declared {
		if declared[i].SilverSourceID != canonical.DeclaredSourceID {
			return 0, fmt.Errorf("SyncDeclaredAccounts: row %d is under source %q, want %q",
				i, declared[i].SilverSourceID, canonical.DeclaredSourceID)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("SyncDeclaredAccounts: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE silver_source_id = ?`,
		canonical.DeclaredSourceID); err != nil {
		return 0, fmt.Errorf("SyncDeclaredAccounts: clear: %w", err)
	}
	if err := NewWriter(tx).UpsertAccounts(ctx, declared); err != nil {
		return 0, fmt.Errorf("SyncDeclaredAccounts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("SyncDeclaredAccounts: commit: %w", err)
	}
	return len(declared), nil
}
