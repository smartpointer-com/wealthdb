package cointracking

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

// Snapshots emits one batch per ChangeWindow, stamped at the
// window's End (= latest dump_run snapshot_at in the window). The
// silver replay reconstructs every prior balance from the full
// trade history on each load, so emitting historical snapshots
// would mostly duplicate what gold already has from the previous
// load. Gold sees one snapshot per silver-load-cycle, with the
// latest balances computed live from `transactions`.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}
	snap := w.End
	batch := canonical.SnapshotBatch{}

	if err := c.appendPortfolios(ctx, &batch, snap); err != nil {
		return nil, err
	}
	if err := c.appendAccounts(ctx, &batch, snap); err != nil {
		return nil, err
	}
	if err := c.appendInstruments(ctx, &batch, snap); err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, &batch, snap); err != nil {
		return nil, err
	}

	return &snapshotStream{batches: []canonical.SnapshotBatch{batch}}, nil
}

func (s *snapshotStream) Next(context.Context) (canonical.SnapshotBatch, bool, error) {
	if s.idx >= len(s.batches) {
		return canonical.SnapshotBatch{}, false, nil
	}
	b := s.batches[s.idx]
	s.idx++
	return b, s.idx < len(s.batches), nil
}

func (s *snapshotStream) Close() error { return nil }

// appendPortfolios emits one PortfolioChange per row in
// silver.portfolios. The base_currency is the portfolio's
// quote_currency in portfolio_prices (each CT portfolio carries
// its own "main fiat" setting; the value is NULL if no
// overview.csv has been ingested yet for that portfolio).
// FirstSeenAt is sourced from the earliest trade for that
// portfolio so gold's running MIN reflects when the portfolio
// was actually first active rather than when gold first observed
// it.
func (c *Connection) appendPortfolios(ctx context.Context, batch *canonical.SnapshotBatch, snap int64) error {
	const q = `
SELECT
    p.portfolio_external_id,
    COALESCE(p.display_name, ''),
    COALESCE(
        (SELECT MIN(quote_currency) FROM portfolio_prices pp
          WHERE pp.portfolio_external_id = p.portfolio_external_id),
        ''
    ) AS base_currency,
    COALESCE(
        CAST((SELECT EXTRACT(epoch FROM MIN(occurred_at))::BIGINT
                FROM transactions t
               WHERE t.portfolio_external_id = p.portfolio_external_id) AS BIGINT),
        ?
    ) AS first_seen,
    CAST(p.payload AS VARCHAR)
  FROM portfolios p
 ORDER BY p.portfolio_external_id`
	rows, err := c.db.QueryContext(ctx, q, snap)
	if err != nil {
		return fmt.Errorf("appendPortfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			extID, name, baseCcy string
			firstSeen            int64
			payload              sql.NullString
		)
		if err := rows.Scan(&extID, &name, &baseCcy, &firstSeen, &payload); err != nil {
			return err
		}
		change := canonical.PortfolioChange{
			PortfolioExternalID: extID,
			FirstSeenAt:         firstSeen,
			LastSeenAt:          snap,
			Payload:             jsonOrNull(payload),
		}
		if name != "" {
			n := name
			change.DisplayName = &n
		}
		if baseCcy != "" {
			b := baseCcy
			change.BaseCurrency = &b
		}
		batch.Portfolios = append(batch.Portfolios, change)
	}
	return rows.Err()
}

// appendAccounts emits one AccountChange per silver.wallets row.
// account_external_id matches silver's composite `<cu>:<wallet>`.
// tax_wrapper defaults to taxable_personal — wealthdb.cfg's
// portfolio_overrides block applies a per-portfolio override at
// load time (e.g. roth_ira for an IRA-held portfolio).
// management_style is always self_directed; account_kind is always
// crypto. base_currency mirrors the parent portfolio's quote.
func (c *Connection) appendAccounts(ctx context.Context, batch *canonical.SnapshotBatch, snap int64) error {
	const q = `
SELECT
    w.portfolio_external_id,
    w.wallet_external_id,
    COALESCE(w.display_name, ''),
    COALESCE(
        (SELECT MIN(quote_currency) FROM portfolio_prices pp
          WHERE pp.portfolio_external_id = w.portfolio_external_id),
        ''
    ) AS base_currency,
    COALESCE(
        CAST((SELECT EXTRACT(epoch FROM MIN(occurred_at))::BIGINT
                FROM transactions t
               WHERE t.wallet_external_id = w.wallet_external_id) AS BIGINT),
        ?
    ) AS first_seen,
    CAST(w.payload AS VARCHAR)
  FROM wallets w
 ORDER BY w.portfolio_external_id, w.wallet_external_id`
	rows, err := c.db.QueryContext(ctx, q, snap)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	style := canonical.ManagementStyleSelfDirected
	for rows.Next() {
		var (
			portfolioID, walletID, name, baseCcy string
			firstSeen                            int64
			payload                              sql.NullString
		)
		if err := rows.Scan(&portfolioID, &walletID, &name, &baseCcy, &firstSeen, &payload); err != nil {
			return err
		}
		// Adapter default — overridden by config at load time. The
		// portfolio-level tax_wrapper override in wealthdb.cfg
		// (e.g. roth_ira for an IRA-held portfolio) wins for every
		// account whose PortfolioExternalID matches; per-account
		// overrides win over that.
		wrapper := canonical.TaxWrapperTaxablePersonal
		portfolio := portfolioID
		change := canonical.AccountChange{
			AccountExternalID:   walletID,
			AccountKind:         canonical.AccountKindCrypto,
			ManagementStyle:     &style,
			TaxWrapper:          &wrapper,
			PortfolioExternalID: &portfolio,
			FirstSeenAt:         firstSeen,
			LastSeenAt:          snap,
			Payload:             jsonOrNull(payload),
		}
		if name != "" {
			n := name
			change.DisplayName = &n
		}
		if baseCcy != "" {
			b := baseCcy
			change.BaseCurrency = &b
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

// appendInstruments emits one InstrumentChange per distinct coin
// ticker observed in silver.transactions (buy_currency or
// sell_currency). The trade history's MIN/MAX(occurred_at) give us
// the actual coin-held span for gold's first_seen_at / last_seen_at.
func (c *Connection) appendInstruments(ctx context.Context, batch *canonical.SnapshotBatch, snap int64) error {
	const q = `
SELECT
    ticker,
    CAST(MIN(occurred_at_ts) AS BIGINT) AS first_seen,
    CAST(MAX(occurred_at_ts) AS BIGINT) AS last_seen
  FROM (
    SELECT buy_currency  AS ticker, EXTRACT(epoch FROM occurred_at) AS occurred_at_ts
      FROM transactions
     WHERE buy_currency IS NOT NULL
    UNION ALL
    SELECT sell_currency AS ticker, EXTRACT(epoch FROM occurred_at) AS occurred_at_ts
      FROM transactions
     WHERE sell_currency IS NOT NULL
  )
 GROUP BY ticker
 ORDER BY ticker`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("appendInstruments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ticker              string
			firstSeen, lastSeen int64
		)
		if err := rows.Scan(&ticker, &firstSeen, &lastSeen); err != nil {
			return err
		}
		name := coinNameFor(ticker)
		sym := ticker
		change := canonical.InstrumentChange{
			InstrumentExternalID: ticker,
			AssetClass:           canonical.AssetClassCrypto,
			Symbol:               &sym,
			Name:                 &name,
			FirstSeenAt:          firstSeen,
			LastSeenAt:           lastSeen,
		}
		batch.Instruments = append(batch.Instruments, change)
	}
	return rows.Err()
}

// appendPositions emits one PositionChange per (account, coin)
// where the running balance from transactions is positive.
//
// Quantity = SUM(buy_amount) − SUM(sell_amount) for the wallet's
// trades. Currency = portfolio's quote_currency (USD or EUR for
// the typical CT setup), defaulting to USD when the portfolio
// has no portfolio_prices entries yet (e.g. a portfolio whose
// overview.csv hasn't been ingested).
//
// MarketValue is resolved in priority order:
//
//   1. The latest portfolio_prices entry for (portfolio, coin) in
//      the position's currency — CT's own per-portfolio valuation,
//      preferred because it reproduces CT's totals exactly.
//   2. For USD-currency positions only, the latest coin_prices
//      entry — the cross-source canonical USD reference. Kicks in
//      for portfolios with no portfolio_prices yet, or for coins
//      CT didn't include in that portfolio's overview.csv.
//   3. For a USD-instrument USD-currency position, the trivial
//      1.0 (USD is its own price; coin_prices doesn't carry it).
//   4. NULL otherwise — most commonly a non-USD-quoted portfolio
//      with a long-tail coin its overview.csv didn't price.
func (c *Connection) appendPositions(ctx context.Context, batch *canonical.SnapshotBatch, snap int64) error {
	// HAVING > 1e-10 drops floating-point dust (positions whose
	// running balance rounds to zero from successive deposits +
	// withdrawals at full DECIMAL precision but lands at a sub-
	// satoshi residual from CT's per-trade decimal scaling). The
	// underlying amounts are DECIMAL(38,18) so this is a safety
	// rail rather than a precision-loss guard.
	const q = `
WITH balances AS (
    SELECT portfolio_external_id, wallet_external_id, instrument_external_id,
           SUM(amount) AS qty
      FROM (
        SELECT portfolio_external_id, wallet_external_id,
               buy_currency  AS instrument_external_id,
               buy_amount    AS amount
          FROM transactions
         WHERE buy_amount IS NOT NULL AND buy_currency IS NOT NULL
        UNION ALL
        SELECT portfolio_external_id, wallet_external_id,
               sell_currency,
              -sell_amount
          FROM transactions
         WHERE sell_amount IS NOT NULL AND sell_currency IS NOT NULL
      )
     GROUP BY 1, 2, 3
    HAVING SUM(amount) > 1e-10
),
quote_by_portfolio AS (
    SELECT portfolio_external_id, MIN(quote_currency) AS quote_currency
      FROM portfolio_prices GROUP BY 1
),
latest_pp_date AS (
    SELECT portfolio_external_id, instrument_external_id, MAX(as_of_date) AS latest_date
      FROM portfolio_prices GROUP BY 1, 2
),
latest_price AS (
    SELECT pp.portfolio_external_id, pp.instrument_external_id,
           pp.quote_currency, pp.price
      FROM portfolio_prices pp
      JOIN latest_pp_date lpd
        ON lpd.portfolio_external_id  = pp.portfolio_external_id
       AND lpd.instrument_external_id = pp.instrument_external_id
     WHERE pp.as_of_date = lpd.latest_date
),
latest_coin_price AS (
    -- Cross-source canonical USD reference, latest per instrument.
    -- QUALIFY-style window pick to avoid a self-join.
    SELECT instrument_external_id, price_usd
      FROM (
        SELECT instrument_external_id, price_usd, as_of_date,
               ROW_NUMBER() OVER (
                   PARTITION BY instrument_external_id
                   ORDER BY as_of_date DESC
               ) AS rn
          FROM coin_prices
      )
     WHERE rn = 1
)
SELECT
    b.portfolio_external_id,
    b.wallet_external_id,
    b.instrument_external_id,
    CAST(b.qty AS VARCHAR) AS qty_str,
    COALESCE(q.quote_currency, 'USD') AS currency,
    CAST(
        COALESCE(
            lp.price,
            -- coin_prices fallback for USD-currency positions.
            CASE WHEN COALESCE(q.quote_currency, 'USD') = 'USD'
                 THEN lcp.price_usd END,
            -- USD-instrument USD-currency trivial price (coin_prices
            -- doesn't carry USD itself; it's excluded as a fiat).
            CASE WHEN b.instrument_external_id = 'USD'
                  AND COALESCE(q.quote_currency, 'USD') = 'USD'
                 THEN 1.0 END
        )
    AS VARCHAR) AS price_str
  FROM balances b
  LEFT JOIN quote_by_portfolio q
    ON q.portfolio_external_id = b.portfolio_external_id
  LEFT JOIN latest_price lp
    ON lp.portfolio_external_id  = b.portfolio_external_id
   AND lp.instrument_external_id = b.instrument_external_id
   AND lp.quote_currency         = q.quote_currency
  LEFT JOIN latest_coin_price lcp
    ON lcp.instrument_external_id = b.instrument_external_id
 ORDER BY 1, 2, 3`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			portfolioID, walletID, ticker, currency string
			qtyStr, priceStr                        sql.NullString
		)
		if err := rows.Scan(&portfolioID, &walletID, &ticker, &qtyStr, &currency, &priceStr); err != nil {
			return err
		}
		instrumentKey := ticker
		change := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    walletID,
			PositionKey:          ticker,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           canonical.AssetClassCrypto,
			Currency:             currency,
			Quantity:             decimalPtrOrNil(qtyStr),
		}
		if change.Quantity != nil && priceStr.Valid && priceStr.String != "" {
			price, err := canonical.NewDecimalFromString(priceStr.String)
			if err == nil {
				mv := change.Quantity.Mul(price)
				change.MarketValue = &mv
			}
		}
		batch.Positions = append(batch.Positions, change)
	}
	return rows.Err()
}

// ---- helpers ------------------------------------------------------

func decimalPtrOrNil(s sql.NullString) *canonical.Decimal {
	if !s.Valid || s.String == "" {
		return nil
	}
	d, err := canonical.NewDecimalFromString(s.String)
	if err != nil {
		return nil
	}
	return &d
}

func jsonOrNull(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}
