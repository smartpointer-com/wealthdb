# Schwab adapter

Adapter that projects the `schwab-dump` silver SQLite into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

## 1. Silver source

- Upstream: `schwab-dump` repository.
- Silver schema: [schwab-dump/migrations/0001_initial.sql](https://github.com/ptu/schwab-dump/blob/main/migrations/0001_initial.sql).
- Silver design: [schwab-dump/DESIGN.md](https://github.com/ptu/schwab-dump/blob/main/DESIGN.md).

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
