package viac

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
	// silver.transactions.kind is already canonical
	// (buy/sell/fee/interest/dividend/corporate_action/deposit);
	// txKindFor just enum-validates. amount_chf is signed at the
	// source (debits negative), so ApplyCanonicalSign is a no-op
	// for kinds with a fixed sign and pass-through for the
	// source-dependent ones (interest, corporate_action).
	//
	// CAST decimals to VARCHAR to dodge SQLite REAL → float64
	// precision loss before parsing through shopspring/decimal.
	// json_extract pulls the human-readable instrument name out
	// of the payload for Description; silver doesn't promote it
	// to a column.
	const q = `
SELECT transaction_external_id, occurred_at, account_external_id,
       kind, currency,
       CAST(amount_chf AS VARCHAR),
       COALESCE(json_extract(payload, '$.description'), ''),
       payload
  FROM transactions
 WHERE occurred_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("viac Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			txID, acct, rawKind, currency, desc, payload string
			occurredAt                                   int64
			amtStr                                       sql.NullString
		)
		if err := rows.Scan(&txID, &occurredAt, &acct, &rawKind, &currency,
			&amtStr, &desc, &payload); err != nil {
			return nil, fmt.Errorf("viac Transactions scan: %w", err)
		}
		kind := txKindFor(rawKind)
		netDec := silver.DecimalPtrOrNil(amtStr)
		tx := canonical.TransactionChange{
			TransactionExternalID: txID,
			OccurredAt:            occurredAt,
			AccountExternalID:     acct,
			Kind:                  kind,
			Currency:              currency,
			NetAmount:             canonical.ApplyCanonicalSign(kind, netDec),
			Payload:               json.RawMessage(payload),
		}
		if desc != "" {
			d := desc
			tx.Description = &d
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}
