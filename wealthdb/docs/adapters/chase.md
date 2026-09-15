# Chase adapter

Adapter that projects the `chase` silver SQLite database into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

The source is a JPMorgan Chase retail relationship. The adapter
handles two kinds of account: **deposit accounts** (checking /
savings) and **credit cards**. Both land in the same silver tables and are told
apart by `accounts.product` (`dda` | `card`). Neither carries an
instrument, so the adapter emits no positions and no instruments at
all — every figure it projects is an account, a cash balance, or a
transaction.

Requires **chase silver schema 3** (cards, plus their
statement-coverage flag). `load` applies the migrations in place,
so any silver a load has touched is at that version.

## 1. Silver source

- Upstream: [`collectors/chase`](../../../collectors/chase).
- Silver schema:
  [migrations/0001_initial.sql](../../../collectors/chase/migrations/0001_initial.sql)
  + [0002_cards.sql](../../../collectors/chase/migrations/0002_cards.sql)
  + [0003_statement_coverage.sql](../../../collectors/chase/migrations/0003_statement_coverage.sql).

Each product has two eras, split at its **export seam** — the
oldest row the CSV/QFX exports reach. At and above the seam the
rows come from the exports; below it they are parsed out of
statement PDFs. The eras differ in what they carry (ids, kinds,
balances), and most of the adapter's shape follows from that.

## 2. Identifier conventions

| Gold field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | silver `accounts.account_external_id` | The provider's own account key; the last-4 mask is a separate column and becomes the display fallback. |
| `transaction_external_id` | silver `transactions.fitid` | **Not** the provider's OFX FITID, despite the column name — see below. |
| `instrument_external_id` | — | Never emitted. |

Silver's `fitid` is a **content-derived, occurrence-indexed key**:
`<product>:<account>:<content key>:<occurrence>` for an export row,
and a statement-period-scoped hash for a statement row. The
provider's OFX `<FITID>` is carried in `payload.fitid` for
traceability only. Two reasons it is not the identity: only the QFX
export carries a FITID and each export format is fetched
separately (so keying on it would give the same rows a second
identity in a run that landed only the CSV), and on a card it is
not unique even within one export — a credit that offsets a charge
is issued the same FITID as the charge it reverses.

Ids converge across re-loads but are **not durable external
references**: a re-priced row is new content and gets a new id.

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Drive `Status` / `ChangeWindow`. |
| `accounts` (`product='dda'`) | `accounts` (kind=`cash`) | Plus a CURRENT `cash_balances` row — §5. |
| `accounts` (`product='card'`) | `accounts` (kind=`card`) | Plus a CURRENT `cash_balances` row, negated — §4, §5. |
| `transactions` | `transactions` | Both products; per-day CLOSING `cash_balances` from the `balance` column — §5, §6. |
| `statement_balances` | `cash_balances` (CLOSING) | Card only, and only for the periods the balance reconstruction does not reach — §5. |
| `documents` | — | Statement inventory (dates + hashes), no parsed figures. The balance series already carries every statement date, so it adds nothing. |
| `accounts.pending_charges` | — | Unposted card activity, already inside the roster balance. Kept in the account payload; never its own row. |

## 4. Account taxonomy

- **`account_kind`** is `cash` for a deposit account and `card`
  for a credit card. A `card` is a revolving-credit liability
  (gold migration 0037): unlike a `mortgage` it carries **no
  position** — its outstanding balance is negative cash on the
  account, the margin-debit precedent.
- **`management_style`** is always `self_directed`.
- **`tax_wrapper`** defaults to `taxable_personal`.
- **`display_name`** is the nickname, falling back to the last-4
  mask. `nickname` is carried separately.

All four are overridable per account via the `account_overrides`
block in `wealthdb.cfg`.

### The liability sign

Silver stores every card figure **provider-verbatim**: the
account's `balance` is the **positive amount owed**, and the ledger
is signed the same way (spend negative, anything reducing the
balance owed positive). Silver never normalises a liability.

**Negating it is this adapter's job**, and it happens in exactly
one place (`signedBalance` in `snapshots.go`). A card balance of
`1234.56` owed projects as `-1234.56`. Nothing downstream re-flips
it.

Available credit and the credit limit are **not** balances of
anything owned and never become `cash_balances` rows. Neither does
`pending_charges`, which is already inside the roster balance.

## 5. Cash balances

Cash is modelled as `cash_balances`, never a position or an
instrument — and so is a card's debt. Gold's `report_cash` macro
synthesises the read-time cash position from these rows, so they
still surface in holdings.

Three sources feed the series. They are **mutually exclusive by
construction**, so no account ever gets two marks on one day:

| Source | Kind | Stamped at | `payload.basis` | Applies to |
| --- | --- | --- | --- | --- |
| The roster's live figure | `current` | The source's latest roster snapshot | `roster` | Both products |
| The ledger's running balance | `closing` | Each day the balance moved | `running_balance` | Both products |
| A statement's printed closing figure | `closing` | `period_end` | `statement_closing` | Cards, uncovered periods only |

### CURRENT is stamped at the source's latest snapshot

Every account's CURRENT balance carries
`MAX(accounts.snapshot_at)` **across the source**, not the
account's own latest snapshot.

Silver content-dedups an unchanged roster row, so a quiet
account's own `MAX(snapshot_at)` falls behind the source's. Gold's
`cash_chosen` keeps only the rows carrying the source's single
`MAX(snapshot_at)`, so an account stamped at its own would drop
out of the latest net-worth view the moment it stopped
changing — silently, and a card would take its debt with it. The
figure is still that account's latest **known** balance; the stamp
says "as of this load", which is exactly what an unchanged roster
row asserts.

### The running balance is the historic series

One CLOSING mark per day the balance moved, valued at that day's
end-of-day running balance from the ledger. On a **deposit**
account this, not the statements, is the source of the historic
marks: the running balance carries a figure at every date, whereas
the collector records statement dates without their parsed
balances. The statement dates are a subset of these, so an as-of
query at any statement date returns that statement's closing
balance.

On a **card** the same column carries the loader's
*reconstruction* — neither card export has a running-balance
column, so the loader rolls the posted ledger between the statement
closing anchors. Two consequences the adapter has to respect: a
span that fails to land on its anchor keeps **no** balance at all,
and the statement era carries none per row by design (a statement
dates its rows by transaction date while the cycle bills by post
date, so the two eras' chains contradict each other).

`posted_at` is day-granular, so a day with several transactions
has one end-of-day balance and the `fitid`-highest row of the day
is taken as its representative. On a card that is **exact** — the
reconstruction rolls forward in `(posted_at, fitid)` order, so the
`fitid`-highest row of a day is the one carrying its end-of-day
balance. On a deposit account the balance is the provider's own
column and the ledger has no intra-day order, so the pick is
arbitrary among that day's rows; the imprecision is bounded by
same-day activity and resolves by the next day.

### Statement closings fill what the reconstruction cannot reach

`statement_balances` holds each card billing period's printed
opening and closing figures — the only place a historic card
balance is stated. The adapter emits the **closing** figure at
`period_end`, negated, for a period where **no reconstructed
balance falls anywhere inside `[period_start, period_end]`**.

That guard is what keeps this source and the running-balance
series apart: `period_end` is inside its own period, so a same-day
collision is impossible. Suppressing the whole period rather than
just the day is also the right reading — the reconstruction is
anchored *to* these closing figures, so wherever it runs it
already carries them, at balance-move density instead of monthly.
In practice the split falls out as: statement era → period
anchors; export era → the per-day reconstruction.

`transactions_covered` is deliberately **not** consulted. It
records whether a period's *transactions* reached silver; the
printed closing balance is asserted by the statement either way,
and a period whose rows were refused is precisely one that needs
its anchor.

## 6. `transactions.kind` mapping

The ledger of both products is projected in full.

**Deposit rows** are classified from the OFX `TRNTYPE` / CSV
`Type` plus the sign, which is the faithful reading for a cash
account with no richer categorisation:

| Silver `kind` | Gold `kind` |
| --- | --- |
| `INT`, `*INTEREST*` | `interest` |
| `SRVCHG`, `*FEE*` | `fee` |
| (anything else, amount < 0) | `withdrawal` |
| (anything else, amount ≥ 0) | `deposit` |

**Card rows** carry one of two vocabularies in the same column:
the CSV export's `Type` above the seam, and below it the statement
section the row was printed under.

| Silver `kind` | Gold `kind` | Notes |
| --- | --- | --- |
| `Sale` | `purchase` | |
| `Return` | `refund` | |
| `Payment` | `card_payment` | |
| `Adjustment` | `refund` / `purchase` | Follows the row's sign; see below. |
| `Fee` | `fee` | |
| `STMT_PURCHASE` | `purchase` | Statement era |
| `STMT_PAYMENT` | `card_payment` | Statement era |
| `STMT_FEE` | `fee` | Statement era |
| `STMT_INTEREST` | `interest` | Statement era |
| (unrecognised) | `other` | Raw type preserved in `payload.source_kind`. |

Two decisions of record:

- **`Adjustment` follows the row's sign, and is never `other`.**
  An adjustment carries the issuer's own sign and goes both ways: a
  credit one offsets prior spend (a goodwill credit, a disputed
  charge reversed, rewards cashed out against the balance) and nets
  as a `refund`; a debit one re-bills it (a dispute decided the
  other way, a goodwill credit withdrawn) and stays spend as a
  `purchase`. Both keep net spend correct inside the spending base;
  `other` in either direction would drop the row out of that base
  altogether and misstate what was spent, and a kind pinned to one
  direction would flip the other.
- **`reward` has no producer here.** Chase issues no reward
  transaction — a redemption surfaces as an `Adjustment` like any
  other credit — so the kind is left unproduced rather than
  guessed at from descriptors.

### Signs, dates and enrichment

- **Deposit** amounts are already signed the canonical way
  (positive = balance increase), so the source sign is preserved
  rather than forced; summing `net_amount` reproduces the
  account's net cash flow.
- **Card** amounts agree with the canonical convention for a
  liability carried as negative cash, but are still routed through
  `canonical.ApplyCanonicalSign`, because the card kinds have a
  fixed canonical direction: a `purchase` comes out negative and a
  `refund` / `card_payment` positive whatever the row said.
  `other` and `interest` have no fixed direction and keep the
  source sign.
- **`occurred_at` is the post date** for both products — the date
  the account's balance moved, which is what the balance series is
  keyed on. A card row's transaction date rides in
  `payload.txn_date` as `YYYY-MM-DD` rather than replacing it; the
  two eras disagree on which date `posted_at` holds (the statement
  era has only the transaction date and fills `posted_at` with
  it).
- **`counterparty`** is the card row's merchant, **verbatim**. It
  is the input to gold's merchant signature, so any reformatting
  here would re-key every merchant it touched.
- **`check_number`** is the cheque number, verbatim, on **outflows only** and
  on the deposit ledger only — a card cannot be drawn on by cheque. Silver fills
  its column from the QFX `CHECKNUM` field and from a deposit export whose
  header reads `Check or Slip #`, so on an inflow the same column can hold a
  deposit *slip* number; the adapter's sign test drops that. The statement-era
  rows carry the number inside `description` and not in the column, so they
  reach gold with `check_number` unset until the statement parser learns to
  extract it.
- **`provider_category`** is the provider's own category,
  verbatim and un-normalised — free text, not an enum. Empty on
  payments, which the provider leaves uncategorised.
- **`currency`** comes from the row when silver carries one
  (the landing spot for a foreign-currency card row) and from the
  account otherwise, defaulting to USD.

## 7. Returns treatment

The source registers `returns.DepositBankPolicy()`
(`internal/silver/chase/policy.go`): regime `flow_complete`, the
standard bank external-capital set, and `AccountsGrainHidden`.

`AccountsGrainHidden` governs the **deposit** accounts: they emit
no per-account return rows at any grain — a deposit account is pure
cash plumbing whose rows are noise (a drained-then-refunded account
chains a permanent −100%) — while their balances and flows still
enter every aggregate, where transfer legs against tracked sources
cancel.

A **card** is excluded more strongly than that, and not by this
policy at all: `account_kind='card'` is returns-invisible
engine-wide — no value, no flow, no row, at any grain
(`appendSeries` in `internal/gold/returns.go`; see
[../RETURNS-NOTES.md](../RETURNS-NOTES.md), "Credit cards are
returns-invisible"). Unlike a hidden deposit account, a card's
balance does not reach the returns value spine either. The card
transaction kinds (`purchase` / `refund` / `card_payment` /
`reward`) sit outside the policy's external-capital set as well, so
card activity would not read as owner capital even if it were
visible.

**A card payment crosses two accounts of the same source, and the
legs do not cancel.** The deposit leg is a `withdrawal` — external
capital under the bank set — while the card leg does not exist in
returns at all. That is the settled answer rather than a gap: the
money left the returns-visible system for a liability the engine
cannot see, so it is capital out, not an internal transfer.

## 8. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)` — the load
clock, or `-1` if `dump_runs` is empty. Each bronze dump loaded
bumps it and a subsequent `wealthdb load` re-emits; an idle reload
is a no-op.

When it triggers, `ChangeWindow` re-emits the **full** history:
`Start`/`End` span every date the projection touches, so gold's
`deleteWindow` over `[Start,End]` makes the re-emit idempotent.
That span is the MIN/MAX over three columns —
`accounts.snapshot_at`, `transactions.posted_at`, and
`statement_balances.period_end`.

The third is required for correctness, not merely for `Status` to
read well: a statement period with no ledger row of its own — a
quiet month, or one whose transactions were refused — still emits
a closing balance at its `period_end`, and a record outside the
delete window is re-inserted without its predecessor being
removed, so it would duplicate on every load. `period_start` is
not included: nothing is emitted at it, and `period_end` already
bounds every row that table produces.

## 9. Open questions

- **A second card vocabulary.** The kind map covers the CSV types
  and statement sections this build translates; a cash advance or a
  balance transfer would land as `other` with its raw type in the
  payload until mapped.
