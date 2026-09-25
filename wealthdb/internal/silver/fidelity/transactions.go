package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
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
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("fidelity Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			activityID, acct, rawKind, instr, currency, payload string
			occurredAt                                          int64
			qtyStr, priceStr, amtStr                            sql.NullString
		)
		if err := rows.Scan(&activityID, &occurredAt, &acct, &rawKind, &instr,
			&currency, &qtyStr, &priceStr, &amtStr, &payload); err != nil {
			return nil, fmt.Errorf("fidelity Transactions scan: %w", err)
		}

		qty := silver.DecimalPtrOrNil(qtyStr)
		p := parseTxPayload(payload)
		// The amount is resolved BEFORE the kind, because one kind
		// depends on it: Fidelity's `WIRE` verb carries no direction,
		// and the sign is the only thing that does.
		netDec := silver.DecimalPtrOrNil(amtStr)
		kind := kindFor(rawKind, qty, netDec, p.Action)
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
			NetAmount:             netDec,
			Quantity:              qty,
			Price:                 silver.DecimalPtrOrNil(priceStr),
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
	}
	return silver.NewTransactionStream(out), rows.Err()
}
