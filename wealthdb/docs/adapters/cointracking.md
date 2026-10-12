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
- Silver schema: the collector's
  [migrations](../../../collectors/cointracking/migrations).
- Silver README: [`cointracking/README.md`](../../../collectors/cointracking/README.md).

The silver is **DuckDB**, not SQLite. Crypto amounts need
DECIMAL(38,18), and the holdings replay uses window functions. The
adapter opens it read-only with the `duckdb/duckdb-go/v2` driver the
gold engine uses.

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
| `positions_daily` | `positions`, `cash_balances` | One snapshot batch per distinct `as_of_date`, forward-filled. Fiat in a wallet is a balance — see §5. |
| `portfolio_prices` | `positions.market_value` | CT's per-portfolio valuations in the portfolio's quote currency. |
| `coin_prices` | `fx_rates` | One (coin → USD) `FxRateChange` per row (plus fiat → USD), so gold can value event-dated flows. |
| `coin_mapping` | (unused) | Internal to the price fetcher. |

## 4. Account taxonomy

- **`account_kind`** is `crypto` for every wallet. CT's wallet
  vocabulary (exchange and wallet-vendor names, free-form) has no
  reliable custodial-vs-self-custody flag, so the adapter uses one
  bucket. The finer `crypto_exchange` and `crypto_self_custody` kinds
  are for sources that state the distinction.

- **`management_style`** is always `self_directed`.

- **`tax_wrapper`** defaults to `taxable_personal`. Config
  `portfolio_overrides` sets it per CT portfolio, for every wallet in
  it. A per-account `account_overrides` entry still wins
  ([../DESIGN.md](../DESIGN.md) §5.1).

## 5. Positions

Emitted as one batch per distinct `positions_daily.as_of_date`, so
gold's `PositionsAsOf` can answer "what was held on day D". Each batch
is the complete portfolio state on that day, forward-filled from the
latest per-(portfolio, wallet, instrument) entry at or before it.
Dimensions (portfolios, accounts, instruments, fx_rates) ride only the
latest batch. A silver with no `positions_daily` rows yet gets one
dimensions-only batch at the window's end.

- **`quantity`** is the wallet's balance from `positions_daily`. The
  silver loader's full-history replay computes it.

- **Fiat** held in a wallet (USD, EUR, CHF, …) is the wallet's cash,
  not a position. It is a `closing` cash balance in that currency, so
  `positions_value` stays the crypto market value alone.

- **`currency`** is the portfolio's quote currency, from its
  `portfolio_prices` rows. It defaults to USD when there are none yet.

- **`market_value`** = `quantity` × the price in effect on the
  snapshot's day, in the portfolio's quote currency. The price is the
  first of:
  1. `portfolio_prices` for the portfolio, coin and quote currency:
     CT's own valuation, which reproduces CT's totals exactly;
  2. `coin_prices.price_usd`, for a USD-quoted portfolio;
  3. 1, for USD held in a USD-quoted portfolio.

  Otherwise `market_value` is NULL. That is most often a long-tail coin
  in a portfolio quoted in another currency.

- **`asset_class`** is `crypto` and **`vehicle`** is `physical` for
  every position: coins and tokens held directly in a wallet or
  exchange account. The pair is one constant (`taxonomy` in
  [`classmap.go`](../../internal/silver/cointracking/classmap.go)),
  since CT only describes digital assets held directly
  ([../TAXONOMY.md](../TAXONOMY.md)).

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
projected rows back to silver's replayed balances. CT puts a Trade
row's fee in one of its own currencies inside the trade amounts, so
it gets no row of its own, which would double-count it. A fee in a
third currency rides the payload. Only the standalone "Other Fee" type
produces a `fee` row.

Every row's payload carries the CT fields the lot engine reads, under
their silver column names, each only when set: `type`, `comment`,
`buy_currency`, `sell_currency`, `fee_amount` and `fee_currency`. The
kind alone cannot tell a deposit from an airdrop or a dust sweep; the
type can. [../LOTS.md](../LOTS.md) §6 says how the lot engine reads them.

## 8. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty. `ChangeWindow` triggers on any new `dump_run`
past the watermark. A trade's `occurred_at` is no trigger: the silver
load replays the full trade history, so a new trade can appear at any
old date. Once triggered, the window spans every transaction, every
`dump_run` and every `coin_prices` date, so the FX rates the adapter
emits fall inside it too.
