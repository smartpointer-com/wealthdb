# Swissquote adapter

Adapter that projects the `swissquote` silver SQLite into
the canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

## 1. Silver source

- Upstream: `swissquote` repository.
- Silver schema: [swissquote/migrations/0001_initial.sql](../../../collectors/swissquote/migrations/0001_initial.sql).
- Silver README: [swissquote/README.md](../../../collectors/swissquote/README.md).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | Swissquote customer ID | Numeric string from silver's `accounts.account_external_id`. |
| `instrument_external_id` | ISIN when known for the `(symbol, currency)` tuple in any row of the silver, else `symbol + '@' + currency` | See §8 for why ISIN beats symbol+currency now that historical PDF rows use the instrument's long name in `symbol` (different tuple, same ISIN). The fallback path keeps pre-migration-0003 silvers that never observed an ISIN still loadable. |
| `transaction_external_id` | synthetic | Silver doesn't expose a stable per-event ID (Order # is shared across partial fills, and cash rows have `00000000`). The adapter derives a hash of `(account, occurred_at, transaction_type, net_amount, symbol or '', currency)` as the gold ID. Replays converge per gold's window-DELETE-then-INSERT model. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Used by `Status` / `ChangeWindow`. |
| `accounts` | `accounts` (kind=`brokerage`) | Customer ID as `account_external_id`. |
| `positions` (`source='live'`) | `positions` | `position_key` = ISIN when known, else `symbol + '@' + currency`. See §4 for `asset_class`. |
| `positions` (`source='pp:<doc_id>'`) | `positions` | Historical year-end snapshots reconstructed from Portfolio Performance PDFs (silver migration 0004). Same mapping as live — see §8. |
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
| `ETFs` | `etf`, then refined by underlying exposure from the security name (`silver.RefineETFClass`): crypto → `crypto`, bullion → `metal`, fixed income → `bond_etf` |
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

## 8. Instrument identity — name, ISIN, and historical positions

Two `swissquote` migrations shape the instrument-side mapping:

- **Migration 0003** added optional `name` and `isin` columns to
  `positions`. `name` is scraped from the hover tooltip on the
  symbol cell of the Portfolio Overview DOM (e.g.
  `iShares ETF (CH)-Core SPI ETF (CH) ANT A CHF DIS`). `isin`
  comes from the FullQuote link `href` path segment.
- **Migration 0004** added a `source` column tagging row
  provenance. Live XLS rows are tagged `'live'`; rows
  reconstructed from a Portfolio Performance PDF are tagged
  `'pp:<doc_id>'` and use `snapshot_at` = PDF as-of date.

The adapter reads `name` and `isin` via a `PRAGMA table_info`-
driven query build (graceful fallback when an older silver lacks
the columns) and projects them as:

- `Name` → gold `instruments.name`.
- `ISIN` → gold `instruments.isin` (indexed for cross-bank join).

### Why `instrument_external_id` is ISIN-keyed when possible

Historical PDF rows store the instrument's **long name** in the
`symbol` column (e.g. `Example Fund CHF DIS`), while live XLS rows
store the **ticker** (`SMMCHA`). They share the same ISIN. A
naive `symbol + '@' + currency` identity would split the same
logical instrument into two gold rows — one historical, one
live — and break per-instrument time-series queries.

The adapter resolves this by building a
`(symbol, currency) → ISIN` lookup once per Snapshots() call from
every row that carries an ISIN, then keying both
`instrument_external_id` and `position_key` by that ISIN.
Pre-migration-0003 rows that lack a column-level ISIN inherit
one from any later live or historical row sharing the same
`(symbol, currency)` — keeping the live track continuous across
the migration boundary too. Rows whose `(symbol, currency)` has
no ISIN reachable anywhere in silver fall back to
`symbol + '@' + currency` and live in gold alongside
ISIN-keyed siblings under different identities.

## 9. Open questions

- **Multi-currency positions.** Silver's PK treats `(symbol, USD)`
  and `(symbol, EUR)` as separate positions. Gold mirrors this:
  even with the same ISIN, the position_key is taken from the
  currency-paired tuple so multi-currency listings stay separable.
  Cross-currency rollups go through `instruments.symbol`.
- **Document indexing.** If a future need arises to enumerate
  Swissquote document metadata in gold (e.g. "which tax statements
  are loaded?"), add a `documents` gold table joined to `accounts`.
  Not in v1.
