package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	// CAST decimals to VARCHAR so SQLite's REAL → float64 round-
	// trip doesn't bleed precision before parse.
	const q = `
SELECT activity_id, timestamp, account_external_id, kind,
       COALESCE(instrument_key, ''),
       currency,
       CAST(quantity AS VARCHAR),
       CAST(price    AS VARCHAR),
       CAST(amount   AS VARCHAR),
       payload
  FROM transactions
 WHERE timestamp BETWEEN ? AND ?`

	out := canonical.TransactionBatch{}
	var windUp []windUpInLeg
	if err := silver.ScanRows(ctx, c.db, "fidelity Transactions", q, func(rows *sql.Rows) error {
		var (
			activityID, acct, rawKind, instr, currency, payload string
			occurredAt                                          int64
			qty, price, amount                                  *canonical.Decimal
		)
		if err := rows.Scan(&activityID, &occurredAt, &acct, &rawKind, &instr,
			&currency, silver.Dec(&qty, silver.DecText), silver.Dec(&price, silver.DecText), silver.Dec(&amount, silver.DecText), &payload); err != nil {
			return fmt.Errorf("fidelity Transactions scan: %w", err)
		}

		p := parseTxPayload(payload)
		// The amount is resolved BEFORE the kind, because some kinds
		// depend on it: Fidelity's `WIRE` and `TRANSFER` verbs carry no
		// direction, and their signs are the only thing that does.
		kind := kindFor(rawKind, qty, amount, p.Action, acct)
		// The narrative is all gold has to categorise a fidelity row
		// by: no merchant, no counterparty, no description. It is what
		// separates the two kinds of fee this source books — an ADR
		// pass-through ("FEE CHARGED <security>") from the account's
		// own management fee ("ADVISOR FEE DEDUCTED …") — and what a
		// rule matches a wire or a withholding on.
		descr := p.narrative()
		// The source sign is kept on every kind, never forced to the
		// kind's canonical one. Fidelity signs each amount from the
		// account's side, so a row that disagrees with its kind is a
		// correction: a cancelled sale booked against the sale, a
		// dividend clawed back, a fee or a withholding refunded.
		// Forcing the canonical sign would turn each into a second
		// booking of what it undoes (canonical/sign.go).
		tx := canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            occurredAt,
			AccountExternalID:     acct,
			Kind:                  kind,
			Currency:              currency,
			NetAmount:             amount,
			Quantity:              qty,
			Price:                 price,
			Payload:               json.RawMessage(payload),
		}
		if descr != "" {
			d := descr
			tx.Description = &d
		}
		if instr != "" {
			s := instr
			tx.InstrumentExternalID = &s
		} else {
			// Stated, never derived: only a row whose builder tried and
			// failed to settle its instrument carries one, and a
			// `transaction_instruments` entry closes it by that token.
			tx.InstrumentHint = strings.TrimSpace(p.InstrumentHint)
		}
		out.Transactions = append(out.Transactions, tx)
		if l, ok := asWindUpInLeg(tx, p.Action); ok {
			windUp = append(windUp, l)
		}
		return nil
	}, w.Start, w.End); err != nil {
		return nil, err
	}
	// A closed account's wind-up out-legs are printed nowhere (windup.go).
	legs, err := c.windUpCounterLegs(ctx, windUp)
	if err != nil {
		return nil, err
	}
	out.Transactions = append(out.Transactions, legs...)
	// Each row states what it does to the lots where its kind does not.
	for i := range out.Transactions {
		tx := &out.Transactions[i]
		var d string
		if tx.Description != nil {
			d = *tx.Description
		}
		tx.Action = lotAction(tx.Kind, d)
	}
	return silver.NewTransactionStream(out), nil
}
