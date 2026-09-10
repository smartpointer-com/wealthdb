# cointracking adapter

Adapter that projects the `cointracking` silver DuckDB into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

cointracking.info is the aggregator-of-record for crypto holdings
across centralised exchanges and on-chain wallets; the silver
loader replays its trade history into a daily-holdings view per
portfolio and into a `portfolio_prices` table that holds CT's
own per-portfolio valuations in each portfolio's quote currency.

## 1. Silver source

- Upstream: [`collectors/cointracking`](../../../collectors/cointracking).
- Silver schema: [`cointracking/migrations/0002_prices.sql`](../../../collectors/cointracking/migrations/0002_prices.sql).
- Silver README: [`cointracking/README.md`](../../../collectors/cointracking/README.md).

The silver is **DuckDB**, not SQLite (one-off exception driven by
DECIMAL(38,18) for crypto amounts + JSON1-style window functions
for the holdings replay). The adapter opens it read-only via the
same `duckdb/duckdb-go/v2` driver the gold engine uses.

## 2. Identifier conventions

| Gold field | Source | Notes |
| --- | --- | --- |
| `portfolio_external_id` | silver `portfolios.portfolio_external_id` | `cu_<id>` form CT uses internally for the per-user-account identifier (the "linked user" surface). |
| `account_external_id` | silver `wallets.wallet_external_id` | Composite `<cu_id>:<wallet_name>` — same key silver uses, so cross-table joins line up. |
| `instrument_external_id` | coin ticker | `BTC`, `ETH`, `USDT`, … Both `position_key` and `instrument_external_id` carry the ticker. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Drive `Status` / `ChangeWindow`. |
| `portfolios` | `portfolios` | Display name lifted directly; `base_currency` derived from `portfolio_prices.quote_currency`. |
| `wallets` | `accounts` (kind=`crypto`) | One account per (portfolio, wallet) — see §4. |
| `transactions` | `transactions` | Every silver row projects to 1–2 canonical rows (trade splitting + the CT-type → `TxKind` map) — see §7. |
| `positions_daily` | drives `positions` | One snapshot batch per distinct `as_of_date`, forward-filled — see §5. |
| `portfolio_prices` | drives `positions.market_value` | CT's per-portfolio valuations in the portfolio's quote currency. |
| `coin_prices` | `fx_rates` | One (coin → USD) `FxRateChange` per row (plus fiat → USD), so gold can value event-dated flows. |
| `coin_mapping` | (unused) | Internal to the price fetcher. |

## 4. Account taxonomy

- **`account_kind`** is `crypto` for every wallet. CT's wallet
  vocabulary (exchange and wallet-vendor names, free-form) doesn't
  expose a reliable custodial-vs-self-custody flag, so the adapter
  uses a single bucket. The finer-grained `crypto_exchange` /
  `crypto_self_custody` enum values stay reserved for future
  adapters that do surface the distinction at source.

- **`management_style`** is always `self_directed`.

- **`tax_wrapper`** defaults to `taxable_personal`. The
  `portfolio_overrides` block in `wealthdb.cfg` overrides it
  per CT portfolio. Example:

  ```json
  "portfolio_overrides": {
      "cointracking": {
          "cu_999999": {"tax_wrapper": "traditional_ira"}
      }
  }
  ```

  Every wallet (= gold account) under `cu_999999` gets stamped
  `ira`; other portfolios keep the default. Per-account
  overrides in the existing `account_overrides` block still win
  over portfolio overrides on the same column (most-specific
  wins).

## 5. Positions

Emitted as one batch per distinct `positions_daily.as_of_date` in
the window, so gold's `PositionsAsOf` can answer historical
"what did I hold on day D" queries. Each batch reflects the
complete portfolio state on that day, forward-filled from the
latest per-(portfolio, wallet, instrument) entry at or before it.
Dimensions (portfolios, accounts, instruments, fx_rates) ride only
the latest batch — they are not snapshot-grain.

- **`quantity`** is the wallet's balance from `positions_daily`
  (the silver loader's full-history replay computes it).

- **`currency`** is the portfolio's quote currency (from its
  `portfolio_prices` rows; defaults to USD if those haven't been
  ingested yet).

- **`market_value`** = `quantity × latest portfolio_prices.price`
  in the portfolio's quote currency. NULL when no price is
  available (long-tail / pre-listing coins, or portfolios whose
  `overview.csv` didn't make it into a bronze snapshot yet); the
  gold layer's per-column upsert leaves prior values intact.

- **`asset_class`** is `crypto` and **`vehicle`** is `physical`
  for every position — coins and tokens held directly in a
  wallet or exchange account. The pair is a single constant
  (`taxonomy` in
  [`classmap.go`](../../internal/silver/cointracking/classmap.go);
  the (`crypto`, `physical`) pair of `docs/TAXONOMY.md`), since CT
  only ever describes digital assets held directly.

## 6. Instruments

One row per distinct coin ticker observed in
`silver.transactions` (across `buy_currency` and `sell_currency`).
`name` comes from a static ticker → coin-name map in
[`coinnames.go`](../../internal/silver/cointracking/coinnames.go);
unknown tickers fall back to the ticker itself.
`first_seen_at` / `last_seen_at` reflect the actual
`MIN/MAX(occurred_at)` for that coin's trades.

## 7. Transactions

Every silver `transactions` row in the window projects into one or
two canonical rows (`transactions.go`):

- **Trade with the portfolio's base currency on one side** → 1 row
  (`buy` / `sell`): instrument = the non-base side, quantity = the
  signed amount, net amount = the signed base-currency cash flow.
- **Trade with no base-currency side** (e.g. crypto-to-crypto) →
  a `sell` + `buy` pair whose ±V base-currency net amounts cancel,
  so SUM-derived cash balances are unaffected by the trade.
- **Non-Trade CT types** (Deposit, Withdrawal, Staking, Income,
  Airdrop, Gift, Spend, Lost, Stolen, …) → 1 row via the CT-type →
  `TxKind` map in `kindmap.go`. Currency is the asset that
  actually moved; non-base assets also carry instrument + quantity
  so instrument-keyed rollups work.

The closing-balance invariant `balance(C) = SUM(quantity WHERE
instrument = C) + SUM(net_amount WHERE currency = C)` ties the
projected rows back to silver's replayed balances. Fees on Trade
rows are internalised by CT into the trade amounts (no separate
fee row — it would double-count); only the standalone "Other Fee"
type produces a `fee` row.

## 8. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty. `ChangeWindow` triggers on any new
`dump_run` past the watermark; per-trade `occurred_at` is not
used as a trigger because every CT transaction is replayed from
the full trade history on each silver load (a new trade
observation surfaces as a new dump_run regardless).
