package ubs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

type txStream struct {
	batch    canonical.TransactionBatch
	consumed bool
}

func (c *psnReader) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return &txStream{consumed: true}, nil
	}

	const q = `
SELECT event_external_id, timestamp, account_external_id, kind, currency_iso, payload
  FROM events
 WHERE timestamp BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			eventID, extID, kind, payload string
			currencyISO                   *string
			occurredAt                    int64
		)
		var ccyNull *string
		if err := rows.Scan(&eventID, &occurredAt, &extID, &kind, &ccyNull, &payload); err != nil {
			return nil, fmt.Errorf("ubs Transactions scan: %w", err)
		}
		currencyISO = ccyNull

		tx, err := buildTransaction(eventID, occurredAt, extID, kind, currencyISO, payload)
		if err != nil {
			return nil, fmt.Errorf("ubs Transactions (event_id=%s): %w", eventID, err)
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

// --- per-kind payload structs ---------------------------------------------

type tradeConfirmationPayload struct {
	Side                 string             `json:"side"`
	ISIN                 string             `json:"isin"`
	GrossAmount          *canonical.Decimal `json:"gross_amount"`
	NetAmount            *canonical.Decimal `json:"net_amount"`
	NetCurrency          string             `json:"net_currency"`
	Price                *canonical.Decimal `json:"price"`
	Quantity             *canonical.Decimal `json:"quantity"`
	CashAccountExternalID string            `json:"cash_account_external_id"`
}

type cashMovementPayload struct {
	Amount      *canonical.Decimal `json:"amount"`
	CreditDebit string             `json:"credit_debit"`
	Narrative   string             `json:"narrative"`
	Account     string             `json:"account"`
	Funds       string             `json:"funds"` // currency
}

type corporateActionPayload struct {
	ISIN        string `json:"isin"`
	Safekeeping string `json:"safekeeping"`
}

// buildTransaction routes a silver event into a TransactionChange.
// Each silverKind variant has its own payload shape; common fields
// (account, instrument, kind) fall out per branch.
func buildTransaction(eventID string, occurredAt int64, defaultAcct, silverKind string, defaultCcy *string, payload string) (canonical.TransactionChange, error) {
	tx := canonical.TransactionChange{
		TransactionExternalID: eventID,
		OccurredAt:            occurredAt,
		AccountExternalID:     defaultAcct,
		Payload:               json.RawMessage(payload),
	}
	if defaultCcy != nil {
		tx.Currency = *defaultCcy
	}

	switch silverKind {
	case "trade_confirmation":
		var p tradeConfirmationPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = canonical.TxKindBuy
		if p.Side == "S" || p.Side == "SELL" {
			tx.Kind = canonical.TxKindSell
		}
		if p.ISIN != "" {
			tx.InstrumentExternalID = &p.ISIN
		}
		if p.CashAccountExternalID != "" {
			tx.AccountExternalID = p.CashAccountExternalID
		}
		if p.NetCurrency != "" {
			tx.Currency = p.NetCurrency
		}
		tx.GrossAmount = p.GrossAmount
		tx.NetAmount = p.NetAmount
		tx.Quantity = p.Quantity
		tx.Price = p.Price

	case "cash_movement":
		var p cashMovementPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = kindFor(silverKind, p.Narrative, p.CreditDebit)
		if p.Account != "" {
			tx.AccountExternalID = p.Account
		}
		if p.Funds != "" {
			tx.Currency = p.Funds
		}
		tx.NetAmount = p.Amount

	case "corporate_action_confirmation",
		"corporate_action_notification",
		"corporate_action_narrative":
		var p corporateActionPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = canonical.TxKindCorporateAction
		if p.ISIN != "" {
			tx.InstrumentExternalID = &p.ISIN
		}
		if p.Safekeeping != "" {
			tx.AccountExternalID = p.Safekeeping
		}

	default:
		tx.Kind = kindFor(silverKind, "", "")
	}

	// Currency is required by the gold schema. If a kind didn't
	// populate it, fall back to "XXX" (ISO 4217 "no currency
	// involved") so the row still loads.
	if tx.Currency == "" {
		tx.Currency = "XXX"
	}

	// Normalise amount signs per the canonical convention. UBS
	// MT940 supplies positive amounts plus a credit/debit flag,
	// and the kind already encodes the direction; collapsing both
	// into a signed amount happens here.
	tx.GrossAmount = canonical.ApplyCanonicalSign(tx.Kind, tx.GrossAmount)
	tx.NetAmount = canonical.ApplyCanonicalSign(tx.Kind, tx.NetAmount)

	return tx, nil
}
