package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
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

		kind := kindFor(rawKind)
		netDec := silver.DecimalPtrOrNil(amtStr)
		tx := canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            occurredAt,
			AccountExternalID:     acct,
			Kind:                  kind,
			Currency:              currency,
			NetAmount:             canonical.ApplyCanonicalSign(kind, netDec),
			Quantity:              silver.DecimalPtrOrNil(qtyStr),
			Price:                 silver.DecimalPtrOrNil(priceStr),
			Payload:               json.RawMessage(payload),
		}
		if instr != "" {
			s := instr
			tx.InstrumentExternalID = &s
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}
