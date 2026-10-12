package relevate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions reads silver.transactions and projects them as
// canonical TransactionChange events.
//
// The populating source is `credit_note_pdf` — the
// Gutschriftsanzeige PDFs the loader parses for Pillar-2
// contribution events. The `deposits_endpoint` source is plumbed
// but yields no rows, since Relevate's web app exposes no
// transaction history; if it ever does, the query picks the rows
// up too.
//
// Silver `kind` mapping → canonical TxKind:
//   - 'contribution' → TxKindDeposit. Pillar-2 vocabulary calls
//     in-payments to a vested-benefits account "Beiträge" /
//     "contributions", but the canonical TxKindContribution slot
//     is documented for private-market capital calls (cash OUT
//     of the funding account). A vested-benefits credit-note is
//     cash IN to the account — the closest generic kind is
//     TxKindDeposit. Same shape Schwab / UBS use for external
//     cash arriving at the account.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	// A security is keyed by its ISIN where it has one (instrumentKey).
	const q = `
SELECT t.transaction_external_id, t.occurred_at, t.account_external_id,
       COALESCE(NULLIF(i.isin, ''), t.instrument_external_id), t.kind, t.currency,
       CAST(t.net_amount AS VARCHAR),
       t.payload
  FROM transactions t
  LEFT JOIN instruments i ON i.instrument_external_id = t.instrument_external_id
 WHERE t.occurred_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("relevate Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			txID, acctID, silverKind, ccy, payload string
			instID                                 sql.NullString
			occurredAt                             int64
			netAmountStr                           sql.NullString
		)
		if err := rows.Scan(&txID, &occurredAt, &acctID,
			&instID, &silverKind, &ccy, &netAmountStr, &payload); err != nil {
			return nil, fmt.Errorf("relevate Transactions scan: %w", err)
		}

		kind := canonicalKindFor(silverKind)
		tx := canonical.TransactionChange{
			TransactionExternalID: txID,
			OccurredAt:            occurredAt,
			AccountExternalID:     acctID,
			Kind:                  kind,
			Currency:              ccy,
			Payload:               json.RawMessage(payload),
		}
		if netAmountStr.Valid && netAmountStr.String != "" {
			d, err := canonical.NewDecimalFromString(netAmountStr.String)
			if err != nil {
				return nil, fmt.Errorf("relevate Transactions parse net_amount %q: %w", netAmountStr.String, err)
			}
			tx.NetAmount = canonical.ApplyCanonicalSign(kind, &d)
		}
		if instID.Valid && instID.String != "" {
			s := instID.String
			tx.InstrumentExternalID = &s
		}
		out.Transactions = append(out.Transactions, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// canonicalKindFor maps the silver `transactions.kind` vocabulary
// (Pillar-2 wording the collector emits) to the canonical TxKind.
// Unknown values fall through to TxKindOther.
func canonicalKindFor(silverKind string) canonical.TxKind {
	switch silverKind {
	case "contribution":
		// See Transactions doc-comment for the rationale on
		// Deposit-not-Contribution.
		return canonical.TxKindDeposit
	case "withdrawal":
		return canonical.TxKindWithdrawal
	case "fee":
		return canonical.TxKindFee
	case "rebalance":
		return canonical.TxKindOther
	default:
		return canonical.TxKindOther
	}
}
