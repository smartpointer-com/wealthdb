# synthetic adapter

Adapter that projects the `synthetic` silver SQLite database into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

`synthetic` is the one generic silver kind. Every other kind reads a
silver shaped by its source. This one reads a silver shaped by the
canonical model itself. There is one table per canonical record type.
A row is the change record it becomes, column for column.

No collector feeds it, and there is no bronze. A generator writes the
file. The demo household under [`demo/`](../../../demo) is one such
generator; the adapter's own tests are another. The adapter knows
nothing about any one generator. It passes rows through, with the
guards every adapter applies.

## 1. Silver source

- Silver schema:
  [testdata/silver_schema.sql](../../internal/silver/synthetic/testdata/silver_schema.sql).
  It is the schema's single definition. The generator creates its
  files from it, and the adapter tests create their fixtures from it.
- Conventions the schema fixes:
  - Money, quantities, prices and rates are decimal strings, never
    REAL.
  - Timestamps are INTEGER unix seconds UTC.
  - A snapshot is stamped at the UTC midnight of its day. It holds that
    day's closing state.
  - `payload` is a JSON object. It is `'{}'` when there is nothing to
    add.
  - Rows are append-only. A run adds the days after the previous run's
    as-of. Nothing older is rewritten.

## 2. Identifier conventions

Ids pass through unchanged. The adapter mints none and rewrites none.

| Gold field | Source | Example |
| --- | --- | --- |
| `portfolio_external_id` | `portfolios.portfolio_id` | `pf-main` |
| `account_external_id` | `accounts.account_id` | `acct-cash` |
| `instrument_external_id` | `instruments.instrument_id` | `inst-eq` |
| `position_key` | `positions.position_key` | `inst-eq` |
| `transaction_external_id` | `transactions.transaction_id` | `t-0001` |

A fact that names an account, a portfolio or an instrument must find
its row in the dimension table. A dangling id fails the load with an
error that names it. The silver is the canonical records themselves, so
a dangling id is a defect in whatever wrote the file. Loading around it
would leave gold holding a fact nothing describes.

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `meta` | none | Bookkeeping about the file. The adapter does not read it. |
| `dump_runs` | none | Drives `Status` and `ChangeWindow` only — see §8. |
| `portfolios` | `portfolios` | Emitted when an emitted account names it. |
| `accounts` | `accounts` | Taxonomy from the row — see §4. |
| `instruments` | `instruments` | One version per `valid_from` — see §6. |
| `positions` | `positions` | One snapshot batch per distinct `snapshot_at` — see §5. |
| `cash_balances` | `cash_balances` | In the same per-instant batches. |
| `fx_rates` | `fx_rates` | In the same per-instant batches, already in gold's direction. |
| `transactions` | `transactions` | Re-signed as a guard — see §7. |

## 4. Account taxonomy

All three taxonomy columns come from the `accounts` row. Nothing is
defaulted.

- **`account_kind`** is required. A value outside
  `canonical.AccountKind` becomes `other`. The raw value is kept as
  `payload.source_account_kind`.
- **`tax_wrapper`** is optional. NULL or empty stays absent. A value
  outside the vocabulary also becomes absent. The raw value is kept as
  `payload.source_tax_wrapper`.
- **`management_style`** works the same way. Its raw value is kept as
  `payload.source_management_style`.
- `display_name`, `base_currency`, `nickname`, `account_category` and
  `portfolio_id` pass through. An empty string is absent.

A deployment's `account_overrides` and `portfolio_overrides` still
apply on top, as for every source.

## 5. Positions

`Snapshots` emits one batch per distinct `snapshot_at` in the window,
in ascending order. The instants are the union over `positions`,
`cash_balances` and `fx_rates`. A batch carries that instant's
positions, cash balances and fx rates, complete.

A batch also carries the dimension records its facts reference, all
seen at that instant:

- every account its positions or cash balances name;
- the portfolio each of those accounts belongs to;
- every instrument its positions name, in the version in effect at the
  instant (§6).

Position columns pass through:

- The (`asset_class`, `vehicle`) pair is kept when
  `canonical.ValidTaxonomyPair` admits it. Otherwise it becomes
  (`other`, `other`). The raw values are kept as
  `payload.source_asset_class` and `payload.source_vehicle`.
- `quantity`, `market_value`, `book_value` and `accrued_interest` are
  optional decimals. An unparseable one is absent.
- A `book_value` follows one convention, which every writer of the
  kind keeps. The adapter stamps it by the pair (DESIGN.md §7.4):
  - a holding: its average cost, with no purchase fee:
    `stated`, `average`, `none`;
  - a `private_equity` `fund` or `spv`: the capital paid in, gross of
    the cash paid back: `stated`, `paid_in`, `none`;
  - `crypto`: its average cost, with the purchase fee left out and
    booked as a `fee` transaction of its own: `stated`, `average`,
    `excluded`.

  A row without a book value carries no stamp.
- `acquisition_date` is `YYYY-MM-DD`. It becomes that day's UTC
  midnight. An unparseable one is absent.

Cash balances and fx rates:

- A cash balance's `amount` is required. An unparseable one fails the
  load with an error that names the row.
- A `balance_kind` outside the vocabulary becomes `closing`. The raw
  value is kept as `payload.source_balance_kind`. When another row of
  the same account, currency and instant is already `closing`, the
  load fails with an error that names both kinds: gold keys a balance
  by its kind, so the two rows cannot both land.
- An fx rate's `mid_rate` is required, like a cash amount. `bid_rate`
  and `ask_rate` are optional.
- The fx direction is gold's: 1 `quote_currency` = `mid_rate`
  `base_currency`.

## 6. Instruments

An instrument can change its descriptive fields over time and keep its
id. A fund is renamed. A ticker changes. Each row of `instruments` is
one version. It applies from its `valid_from` until the next version's.

A reference at instant `t` gets the version with the greatest
`valid_from` at or before `t`. A reference earlier than every version
gets the earliest one. The instrument existed, and that is the oldest
description of it.

Gold keeps one row per instrument. The per-column upsert keeps the
newest non-NULL observation (DESIGN.md §8.4). So a load that reaches
past a rename ends with the renamed version.

The pair guard of §5 applies to every version.

## 7. Transactions

`Transactions` yields every row with `occurred_at` in the window. It
is one batch, ordered by (`occurred_at`, `transaction_id`).

**Signs.** Silver stores amounts with gold's canonical sign already,
from the account's own side (`canonical/sign.go`). The adapter still
routes `gross_amount` and `net_amount` through
`canonical.ApplyCanonicalSign`, as a guard:

- A kind with a fixed direction comes out signed that way. A `buy`
  stored positive comes out negative. A `card_payment` stored negative
  comes out positive.
- A kind whose sign depends on the event keeps the row's own. That is
  `interest`, `staking`, `capital_gain`, the FX family,
  `corporate_action`, `journal` and `other`.

**Fallbacks.**

- A `kind` outside `canonical.TxKind` becomes `other`. The raw value is
  kept as `payload.source_kind`. `status -v` counts the `other` rows per
  source, so the drift stays visible.
- The traded (`asset_class`, `vehicle`) pair is optional. Both empty is
  the ordinary case. One half alone passes when it belongs to its own
  vocabulary. A pair that fails the gold writer's gate becomes both
  empty, and the instrument's own pair answers. The raw values are kept
  as `payload.source_asset_class` and `payload.source_vehicle`.

**Pass-through.**

- `description`, `memo`, `counterparty` and `provider_category` pass
  through. An empty string is absent. The memo stays a field of its
  own. The gold writer joins it to the description.
- `instrument_hint` is kept only when `instrument_id` is NULL or empty.
  It is the token an instrument lookup failed on, so it means nothing
  beside a known instrument.
- `check_number` is kept only on an outflow (a negative net amount).
  Gold's contract is that the field names an outgoing payment.
- `payload` passes through verbatim, apart from the `source_*` keys a
  fallback adds. The keys gold reads from a transaction payload arrive
  untouched: `bank_ref`, `counter_account`, `counter_currency` and
  `counter_amount`.

**Dimensions.** Dimensions travel only on the snapshot stream. Every
account and instrument named by the window's transactions is emitted by
`Snapshots` too, whether or not a snapshot in the window also names it.
It goes on the last batch. Where the window holds transactions and no
snapshot, it gets a batch of its own. Its seen range is the span of
those transactions. An instrument comes in the version in effect at the
latest of them. The account's portfolio comes with it.

Gold's upsert widens a seen range and keeps the version seen last. So
the dimension rows gold ends with are the same however the load
windows are cut.

## 8. Change number and the incremental window

`LatestChangeNumber = MAX(dump_runs.change_number)`, or `-1` when
`dump_runs` is empty. `change_number` is strictly increasing across
runs.

A run's `[window_start, window_end]` (inclusive) bounds every
`snapshot_at` and `occurred_at` it added. That is the contract the
generator keeps.

`ChangeWindow(since)` is **incremental**. It reads the runs with
`change_number > since`:

- None: no changes. `NewChangeNumber` stays `since`.
- Otherwise: `Start = MIN(window_start)`, `End = MAX(window_end)`,
  `NewChangeNumber = MAX(change_number)`.

A fresh gold has watermark `-1` and takes every run at once. A later
load takes only the runs appended since. Gold's windowed delete then
touches only those days. An append run leaves everything older exactly
as the previous load wrote it.

This is the kind's point. The dump-driven kinds re-emit their whole
history on every load, because a dump re-reads everything. An
append-only silver has no such need.

`Snapshots` and `Transactions` read by time, not by run. Where two
runs' windows overlap, every row inside `[Start, End]` is still
re-emitted, whichever run added it.

A rewritten file whose change numbers restart lower is caught as
silver going backwards (DESIGN.md §8.5). A rewritten file whose latest
change number equals the one gold already read looks unchanged, and
gold does not load it. `wealthdb reset` clears the source for a fresh
load in both cases.

## 9. Returns policy

A generic kind cannot know which institution a source models. One
synthetic silver may stand for a bank, the next for a broker, a crypto
wallet or a book of private holdings.

So the kind registers the bank-style default:

- regime `flow_complete`;
- the bank external set: `deposit`, `withdrawal`, `transfer_in`,
  `transfer_out`, `journal`;
- the bank transfer-like set: `transfer_in`, `transfer_out`,
  `journal`;
- every other knob at its default.

A deployment states each source's own regime with config
`returns_policy_overrides` (DESIGN.md §5.6). Keys are source ids, so
two synthetic sources override independently.

```json
"returns_policy_overrides": {
    "demo-crypto":  { "flow_regime": "crypto_partial" },
    "demo-private": { "flow_regime": "nav_only" },
    "demo-bank":    { "accounts_grain": "hidden" }
}
```

## 10. Provider vocabulary

A synthetic writer may leave `provider_category` empty. When it files
a row, the value is a taxonomy value, and its family follows the row's
kind, not its sign:

- A kind the spending report reads carries a `spend_detailed` value:
  `purchase`, `refund`, `withdrawal`, `fee`, `tax`, and `interest` when
  it is negative. A refund is an inflow and still carries the value of
  the purchase it reverses.
- A kind the income report reads carries an `income_detailed` value:
  `deposit`, `dividend`, `coupon`, `staking`, `capital_gain`, `reward`,
  `distribution`, and `interest` when it is positive.

The spending provider tier translates it by identity
(`internal/spending/providermap.go`):

- The spending map holds every spending value of
  `canonical.SpendCategories`, each mapped to itself.
- The income map does the same for every income value.
- Both are built from the table at start-up. A value added to the
  taxonomy needs no edit here.

The vocabulary is **not categorical**:

- A value is the provider's verdict, not a coarse bucket. A catch-all
  it states, such as `BANK_FEES_OTHER_BANK_FEES`, claims the row. It
  is not deferred to the model tier.
- A value outside the taxonomy is not counted as vocabulary drift. It
  falls through untranslated, and later tiers place the row.
