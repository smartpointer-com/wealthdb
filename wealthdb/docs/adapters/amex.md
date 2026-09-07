# Amex adapter

Adapter that projects the `amex` silver SQLite database into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

The source is an American Express card relationship, scoped to **credit
and charge cards**. A card carries no instrument, so the adapter emits
no positions and no instruments at all — every figure it projects is an
account, a cash balance, or a transaction.

It is the card half of the [chase](chase.md) adapter without the
deposit half, and simpler in three ways that all come from the source:
one stable transaction id shared by every channel (so no
content-derived id and no export join), a spend category on the modern
era's rows bar the ones Amex leaves uncategorised (so the spending
provider tier places those without the model, while the deep era's rows
carry none and fall to it), and no
running balance anywhere (so the balance history is monthly rather than
daily).

Requires **amex silver schema 1**. `load` applies the migrations in
place, so any silver a load has touched is at that version.

## 1. Silver source

- Upstream: [`collectors/amex`](../../../collectors/amex).
- Silver schema:
  [migrations/0001_initial.sql](../../../collectors/amex/migrations/0001_initial.sql).

The ledger has two eras, split at the **structured horizon** — 24
months, which is as far back as the activity JSON and every export
reach. Above it the rows come from the activity JSON; below it they can
only come from statement PDFs, which the provider offers for its full
~7-year retention. The eras differ in what they carry, and silver marks
each row's `source`.

## 2. Identifier conventions

| Gold field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | silver `accounts.account_external_id` | The provider's `accountKey`, a 32-hex opaque key. Not a card number; the displayed mask is a separate column and becomes the display fallback. |
| `transaction_external_id` | silver `transactions.txn_id` | Modern era: the provider's own stable 18-digit reference, which is ALSO the QFX `FITID` and (unquoted) the CSV `Reference` — unlike chase, no content-derived id is needed. Deep era: a synthetic positional id, below. |
| `instrument_external_id` | — | Never emitted. |

A **deep-era** row has no provider id to carry, because a statement
states none. Silver keys those rows `stmt:<account>:<period_end>:<index>`
— the period plus the row's position within it — which is deterministic
and stable across re-parses of the same document, but positional: a
parse that reads a period differently renumbers it.

A **pending** row's id is the exception: it is provisional and changes
when the charge posts. Silver replaces the pending set wholesale on
each load and gold re-emits the full window on every load, so the
provisional row is deleted by the same `deleteWindow` that re-applies
the posted one.

## 3. Accounts

One `AccountChange` per card, from its latest silver snapshot:

| Gold field | Value |
| --- | --- |
| `account_kind` | `card` — always. |
| `display_name` | the product's own name, falling back to the displayed mask. |
| `tax_wrapper` | `taxable_personal`. |
| `management_style` | `self_directed`. |

Both defaults are overridable through config `account_overrides`.

## 4. Cash balances — and the one sign flip

A card's outstanding balance is a **liability carried as negative
cash** (the margin-debit precedent, migration 0037). Silver holds every
figure the provider's way — the balance is the POSITIVE amount owed —
and `signedBalance` negates it. That is the **only** place the flip
happens, and every account this source emits is a card, so it is
unconditional.

A credit limit and available credit are not balances of anything owned
and never become rows.

Two kinds of mark feed the series, told apart by kind rather than by
date — they can fall on the same day without competing:

| Mark | When | From |
| --- | --- | --- |
| `CURRENT` | the source's latest load | the roster's live balance |
| `CLOSING` | each period's own `period_end` | that period's stated closing figure: printed on the statement for the archive, or the activity payload's cycle summary at the end of the window a run fetched |

**There is no per-day series**, because no Amex channel carries a
running balance on a card: not the activity JSON, not the CSV, not the
QFX. The balance history is therefore monthly — the density at which
the provider actually asserts it — and an as-of query between two
period ends returns the earlier one's closing balance. chase
reconstructs a daily series by rolling its ledger between the same
anchors; that is available here later if the granularity proves
insufficient, and is deliberately not guessed at now.

The `CURRENT` mark is stamped at the **source's** latest roster
snapshot, not the account's own — silver content-dedups an unchanged
roster row, and gold's `cash_chosen` keeps only the rows carrying the
source's single `MAX(snapshot_at)`, so a card stamped at its own would
drop out of the latest net-worth view the moment it stopped changing,
taking its debt with it.

## 5. Transactions

The whole card ledger. Silver already holds the fleet's card convention
— spend negative, anything reducing the balance owed positive — the
collector having negated Amex's own spend-positive figures at load.
Kinds are still routed through `ApplyCanonicalSign`, because each has a
fixed canonical direction.

**Kind mapping.** Two vocabularies share silver's `kind` column, one per
era.

**The modern era** (activity JSON) states a direction, read together with
the spend category:

| Row | Kind | Why |
| --- | --- | --- |
| `DEBIT`, categorised under fees | `fee` | The bucket Amex bills annual fees, late fees and interest into. |
| `DEBIT`, otherwise | `purchase` | |
| `CREDIT`, **no** category | `card_payment` | Amex leaves the monthly bill uncategorised — the same tell Chase has. |
| `CREDIT`, categorised | `refund` | A statement credit or a merchant refund, which nets against the spend it reverses. |
| no usable direction | `purchase` / `card_payment` by sign, `source_kind` in payload | Keeps the row on the side of the ledger silver already signed it onto; `other` reaches neither the spending base nor the matcher. |

The uncategorised-credit rule is what makes the whole source pay off
for spending (§7), and it inherits chase's stated cost: an
uncategorised merchant refund is kinded `card_payment` and leaves the
spending base. The converse mapping would mis-state every monthly bill
instead, which is the larger and more frequent error.

**The deep era** (statement PDFs) carries no category at all, so the
SECTION a row was printed under is the only thing separating its kinds —
which is why the collector stamps it:

| Section | Kind |
| --- | --- |
| `STMT_PAYMENT` | `card_payment` |
| `STMT_CREDIT` | `refund` |
| `STMT_PURCHASE` | `purchase` |
| `STMT_FEE` | `fee` |
| `STMT_INTEREST` | `interest` |

**Interest is not separated from fees in the modern era.** Amex bills it
into the same category, and guessing it from a descriptor would be a worse
answer than `fee`. The deep era is finer here — a statement states interest
as its own section — and the period's own figure is in silver's
`statement_balances.interest` either way.

`TxKindReward` has no producer. Amex marks the rows that EARN cash
back, on the purchases themselves, and a redemption arrives as an
ordinary categorised credit — there is no reward transaction to map.

`OccurredAt` is the **post** date in the modern era. The charge date
rides along in `payload.txn_date` rather than substituting for it — and a
statement-era row knows only the charge date and fills `posted_at` with
it, which `payload.posted_at_basis` records. The two eras therefore
disagree about what `posted_at` holds, which is why the charge date is
carried rather than swapped in.

## 6. Returns — nothing, by construction

The returns engine drops every `card` account at the loader
(`returnsInvisibleKind`), because a revolving-credit liability is a
spending instrument, not an investment: running its balance swings
through TWR/MWR would report shopping as performance. Since this source
emits only cards, it contributes **nothing to any return series at any
grain**.

The registered `ReturnsPolicy` (`returns.CardIssuerPolicy`) exists
because gold guards that every whitelisted silver kind declares one —
an unregistered kind falls through to a conservative default and is
reported as `unknown_adapter_policy` — and it declares exactly that.

Consequence, inherited from cards generally: money leaving a tracked
cash account to pay this card is a real outflow from the returns value
spine, because the spine omits the card. That is self-consistent, not a
gap.

## 7. Spending — where this source earns its keep

Two things follow from collecting the card, and both are the point of
building it:

1. **The modern era files its rows under the provider's own spend
   category**, which the provider tier translates without the model
   (`internal/spending/providermap.go`, `amexCardCategories`). Two
   rows it will not place, and neither is a gap: the monthly bill,
   which Amex leaves uncategorised — the tell §5's kind rule turns on —
   and Amex's own residual bucket, listed as `untranslatable`:
   reviewed, so not counted as drift, and left for the model tier,
   which is exactly the row the model exists for.
2. **The bills net out.** An Amex bill paid from a collected cash
   account is placed by the built-in card-payment rule as `card_spend`,
   a placeholder meaning "real consumption on a card this deployment
   does not itemise" ([../SPENDING.md](../SPENDING.md) §2). Once this
   source is loaded, the card's own `card_payment` leg exists and the
   internal-transfer matcher pairs the two, marking both
   `internal_transfer`. The matcher outranks the rule, so the
   placeholder disappears for this card and its actual purchases show
   instead.

   The pairing is **cross-source** here — the card is its own silver
   source, not a sibling product of the paying bank — which is the
   commoner arrangement and the one the placeholder was written for.
   `TestPassAmexBillPairsWithTheCollectedCard` pins it, and pins the
   uncategorised-credit mapping with it: kind the bill `refund` and the
   pair never forms.

   The `American Express` entry in the rule's issuer table needs no
   narrowing. It only LABELS the delta line; once the pair forms, the
   matcher's verdict replaces the rule's and the label with it.
