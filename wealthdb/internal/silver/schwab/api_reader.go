package schwab

import "database/sql"

// apiReader reads from the schwab-api silver SQLite. Methods
// are spread across snapshots.go / transactions.go / status.go —
// this file just defines the type and lifecycle. Mirrors
// internal/silver/ubs/psn_reader.go.
type apiReader struct {
	db *sql.DB
}

func (r *apiReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}
