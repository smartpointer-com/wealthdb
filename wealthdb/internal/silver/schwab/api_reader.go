package schwab

import (
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// apiReader reads from the schwab-api silver SQLite. This file defines
// the type and its lifecycle; the reads are in the package's api files
// (snapshots.go, holdings.go, transactions.go, couponlink.go, status.go).
// Mirrors internal/silver/ubs/psn_reader.go.
type apiReader struct {
	db *sql.DB
	// marks tunes the guard on a spike that reverts (marks.go); a zero
	// field takes the default.
	marks MarkGuard
	// parts is the file's optional schema (schema).
	parts silver.Lazy[apiSchema]
	note  silver.SchemaNote
	file  string // the silver's base name, for the note
}

func (c *apiReader) Close() error { return silver.CloseDB(&c.db) }
