package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

type txStream struct {
	batch    canonical.TransactionBatch
	consumed bool
}

func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return &txStream{consumed: true}, nil
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
		netDec := decimalPtrOrNil(amtStr)
		tx := canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            occurredAt,
			AccountExternalID:     acct,
			Kind:                  kind,
			Currency:              currency,
			NetAmount:             canonical.ApplyCanonicalSign(kind, netDec),
			Quantity:              decimalPtrOrNil(qtyStr),
			Price:                 decimalPtrOrNil(priceStr),
			Payload:               json.RawMessage(payload),
		}
		if instr != "" {
			s := instr
			tx.InstrumentExternalID = &s
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return &txStream{batch: out}, rows.Err()
}

func (s *txStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	if s.consumed {
		return canonical.TransactionBatch{}, false, nil
	}
	s.consumed = true
	return s.batch, false, nil
}

func (s *txStream) Close() error { return nil }
