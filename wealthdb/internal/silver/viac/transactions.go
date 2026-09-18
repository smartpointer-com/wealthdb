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
	//
	// The instrument and the instrument's own class come from the JOIN
	// rather than from the transaction row: silver resolved the id at
	// load (migration 0005) and the class lives on the instrument, so
	// the taxonomy pair is derived by the SAME taxonomyFor that
	// positions use rather than by a second mapping that could drift.
	const q = `
SELECT t.transaction_external_id, t.occurred_at, t.account_external_id,
       t.kind, t.currency,
       CAST(t.amount_chf AS VARCHAR),
       COALESCE(json_extract(t.payload, '$.description'), ''),
       t.payload,
       t.instrument_external_id,
       COALESCE(i.asset_class, ''), COALESCE(i.name, '')
  FROM transactions t
  LEFT JOIN instruments i
         ON i.instrument_external_id = t.instrument_external_id
 WHERE t.occurred_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("viac Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			txID, acct, rawKind, currency, desc, payload string
			instClass, instName                          string
			occurredAt                                   int64
			amtStr                                       sql.NullString
			instID                                       sql.NullString
		)
		if err := rows.Scan(&txID, &occurredAt, &acct, &rawKind, &currency,
			&amtStr, &desc, &payload, &instID, &instClass, &instName); err != nil {
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
		// What was traded, where silver could say. A row silver left
		// unresolved carries neither, which is what an untracked
		// destination is supposed to look like.
		if instID.Valid && instID.String != "" {
			id := instID.String
			tx.InstrumentExternalID = &id
			tx.AssetClass, tx.Vehicle = taxonomyFor(instClass, instName)
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}
