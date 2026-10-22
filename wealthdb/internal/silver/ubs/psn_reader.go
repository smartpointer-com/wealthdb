package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// psnReader reads from the ubs-psn silver SQLite. Methods are
// spread across snapshots.go / transactions.go / status.go — this
// file defines the type, its lifecycle and the shared event scan.
type psnReader struct {
	db *sql.DB

	// coverage and mirrors keep what cashCoverage and conversionMirrors
	// read. Each scans every cash movement of the silver, unwindowed,
	// and a connection serves one load over a silver that does not
	// change under it, so each is read once per connection however many
	// passes ask for it.
	coverage psnCashCoverage
	mirrors  *conversionMirrors
}

func (r *psnReader) Close() error { return silver.CloseDB(&r.db) }

// psnCashRow is one cash_movement event of the PSN silver.
type psnCashRow struct {
	eventID, account, payload string
	at                        int64
	currency                  sql.NullString
}

// project is the row as the transaction stream emits it
// (buildTransaction), before any pass reclassifies it or books it at a
// gross, and its decoded payload. Every pass that keys on a feed line
// reads it here, so each reads the account, currency, kind and signed
// cash the stream emits. false for a payload that does not decode.
func (row psnCashRow) project() (canonical.TransactionChange, cashMovementPayload, bool) {
	var p cashMovementPayload
	if err := json.Unmarshal([]byte(row.payload), &p); err != nil {
		return canonical.TransactionChange{}, p, false
	}
	var ccy *string
	if row.currency.Valid {
		c := row.currency.String
		ccy = &c
	}
	tx, err := buildTransaction(row.eventID, row.at, row.account, "cash_movement", ccy, row.payload, nil)
	if err != nil || tx.NetAmount == nil {
		return canonical.TransactionChange{}, p, false
	}
	return tx, p, true
}

// dividendCredit is the row projected (project) where it is a dividend
// line that credits, marked `C`; false for any other line.
func (row psnCashRow) dividendCredit() (canonical.TransactionChange, cashMovementPayload, bool) {
	tx, p, ok := row.project()
	if !ok || tx.Kind != canonical.TxKindDividend || p.CreditDebit != "C" {
		return canonical.TransactionChange{}, cashMovementPayload{}, false
	}
	return tx, p, true
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
