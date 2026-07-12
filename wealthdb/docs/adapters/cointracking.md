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
| `transactions` | (in-process source of derived positions) | NOT yet projected to gold's `transactions` table — see §7. |
| `positions_daily` | (unused) | Per-portfolio daily aggregate; the adapter derives per-wallet balances from the trade history instead. |
| `portfolio_prices` | drives `positions.market_value` | CT's per-portfolio valuations in the portfolio's quote currency. |
| `coin_prices` | (unused in this iteration) | Cross-portfolio canonical USD reference; available to the gold layer for downstream FX paths. |
| `coin_mapping` | (unused) | Internal to the price fetcher. |

## 4. Account taxonomy

- **`account_kind`** is `crypto` for every wallet. CT's wallet
  vocabulary (an exchange, a hardware wallet, a staking provider, …) doesn't
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
          "cu_999999": {"tax_wrapper": "roth_ira"}
      }
  }
  ```

  Every wallet (= gold account) under `cu_999999` gets stamped
  `roth_ira`; other portfolios keep the default. Per-account
  overrides in the existing `account_overrides` block still win
  over portfolio overrides on the same column (most-specific
  wins).

  An IRA wrapper is a tax sleeve; how trades are placed is the
  orthogonal `management_style`.

## 5. Positions

Emitted at one `snapshot_at` per ChangeWindow (= `MAX(dump_runs.
snapshot_at)` in the window). The silver loader replays every
prior balance from the full trade history on each load, so
historical snapshots would mostly duplicate what gold already has
from the previous load.

- **`quantity`** is the running coin balance computed in-line from
  `SUM(buy_amount) − SUM(sell_amount)` over the wallet's
  transactions. A `> 1e-10` filter drops sub-satoshi dust that
  successive deposits + withdrawals can leave from CT's per-trade
  decimal scaling.

- **`currency`** is the portfolio's quote currency (USD for some,
  EUR for others; defaults to USD if the portfolio's
  `portfolio_prices` rows haven't been ingested yet).

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

NOT emitted in this iteration. The silver `transactions` table is
the authoritative trade-history record at full per-row fidelity;
downstream queries can read it directly. Promoting the per-trade
records into gold's `transactions` table would mean classifying
CT's per-row `type` (Trade / Deposit / Withdrawal / Income / Spend
/ Lost / Stolen / Gift / Mining / Staking / Airdrop / …) into the
canonical `TxKind` enum, splitting two-sided trades into a buy +
sell pair, deriving the canonical signed amounts in the
portfolio's quote currency, and validating sign convention against
the running balance reconciliation. Left as a follow-up for when
a downstream query actually needs it.

## 8. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty. `ChangeWindow` triggers on any new
`dump_run` past the watermark; per-trade `occurred_at` is not
used as a trigger because every CT transaction is replayed from
the full trade history on each silver load (a new trade
observation surfaces as a new dump_run regardless).
