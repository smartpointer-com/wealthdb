package cointracking

import (
	"context"
	"database/sql"
	"encoding/json"
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
// A trade's fee in one of its own currencies is inside its amounts; one
// in a third currency is not. The row's action says so, and the fee
// rides the payload (rowPayload) for the lot engine to value. Only the
// standalone "Other Fee" type produces a fee row.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	q, args := transactionsQuery(w)
	out := canonical.TransactionBatch{}
	if err := silver.ScanRows(ctx, c.db, "cointracking Transactions", q, func(rows *sql.Rows) error {
		var (
			txID, portfolioID, walletID, ctType       string
			buyCcy, sellCcy, feeCcy, comment, base    string
			occurredAt                                int64
			buyAmtStr, sellAmtStr, feeAmtStr, payload sql.NullString
			prices                                    tradePrices
		)
		if err := rows.Scan(&txID, &portfolioID, &walletID, &occurredAt,
			&ctType, &buyAmtStr, &buyCcy, &sellAmtStr, &sellCcy,
			&feeAmtStr, &feeCcy, &payload, &comment, &base,
			&prices.sellPortfolio, &prices.sellCoin, &prices.buyPortfolio, &prices.buyCoin); err != nil {
			return err
		}
		payload, err := rowPayload(payload, ctType, comment,
			buyCcy, sellCcy, feeAmtStr, feeCcy)
		if err != nil {
			return fmt.Errorf("tx %s payload: %w", txID, err)
		}
		if base == "" {
			base = "USD"
		}

		var emitted []canonical.TransactionChange
		if ctType == "Trade" {
			emitted, err = projectTrade(
				txID, walletID, occurredAt, base,
				buyAmtStr, buyCcy, sellAmtStr, sellCcy,
				thirdCurrencyFee(feeAmtStr, feeCcy, buyCcy, sellCcy), payload, prices)
		} else {
			emitted, err = projectNonTrade(
				txID, walletID, occurredAt, base, ctType, comment,
				buyAmtStr, buyCcy, sellAmtStr, sellCcy,
				payload)
		}
		if err != nil {
			return err
		}
		out.Transactions = append(out.Transactions, emitted...)
		return nil
	}, args...); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}

// transactionsQuery reads the rows in w with what a trade's value needs
// beside each: the portfolio's base currency, and the price of either
// side on the trade's day, from the portfolio's prices and from the coin
// prices (tradePrices).
//
// The base currency is the portfolio's quote currency in
// portfolio_prices, "" for a portfolio without prices (the caller
// defaults it to USD, as snapshots.go does). A price is the latest entry
// on or before the trade's day (ASOF), the day being the trade's instant
// as a date. Among coin prices one day states twice (two sources), the
// first stored stands.
func transactionsQuery(w canonical.Window) (string, []any) {
	// A full window reads every row: its limits are out of a TIMESTAMP's
	// range.
	where, args := ` WHERE occurred_at BETWEEN to_timestamp(?) AND to_timestamp(?)`, []any{w.Start, w.End}
	if w.Full {
		where, args = "", nil
	}
	return `
WITH base AS (
    SELECT portfolio_external_id, MIN(quote_currency) AS ccy
      FROM portfolio_prices GROUP BY 1),
coin AS (
    SELECT instrument_external_id, as_of_date, price_usd
      FROM coin_prices
   QUALIFY row_number() OVER (PARTITION BY instrument_external_id, as_of_date ORDER BY rowid) = 1),
tx AS (
    SELECT t.*, CAST(EXTRACT(epoch FROM t.occurred_at) AS BIGINT) AS occurred_at_secs,
           COALESCE(b.ccy, '') AS base_ccy
      FROM transactions t
      LEFT JOIN base b ON b.portfolio_external_id = t.portfolio_external_id` + where + `),
keyed AS (
    SELECT *, to_timestamp(occurred_at_secs)::DATE AS price_day,
           upper(COALESCE(NULLIF(base_ccy, ''), 'USD')) AS base_key,
           upper(COALESCE(sell_currency, '')) AS sell_key,
           upper(COALESCE(buy_currency, '')) AS buy_key
      FROM tx)
SELECT
    k.transaction_external_id,
    k.portfolio_external_id,
    k.wallet_external_id,
    k.occurred_at_secs,
    k.type,
    CAST(k.buy_amount  AS VARCHAR), COALESCE(k.buy_currency,  ''),
    CAST(k.sell_amount AS VARCHAR), COALESCE(k.sell_currency, ''),
    CAST(k.fee_amount  AS VARCHAR), COALESCE(k.fee_currency,  ''),
    CAST(k.payload     AS VARCHAR), COALESCE(k.comment, ''),
    k.base_ccy,
    CAST(ps.price AS VARCHAR), CAST(cs.price_usd AS VARCHAR),
    CAST(pb.price AS VARCHAR), CAST(cb.price_usd AS VARCHAR)
  FROM keyed k
  ASOF LEFT JOIN portfolio_prices ps
    ON ps.portfolio_external_id = k.portfolio_external_id
   AND ps.instrument_external_id = k.sell_key AND ps.quote_currency = k.base_key
   AND k.price_day >= ps.as_of_date
  ASOF LEFT JOIN coin cs
    ON cs.instrument_external_id = k.sell_key AND k.price_day >= cs.as_of_date
  ASOF LEFT JOIN portfolio_prices pb
    ON pb.portfolio_external_id = k.portfolio_external_id
   AND pb.instrument_external_id = k.buy_key AND pb.quote_currency = k.base_key
   AND k.price_day >= pb.as_of_date
  ASOF LEFT JOIN coin cb
    ON cb.instrument_external_id = k.buy_key AND k.price_day >= cb.as_of_date
 ORDER BY k.occurred_at, k.transaction_external_id`, args
}

// tradePrices are the prices transactionsQuery reads beside a row: for
// its sell and its buy side, the portfolio's price in its base currency
// and the coin's price in USD, each NULL where there is none.
type tradePrices struct {
	sellPortfolio, sellCoin, buyPortfolio, buyCoin sql.NullString
}

// projectTrade splits a Trade silver row into 1 row (one side is
// the base currency) or 2 rows (neither side is the base). A fee in a
// third currency (feeApart) goes on the row that buys, or on the one row
// of a trade with the base currency on one side.
func projectTrade(
	txID, walletID string, occurredAt int64, base string,
	buyAmtStr sql.NullString, buyCcy string,
	sellAmtStr sql.NullString, sellCcy string,
	feeApart bool, payload sql.NullString, prices tradePrices,
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
	var fee canonical.TxAction
	if feeApart {
		fee = canonical.TxActionThirdCurrencyFee
	}

	base = strings.ToUpper(base)
	buyCcy = strings.ToUpper(buyCcy)
	sellCcy = strings.ToUpper(sellCcy)

	switch {
	case sellCcy == base:
		// 1-leg buy: cash (base) → asset (other).
		return []canonical.TransactionChange{
			buildBaseLegTradeRow(txID, walletID, occurredAt, base,
				buyCcy, *buyAmt, *sellAmt, canonical.TxKindBuy, fee, pl)}, nil
	case buyCcy == base:
		// 1-leg sell: asset (other) → cash (base).
		return []canonical.TransactionChange{
			buildBaseLegTradeRow(txID, walletID, occurredAt, base,
				sellCcy, *sellAmt, *buyAmt, canonical.TxKindSell, fee, pl)}, nil
	default:
		// 2-leg split (crypto-to-crypto or fiat-to-fiat-without-base).
		v := tradeValue(prices, base, sellCcy, *sellAmt, buyCcy, *buyAmt)
		sellRow := buildSplitLeg(txID, sellLeg, walletID, occurredAt, base,
			sellCcy, *sellAmt, canonical.TxKindSell, v, "", pl)
		buyRow := buildSplitLeg(txID, buyLeg, walletID, occurredAt, base,
			buyCcy, *buyAmt, canonical.TxKindBuy, v, fee, pl)
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
	action canonical.TxAction, payload []byte,
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
		Action:                action,
		Payload:               payload,
	}
}

// sellLeg and buyLeg suffix the ids of a split trade's two legs
// ("<id>:s", "<id>:b").
const (
	sellLeg = "s"
	buyLeg  = "b"
)

// buildSplitLeg constructs one leg of a 2-leg split trade. Both
// legs share the same |NetAmount| (= V, the trade's base-currency
// value) so they cancel in any base-currency SUM. V is nil when
// neither side's price lookup resolved; both legs then emit
// NetAmount=NULL.
func buildSplitLeg(
	txID, suffix, walletID string, occurredAt int64, base, instrument string,
	qty canonical.Decimal, kind canonical.TxKind, v *canonical.Decimal,
	action canonical.TxAction, payload []byte,
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
		Action:                action,
		Payload:               payload,
	}
}

// tradeValue computes V, the base-currency value of a 2-leg trade.
// Sell-side price tried first; buy-side fallback. Returns nil when
// neither resolves — gold tolerates NULL NetAmount and the trade simply
// doesn't contribute to any base-currency rollup.
func tradeValue(prices tradePrices, base string,
	sellCcy string, sellAmt canonical.Decimal,
	buyCcy string, buyAmt canonical.Decimal,
) *canonical.Decimal {
	if p := unitPrice(prices.sellPortfolio, prices.sellCoin, sellCcy, base); p != nil {
		v := sellAmt.Mul(*p)
		return &v
	}
	if p := unitPrice(prices.buyPortfolio, prices.buyCoin, buyCcy, base); p != nil {
		v := buyAmt.Mul(*p)
		return &v
	}
	return nil
}

// unitPrice is the per-unit price of `instrument` in `base` currency as
// of the trade, from the prices transactionsQuery read. Hierarchy
// mirrors snapshots.go's appendPositionsAcrossSnapshots:
//
//  1. portfolio_prices for (portfolio, instrument, base) — the
//     latest entry on or before the trade date.
//  2. coin_prices for (instrument) — only when base = USD.
//  3. Trivial 1.0 when instrument == base.
//  4. nil otherwise.
func unitPrice(portfolio, coin sql.NullString, instrument, base string) *canonical.Decimal {
	instrument = strings.ToUpper(instrument)
	base = strings.ToUpper(base)
	if portfolio.Valid && portfolio.String != "" {
		if d, err := canonical.NewDecimalFromString(portfolio.String); err == nil {
			return &d
		}
	}
	if base == "USD" && coin.Valid && coin.String != "" {
		if d, err := canonical.NewDecimalFromString(coin.String); err == nil {
			return &d
		}
	}
	if instrument == base {
		d := canonical.NewDecimalFromInt(1)
		return &d
	}
	return nil
}

// projectNonTrade maps a non-Trade silver row to its canonical
// counterpart (at most one row). Its kind, direction (inbound vs
// outbound) and action are the CT type's by kindTable; Currency is the
// asset that moved and NetAmount the signed amount of it; Instrument
// and Quantity are set only when that asset is NOT the base
// (position event).
func projectNonTrade(
	txID, walletID string, occurredAt int64, base, ctType, comment string,
	buyAmtStr sql.NullString, buyCcy string,
	sellAmtStr sql.NullString, sellCcy string,
	payload sql.NullString,
) ([]canonical.TransactionChange, error) {
	// Which CT silver column carries the currency + amount?
	//
	//   - Inbound types (Deposit, Staking, Reward / Bonus, …):
	//     buy_currency / buy_amount.
	//   - Outbound types (Withdrawal, Lost, Spend, Other Fee, …):
	//     sell_currency / sell_amount. CT puts standalone fees on
	//     the sell-side. A fee in a row's fee_amount / fee_currency
	//     columns rides the payload (rowPayload), never a row of its
	//     own.
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

	rule, _ := silver.Classify(kindTable, silver.KindRow{
		Verb: ctType, Text: strings.TrimSpace(comment), Cash: isFiat(currency)})
	kind, dir := rule.Kind, directionOf(rule.Action)
	if kind == "" {
		// A type the table gives no kind, a new one among them: other,
		// in the direction of the side its amount is on.
		kind, dir = canonical.TxKindOther, dirOutbound
		if buyCcy != "" {
			dir = dirInbound
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
	if dir == dirOutbound {
		signed = amount.Neg()
	}
	netAmount := canonical.ApplyCanonicalSign(kind, &signed)

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
		Kind:                  kind,
		Currency:              currency,
		GrossAmount:           netAmount,
		NetAmount:             netAmount,
		Action:                rule.Action,
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

	// Other collapses several types into one kind; the type, kept as
	// the description, is what tells them apart.
	if kind == canonical.TxKindOther {
		desc := ctType
		out.Description = &desc
	}

	return []canonical.TransactionChange{out}, nil
}

// rowPayload is a row's payload: silver's own, with the row's
// CoinTracking fields added under their silver column names: the type
// and comment, which say what the row is, and the trade's currencies and
// fee. The lot engine reads the fee to value one in a third currency
// (docs/LOTS.md §6). Empty fields are left out.
func rowPayload(silverPayload sql.NullString, ctType, comment,
	buyCcy, sellCcy string, feeAmt sql.NullString, feeCcy string) (sql.NullString, error) {
	m := map[string]any{}
	if silverPayload.Valid && silverPayload.String != "" {
		if err := json.Unmarshal([]byte(silverPayload.String), &m); err != nil {
			return sql.NullString{}, err
		}
	}
	for k, v := range map[string]string{
		"type": ctType, "comment": comment,
		"buy_currency": buyCcy, "sell_currency": sellCcy, "fee_currency": feeCcy,
	} {
		if v != "" {
			m[k] = v
		}
	}
	if feeAmt.Valid && feeAmt.String != "" {
		if d, err := canonical.NewDecimalFromString(feeAmt.String); err == nil && !d.IsZero() {
			m["fee_amount"] = json.Number(d.String())
		}
	}
	if len(m) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
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
