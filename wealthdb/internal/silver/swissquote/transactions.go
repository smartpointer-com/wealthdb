package swissquote

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

	// CAST(net_amount AS VARCHAR) preserves precision on the way
	// out — SQLite stores it as REAL (IEEE 754 double) but we
	// want exact decimal arithmetic in canonical types.
	const q = `
SELECT account_external_id, occurred_at, transaction_type,
       isin, symbol, currency, CAST(net_amount AS VARCHAR), payload
  FROM transactions
 WHERE occurred_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("swissquote Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			extID, txType, currency, payload string
			isin, symbol                     sql.NullString
			occurredAt                       int64
			netAmountStr                     string
		)
		if err := rows.Scan(&extID, &occurredAt, &txType, &isin, &symbol, &currency, &netAmountStr, &payload); err != nil {
			return nil, fmt.Errorf("swissquote Transactions scan: %w", err)
		}
		netDec, err := canonical.NewDecimalFromString(netAmountStr)
		if err != nil {
			return nil, fmt.Errorf("swissquote Transactions parse net_amount %q: %w", netAmountStr, err)
		}

		kind := kindFor(txType)
		tx := canonical.TransactionChange{
			TransactionExternalID: syntheticTxID(extID, occurredAt, txType,
				nullStringOrEmpty(symbol), currency, netDec),
			OccurredAt:        occurredAt,
			AccountExternalID: extID,
			Kind:              kind,
			Currency:          currency,
			NetAmount:         canonical.ApplyCanonicalSign(kind, &netDec),
			Description:       silver.StrPtrIfNonEmpty(narrative(txType, payload)),
			// The bank's own booking type, as UBS's rows carry theirs: a
			// row whose narrative is nothing more is filing-only, and a
			// rule can read it.
			ProviderCategory: silver.StrPtrIfNonEmpty(txType),
			Payload:          json.RawMessage(payload),
		}

		if isin.Valid && isin.String != "" {
			s := isin.String
			tx.InstrumentExternalID = &s
		} else if symbol.Valid && symbol.String != "" {
			// Fall back to symbol if ISIN is missing — Swissquote
			// often omits ISIN for cash-only rows; symbol is then
			// also empty, which means InstrumentExternalID stays
			// nil (cash event).
			s := symbol.String
			tx.InstrumentExternalID = &s
		}

		// Try to pull quantity / price out of payload for trade
		// rows. payload.quantity / payload.unit_price aren't
		// always present.
		if qty, price, ok := extractTradeFields(payload); ok {
			tx.Quantity = qty
			tx.Price = price
		}

		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}

// narrative is the row's narrative for gold: the booking type, which is
// the movement ("Custody Fees", "Dividend", "Payment"), then the
// security's name where the row names one — the fidelity adapter's
// action-then-security reading. The export carries nothing else a reader
// could use.
func narrative(txType, payload string) string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	return strings.TrimSpace(strings.TrimSpace(txType) + " " + strings.TrimSpace(p.Name))
}

// extractTradeFields pulls quantity and unit_price from the
// transaction payload if present. Returns ok=false if neither is
// usable. unit_price may carry a "%" suffix for bond rows (price
// as percent of face); we strip it.
func extractTradeFields(payload string) (*canonical.Decimal, *canonical.Decimal, bool) {
	var p struct {
		Quantity  *canonical.Decimal `json:"quantity"`
		UnitPrice json.RawMessage    `json:"unit_price"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, nil, false
	}

	var price *canonical.Decimal
	if len(p.UnitPrice) > 0 {
		// unit_price might be a number, a quoted string, or a
		// string ending in "%". Trim quotes and trailing "%".
		raw := string(p.UnitPrice)
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		}
		if len(raw) > 0 && raw[len(raw)-1] == '%' {
			raw = raw[:len(raw)-1]
		}
		if d, err := canonical.NewDecimalFromString(raw); err == nil {
			price = &d
		}
	}

	if p.Quantity == nil && price == nil {
		return nil, nil, false
	}
	return p.Quantity, price, true
}

func nullStringOrEmpty(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}
