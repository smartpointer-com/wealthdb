package ubs

import (
	"context"
	"database/sql"
	"fmt"
)

// psnReader reads from the ubs-psn silver SQLite. Methods are
// spread across snapshots.go / transactions.go / status.go — this
// file defines the type, its lifecycle and the shared event scan.
type psnReader struct {
	db *sql.DB
}

func (r *psnReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// psnCashRow is one cash_movement event of the PSN silver.
type psnCashRow struct {
	eventID, account, payload string
	at                        int64
	currency                  sql.NullString
}

// eachCashMovement calls fn on each cash_movement event; a reader with
// no silver behind it has none.
func (r *psnReader) eachCashMovement(ctx context.Context, what string, fn func(psnCashRow) error) error {
	if r == nil || r.db == nil {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT event_external_id, timestamp, account_external_id, currency_iso, payload
  FROM events
 WHERE kind = 'cash_movement'`)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row psnCashRow
		if err := rows.Scan(&row.eventID, &row.at, &row.account, &row.currency, &row.payload); err != nil {
			return fmt.Errorf("%s scan: %w", what, err)
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}
