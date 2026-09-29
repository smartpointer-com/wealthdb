package synthetic

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions yields every transaction whose occurred_at falls in the
// window, in one batch ordered by (occurred_at, transaction_id).
//
// SIGNS. Silver stores amounts with gold's canonical sign already, from the
// account's own side (canonical/sign.go). They still go through
// ApplyCanonicalSign as a guard, so a kind with a fixed direction comes out
// signed that way whatever the row said: a buy is negative and a card
// payment positive. A kind whose sign depends on the event — interest,
// the FX family, the catch-alls — keeps the row's own.
//
// Everything else passes through. The memo stays a field of its own (the
// gold writer joins it to the description), and the payload is carried
// verbatim, so the keys gold reads out of it — `bank_ref`,
// `counter_account`, `counter_currency` / `counter_amount` — arrive as the
// writer put them. The one field that is conditional is the cheque number,
// which names an outgoing payment and so is kept only on an outflow.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT transaction_id, occurred_at, account_id, instrument_id, asset_class, vehicle,
       instrument_hint, kind, currency, gross_amount, net_amount, quantity, price,
       description, memo, counterparty, provider_category, check_number, payload
  FROM transactions
 WHERE occurred_at BETWEEN ? AND ?
 ORDER BY occurred_at, transaction_id`, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("synthetic Transactions: %w", err)
	}
	defer rows.Close()

	var out canonical.TransactionBatch
	for rows.Next() {
		var (
			id, account, rawKind, ccy, payload    string
			occurred                              int64
			instrument, assetClass, vehicle, hint sql.NullString
			gross, net, quantity, price           sql.NullString
			description, memo, counterparty       sql.NullString
			providerCategory, checkNumber         sql.NullString
		)
		if err := rows.Scan(&id, &occurred, &account, &instrument, &assetClass, &vehicle,
			&hint, &rawKind, &ccy, &gross, &net, &quantity, &price,
			&description, &memo, &counterparty, &providerCategory, &checkNumber,
			&payload); err != nil {
			return nil, err
		}
		var extra annotations
		kind := txKind(rawKind, &extra)
		ac, veh := tradedPair(assetClass.String, vehicle.String, &extra)
		netAmount := canonical.ApplyCanonicalSign(kind, silver.DecimalPtrOrNil(net))
		tx := canonical.TransactionChange{
			TransactionExternalID: id,
			OccurredAt:            occurred,
			AccountExternalID:     account,
			InstrumentExternalID:  silver.StrPtrIfNonEmpty(instrument.String),
			AssetClass:            ac,
			Vehicle:               veh,
			Kind:                  kind,
			Currency:              ccy,
			GrossAmount:           canonical.ApplyCanonicalSign(kind, silver.DecimalPtrOrNil(gross)),
			NetAmount:             netAmount,
			Quantity:              silver.DecimalPtrOrNil(quantity),
			Price:                 silver.DecimalPtrOrNil(price),
			Description:           silver.StrPtrIfNonEmpty(description.String),
			Memo:                  silver.StrPtrIfNonEmpty(memo.String),
			Counterparty:          silver.StrPtrIfNonEmpty(counterparty.String),
			ProviderCategory:      silver.StrPtrIfNonEmpty(providerCategory.String),
			CheckNumber:           checkNumberOnOutflow(checkNumber.String, netAmount),
			Payload:               payloadWith(payload, extra),
		}
		// The hint is the token an instrument lookup failed on, so it
		// means something only where there is no instrument.
		if tx.InstrumentExternalID == nil {
			tx.InstrumentHint = hint.String
		}
		out.Transactions = append(out.Transactions, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// checkNumberOnOutflow returns the cheque number only when the row is money
// leaving the account. Gold's contract (migration 0075) is that the field
// names an outgoing payment, so a number on an inflow or a zero-amount row is
// dropped rather than carried as if it were one.
func checkNumberOnOutflow(checkNo string, net *canonical.Decimal) *string {
	if net == nil || !net.IsNegative() {
		return nil
	}
	return silver.StrPtrIfNonEmpty(checkNo)
}
