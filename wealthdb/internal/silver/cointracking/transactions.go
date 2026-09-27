package cointracking

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions projects every silver transactions row in the window into
// one or two canonical records (docs/adapters/cointracking.md §7):
//
//   - a Trade with the portfolio's base currency on one side is one buy or
//     sell of the non-base side, its net amount the base-currency cash flow;
//   - a Trade with no base-currency side is a sell and a buy whose ±V
//     base-currency net amounts cancel;
//   - every other CT type is one row by kindmap.go, in the currency of the
//     asset that moved, carrying instrument and quantity for a non-base
//     asset.
//
// The rows keep the closing-balance invariant against silver's replayed
// positions_daily, for any asset C:
//
//	balance(C) = SUM(Quantity  WHERE Instrument = C)
//	           + SUM(NetAmount WHERE Currency   = C)
//
// A trade's fee is already inside its amounts, so only the standalone
// "Other Fee" type produces a fee row.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	baseByPortfolio, err := c.portfolioBaseCurrencies(ctx)
	if err != nil {
		return nil, err
	}

	const q = `
SELECT
    transaction_external_id,
    portfolio_external_id,
    wallet_external_id,
    CAST(EXTRACT(epoch FROM occurred_at) AS BIGINT) AS occurred_at_secs,
    type,
    CAST(buy_amount  AS VARCHAR), COALESCE(buy_currency,  ''),
    CAST(sell_amount AS VARCHAR), COALESCE(sell_currency, ''),
    CAST(fee_amount  AS VARCHAR), COALESCE(fee_currency,  ''),
    CAST(payload     AS VARCHAR)
  FROM transactions
 WHERE occurred_at BETWEEN to_timestamp(?) AND to_timestamp(?)
 ORDER BY occurred_at, transaction_external_id`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("cointracking Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			txID, portfolioID, walletID, ctType       string
			buyCcy, sellCcy, feeCcy                   string
			occurredAt                                int64
			buyAmtStr, sellAmtStr, feeAmtStr, payload sql.NullString
		)
		if err := rows.Scan(&txID, &portfolioID, &walletID, &occurredAt,
			&ctType, &buyAmtStr, &buyCcy, &sellAmtStr, &sellCcy,
			&feeAmtStr, &feeCcy, &payload); err != nil {
			return nil, err
		}
		base := baseByPortfolio[portfolioID]
		if base == "" {
			base = "USD"
		}

		var emitted []canonical.TransactionChange
		if ctType == "Trade" {
			emitted, err = c.projectTrade(ctx,
				txID, walletID, occurredAt, base, portfolioID,
				buyAmtStr, buyCcy, sellAmtStr, sellCcy, payload)
		} else {
			emitted, err = projectNonTrade(
				txID, walletID, occurredAt, base, ctType,
				buyAmtStr, buyCcy, sellAmtStr, sellCcy,
				feeAmtStr, feeCcy, payload)
		}
		if err != nil {
			return nil, err
		}
		out.Transactions = append(out.Transactions, emitted...)
	}
	return silver.NewTransactionStream(out), rows.Err()
}

// portfolioBaseCurrencies maps every portfolio_external_id seen
// in portfolio_prices to its quote_currency. Portfolios that
// don't appear (no overview.csv ingested yet) default to USD at
// the caller — matching the snapshots.go default so the gold
// view is consistent across snapshots and transactions.
func (c *Connection) portfolioBaseCurrencies(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT portfolio_external_id, MIN(quote_currency)
  FROM portfolio_prices GROUP BY 1`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("portfolioBaseCurrencies: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var p, ccy string
		if err := rows.Scan(&p, &ccy); err != nil {
			return nil, err
		}
		out[p] = ccy
	}
	return out, rows.Err()
}

// projectTrade splits a Trade silver row into 1 row (one side is
// the base currency) or 2 rows (neither side is the base).
func (c *Connection) projectTrade(
	ctx context.Context,
	txID, walletID string, occurredAt int64, base, portfolioID string,
	buyAmtStr sql.NullString, buyCcy string,
	sellAmtStr sql.NullString, sellCcy string,
	payload sql.NullString,
) ([]canonical.TransactionChange, error) {
	buyAmt, err := decimalOrNil(buyAmtStr)
	if err != nil {
		return nil, fmt.Errorf("tx %s buy_amount: %w", txID, err)
	}
	sellAmt, err := decimalOrNil(sellAmtStr)
	if err != nil {
		return nil, fmt.Errorf("tx %s sell_amount: %w", txID, err)
	}
	if buyAmt == nil || sellAmt == nil || buyCcy == "" || sellCcy == "" {
		// Malformed Trade row (one side missing) — skip with no emit.
		return nil, nil
	}
	pl := silver.JSONOrNil(payload)

	base = strings.ToUpper(base)
	buyCcy = strings.ToUpper(buyCcy)
	sellCcy = strings.ToUpper(sellCcy)

	switch {
	case sellCcy == base:
		// 1-leg buy: cash (base) → asset (other).
		return []canonical.TransactionChange{
			buildBaseLegTradeRow(txID, walletID, occurredAt, base,
				buyCcy, *buyAmt, *sellAmt, canonical.TxKindBuy, pl)}, nil
	case buyCcy == base:
		// 1-leg sell: asset (other) → cash (base).
		return []canonical.TransactionChange{
			buildBaseLegTradeRow(txID, walletID, occurredAt, base,
				sellCcy, *sellAmt, *buyAmt, canonical.TxKindSell, pl)}, nil
	default:
		// 2-leg split (crypto-to-crypto or fiat-to-fiat-without-base).
		v, err := c.lookupTradeValue(ctx, portfolioID, base, occurredAt,
			sellCcy, *sellAmt, buyCcy, *buyAmt)
		if err != nil {
			return nil, err
		}
		sellRow := buildSplitLeg(txID, "s", walletID, occurredAt, base,
			sellCcy, *sellAmt, canonical.TxKindSell, v, pl)
		buyRow := buildSplitLeg(txID, "b", walletID, occurredAt, base,
			buyCcy, *buyAmt, canonical.TxKindBuy, v, pl)
		return []canonical.TransactionChange{sellRow, buyRow}, nil
	}
}

// buildBaseLegTradeRow constructs the single row for a 1-leg
// trade where one side is the portfolio's base currency.
// Instrument = the non-base ticker; Quantity = signed amount of
// it; NetAmount = signed base-currency cash flow.
func buildBaseLegTradeRow(
	txID, walletID string, occurredAt int64, base, instrument string,
	qty, cashAmt canonical.Decimal, kind canonical.TxKind,
	payload []byte,
) canonical.TransactionChange {
	quantity := qty
	if kind == canonical.TxKindSell {
		quantity = qty.Neg()
	}
	netAmount := canonical.ApplyCanonicalSign(kind, &cashAmt)
	instrumentKey := instrument
	return canonical.TransactionChange{
		TransactionExternalID: txID,
		OccurredAt:            occurredAt,
		AccountExternalID:     walletID,
		InstrumentExternalID:  &instrumentKey,
		Kind:                  kind,
		Currency:              base,
		Quantity:              &quantity,
		GrossAmount:           netAmount,
		NetAmount:             netAmount,
		Payload:               payload,
	}
}

// buildSplitLeg constructs one leg of a 2-leg split trade. Both
// legs share the same |NetAmount| (= V, the trade's base-currency
// value) so they cancel in any base-currency SUM. V is nil when
// neither side's price lookup resolved; both legs then emit
// NetAmount=NULL.
func buildSplitLeg(
	txID, suffix, walletID string, occurredAt int64, base, instrument string,
	qty canonical.Decimal, kind canonical.TxKind, v *canonical.Decimal,
	payload []byte,
) canonical.TransactionChange {
	quantity := qty
	if kind == canonical.TxKindSell {
		quantity = qty.Neg()
	}
	var netAmount *canonical.Decimal
	if v != nil {
		netAmount = canonical.ApplyCanonicalSign(kind, v)
	}
	instrumentKey := instrument
	return canonical.TransactionChange{
		TransactionExternalID: txID + ":" + suffix,
		OccurredAt:            occurredAt,
		AccountExternalID:     walletID,
		InstrumentExternalID:  &instrumentKey,
		Kind:                  kind,
		Currency:              base,
		Quantity:              &quantity,
		GrossAmount:           netAmount,
		NetAmount:             netAmount,
		Payload:               payload,
	}
}

// lookupTradeValue computes V, the base-currency value of a 2-leg
// trade. Sell-side price tried first; buy-side fallback. Returns
// nil when neither resolves — gold tolerates NULL NetAmount and
// the trade simply doesn't contribute to any base-currency rollup.
func (c *Connection) lookupTradeValue(
	ctx context.Context, portfolioID, base string, occurredAt int64,
	sellCcy string, sellAmt canonical.Decimal,
	buyCcy string, buyAmt canonical.Decimal,
) (*canonical.Decimal, error) {
	sellPrice, err := c.lookupUnitPrice(ctx, portfolioID, sellCcy, base, occurredAt)
	if err != nil {
		return nil, err
	}
	if sellPrice != nil {
		v := sellAmt.Mul(*sellPrice)
		return &v, nil
	}
	buyPrice, err := c.lookupUnitPrice(ctx, portfolioID, buyCcy, base, occurredAt)
	if err != nil {
		return nil, err
	}
	if buyPrice != nil {
		v := buyAmt.Mul(*buyPrice)
		return &v, nil
	}
	return nil, nil
}

// lookupUnitPrice returns the per-unit price of `instrument` in
// `base` currency as of `occurredAt`. Hierarchy mirrors
// snapshots.go's appendPositions:
//
//  1. portfolio_prices for (portfolio, instrument, base) — the
//     latest entry on or before the trade date.
//  2. coin_prices for (instrument) — only when base = USD.
//  3. Trivial 1.0 when instrument == base.
//  4. nil otherwise.
func (c *Connection) lookupUnitPrice(
	ctx context.Context, portfolioID, instrument, base string, occurredAt int64,
) (*canonical.Decimal, error) {
	instrument = strings.ToUpper(instrument)
	base = strings.ToUpper(base)

	const ppQ = `
SELECT CAST(price AS VARCHAR)
  FROM portfolio_prices
 WHERE portfolio_external_id  = ?
   AND instrument_external_id = ?
   AND quote_currency         = ?
   AND as_of_date            <= to_timestamp(?)::DATE
 ORDER BY as_of_date DESC LIMIT 1`
	var ppStr sql.NullString
	err := c.db.QueryRowContext(ctx, ppQ,
		portfolioID, instrument, base, occurredAt).Scan(&ppStr)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("lookupUnitPrice portfolio_prices: %w", err)
	}
	if ppStr.Valid && ppStr.String != "" {
		d, e := canonical.NewDecimalFromString(ppStr.String)
		if e == nil {
			return &d, nil
		}
	}

	if base == "USD" {
		const cpQ = `
SELECT CAST(price_usd AS VARCHAR)
  FROM coin_prices
 WHERE instrument_external_id = ?
   AND as_of_date            <= to_timestamp(?)::DATE
 ORDER BY as_of_date DESC LIMIT 1`
		var cpStr sql.NullString
		err := c.db.QueryRowContext(ctx, cpQ, instrument, occurredAt).Scan(&cpStr)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("lookupUnitPrice coin_prices: %w", err)
		}
		if cpStr.Valid && cpStr.String != "" {
			d, e := canonical.NewDecimalFromString(cpStr.String)
			if e == nil {
				return &d, nil
			}
		}
	}

	if instrument == base {
		d := canonical.NewDecimalFromInt(1)
		return &d, nil
	}
	return nil, nil
}

// projectNonTrade maps a non-Trade silver row to its canonical
// counterpart (at most one row). Direction (inbound vs outbound)
// is determined by the CT type via classifyCTType; Currency
// always = the portfolio's base for valuation context; Instrument
// is set only when the currency that moved is NOT the base
// (position event); NetAmount is set only when the currency
// equals the base (real cash flow).
func projectNonTrade(
	txID, walletID string, occurredAt int64, base string, ctType string,
	buyAmtStr sql.NullString, buyCcy string,
	sellAmtStr sql.NullString, sellCcy string,
	feeAmtStr sql.NullString, feeCcy string,
	payload sql.NullString,
) ([]canonical.TransactionChange, error) {
	// Which CT silver column carries the currency + amount?
	//
	//   - Inbound types (Deposit, Staking, Reward / Bonus, …):
	//     buy_currency / buy_amount.
	//   - Outbound types (Withdrawal, Lost, Spend, Other Fee, …):
	//     sell_currency / sell_amount. CT puts standalone fees on
	//     the sell-side; the dedicated fee_amount / fee_currency
	//     columns are reserved for fees ATTACHED to a Trade (which
	//     this adapter doesn't emit as separate rows per the spec
	//     — the fee is already internalised in the trade's amount).
	_ = feeAmtStr
	_ = feeCcy
	var (
		currency string
		amount   canonical.Decimal
		haveAmt  bool
	)
	if buyCcy != "" {
		currency = strings.ToUpper(buyCcy)
		amtPtr, e := decimalOrNil(buyAmtStr)
		if e != nil {
			return nil, fmt.Errorf("tx %s buy_amount: %w", txID, e)
		}
		if amtPtr != nil {
			amount = *amtPtr
			haveAmt = true
		}
	} else if sellCcy != "" {
		currency = strings.ToUpper(sellCcy)
		amtPtr, e := decimalOrNil(sellAmtStr)
		if e != nil {
			return nil, fmt.Errorf("tx %s sell_amount: %w", txID, e)
		}
		if amtPtr != nil {
			amount = *amtPtr
			haveAmt = true
		}
	} else {
		// Neither side populated — nothing to emit.
		return nil, nil
	}
	if !haveAmt {
		return nil, nil
	}

	base = strings.ToUpper(base)
	isBase := currency == base

	cls, known := classifyCTType(ctType, isFiat(currency))
	if !known {
		// New CT type — emit as Other with the raw value in the
		// description so a reader can recover it without payload
		// introspection.
		cls = classification{kind: canonical.TxKindOther, preserveRaw: true}
		if buyCcy != "" {
			cls.dir = dirInbound
		} else {
			cls.dir = dirOutbound
		}
	}

	// Signed amount (positive inbound, negative outbound) used for
	// both Quantity (non-base position events) and NetAmount (the
	// row's cash-flow in its Currency). ApplyCanonicalSign enforces
	// the kind's sign for fixed-sign kinds (deposit/withdrawal/
	// transfer_in/transfer_out/fee), agreeing with direction; for
	// source-dependent kinds (interest, staking, other) it passes
	// the value through unchanged, so the pre-signing per
	// direction wins.
	signed := amount
	if cls.dir == dirOutbound {
		signed = amount.Neg()
	}
	netAmount := canonical.ApplyCanonicalSign(cls.kind, &signed)

	// Currency is always the asset that moved — not the portfolio's
	// base currency. Combined with NetAmount=signed-amount-in-
	// Currency this lets the gold layer compute a USD valuation for
	// any non-Trade event by joining on the asset's price-on-day
	// (e.g. a staking row of 0.001 ETH has Currency=ETH and
	// NetAmount=+0.001; value_USD = NetAmount * ETH-USD-on-day).
	//
	// The closing-holdings invariant still holds: gold readers
	// excluding the Instrument==Currency overlap from the
	// NetAmount sum (the standard form) avoid double-counting the
	// non-base asset events, and the Trade rows (Currency=base,
	// Instrument=non-base) continue to contribute their cash-flow
	// to the base balance unaffected.
	out := canonical.TransactionChange{
		TransactionExternalID: txID,
		OccurredAt:            occurredAt,
		AccountExternalID:     walletID,
		Kind:                  cls.kind,
		Currency:              currency,
		GrossAmount:           netAmount,
		NetAmount:             netAmount,
		Payload:               silver.JSONOrNil(payload),
	}

	// Instrument / Quantity are tracked only for non-base assets;
	// base-currency cash events are cash-flow-only and stay
	// Instrument=NULL / Quantity=NULL so a position-side rollup
	// keyed on Instrument doesn't accidentally include them.
	if !isBase {
		instrumentKey := currency
		out.InstrumentExternalID = &instrumentKey
		out.Quantity = &signed
	}

	if cls.preserveRaw {
		desc := ctType
		out.Description = &desc
	}

	return []canonical.TransactionChange{out}, nil
}

// decimalOrNil parses a string-encoded decimal from silver.
// Returns (nil, nil) for NULL / empty input.
func decimalOrNil(s sql.NullString) (*canonical.Decimal, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	d, err := canonical.NewDecimalFromString(s.String)
	if err != nil {
		return nil, err
	}
	return &d, nil
}
