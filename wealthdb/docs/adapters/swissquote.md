# Swissquote adapter

Adapter that projects the `swissquote-dump` silver SQLite into
the canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../../DESIGN.md](../../DESIGN.md) §6.

## 1. Silver source

- Upstream: `swissquote-dump` repository.
- Silver schema: [swissquote-dump/migrations/0001_initial.sql](https://github.com/ptu/swissquote-dump/blob/main/migrations/0001_initial.sql).
- Silver README: [swissquote-dump/README.md](https://github.com/ptu/swissquote-dump/blob/main/README.md).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | Swissquote customer ID | Numeric string from silver's `accounts.account_external_id`. |
| `instrument_external_id` | symbol + `@` + currency | Mirrors silver's `(symbol, currency)` composite PK; Swissquote often lacks ISIN in the Positions export. The optional ISIN that the loader backfills from the transactions stream is promoted to `instruments.isin` when present. |
| `transaction_external_id` | synthetic | Silver doesn't expose a stable per-event ID (Order # is shared across partial fills, and cash rows have `00000000`). The adapter derives a hash of `(account, occurred_at, transaction_type, net_amount, symbol or '', currency)` as the gold ID. Replays converge per gold's window-DELETE-then-INSERT model. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Used by `Status` / `ChangeWindow`. |
| `accounts` | `accounts` (kind=`brokerage`) | Customer ID as `account_external_id`. |
| `positions` | `positions` | `position_key` = `symbol + '@' + currency`; see §4 for `asset_class`. |
| `currency_balances` | `cash_balances`; also derives `fx_rates` from `rate_to_chf` | See §5. |
| `transactions` | `transactions` | See `transaction_type` mapping in §6. |
| `documents` | — | PDFs are bronze-only; gold doesn't store binaries. |

## 4. `asset_class` derivation for `positions`

The Swissquote Positions XLS export groups rows under section
headers ("ETFs", "Bonds", "Shares", "Funds", "Structured Products",
...) — silver preserves this as `asset_class` inside `payload`.

| Swissquote section header | Gold `asset_class` |
| --- | --- |
| `Shares` / `Stocks` | `equity` |
| `ETFs` | `etf` |
| `Bonds` | `bond` |
| `Funds` | `fund` |
| `Structured Products` | `other` (refine if/when a richer category lands in gold) |
| `Precious Metals` | `metal` |
| `Options` | `option` |
| (other) | `other`, raw header preserved in payload |

## 5. `currency_balances` → `cash_balances` + `fx_rates`

Each silver `currency_balances` row carries (per the silver
schema comment) `rate_to_chf`, `cash_balance`, `positions_value`,
`total_value`, `valuation_chf`, and `account_pct`. The adapter
projects it as:

- One `CashBalanceChange` per row with `balance_kind = 'closing'`,
  `amount = payload.cash_balance`, `currency = silver.currency`.
- One `FxRateChange` per non-CHF row: `base_currency = 'CHF'`,
  `quote_currency = silver.currency`, `mid_rate = 1.0 /
  payload.rate_to_chf` (so that `quantity_in_quote * mid_rate =
  amount_in_base`; double-check the convention against UBS rates
  before publishing).

The CHF row's `rate_to_chf = 1.0` is skipped (no useful FX rate).
The `positions_value` field is not projected to gold — it's
redundant with the rollup of `positions` rows for the same
snapshot.

## 6. `transactions.transaction_type` mapping

Swissquote's `transaction_type` discriminator, observed in real
data, maps to the canonical gold `kind` taxonomy:

| Swissquote | Gold |
| --- | --- |
| `Buy` | `buy` |
| `Sell` | `sell` |
| `Dividend` | `dividend` |
| `Coupon` | `coupon` |
| `Capital Gain` | `capital_gain` |
| `Custody Fees` | `fee` |
| `Fees Tax Statement` | `fee` |
| `Interest on deposits` | `interest` |
| `Payment` | `deposit` |
| `Debit` | `withdrawal` |

Per the global fallback rule (DESIGN.md §6.7): any new Swissquote
`transaction_type` value an adapter version doesn't recognise
lands as `other` with the raw type preserved in payload.

## 7. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty.

## 8. Open questions

- **ISIN coverage.** The Positions export omits ISINs, but the
  transactions CSV carries them. The adapter's instrument upsert
  should backfill `instruments.isin` from later transaction rows
  when a position originally landed without one. Verify this
  doesn't churn `instruments.last_seen_at`.
- **Multi-currency positions.** Silver's PK treats `(symbol, USD)`
  and `(symbol, EUR)` as separate positions. Gold mirrors this
  via `position_key = symbol + '@' + currency`. Cross-currency
  rollups go through `instruments.symbol`.
- **Document indexing.** If a future need arises to enumerate
  Swissquote document metadata in gold (e.g. "which tax statements
  are loaded?"), add a `documents` gold table joined to `accounts`.
  Not in v1.
