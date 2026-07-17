# Schwab adapter

Adapter that projects two Schwab silver SQLite databases into the
canonical gold schema:

- `schwab-api` — Trader-API JSON, live position / cash
  snapshots and ~2y of transactions.
- `schwab-web` — netbanking scrape (live account list +
  reconstructed historical positions, cash balances, and
  transactions from monthly statement PDFs).

Implements the `silver.Adapter` / `silver.Connection` interface
defined in [../DESIGN.md](../DESIGN.md) §6. When both subsources
are configured the orchestrator (`merge.go`) splices them: web
backfills pre-api-coverage transactions and contributes
per-statement-period historical positions and cash balances that
api doesn't surface at all. See §8 below for the merge contract
and §1 below for the suffix↔hashValue bridge.

Single-path mode (`"kind": "schwab", "path": ...`) is preserved
for users with only the api silver — it's treated as api-only
and skips the orchestrator's merge layer.

## 1. Silver sources

- API: [`schwab-api`](../../../collectors/schwab-api/).
  Silver schema: [migrations/0001_initial.sql](../../../collectors/schwab-api/migrations/0001_initial.sql).
- Web: [`schwab-web`](../../../collectors/schwab-web/).
  Silver schema: [migrations/0001_initial.sql](../../../collectors/schwab-web/migrations/0001_initial.sql)
  + [migrations/0002_historical_snapshots.sql](../../../collectors/schwab-web/migrations/0002_historical_snapshots.sql).
- Cross-collector interop notes: [schwab-web/INTEROP.md](../../../collectors/schwab-web/INTEROP.md).

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

All other Schwab positions land in `positions` with the
`(asset_class, vehicle)` pair derived from
`payload.instrument.assetType` (plus, for collective vehicles,
`instrument.type` and the security name):

| Schwab `assetType` | Gold `(asset_class, vehicle)` |
| --- | --- |
| `EQUITY` | `(public_equity, stock)` |
| `ETF` | `(RefineETFExposure(name), etf)` (in the API's enum, but real dumps use `COLLECTIVE_INVESTMENT` below) |
| `MUTUAL_FUND` | `(fundExposure(name), fund)` — name-derived exposure, money-market funds → `cash` |
| `BOND` / `FIXED_INCOME` | `(fixed_income, bond)` |
| `OPTION` | `(public_equity, option)` (equity underlying) |
| `FUTURE` | `(public_equity, future)` (equity underlying) |
| `COLLECTIVE_INVESTMENT` | `(fundExposure(name), fund)`; with `instrument.type = EXCHANGE_TRADED_FUND` → `(RefineETFExposure(name), etf)` — this is how the Trader API actually types ETFs |
| (unrecognised) | `(other, other)` (original `assetType` preserved in payload) |

For ETF and fund wrappers the exposure is refined from
`instrument.description` (`silver.RefineETFExposure`): crypto →
`crypto`, bullion → `metal` (miners funds stay `public_equity` —
they hold stocks), bond / fixed-income → `fixed_income`, everything
else → `public_equity`. Purchased money-market funds reaching the
`MUTUAL_FUND` / `COLLECTIVE_INVESTMENT` path take `cash` instead
(`fundExposure` routes a money-fund name to cash before name-based
exposure refinement). Name-shy exchange-traded products are pinned
via the config's `instrument_overrides` (DESIGN.md §13.9).

### `instrument.name` is empty for EQUITY positions

The `instruments.name` column populates from `position.payload.
instrument.description`. Schwab's Trader API returns `description`
in `COLLECTIVE_INVESTMENT` / `MUTUAL_FUND` / `BOND` payloads but
**not** in `EQUITY` payloads — equity instrument blocks carry only
`{assetType, cusip, symbol, netChange}`. The same omission applies
to `transactions[].transferItems[].instrument` for equity legs:
`description` is absent.

So gold's `name` column is empty for any Schwab equity position
(visible in `wealthdb holdings positions --columns all`, where the
`name` column shows blank for equity rows but populated for
funds/bonds). The company name lives on Schwab's
`/marketdata/instruments` or `/marketdata/quotes` endpoints,
which `schwab-api` doesn't currently call.

Three places this could be fixed; we're deliberately not doing
any of them in wealthdb v1:
1. `schwab-api` enriches positions/instruments by calling
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
planned `schwab-web` source is a likely third path —
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

## 7. Web subsource (schwab-web)

When the `schwab-web` subsource is configured, the adapter
contributes three things the api silver doesn't have:

- **Historical position snapshots.** `historical_position_snapshots`
  carries per-statement-period holdings (one row per (period_end,
  account, instrument_key)). Cadence is monthly when statements
  are available. Statements carry no CFI/assetType code, so the
  `(asset_class, vehicle)` pair is derived from the statement
  section (`Equities` → `(public_equity, stock)`, `Exchange Traded
  Funds` → `(RefineETFExposure(description), etf)`, `Fixed Income`
  → `(fixed_income, bond)`, `Options` → `(public_equity, option)`),
  falling back to instrument-key / description shape heuristics for
  other sections — OCC symbol or CALL/PUT → `(public_equity,
  option)`, money-market name or XX-ending money-fund ticker →
  `(cash, fund)`, CUSIP or coupon → `(fixed_income, bond)`,
  word-ETF → etf, four-letter-plus-X ticker → fund, ETF-only
  issuer name (iShares / SPDR / Vanguard / …) → etf, else
  `(public_equity, stock)`. Per-column upsert lets a
  later api emission win on the underlying instrument row's
  dimension when the same key reappears source-classified.
- **Historical cash balances.** `historical_cash_balances`
  carries opening + closing balances per statement period.
  Opening lands at `period_start`, closing at `period_end`; rows
  with NULL on a side skip that side rather than coercing to 0.
- **Pre-api transaction backfill.** Web's transactions reach
  ~3-4y back (limited by Schwab's transaction-history export);
  the api only covers ~2y. The adapter emits web transactions
  strictly older than each account's api-coverage-start.
- **Nickname.** Web's `accounts.nickname` is the account
  label as Schwab renders it in the UI (e.g. an account-type
  hint like "IRA Account …NNN"). Promoted onto AccountChange so
  per-column upsert merges it with api's other account columns.

### 7.1. Suffix ↔ hashValue bridge

Web stores `account_external_id` as the 3-to-5-digit account
suffix Schwab shows in the UI. The api stores Schwab's opaque
`hashValue`. The orchestrator builds the suffix → hashValue
bridge lazily on first Status/Snapshots/Transactions call, in
two tiers per web account:

1. **Exact.** When the web account's payload carries a non-empty
   `account_number_full` (the number as printed on statement
   PDFs, e.g. `1234-5678`), both it and every api
   `accounts.account_number` (a column the api silver promotes
   from the `/accounts/accountNumbers` response) are reduced to
   their digit sequences and compared for equality. The UI
   suffix is by construction the trailing digits of the account
   number, so a full number that doesn't end in the account's
   own suffix can only be a mis-parsed statement header — that
   disagreement fails the bridge loudly rather than rebridging
   the account's history onto the wrong hashValue. Two api
   accounts can't share a full number, but the case is guarded
   with the same loud failure as the suffix tier. A full number
   matching no api account falls through to the suffix tier —
   the api roster may lag the statement side.
2. **Suffix.** Without a resolving full number, the web suffix
   is matched against the trailing digits of every api
   `account_number`. This tier is unambiguous as long as no two
   api accounts share the same trailing-N digits; a web account
   that does trigger ambiguity fails the bridge loudly rather
   than silently mismapping.

Web rows whose suffix doesn't bridge to any api hashValue are
dropped silently — they have no api counterpart to merge with
and emitting them under the raw suffix would create orphaned
gold rows.

### 7.2. Transaction splice — hard cut, not overlap merge

Per [INTEROP §2](../../../collectors/schwab-web/INTEROP.md#2-transaction-identifier-mismatch):
the two silvers' `activity_id` spaces are disjoint (api uses
Schwab's real `activityId`; web uses a synthetic SHA-256 prefix).
Any cross-source per-row match would be heuristic and risk
double-counting. Inside the api window, api wins (real
`activity_id`, no parser approximation); web emits only
timestamps strictly less than `MIN(api.timestamp)` for that
account.

### 7.3. PDF sha256 churn

INTEROP §3 documents that Schwab regenerates statement PDFs per
download (different sha256 each time, same logical content).
Mitigation lives in `schwab-web` silver — the historical
tables use INSERT OR REPLACE on the natural PK
`(as_of_date | period_end, account, instrument_key | currency)`
so a re-parse of a churned PDF converges on a single row.
wealthdb doesn't need to dedupe further.

## 8. Open questions

- **Tax withholding on dividends.** Schwab reports the withholding
  inside the `DIVIDEND_OR_INTEREST` payload's `transferItems`. The
  current mapping puts the dividend gross in `gross_amount` and net
  in `net_amount`; a separate `tax` row is not generated. Revisit
  if tax-lot work needs the withholding as a distinct event.
- **`open_orders` projection.** Reserved for a future `wealthdb
  orders` subcommand; no schema work needed in gold yet.
- **1099-XML structured tax-lot data.** Per
  [INTEROP §4](../../../collectors/schwab-web/INTEROP.md#4-tax-form-structure-has-no-api-equivalent),
  schwab-web silver carries 1099 Composite as PDF/XML/CSV; the
  XML has lot-level detail (cost basis, term, wash-sale flag)
  the api doesn't surface. Wealthdb doesn't ingest this yet — a
  future `tax_lots` gold table could project it.
- **Explicit suffix→hashValue override config.** The bridge is
  auto-only. The exact tier (§7.1) resolves any realistic suffix
  collision once `account_number_full` is present, so an
  `account_bridge: {<api_hash>: <web_suffix>}` field on the
  schwab silver_source config is needed only if both tiers fail
  ambiguously — a suffix collision on an account whose payload
  carries no full number.
