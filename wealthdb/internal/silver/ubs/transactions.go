package ubs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Transactions emits PSN events. offsetVeto carries the event ids whose cash
// movement pairs a web-side mirror in the same-day offset veto (see
// buildSameDayOffsetVeto); those are demoted to a non-flow kind here, exactly
// as the web loop demotes its half — a pair must drop on both sides or the
// survivor books a one-sided phantom external flow. Nil when the merged
// connection has no web subsource.
func (c *psnReader) Transactions(ctx context.Context, w canonical.Window, offsetVeto map[string]bool) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
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
		if err := rows.Scan(&eventID, &occurredAt, &extID, &kind, &currencyISO, &payload); err != nil {
			return nil, fmt.Errorf("ubs Transactions scan: %w", err)
		}

		tx, err := buildTransaction(eventID, occurredAt, extID, kind, currencyISO, payload)
		if err != nil {
			return nil, fmt.Errorf("ubs Transactions (event_id=%s): %w", eventID, err)
		}
		// Same-day offset veto, PSN half. The kind flips AFTER
		// ApplyCanonicalSign ran inside buildTransaction, so the signed
		// amount is untouched — only the flow classification changes.
		if offsetVeto[eventID] &&
			(tx.Kind == canonical.TxKindDeposit || tx.Kind == canonical.TxKindWithdrawal) {
			tx.Kind = canonical.TxKindOther
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return silver.NewTransactionStream(out), rows.Err()
}

// --- per-kind payload structs ---------------------------------------------

type tradeConfirmationPayload struct {
	Side                  string             `json:"side"`
	ISIN                  string             `json:"isin"`
	GrossAmount           *canonical.Decimal `json:"gross_amount"`
	NetAmount             *canonical.Decimal `json:"net_amount"`
	NetCurrency           string             `json:"net_currency"`
	Price                 *canonical.Decimal `json:"price"`
	Quantity              *canonical.Decimal `json:"quantity"`
	CashAccountExternalID string             `json:"cash_account_external_id"`
	SecurityName          string             `json:"security_name"`
}

type cashMovementPayload struct {
	Amount      *canonical.Decimal `json:"amount"`
	CreditDebit string             `json:"credit_debit"`
	Narrative   string             `json:"narrative"`
	Account     string             `json:"account"`
	Funds       string             `json:"funds"` // currency
	// TxnType is the MT940 :61: transaction type identification code
	// (NTRF, NMSC, NCHG, ...): the bank's own classification of the entry.
	TxnType string `json:"txn_type"`
	// BankRef is the :61: account-servicing-institution reference — the
	// bank's own number for the entry, which the account statement
	// prints as "Transaction no.". It is what lets the era text fold
	// (merge.go) find the export's record of the same booking. Absent
	// where the statement carried none, in which case the collector
	// synthesises the event id from the entry's own fields instead.
	BankRef string `json:"bank_ref"`
}

type corporateActionPayload struct {
	ISIN        string `json:"isin"`
	Safekeeping string `json:"safekeeping"`
	// CAEV is the ISO 15022 corporate-action event indicator (DVCA, ...).
	CAEV string `json:"caev"`
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
		tx.Kind = kindFor(silverKind, "", "") // Buy default; side flips below
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
		tx.Description = textPtr(p.SecurityName)

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
		// MT940 stores amount as a positive number with the
		// direction in `credit_debit` ("C" / "D"). Pre-sign here
		// so reversals from elsewhere — sources that DO supply a
		// signed amount with a deliberate negative — aren't
		// silently re-flipped by ApplyCanonicalSign. After this
		// branch the helper sees an already-signed amount and
		// only acts when sign and kind agree.
		amt := p.Amount
		if amt != nil && p.CreditDebit == "D" && !amt.IsNegative() {
			n := amt.Neg()
			amt = &n
		}
		tx.NetAmount = amt
		// Text columns (docs/adapters/ubs.md §7): the :86: narrative,
		// flattened to one line, is the description — a bare code
		// passes through as that code — and the :61: type code is the
		// provider category. MT940 carries no structured payee, so no
		// counterparty is derived from the free text. The kind above
		// was classified from the raw narrative and is unaffected.
		tx.Description = narrativeText(p.Narrative)
		tx.ProviderCategory = silver.StrPtrIfNonEmpty(p.TxnType)

	case "corporate_action_confirmation",
		"corporate_action_notification",
		"corporate_action_narrative":
		var p corporateActionPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = kindFor(silverKind, "", "")
		if p.ISIN != "" {
			tx.InstrumentExternalID = &p.ISIN
		}
		if p.Safekeeping != "" {
			tx.AccountExternalID = p.Safekeeping
		}
		tx.Description = textPtr(p.CAEV)

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
