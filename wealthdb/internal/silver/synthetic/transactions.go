package synthetic

import (
	"context"
	"database/sql"

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
// writer put them. Two fields are conditional: the cheque number names an
// outgoing payment and so is kept only on an outflow, and the instrument
// hint is kept only when no instrument id is set.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	var out canonical.TransactionBatch
	if err := silver.ScanRows(ctx, c.db, "synthetic Transactions", `
SELECT transaction_id, occurred_at, account_id, instrument_id, asset_class, vehicle,
       instrument_hint, kind, currency, gross_amount, net_amount, quantity, price,
       description, memo, counterparty, provider_category, check_number, payload
  FROM transactions
 WHERE occurred_at BETWEEN ? AND ?
 ORDER BY occurred_at, transaction_id`, func(rows *sql.Rows) error {
		var (
			id, account, rawKind, ccy, payload    string
			occurred                              int64
			instrument, assetClass, vehicle, hint sql.NullString
			quantity, price                       *canonical.Decimal
			gross, net                            *canonical.Decimal
			description, memo, counterparty       sql.NullString
			providerCategory, checkNumber         sql.NullString
		)
		if err := rows.Scan(&id, &occurred, &account, &instrument, &assetClass, &vehicle,
			&hint, &rawKind, &ccy, silver.Dec(&gross, silver.DecText), silver.Dec(&net, silver.DecText), silver.Dec(&quantity, silver.DecText), silver.Dec(&price, silver.DecText),
			&description, &memo, &counterparty, &providerCategory, &checkNumber,
			&payload); err != nil {
			return err
		}
		var extra annotations
		kind := txKind(rawKind, &extra)
		ac, veh := tradedPair(assetClass.String, vehicle.String, &extra)
		netAmount := canonical.ApplyCanonicalSign(kind, net)
		tx := canonical.TransactionChange{
			TransactionExternalID: id,
			OccurredAt:            occurred,
			AccountExternalID:     account,
			InstrumentExternalID:  silver.StrPtrIfNonEmpty(instrument.String),
			AssetClass:            ac,
			Vehicle:               veh,
			Kind:                  kind,
			Currency:              ccy,
			GrossAmount:           canonical.ApplyCanonicalSign(kind, gross),
			NetAmount:             netAmount,
			Quantity:              quantity,
			Price:                 price,
			Description:           silver.StrPtrIfNonEmpty(description.String),
			Memo:                  silver.StrPtrIfNonEmpty(memo.String),
			Counterparty:          silver.StrPtrIfNonEmpty(counterparty.String),
			ProviderCategory:      silver.StrPtrIfNonEmpty(providerCategory.String),
			CheckNumber:           silver.CheckNumberOnOutflow(checkNumber.String, netAmount),
			Payload:               payloadWith(payload, extra),
		}
		// The hint is the token an instrument lookup failed on, so it
		// means something only where there is no instrument.
		if tx.InstrumentExternalID == nil {
			tx.InstrumentHint = hint.String
		}
		out.Transactions = append(out.Transactions, tx)
		return nil
	}, w.Start, w.End); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}
