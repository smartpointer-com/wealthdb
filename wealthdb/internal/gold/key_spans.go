package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// pinnedKeyMacros are the macros beginPinned reads once and shadows: the
// endings first, since the spans read them (migration 0145).
var pinnedKeyMacros = []string{"pos_key_ends", "cash_key_ends", "pos_key_spans", "cash_key_spans"}

// beginPinned begins a transaction of db in which every key's endings and
// spans are read once. They go into temporary tables of the transaction's
// connection, and temporary macros of the same names read them. A
// temporary macro binds before the schema's, also inside another macro
// and under a `main.` name, so every point-in-time reader the transaction
// runs picks from the pinned ones (TestPinnedKeySpansHoldForTheTransaction
// holds DuckDB to that). A materializer, which reads one gold at many
// instants, runs in one.
//
// The transaction writes neither positions nor cash_balances: the pinned
// endings would not follow. It ends by commit, which drops the pin before
// it commits, so no pooled connection keeps it, or by a rollback, which
// drops it too; never by tx.Commit.
func beginPinned(ctx context.Context, db *sql.DB) (tx *sql.Tx, commit func() error, err error) {
	if tx, err = db.BeginTx(ctx, nil); err != nil {
		return nil, nil, fmt.Errorf("begin: %w", err)
	}
	pin := func() error {
		if err := unpinKeys(ctx, tx); err != nil {
			return err
		}
		for _, m := range pinnedKeyMacros {
			for _, stmt := range []string{
				`CREATE TEMP TABLE pinned_` + m + ` AS SELECT * FROM ` + m + `()`,
				`CREATE TEMP MACRO ` + m + `() AS TABLE SELECT * FROM pinned_` + m,
			} {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("pin %s: %w", m, err)
				}
			}
		}
		return nil
	}
	if err := pin(); err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}
	return tx, func() error {
		if err := unpinKeys(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}, nil
}

// unpinKeys drops what beginPinned pins, where it exists.
func unpinKeys(ctx context.Context, tx *sql.Tx) error {
	for _, m := range pinnedKeyMacros {
		for _, stmt := range []string{`DROP MACRO TABLE IF EXISTS temp.` + m, `DROP TABLE IF EXISTS temp.pinned_` + m} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("unpin %s: %w", m, err)
			}
		}
	}
	return nil
}
