# Swissquote adapter

The adapter projects the `swissquote` silver SQLite into the canonical
gold schema. It implements the `silver.Adapter` / `silver.Connection`
interface of [../DESIGN.md](../DESIGN.md) §6.

## 1. Silver source

- Upstream: [`collectors/swissquote`](../../../collectors/swissquote).
- Silver schema: the collector's
  [migrations](../../../collectors/swissquote/migrations).
- Silver README: [swissquote/README.md](../../../collectors/swissquote/README.md).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | Swissquote customer ID | Numeric string from silver's `accounts.account_external_id`. |
| `instrument_external_id` | ISIN when any row of the silver states one for the `(symbol, currency)` pair, else `symbol + '@' + currency` | §8 says why. A silver that states no ISIN still loads. |
| `transaction_external_id` | synthetic | Silver has no stable per-event id: the order number is shared across partial fills, and cash rows carry `00000000`. The adapter hashes `(account, occurred_at, transaction_type, symbol or '', currency, net_amount)` (`syntheticTxID`). The hash is the same on every reload. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Read by `Status` and `ChangeWindow` (§7). |
| `accounts` | `accounts` (kind `brokerage`) | Customer id as `account_external_id`. `account_product` (or `account_type` on an older silver) is the `account_category`, and sets the tax wrapper: `Trading` and `Savings` → `taxable_personal`, `Säule 3a` → `pillar_3a`, `Freizügigkeit` → `vested_benefits`. Any other product leaves the wrapper unset. |
| `positions` (`source='live'`) | `positions` | `position_key` = ISIN when known, else `symbol + '@' + currency`. §4 has the `(asset_class, vehicle)` pair, §9 the book value. |
| `positions` (`source='pp:<doc_id>'`) | `positions` | Year-end snapshots reconstructed from Portfolio Performance PDFs. Same mapping as live (§8, §9). |
| `currency_balances` | `cash_balances`; also derives `fx_rates` from `rate_to_chf` | See §5. |
| `transactions` | `transactions` | See `transaction_type` mapping in §6. |
| `documents` | — | PDFs are bronze-only; gold doesn't store binaries. |

## 4. `(asset_class, vehicle)` derivation for `positions`

The Swissquote Positions XLS export groups rows under section headers
("ETFs", "Bonds", "Shares", "Funds", "Structured Products", ...). Silver
keeps the header as `asset_class` inside `payload`.

- The header pins the gold `vehicle`.
- The exposure is fixed per header, except in the two collective
  sections (`ETFs`, `Funds`). There `silver.RefineETFExposure` reads it
  from the security name.

| Swissquote section header | Gold `(asset_class, vehicle)` |
| --- | --- |
| `Shares` / `Stocks` | `(public_equity, stock)` |
| `ETFs` | `(RefineETFExposure(name), etf)`: crypto → `crypto`, bullion → `metal`, bond keywords → `fixed_income`, else `public_equity` |
| `Bonds` | `(fixed_income, bond)` |
| `Funds` | `(RefineETFExposure(name), fund)` |
| `Structured Products` | `(public_equity, structured_product)`: equity exposure by default, as the ubs adapter does for certificate CFIs |
| `Precious Metals` | `(metal, physical)` |
| `Options` | `(public_equity, option)`: Swissquote lists equity options |
| (other) | `(other, other)`; the raw header stays in the payload. Config `instrument_overrides` corrects such a holding. |

## 5. `currency_balances` → `cash_balances` + `fx_rates`

Each silver `currency_balances` row carries `rate_to_chf`,
`cash_balance`, `positions_value`, `total_value`, `valuation_chf` and
`account_pct` in its payload. The adapter projects it as:

- One `CashBalanceChange` with `balance_kind = 'closing'`,
  `amount = payload.cash_balance` and `currency = silver.currency`. A
  row without a cash balance emits none.
- One `FxRateChange` per non-CHF row: `base_currency = 'CHF'`,
  `quote_currency = silver.currency`,
  `mid_rate = payload.rate_to_chf`. "1 of this currency = N CHF" is the
  1-quote-in-base-units convention UBS rates use too. A zero or missing
  rate emits none.

The CHF row (`rate_to_chf = 1.0`) adds no FX rate. `positions_value`
is not projected: it repeats the rollup of the snapshot's `positions`
rows.

## 6. `transactions.transaction_type` mapping

Swissquote's `transaction_type` maps to gold's `kind` (`kindFor`):

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

Any other value lands as `other`, with the raw type in the payload
([../DESIGN.md](../DESIGN.md) §6.8).

## 7. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty.

- A dump or a transaction past the watermark triggers a load.
- The window then widens to every historical position date
  (`MIN` and `MAX` of `positions.snapshot_at`). So the loader's
  window-DELETE covers the historical gold rows before they are
  re-inserted.
- Historical dates never move the change number, so an idle reload is a
  no-op. They do widen `Status`'s snapshot range.
- Each run reads the holdings and the ledger, so both observed
  watermarks are the latest run (`silver.ObserveByRuns`).

## 8. Instrument identity: name, ISIN and historical positions

Two silver columns of `positions` carry the instrument's identity
(silver migration 0003):

- `name`, read from the hover tooltip on the symbol cell of the
  Portfolio Overview page (e.g. `Example Fund (CH) - Core Index ETF CHF
  DIS`). It becomes `instruments.name`.
- `isin`, read from the FullQuote link. It becomes `instruments.isin`.

The `source` column (silver migration 0004) tags a row's provenance.
A live XLS row is `'live'`. A row reconstructed from a Portfolio
Performance PDF is `'pp:<doc_id>'`, with `snapshot_at` the PDF's as-of
date.

A column an older silver lacks reads as NULL (`silver.HasColumn`), so
that silver still loads.

### Why `instrument_external_id` is ISIN-keyed when possible

A historical PDF row stores the instrument's long name in `symbol`
(e.g. `Example Fund CHF DIS`). A live XLS row stores the ticker
(e.g. `EXMPL`). Both share one ISIN. A `symbol + '@' + currency` identity
would split one instrument into two gold rows and break
per-instrument time series.

So the adapter builds a `(symbol, currency) → ISIN` lookup once per
`Snapshots` call, from every row that states an ISIN
(`buildISINBySymbol`). Both `instrument_external_id` and `position_key`
key on that ISIN.

- A row without its own ISIN takes the one another row states for the
  same `(symbol, currency)`. The live track stays continuous over rows
  from before silver stated ISINs.
- A `(symbol, currency)` with no ISIN anywhere in silver keys on
  `symbol + '@' + currency`.

## 9. Cost basis

Silver migration 0006 states each position's `average_cost` per unit,
in the row's currency, and its `price_quote`. The adapter maps them
as follows (DESIGN.md §7.4 has the stamp's meaning):

- `book_value` is `quantity × average_cost`.
- A statement prints a bond's prices in percent of nominal
  (`price_quote = 'percent'`). Its quantity is the nominal, so the
  product is divided by 100.
- The stamp is `derived`, `average`, `excluded`. Swissquote's average
  cost leaves the purchase fees out.
- A row without an average cost has no book value and no stamp.
- A silver without the 0006 columns still loads, with no book value.
- No acquisition date: Swissquote states none.

The same price quote applies to the market value. Where a row states
no value in its own currency, the adapter computes
`quantity × market_price`, divided by 100 for a percent quote.

Every value Swissquote prints is clean. A statement prints a bond's
accrued interest on its own line, in CHF (silver
`accrued_interest_chf`). A CHF bond's market value includes it and
`accrued_interest` says how much it is (DESIGN.md §7.1). A bond in
another currency keeps its clean value: adding a CHF figure would need
a rate the statement does not state, so it stays in the payload. The
live export states no accrued interest.

A live row also states its market value and its P&L in CHF
(`market_value_chf`, `unrealized_gain_loss_chf`). Their difference is
the cost in CHF at the rates of the purchases. Gold's book value is
in the row's currency, so these figures stay in the payload
(`total_value_chf`, `pl_nominal_chf`).

## 10. Open questions

- **One ISIN in two currencies.** Silver keeps `(symbol, USD)` and
  `(symbol, EUR)` apart. Gold keys a position by its ISIN alone, so two
  listings of one ISIN in one account and snapshot share a
  `position_key` and collide on the gold `positions` key. Keying on
  ISIN and currency would keep them apart.
- **Document indexing.** Gold holds no Swissquote document metadata. A
  `documents` gold table joined to `accounts` would answer "which tax
  statements are loaded?".
