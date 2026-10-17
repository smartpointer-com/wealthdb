package schwab

import (
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// apiReader reads from the schwab-api silver SQLite. Methods
// are spread across snapshots.go / transactions.go / status.go —
// this file just defines the type and lifecycle. Mirrors
// internal/silver/ubs/psn_reader.go.
type apiReader struct {
	db *sql.DB
}

func (c *apiReader) Close() error { return silver.CloseDB(&c.db) }
