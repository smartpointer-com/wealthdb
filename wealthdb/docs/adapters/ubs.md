# UBS adapter

Adapter that projects the `ubs-psn-dump` silver SQLite into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

## 1. Silver source

- Upstream: `ubs-psn-dump` repository.
- Silver schema: [ubs-psn-dump/migrations/0001_initial.sql](https://github.com/ptu/ubs-psn-dump/blob/main/migrations/0001_initial.sql).
- Silver README: [ubs-psn-dump/README.md](https://github.com/ptu/ubs-psn-dump/blob/main/README.md).

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
| `C` | `fund` (Collective investment) |
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

### MT535 SWIFT-tag parsing (deferred)

Each `holdings.payload` carries the raw MT535 fields under
`payload.fields.{"19A","93B","35B",...}`, where each tag is an
array of raw SWIFT subblock strings (e.g. `":HOLD//CHF12345.67"`).
Parsing those out to populate `PositionChange.Quantity` and
`MarketValue` is non-trivial and deferred to a follow-up. For
v1 the projected position rows have `quantity = NULL` and
`market_value = NULL`; the position's identity, account, asset
class, and currency are populated.

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

## 8. Open questions

- **MT568 vs MT566 collapsing.** Both carry corporate-action info;
  MT568 is narrative supplementing MT566. The adapter currently
  emits both as separate `corporate_action` events. Consider
  merging when MT566 and MT568 share an `event_external_id`.
- **`pending_securities` projection.** If a use case for T+2
  visibility lands, project into a new gold table
  `pending_transactions` rather than mixing with settled
  `transactions`.
- **`portfolios` as accounts.** UBS portfolios are reporting
  units, not holding containers. Modelling them as `accounts`
  with kind=`portfolio` is a pragmatic shortcut. Revisit if
  portfolio-level reporting needs differ from account-level.
