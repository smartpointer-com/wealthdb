package viac

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
	// instrument_external_id is silver's own column, resolved from the
	// description at load (migration 0005). No join to `instruments`:
	// what the instrument IS belongs to the instrument row, and the
	// gold adapter classifies that from the same silver in one place.
	const q = `
SELECT transaction_external_id, occurred_at, account_external_id,
       type, kind, currency,
       CAST(amount_chf AS VARCHAR),
       COALESCE(json_extract(payload, '$.description'), ''),
       payload,
       instrument_external_id
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
			txID, acct, rawType, rawKind, currency, desc, payload string
			occurredAt                                            int64
			amtStr, instID                                        sql.NullString
		)
		if err := rows.Scan(&txID, &occurredAt, &acct, &rawType, &rawKind, &currency,
			&amtStr, &desc, &payload, &instID); err != nil {
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
		// A fee, an interest credit or a contribution carries no
		// description, and its type is then the whole narrative. The
		// type is VIAC's own booking type, carried verbatim as the
		// provider category, so a row whose narrative is nothing more
		// is filing-only.
		narrative := desc
		if narrative == "" {
			narrative = humaniseType(rawType)
		}
		tx.Description = silver.StrPtrIfNonEmpty(narrative)
		tx.ProviderCategory = silver.StrPtrIfNonEmpty(rawType)
		// What was traded, where silver could say. A row silver left
		// unresolved carries nothing, which is what an untracked
		// destination is supposed to look like.
		if instID.Valid && instID.String != "" {
			id := instID.String
			tx.InstrumentExternalID = &id
		} else if desc != "" && (kind == canonical.TxKindBuy || kind == canonical.TxKindSell) {
			// The fund VIAC named, which this silver's instrument table
			// does not carry under that spelling — a fund renamed after
			// the trade, most often. Stated so a config link can close
			// it; only on a trade, since no other kind names a fund.
			tx.InstrumentHint = desc
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}

// humaniseType reads VIAC's type as words: `FEE_CHARGE` is "Fee charge".
func humaniseType(t string) string {
	words := strings.Fields(strings.ToLower(strings.ReplaceAll(t, "_", " ")))
	if len(words) == 0 {
		return ""
	}
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}
