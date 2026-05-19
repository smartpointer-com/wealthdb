# Schwab adapter

Adapter that projects the `schwab-api-dump` silver SQLite into
the canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

A separate `schwab-web-dump` project is planned for historic
account statements scraped from the Schwab web app (analogous to
[ubs-web-dump](https://github.com/ptu/ubs-web-dump)). When that
lands the adapter will split into subsources (`schwab-api`,
`schwab-web`) the same way the UBS adapter does. Until then the
adapter operates in single-path mode against the API silver.

## 1. Silver source

- Upstream: `schwab-api-dump` repository.
- Silver schema: [schwab-api-dump/migrations/0001_initial.sql](https://github.com/ptu/schwab-api-dump/blob/main/migrations/0001_initial.sql).
- Silver design: [schwab-api-dump/DESIGN.md](https://github.com/ptu/schwab-api-dump/blob/main/DESIGN.md).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | Schwab `hashValue` | Opaque account hash, stable per Schwab developer-app. Plaintext account number stays in payload. |
| `instrument_external_id` | CUSIP if present, else symbol | Mirrors silver's `instrument_key`. |
| `transaction_external_id` | Schwab `activityId` | Already globally unique within Schwab. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Used by `Status` / `ChangeWindow`; not projected as facts. |
| `accounts` | `accounts` (kind=`brokerage`) | Account hash is `account_external_id`. |
| `user_preference` | — | Schwab-internal UI/streamer preferences; no portfolio relevance. Likely permanent omit. |
| `account_balances` | `cash_balances` | One row per `balance_kind`; cash-relevant fields only. |
| `positions` | `positions`; `CASH_EQUIVALENT` rows → `cash_balances` | See §4 below. |
| `open_orders` | — | Operational state, not portfolio state. Deferred. |
| `transactions` | `transactions` | See `kind` mapping in §5. |

## 4. Position routing

Schwab models cash positions as instruments with
`assetType = 'CASH_EQUIVALENT'`, but gold separates cash into
`cash_balances`. The adapter detects `CASH_EQUIVALENT` (and
`MONEY_MARKET` mutual-fund-style cash holdings, case-by-case) and
emits a `CashBalanceChange` instead of a `PositionChange`. The
`currency` and `marketValue` carry over directly.

All other Schwab positions land in `positions` with `asset_class`
derived from `payload.instrument.assetType`:

| Schwab `assetType` | Gold `asset_class` |
| --- | --- |
| `EQUITY` | `equity` |
| `ETF` | `etf` |
| `MUTUAL_FUND` | `fund` |
| `BOND` / `FIXED_INCOME` | `bond` |
| `OPTION` | `option` |
| `FUTURE` | `future` |
| `COLLECTIVE_INVESTMENT` | `fund` |
| (unrecognised) | `other` (original `assetType` preserved in payload) |

### `instrument.name` is empty for EQUITY positions

The `instruments.name` column populates from `position.payload.
instrument.description`. Schwab's Trader API returns `description`
in `COLLECTIVE_INVESTMENT` / `MUTUAL_FUND` / `BOND` payloads but
**not** in `EQUITY` payloads — equity instrument blocks carry only
`{assetType, cusip, symbol, netChange}`. The same omission applies
to `transactions[].transferItems[].instrument` for equity legs:
`description` is absent.

So gold's `name` column is empty for any Schwab equity position
(visible in `wealthdb positions --columns all`, where the
`name` column shows blank for equity rows but populated for
funds/bonds). The company name lives on Schwab's
`/marketdata/instruments` or `/marketdata/quotes` endpoints,
which `schwab-api-dump` doesn't currently call.

Three places this could be fixed; we're deliberately not doing
any of them in wealthdb v1:
1. `schwab-api-dump` enriches positions/instruments by calling
   `/marketdata/quotes` (or `instruments?projection=symbol-search`)
   once per held symbol. Right place architecturally — silver is
   the per-broker faithful projection.
2. A future market-data ingest in wealthdb (DESIGN.md §13.8)
   would populate instrument names from a vendor feed regardless
   of which silver registered the position.
3. (Don't.) Synthesise `name = symbol`, hard-code a CSV, etc.
   Rejected — equity ticker IS the name to a human reader, no
   need to duplicate.

Until either (1) or (2) lands, equity rows render with an empty
`name` column. The `symbol` column is still populated. (The
planned `schwab-web-dump` source is a likely third path —
scraping the company name out of the brokerage UI's holdings
page — but that's its own design question.)

## 5. `transactions.kind` mapping

Schwab's `silver.transactions.kind` discriminator, observed in
real data, maps to the canonical gold `kind` taxonomy:

| Schwab | Gold | Adapter notes |
| --- | --- | --- |
| `TRADE` | `buy` or `sell` | sign of `transferItems[].cost` / `positionEffect` |
| `JOURNAL` | `journal` | catch-all internal cash move |
| `DIVIDEND_OR_INTEREST` | `dividend` or `interest` | from `payload` subtype |
| `WIRE_IN` / `CASH_RECEIPT` / `ELECTRONIC_FUND`(+) | `deposit` | |
| `WIRE_OUT` / `CASH_DISBURSEMENT` / `ELECTRONIC_FUND`(−) | `withdrawal` | |
| `RECEIVE_AND_DELIVER` | `transfer_in` or `transfer_out` | sign-driven |
| `SMA_ADJUSTMENT` | `other` | margin-related, rare |

Per the global fallback rule (DESIGN.md §6.7): any new Schwab
`kind` value an adapter version doesn't recognise lands as `other`
with the original string preserved in payload.

## 6. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty.

## 7. Open questions

- **Tax withholding on dividends.** Schwab reports the withholding
  inside the `DIVIDEND_OR_INTEREST` payload's `transferItems`. The
  current mapping puts the dividend gross in `gross_amount` and net
  in `net_amount`; a separate `tax` row is not generated. Revisit
  if tax-lot work needs the withholding as a distinct event.
- **`open_orders` projection.** Reserved for a future `wealthdb
  orders` subcommand; no schema work needed in gold yet.
