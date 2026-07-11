package cointracking

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits one batch per ChangeWindow, stamped at the
// window's End (= latest dump_run snapshot_at in the window). The
// silver replay reconstructs every prior balance from the full
// trade history on each load, so emitting historical snapshots
// would mostly duplicate what gold already has from the previous
// load. Gold sees one snapshot per silver-load-cycle, with the
// latest balances computed live from `transactions`.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}

	// One batch per distinct positions_daily.as_of_date so gold's
	// `PositionsAsOf` can answer historical "what did I hold on
	// 2024-12-31" queries. Each batch's positions reflect the
	// complete portfolio state on that day (forward-filled from
	// the latest per-(portfolio,wallet,instrument) entry whose
	// as_of_date <= the batch's snapshot_at). Dimensions
	// (portfolios, accounts, instruments, fx_rates) only ride
	// the latest batch — they're not snapshot-grain and a single
	// upsert is enough.
	snapDates, err := c.snapshotTimestamps(ctx)
	if err != nil {
		return nil, err
	}
	if len(snapDates) == 0 {
		// No positions_daily rows yet → fall back to a single
		// dimensions-only batch at w.End so portfolios / accounts
		// still get upserted on a fresh load with no trade history.
		batch := canonical.SnapshotBatch{}
		if err := c.appendPortfolios(ctx, &batch, w.End); err != nil {
			return nil, err
		}
		if err := c.appendAccounts(ctx, &batch, w.End); err != nil {
			return nil, err
		}
		if err := c.appendInstruments(ctx, &batch, w.End); err != nil {
			return nil, err
		}
		if err := c.appendFxRates(ctx, &batch); err != nil {
			return nil, err
		}
		return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
	}

	byTime := make(map[int64]*canonical.SnapshotBatch, len(snapDates))
	for _, t := range snapDates {
		byTime[t] = &canonical.SnapshotBatch{}
	}
	if err := c.appendPositionsAcrossSnapshots(ctx, byTime); err != nil {
		return nil, err
	}

	latest := snapDates[len(snapDates)-1]
	latestBatch := byTime[latest]
	if err := c.appendPortfolios(ctx, latestBatch, latest); err != nil {
		return nil, err
	}
	if err := c.appendAccounts(ctx, latestBatch, latest); err != nil {
		return nil, err
	}
	if err := c.appendInstruments(ctx, latestBatch, latest); err != nil {
		return nil, err
	}
	if err := c.appendFxRates(ctx, latestBatch); err != nil {
		return nil, err
	}

	batches := make([]canonical.SnapshotBatch, 0, len(snapDates))
	for _, t := range snapDates {
		batches = append(batches, *byTime[t])
	}
	return silver.NewSnapshotStream(batches), nil
}

// snapshotTimestamps returns the sorted list of distinct
// positions_daily.as_of_date values, converted to Unix epoch
// seconds at UTC midnight. Each value becomes one snapshot in
// gold; for asOf queries that land between change-days, gold's
// "latest snapshot ≤ asOf" picks the right one automatically.
func (c *Connection) snapshotTimestamps(ctx context.Context) ([]int64, error) {
	const q = `
SELECT DISTINCT CAST(EXTRACT(epoch FROM CAST(as_of_date AS TIMESTAMP)) AS BIGINT) AS snap_at
  FROM positions_daily
 ORDER BY snap_at`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimestamps: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

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
			Payload:             silver.JSONOrNil(payload),
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
			Payload:             silver.JSONOrNil(payload),
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
		acNew, vehicle := taxonomyV2()
		change := canonical.InstrumentChange{
			InstrumentExternalID: ticker,
			AssetClass:           canonical.AssetClassCrypto,
			AssetClassNew:        acNew,
			Vehicle:              vehicle,
			Symbol:               &sym,
			Name:                 &name,
			FirstSeenAt:          firstSeen,
			LastSeenAt:           lastSeen,
		}
		batch.Instruments = append(batch.Instruments, change)
	}
	return rows.Err()
}

// appendPositionsAcrossSnapshots populates one PositionChange per
// (snap_date, portfolio, wallet, instrument) with the
// forward-filled holdings reconstructed from positions_daily.
// Drives gold's historical asOf queries: each snapshot_at is a
// distinct positions_daily.as_of_date (= a day on which any
// holdings actually changed), and the row emitted for that
// snapshot is the latest pre-snap entry per (portfolio, wallet,
// instrument) — so even a position that hasn't traded in a year
// reappears in every snapshot until its next change.
//
// Currency = portfolio's quote_currency (USD or EUR for the
// typical CT setup), defaulting to USD when the portfolio has no
// portfolio_prices entries yet.
//
// MarketValue is resolved in priority order, as-of the snapshot's
// date (not the latest available):
//
//   1. portfolio_prices for (portfolio, coin, currency) — CT's
//      own per-portfolio valuation, preferred because it
//      reproduces CT's totals exactly.
//   2. For USD-currency positions, coin_prices.price_usd — the
//      cross-source canonical USD reference, kicks in for
//      portfolios with no portfolio_prices yet, or for coins CT
//      didn't include in that portfolio's overview.csv.
//   3. For a USD-instrument USD-currency position, the trivial
//      1.0.
//   4. NULL otherwise — most commonly a non-USD-quoted portfolio
//      with a long-tail coin its overview.csv didn't price on
//      that day.
//
// All three price tables are joined via the same LEAD-based
// interval trick used for positions_daily, so each row resolves
// in O(active intervals overlapping the snap date) rather than
// O(snap_days × full table).
func (c *Connection) appendPositionsAcrossSnapshots(ctx context.Context, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
WITH distinct_days AS (
    SELECT DISTINCT as_of_date FROM positions_daily
),
pos_intervals AS (
    SELECT
        portfolio_external_id, wallet_external_id, instrument_external_id,
        as_of_date, amount,
        LEAD(as_of_date) OVER (
            PARTITION BY portfolio_external_id, wallet_external_id, instrument_external_id
            ORDER BY as_of_date
        ) AS next_d
      FROM positions_daily
),
position_states AS (
    SELECT
        d.as_of_date AS snap_date,
        i.portfolio_external_id, i.wallet_external_id, i.instrument_external_id,
        i.amount
      FROM distinct_days d
      JOIN pos_intervals i
        ON i.as_of_date <= d.as_of_date
       AND (i.next_d IS NULL OR i.next_d > d.as_of_date)
     WHERE i.amount > 1e-10
),
quote_by_portfolio AS (
    SELECT portfolio_external_id, MIN(quote_currency) AS quote_currency
      FROM portfolio_prices GROUP BY 1
),
pp_intervals AS (
    SELECT
        portfolio_external_id, instrument_external_id, quote_currency,
        as_of_date, price,
        LEAD(as_of_date) OVER (
            PARTITION BY portfolio_external_id, instrument_external_id, quote_currency
            ORDER BY as_of_date
        ) AS next_d
      FROM portfolio_prices
),
cp_intervals AS (
    SELECT
        instrument_external_id, as_of_date, price_usd,
        LEAD(as_of_date) OVER (
            PARTITION BY instrument_external_id ORDER BY as_of_date
        ) AS next_d
      FROM coin_prices
)
SELECT
    CAST(EXTRACT(epoch FROM CAST(s.snap_date AS TIMESTAMP)) AS BIGINT) AS snap_at,
    s.portfolio_external_id,
    s.wallet_external_id,
    s.instrument_external_id,
    CAST(s.amount AS VARCHAR) AS qty_str,
    COALESCE(q.quote_currency, 'USD') AS currency,
    CAST(
        COALESCE(
            pp.price,
            CASE WHEN COALESCE(q.quote_currency, 'USD') = 'USD'
                 THEN cp.price_usd END,
            CASE WHEN s.instrument_external_id = 'USD'
                  AND COALESCE(q.quote_currency, 'USD') = 'USD'
                 THEN 1.0 END
        )
    AS VARCHAR) AS price_str
  FROM position_states s
  LEFT JOIN quote_by_portfolio q USING (portfolio_external_id)
  LEFT JOIN pp_intervals pp
    ON pp.portfolio_external_id  = s.portfolio_external_id
   AND pp.instrument_external_id = s.instrument_external_id
   AND pp.quote_currency         = COALESCE(q.quote_currency, 'USD')
   AND pp.as_of_date <= s.snap_date
   AND (pp.next_d IS NULL OR pp.next_d > s.snap_date)
  LEFT JOIN cp_intervals cp
    ON cp.instrument_external_id = s.instrument_external_id
   AND cp.as_of_date <= s.snap_date
   AND (cp.next_d IS NULL OR cp.next_d > s.snap_date)
 ORDER BY snap_at, s.portfolio_external_id, s.wallet_external_id, s.instrument_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("appendPositionsAcrossSnapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                    int64
			portfolioID, walletID, ticker, currency string
			qtyStr, priceStr                        sql.NullString
		)
		if err := rows.Scan(&snap, &portfolioID, &walletID, &ticker,
			&qtyStr, &currency, &priceStr); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			// Snapshot date not in our planned set — shouldn't
			// happen since byTime was built from the same DISTINCT
			// as_of_date list, but skip defensively.
			continue
		}
		qty := silver.DecimalPtrOrNil(qtyStr)
		if qty == nil {
			continue
		}
		// Fiat held inside a crypto wallet (USD/EUR/CHF/etc.) is
		// the wallet's CASH leg, not a position — gold's
		// `cash_balance` rollup is where it belongs. Splitting
		// here keeps `positions_value` strictly the crypto
		// market value and `cash_balance` strictly the fiat,
		// matching how the brokerage adapters draw the line.
		if isFiat(ticker) {
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: walletID,
				Currency:          ticker,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            *qty,
			})
			continue
		}
		instrumentKey := ticker
		acNew, vehicle := taxonomyV2()
		change := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    walletID,
			PositionKey:          ticker,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           canonical.AssetClassCrypto,
			AssetClassNew:        acNew,
			Vehicle:              vehicle,
			Currency:             currency,
			Quantity:             qty,
		}
		if priceStr.Valid && priceStr.String != "" {
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


// appendFxRates emits one FxRateChange per row in silver.coin_prices:
// the (instrument → USD) pair the price fetcher populated. Drives
// gold's `value_USD` / `wealthdb transactions -x USD` for any row
// whose Currency is a non-base asset — staking rewards, crypto
// transfers, Other Fees paid in non-base coins, etc. — by giving
// gold's SQL FX layer a direct base→quote rate.
//
// Each rate's snapshot_at is the price's `as_of_date` at UTC
// midnight, so gold's historic-mode (nearest rate at or before the
// day) lookup picks up the right day. Fiat-to-USD rates (EUR→USD,
// CHF→USD, frankfurter-sourced) get the same treatment, which means
// an EUR-base portfolio's USD valuation works through the direct
// pair as well.
func (c *Connection) appendFxRates(ctx context.Context, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT
    instrument_external_id,
    CAST(EXTRACT(epoch FROM CAST(as_of_date AS TIMESTAMP)) AS BIGINT) AS snapshot_at,
    CAST(price_usd AS VARCHAR)
  FROM coin_prices
 ORDER BY as_of_date, instrument_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("appendFxRates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			base       string
			snap       int64
			priceStr   sql.NullString
		)
		if err := rows.Scan(&base, &snap, &priceStr); err != nil {
			return err
		}
		if !priceStr.Valid || priceStr.String == "" {
			continue
		}
		rate, err := canonical.NewDecimalFromString(priceStr.String)
		if err != nil {
			continue
		}
		// Gold's mid_rate convention is "1 unit of QUOTE = MidRate
		// units of BASE" (matches Swissquote / UBS rate emission).
		// `coin_prices.price_usd` carries "1 unit of instrument =
		// price_usd USD", so the instrument is the QUOTE and USD
		// is the BASE.
		batch.FxRates = append(batch.FxRates, canonical.FxRateChange{
			SnapshotAt:    snap,
			BaseCurrency:  "USD",
			QuoteCurrency: base,
			MidRate:       rate,
		})
	}
	return rows.Err()
}

