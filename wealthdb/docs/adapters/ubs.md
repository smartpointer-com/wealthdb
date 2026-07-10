# UBS adapter

Adapter that projects two UBS silver SQLite databases into the
canonical gold schema:

- `ubs-psn` — daily SDFI/MT-message feed delivered via SFTP.
- `ubs-web` — netbanking scrape (live snapshots + reconstructed
  historical PDFs).

Implements the `silver.Adapter` / `silver.Connection` interface
defined in [../DESIGN.md](../DESIGN.md) §6. When both subsources
are configured the orchestrator (`merge.go`) splices them: web
emits dimensions before PSN-start, PSN takes over from then on,
and PDF-reconstructed historical fills the pre-PSN-start range.

## 1. Silver sources

- Upstream feeds:
  - PSN: [`ubs-psn`](../../../collectors/ubs-psn/).
    Silver schema: [migrations/0001_initial.sql](../../../collectors/ubs-psn/migrations/0001_initial.sql).
  - Web: [`ubs-web`](../../../collectors/ubs-web/).
    Silver schema: [migrations/0001_initial.sql](../../../collectors/ubs-web/migrations/0001_initial.sql)
    + [migrations/0002_historical_snapshots.sql](../../../collectors/ubs-web/migrations/0002_historical_snapshots.sql).

## 2. Identifier conventions

UBS is the most dimensionally rich of the three silvers. Two
identifier dimensions show up in every gold row:

| Field | Source | Notes |
| --- | --- | --- |
| `relationship_id` | UBS Server ID (`SFTPCHxx`, `SFTPCHyy`, ...) | One per banking relationship under the single SFTP login. Stored on the gold `accounts` row as a discriminator. |
| `account_external_id` | IBAN for cash accounts; UBS 28-char safekeeping code for safekeeping accounts; `PrtflId` for portfolios | Whichever identifies the account uniquely within the relationship. |
| `instrument_external_id` | ISIN | UBS always supplies ISINs in SDFI. |
| `transaction_external_id` | UBS `:20C::SEME//` or MT940 `:61:` ref | Whatever the source MT message uses as its stable event reference. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Used by `Status` / `ChangeWindow`. |
| `account_holders` | — | Client/legal-owner metadata; not an account. Deferred. |
| `cash_accounts` | `accounts` (kind=`cash`) | IBAN as `account_external_id`. |
| `safekeeping_accounts` | `accounts` (kind=`safekeeping`) | UBS safekeeping code as `account_external_id`. |
| `portfolios` | `accounts` (kind=`portfolio`) | `PrtflId` as `account_external_id`. |
| `instruments` | `instruments` | See §4 for `asset_class` derivation. |
| `holdings` | `positions` | One securities holding per row. |
| `cash_balances` | `cash_balances` | Direct one-to-one; UBS `balance_kind` enum carries over. |
| `pending_securities` | — | Most rows are "no activity" markers (`ACTI//N`). Deferred. |
| `fx_rates` | `fx_rates` | Base currency is CHF in the current PSN setup. |
| `portfolio_performance` | — | Monthly TDPOPF analytics. Deferred. |
| `cash_account_pricing` | — | TDCAPI interest-rate config. Deferred. |
| `forward_contracts` | `positions` (asset_class=`fx_forward`) | `contract_external_id` is `position_key`. |
| `option_contracts` | `positions` (asset_class=`fx_option`) | Same. |
| `money_market_contracts` | `positions` (asset_class=`money_market`) | Same. |
| `otc_contracts` | `positions` (asset_class=`otc_derivative`) | Same. |
| `events` | `transactions` | See `kind` mapping in §5. |

## 4. `asset_class` derivation for `holdings`

`silver.holdings` doesn't carry an asset-class column directly.
The adapter looks up the holding's ISIN in `silver.instruments`
and reads the ISO 10962 CFI code (`InstrCtgyCFI` in the SDFI
payload). The CFI's first character is the asset category, so a
one-letter switch is enough:

| CFI first char | Gold `asset_class` |
| --- | --- |
| `E` | `equity` |
| `C` | `fund` (Collective investment); the `CE` group (ISO 10962:2015 ETFs) maps to `etf` and is then refined by underlying exposure from the instrument name (`silver.RefineETFClass` — crypto → `crypto`, bullion → `metal`, fixed income → `bond_etf`) |
| `D` | `bond` (Debt) |
| `O` | `option` |
| `F` | `future` |
| `M` | `money_market` |
| other / empty | `other` (e.g. `T` structured, `R` rights, or instruments UBS ships without a CFI code) |

### Latest-known-instruments lookup

UBS silver's loader applies content-based dedup on instruments —
a fresh row is written only when the SDFI payload changes. Most
snapshots therefore don't carry an instruments row for a given
ISIN, so a same-snapshot lookup misses for most holdings. The
adapter compensates by building a `map[isin]meta` from the
**most-recent** instrument row across the entire silver DB, then
using that for every holding. This is correct in practice
because instrument metadata (name, CFI category) is functionally
immutable — being stale by one snapshot is harmless. Discovered
empirically during M7's load against real silver; documented here
so the next maintainer doesn't undo it.

### MT535 SWIFT-tag parsing (implemented)

Each `holdings.payload` carries the raw MT535 fields under
`payload.fields.{"19A","93B","35B",...}`, where each tag is an
array of raw SWIFT subblock strings. `mt535.go` decodes the two
tags we surface in gold:

- **`93B`** — quantity. Subfield format
  `:<qualifier>//<format>/<value>` where qualifier is one of
  `AGGR` (aggregate), `AVAI` (available), `NAVL` (not available),
  `AWAS` (awaiting settlement), etc.; format is `UNIT` (shares /
  contracts) or `FAMT` (face amount, for bonds). The adapter
  prefers `AGGR`, falling back to `AVAI` if missing.

- **`19A`** — monetary amount. Subfield format
  `:<qualifier>//<CCY><value>` where qualifier is `HOLD` (current
  market value), `BOOK` (book / cost basis), `ACRU` (accrued
  interest), and similar. A single holding typically carries
  multiple `19A` entries — the same `HOLD` value in the
  position's trade currency and again in the relationship's
  reference currency (CHF). The adapter prefers the `HOLD` entry
  whose currency matches the instrument's natural currency,
  falling back to the first `HOLD` entry if no exact match
  exists.

SWIFT value convention: comma is the decimal separator, and a
trailing comma is the end-of-amount terminator (so `1500000,`
parses to `1500000` and `150,123456` parses to `150.123456`).
`parseSwiftDecimal` handles both forms.

Holdings whose `payload.fields` is empty or whose 19A/93B
subfields don't match the expected shape leave the gold columns
NULL — the parser degrades gracefully so a single malformed
payload doesn't fail the whole load.

## 5. `events.kind` mapping

UBS's `silver.events.kind` discriminator (per the comment block
in `silver.events`) maps to gold's canonical `kind` taxonomy:

| UBS | Gold | Adapter notes |
| --- | --- | --- |
| `cash_movement` | `deposit` / `withdrawal` / `fee` / `interest` / `tax` | from MT940 `:86:` narrative (adapter splits — see §6) |
| `securities_movement` | `transfer_in` / `transfer_out` | sign-driven |
| `trade_confirmation` | `buy` or `sell` | from MT515 payload `side` |
| `fx_confirmation` | `fx_spot` | MT300 |
| `fx_option_confirmation` | `fx_option` (settlement) | MT305 |
| `loan_deposit_confirmation` | `money_market` (settlement) | MT320/MT330/MT350 |
| `corporate_action_notification` | `corporate_action` | MT564 |
| `corporate_action_confirmation` | `corporate_action` | MT566 |
| `corporate_action_narrative` | `corporate_action` | MT568 (narrative; may collapse with the MT566 row) |
| `securities_settlement_advice` | `transfer_in` / `transfer_out` | MT544–548 |
| `precious_metal_trade` | `buy` or `sell` | MT600/MT601 |
| `charges_advice` | `fee` | MT590/MT990 |
| `debit_credit_confirmation` | `deposit` / `withdrawal` | MT900/MT910 |

## 6. MT940 `:86:` narrative parsing

`cash_movement` events (MT940 `:61:` lines) carry a free-text
narrative in the `:86:` continuation. The adapter parses this to
split bare `cash_movement` into more specific canonical kinds.
The narrative parser is conservative — unmapped narratives fall
through to `deposit` or `withdrawal` based on the amount sign,
with the raw narrative preserved in payload.

Common narrative prefixes (extend as observed):

| Narrative prefix | Gold `kind` |
| --- | --- |
| `INT` / `INTERETS` / `ZINSEN` | `interest` |
| `COMM` / `FRAIS` / `GEBUEHREN` | `fee` |
| `IMP` / `IMPOT` / `STEUER` | `tax` |
| `DIV` | `dividend` |
| (other) | `deposit` / `withdrawal` per sign |

## 7. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty.

## 8. Historical (PDF-reconstructed) data — ubs-web migration 0002

`ubs-web` migration 0002 added two parallel tables built
from the customer's eDocuments PDF archive:

| Silver table | Source PDF | Cadence | Gold target |
| --- | --- | --- | --- |
| `historical_position_snapshots` | "Statement of Assets" | Quarterly | `positions` (securities) — cash rows skipped, see below |
| `historical_cash_balances` | "Account Statement" | Monthly | `cash_balances` (opening + closing) |

These predate the PSN feed's go-live and complement the intra-day
live web positions. The adapter emits them as a separate stream
(`webReader.snapshotsHistorical`) that runs before the live
overlap stream and PSN stream so chronologically the gold
`positions` and `cash_balances` tables fill in oldest-first.

### Security positions

`historical_position_snapshots` rows where `instrument_isin IS NOT
NULL`. UBS doesn't surface the safekeeping account reliably in
the PDF text, so silver leaves `account_external_id = ''`. The
adapter attaches each security position to the per-portfolio
overlay account (`'<portfolio>:overlay'`, `account_kind=overlay`)
that PSN already uses for forward contracts — preserving the
invariant that every gold `positions` row is owned by an
`accounts` row.

| Silver column | Gold mapping |
| --- | --- |
| `as_of_date` | `positions.snapshot_at` |
| `portfolio_external_id` | `accounts.portfolio_external_id` (BBBBAAAAAAAANN form, 16 chars including the leading-zero branch prefix, matches PSN) |
| `instrument_isin` | `positions.instrument_external_id`, `instruments.isin` |
| `currency_iso` | `instruments.currency` |
| `units` | `positions.quantity` |
| `market_value` | `positions.market_value` (in `market_value_currency`, typically portfolio base) |
| `cost_price * units` | `positions.book_value` |
| `accrued_interest` | `positions.accrued_interest` |
| `description` | `instruments.name` |
| `sector` | (kept in payload only) |

`asset_class` defaults to `other` for historical rows — the PDFs
don't carry a CFI code. The per-column upsert guard means a later
PSN snapshot containing the same ISIN will overwrite `asset_class`
with the CFI-derived value, so the `other` is only ever the
visible value for instruments that never made it into PSN.

### Cash balances

`historical_cash_balances` is the richer monthly source.
`historical_position_snapshots` cash rows (instrument_isin NULL)
are **skipped** to avoid colliding with the cash_balances PK —
their quarter-end timestamps would land on the same
`(account, currency, balance_kind)` tuple as the matching month-
end row from `historical_cash_balances`.

| Silver column | Gold mapping |
| --- | --- |
| `period_start`, `opening_balance` | `cash_balances` row, `balance_kind=opening`, `snapshot_at=period_start` |
| `period_end`, `closing_balance` | `cash_balances` row, `balance_kind=closing`, `snapshot_at=period_end` |
| `account_external_id` | `cash_balances.account_external_id`; also emits an `accounts` row (`kind=cash`) once per period |
| `total_debits`, `total_credits` | kept in payload, not projected |

Each row produces zero, one, or two `cash_balances` writes
depending on which of `opening_balance` / `closing_balance` are
non-NULL. Rows with NULL on a side (closed-account statements,
mid-period exports that haven't crystallised yet) skip that
side rather than coercing to 0.0 — the silver loader leaves NULL
distinguishable from a real zero-flow month.

### Window semantics

The webReader's `ChangeWindow` extends `Start` back to
`MIN(historical times)` whenever there's any new live content (a
new `dump_runs` row). This guarantees the loader's window-DELETE
covers any existing historical gold rows before they're
re-inserted, so a reload never collides on the gold PK. The
`NewChangeNumber` stays a live-time concept — when no new live
content has arrived, the load is a no-op even with historical
present in silver.

## 9. Open questions

- **MT568 vs MT566 collapsing.** Both carry corporate-action info;
  MT568 is narrative supplementing MT566. The adapter currently
  emits both as separate `corporate_action` events. Consider
  merging when MT566 and MT568 share an `event_external_id`.
- **`pending_securities` projection.** If a use case for T+2
  visibility lands, project into a new gold table
  `pending_transactions` rather than mixing with settled
  `transactions`.
- **Historical asset_class.** Historical security positions
  default to `asset_class=other` because the PDFs don't carry a
  CFI code. When PSN data exists for an ISIN the per-column
  upsert backfills with the CFI-derived value, but instruments
  that pre-date PSN (closed positions, instruments since
  delisted) stay `other`. Consider a per-ISIN asset-class lookup
  populated from an external catalogue if richer historical
  classification is needed.
