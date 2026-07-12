package ubs

import "database/sql"

// psnReader reads from the ubs-psn silver SQLite. Methods are
// spread across snapshots.go / transactions.go / status.go — this
// file just defines the type and lifecycle.
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
