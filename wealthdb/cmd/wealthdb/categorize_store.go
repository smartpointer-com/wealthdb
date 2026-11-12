package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// persistCategorizations upserts the verdicts into the family's global
// verdict store and returns the table's row count afterwards. The store is
// keyed by signature alone, so a re-run with a better model simply
// overwrites; signature_version stamps the normalisation that produced
// the key, which is what lets a later bump carry the verdict forward.
func persistCategorizations(ctx context.Context, db *sql.DB, fam categorizeFamily, rows []categorization, assignedAt int64, modelName string) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("categorize: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO `+fam.storeTable+`
    (`+fam.signatureColumn+`, `+fam.storeNameColumn+`, `+fam.valueColumn+`, signature_version, assigned_at, model_name)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (`+fam.signatureColumn+`) DO UPDATE SET
    `+fam.storeNameColumn+` = EXCLUDED.`+fam.storeNameColumn+`,
    `+fam.valueColumn+`     = EXCLUDED.`+fam.valueColumn+`,
    signature_version       = EXCLUDED.signature_version,
    assigned_at             = EXCLUDED.assigned_at,
    model_name              = EXCLUDED.model_name`)
	if err != nil {
		return 0, fmt.Errorf("categorize: prepare upsert: %w", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.Signature, r.MerchantName, r.Detailed,
			spending.SignatureVersion, assignedAt, modelName); err != nil {
			return 0, fmt.Errorf("categorize: upsert %q: %w", r.Signature, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("categorize: commit: %w", err)
	}
	committed = true

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+fam.storeTable).Scan(&total); err != nil {
		return 0, fmt.Errorf("categorize: count the %s store: %w", fam.counterparty, err)
	}
	return total, nil
}
