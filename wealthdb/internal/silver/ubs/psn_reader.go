package ubs

import (
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// psnReader reads from the ubs-psn silver SQLite. Methods are
// spread across snapshots.go / transactions.go / status.go — this
// file defines the type and its lifecycle.
type psnReader struct {
	db *sql.DB
	// parts is the file's optional schema (schema).
	parts silver.Lazy[psnSchema]
	note  silver.SchemaNote
	file  string // the silver's base name, for the note
	// source is the source's id in gold, which a contract's key is
	// scoped by.
	source string

	// events, accounts, coverage, mirrors and confirmations keep what
	// feed, roster, cashCoverage, conversionMirrors and
	// cashConfirmations read. Each reads the whole silver, unwindowed,
	// and a connection serves one load over a silver that does not
	// change under it, so each is read once per connection however many
	// passes ask for it (silver.Lazy).
	events        silver.Lazy[psnFeed]
	accounts      silver.Lazy[*psnRoster]
	coverage      silver.Lazy[psnCashCoverage]
	mirrors       silver.Lazy[*conversionMirrors]
	confirmations silver.Lazy[[]cashConfirmation]
}

func (r *psnReader) Close() error { return silver.CloseDB(&r.db) }
