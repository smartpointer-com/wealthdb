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
api doesn't surface at all. See §7 below for the merge contract,
§7.1 for the suffix↔hashValue bridge and §8 for cost basis and tax
lots.

Single-path mode (`"kind": "schwab", "path": ...`) is preserved
for users with only the api silver — it's treated as api-only
and skips the orchestrator's merge layer.

## 1. Silver sources

- API: [`schwab-api`](../../../collectors/schwab-api/).
  Silver schema: the collector's [migrations](../../../collectors/schwab-api/migrations).
- Web: [`schwab-web`](../../../collectors/schwab-web/).
  Silver schema: the collector's [migrations](../../../collectors/schwab-web/migrations).
- Cross-collector interop notes: [schwab-web/INTEROP.md](../../../collectors/schwab-web/INTEROP.md).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | Schwab `hashValue` | Opaque account hash, stable per Schwab developer-app. Plaintext account number stays in payload. |
| `instrument_external_id` | CUSIP if present, else symbol | Mirrors silver's `instrument_key`. |
| `transaction_external_id` | Schwab `activityId` | Already globally unique within Schwab. |
| `realized_lot_external_id` | web `closed_lots` key | A hash of `(logical_doc_key, document_kind, lot_index)`: stable across loads, one per copy of a lot. |
| `position_lots.lot_key` | web `open_lots.lot_index` | The lot's place in print order under its holding. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Used by `Status` / `ChangeWindow`; not projected as facts. |
| `accounts` | `accounts` (kind=`brokerage`) | Account hash is `account_external_id`. |
| `user_preference` | — | Schwab-internal UI/streamer preferences; no portfolio relevance. Likely permanent omit. |
| `account_balances` | `cash_balances` | One row per `balance_kind`; cash-relevant fields only. |
| `positions` | `positions`; `CASH_EQUIVALENT` rows → `cash_balances` | See §4 below; book value in §8. |
| `open_orders` | — | Operational state, not portfolio state. Deferred. |
| `transactions` | `transactions` | See `kind` mapping in §5. |

The web silver's tables are in §7 and §8.

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

Three places this could be fixed; none is done in wealthdb v1,
deliberately:
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
`name` column. The `symbol` column is still populated.

## 5. `transactions.kind` mapping

Schwab's `silver.transactions.kind` discriminator, observed in
real data, maps to the canonical gold `kind` taxonomy:

| Schwab | Gold | Adapter notes |
| --- | --- | --- |
| `TRADE` | `buy`, `sell` or `other` | sign of `netAmount` (negative = buy); zero is `other` (see below) |
| `JOURNAL` | `journal` | catch-all internal cash move |
| `DIVIDEND_OR_INTEREST` | `dividend` or `interest` | from `payload` subtype |
| `WIRE_IN` / `CASH_RECEIPT` / `ELECTRONIC_FUND`(+) | `deposit` | |
| `WIRE_OUT` / `CASH_DISBURSEMENT` / `ELECTRONIC_FUND`(−) | `withdrawal` | |
| `RECEIVE_AND_DELIVER` | `corporate_action`, `journal`, `transfer_in` or `transfer_out` | see below |
| `SMA_ADJUSTMENT` | `other` | margin-related, rare |

A `TRADE` with a zero `netAmount` buys and sells nothing. Schwab books
a "System transfer" this way. It restates a holding at its cost, and the
shares stay in the account. The row is `other`, and the lot engine
skips it.

`RECEIVE_AND_DELIVER` carries a zero `netAmount`, so the adapter types
it from the description and the security leg. Schwab books the legs of
one event on the same account at the same instant, and the adapter
reads them as a group:

- A row whose description names a split, merger, expiration, spin-off
  or name change is a `corporate_action`. So is every row booked with
  it, such as the new-share leg of a reverse split.
- Legs of one instrument whose quantities cancel move shares between
  the account's cash and margin sub-accounts. They are a `journal`.
- Any other row is a delivery: `transfer_in` for a positive quantity,
  `transfer_out` for a negative one.

Per the global fallback rule (DESIGN.md §6.8): any new Schwab
`kind` value an adapter version doesn't recognise lands as `other`
with the original string preserved in payload.

The API signs `netAmount` from the account's side, and the adapter
keeps that sign on every kind. A row signed against its kind is a
correction, such as a dividend clawed back, and nets against the
booking it corrects. The web feeds print most figures as magnitudes,
so there the kind orients the figure; only a minus printed on an
inflow kind is kept, because the statements print one only on such a
correction. The statement parser's `Unknown` bucket is the exception:
its rows carry the figure as printed, and the cash shapes read out of it
by name (a short sale and its cover, a pass-through or borrow fee, a
withholding or its reclaim, a fund's capital-gain payout) keep that sign.
Anything else the bucket carries (a share journal, an in-kind
transfer, a corporate action, an expiry) stays `other`.

## 6. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty.

## 7. Web subsource (schwab-web)

When the `schwab-web` subsource is configured, the adapter
contributes what the api silver doesn't have. The tax lots, open
and realized, are in §8.

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
  A statement prints a holding's accrued interest or declared
  dividend apart from its market value. Gold's `market_value`
  includes it and `accrued_interest` says how much it is (DESIGN.md
  §7.1); the printed value stays in the payload as
  `printed_market_value`. The api positions state none.
- **Historical cash balances.** `historical_cash_balances`
  carries opening + closing balances per statement period.
  Opening lands at `period_start`, closing at `period_end`; rows
  with NULL on a side skip that side rather than coercing to 0.
- **Pre-api transaction backfill.** Web's transactions reach
  ~3-4y back (limited by Schwab's transaction-history export);
  the api only covers ~2y. The adapter emits web transactions
  strictly older than each account's api-coverage-start. The
  statement and history feeds carry the sales; the `form_1099b`
  rows stay out of the transactions (§8.3).
- **Nickname.** Web's `accounts.nickname` is the account
  label as Schwab renders it in the UI (e.g. an account-type
  hint like "IRA Account …NNN"). Promoted onto AccountChange so
  per-column upsert merges it with api's other account columns.

### 7.1. Suffix ↔ hashValue bridge

Web stores `account_external_id` as the 3-to-5-digit account
suffix Schwab shows in the UI. The api stores Schwab's opaque
`hashValue`. The orchestrator builds the suffix → hashValue
bridge lazily on first Snapshots/Transactions call, in
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
timestamps strictly before the account's api coverage start, and
api only those from it on.

The coverage start is the account's first api row, unless that row
is a stray: the api can return one trade from months before its
history proper, then fall silent. A leading api row followed by more
than 30 days of api silence, during which the web books the account
as active, is set aside, and the history starts at the next row. The
web's activity is the evidence; a quiet account keeps its first row
as its start.

### 7.3. PDF sha256 churn

INTEROP §3 documents that Schwab regenerates statement PDFs per
download (different sha256 each time, same logical content). The
mitigation lives in `schwab-web` silver: every load gate keys on the
logical document, not its bytes, and the statement tables keep one
row per account and period end, owned by the first statement that
prints it (schwab-web DESIGN.md §4.4 and §4.5). wealthdb doesn't need
to dedupe further.

## 8. Cost basis and tax lots

Schwab's basis is the sum of a holding's tax lots, commissions
included. Every book value carries its stamp (DESIGN.md §7.4).

### 8.1. Positions

| Source | `book_value` | Stamp |
| --- | --- | --- |
| api `positions` | market value − `unrealized_gain_loss` | derived, lots, included |
| web statement holdings | `cost_basis` as printed | stated, lots, included |
| web statement holdings without `cost_basis` | market value − `unrealized_gain_loss` | derived, lots, included |

- The api's open P/L is the long side's, or the short side's when
  the position is short. A short position's basis is what the short
  sale raised. It is negative, like the position and like the basis
  the statements print for a short holding.
- An api row that is long and short at once has no basis: its P/L
  covers one side only.
- A silver from before api migration 0005 has no
  `unrealized_gain_loss`. Its positions have no basis.
- A statement holding's `acquisition_date` is its earliest lot's
  acquired date. Holdings without lots have none.

### 8.2. Open lots

`open_lots` holds the lots the 2020-2024 statements print under each
holding. Each lot lands in `position_lots` beside its holding's
position row: same period end, bridged account and position key
(the holding's `instrument_key`).

- `lot_key` is the lot's place in print order (`lot_index`).
- `quantity` and `book_value` are as printed. A short lot prints
  both negative.
- A lot whose basis Schwab does not know ("N/A") has no book value
  and no `basis_origin`.
- `term` is `short` or `long` where the statement prints it.
  `covered` stays NULL: statements do not say.
- The payload keeps the endnote markers, the cost per share, the
  unrealized gain, the holding days and the raw line.
- A lot whose holding row is missing has no position to sit beside.
  It is dropped and counted in the load log.

### 8.3. Realized lots

`closed_lots` holds the realized lots of three year-end documents.
The adapter returns every one of them as a realized lot. They are
not cut at the api's coverage start, so every tax year reaches gold.
Three kinds of lot are dropped and counted in the load log:

- a lot of an account the bridge does not resolve (§7.1);
- a lot of a document kind gold does not know;
- a lot that states neither a tax year nor a disposal date.

The same sale appears in several documents, so one set is primary
per account and tax year:

1. The best document kind present: the 1099-B, then the Year-End
   Summary, then the Gain/Loss Report.
2. Within that kind, the latest document. A corrected 1099 repeats
   the original. A Year-End Summary can arrive twice, as its own PDF
   and inside the 1099 Composite. The document date comes from
   `logical_doc_key`. Two copies dated alike go to the greater key.
   A key whose date does not read counts as the oldest.

The figures map as printed:

- Quantity, proceeds and cost basis are magnitudes. A NULL cost
  basis stays NULL.
- The gain is signed. The 1099-B prints none, so it is NULL there.
- `Various` sets `acquired_various` and leaves the date NULL.
- The tax year is the document's, else the disposal year.
- The payload is the silver row's own, including a bond's adjusted
  basis on the Year-End Summary.

Instruments resolve the way the web transactions do: a ticker goes
through the symbol→CUSIP bridge, anything else stays as stored.

- The Year-End Summary keys a lot by CUSIP, the api's own key.
- The Gain/Loss Report keys a lot by ticker.
- Both key an option by its contract, as the statements print it.
- A 1099-B lot states only `security_name`. A plain ticker or an
  option contract is read as that key (`securityNameKey`). An option
  keeps its contract and does not resolve to its underlying: the lot
  and its gain are the contract's.
- A name of neither shape resolves to nothing. It becomes the
  `instrument_hint`, which a config link can close.

The `form_1099b` rows of the web `transactions` table do not enter
gold's transactions. A 1099-B lot is a tax record, not a cash
movement. It prints a closed short's opening premium as proceeds on
the closing date, and an order filled in several lots as several
sales. The statement and history feeds book each sale as cash once.
The `form_1099b` rows also stay out of the transaction range that
`Status` and `ChangeWindow` report, so they widen no load window.

## 9. Open questions

- **Tax withholding on dividends.** Schwab reports the withholding
  inside the `DIVIDEND_OR_INTEREST` payload's `transferItems`. The
  current mapping puts the dividend gross in `gross_amount` and net
  in `net_amount`; a separate `tax` row is not generated. Revisit
  if tax-lot work needs the withholding as a distinct event.
- **`open_orders` projection.** Reserved for a future `wealthdb
  orders` subcommand; no schema work needed in gold yet.
- **Cost-basis methods.** A Gain/Loss Report prints the account's
  relief method per asset class (`cost_basis_methods`, schwab-web
  DESIGN.md §9.3). Gold does not map it yet.
- **Explicit suffix→hashValue override config.** The bridge is
  auto-only. The exact tier (§7.1) resolves any realistic suffix
  collision once `account_number_full` is present, so an
  `account_bridge: {<api_hash>: <web_suffix>}` field on the
  schwab silver_source config is needed only if both tiers fail
  ambiguously — a suffix collision on an account whose payload
  carries no full number.
