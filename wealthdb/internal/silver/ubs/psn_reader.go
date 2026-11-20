package ubs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// psnReader reads from the ubs-psn silver SQLite. This file defines the
// type and its lifecycle; each pass lives in the file of its job.
type psnReader struct {
	db *sql.DB
	// parts is the file's optional schema (schema).
	parts silver.Lazy[psnSchema]
	note  silver.SchemaNote
	file  string // the silver's base name, for the note
	// source is the source's id in gold, which a contract's key is
	// scoped by.
	source string

	// events, accounts, coverage, mirrors, actions, confirmations and
	// held keep what feed, roster, cashCoverage, conversionMirrors,
	// corporateActions, cashConfirmations and holdingPairs read. Each
	// reads the whole silver, unwindowed, and a connection serves one
	// load over a silver that does not change under it, so each is read
	// once per connection however many passes ask for it (silver.Lazy).
	events        silver.Lazy[psnFeed]
	accounts      silver.Lazy[*psnRoster]
	coverage      silver.Lazy[psnCashCoverage]
	mirrors       silver.Lazy[*conversionMirrors]
	actions       silver.Lazy[corporateActions]
	confirmations silver.Lazy[[]cashConfirmation]
	held          silver.Lazy[[]heldPair]
}

func (r *psnReader) Close() error { return silver.CloseDB(&r.db) }

// heldPair is one ISIN a safekeeping account of the feed's holdings
// reports.
type heldPair struct{ isin, safekeeping string }

// holdingPairs is every distinct ISIN and safekeeping account the feed's
// holdings state, read once per connection. None for a nil reader.
func (r *psnReader) holdingPairs(ctx context.Context) ([]heldPair, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	return r.held.Get(func() ([]heldPair, error) {
		var out []heldPair
		if err := eachPair(ctx, r.db, `SELECT DISTINCT isin, safekeeping_external_id FROM holdings`,
			func(isin, safekeeping string) { out = append(out, heldPair{isin, safekeeping}) }); err != nil {
			return nil, fmt.Errorf("ubs psn holdings: %w", err)
		}
		return out, nil
	})
}
