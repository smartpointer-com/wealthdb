package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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
		// The amount is resolved BEFORE the kind, because one kind
		// depends on it: Fidelity's `WIRE` verb carries no direction,
		// and the sign is the only thing that does.
		netDec := silver.DecimalPtrOrNil(amtStr)
		kind := kindFor(rawKind, qty, netDec, payload)
		// The Action text is this source's narrative, and without it
		// gold has nothing to categorise a fidelity row by: no
		// merchant, no counterparty, no description. It is what
		// separates the two kinds of fee this source books — an ADR
		// pass-through ("FEE CHARGED <security>") from the account's
		// own management fee ("ADVISOR FEE DEDUCTED …") — and what a
		// rule matches a wire or a withholding on. Fidelity's own
		// Description column is the SECURITY name, which says nothing
		// about the movement, so it is the fallback rather than the
		// first choice.
		descr := payloadNarrative(payload)
		tx := canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            occurredAt,
			AccountExternalID:     acct,
			Kind:                  kind,
			Currency:              currency,
			NetAmount:             canonical.ApplyCanonicalSign(kind, netDec),
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
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}
