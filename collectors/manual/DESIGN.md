# manual — design notes

**Scaffold stage (2026-06-10).** `load.py` is implemented and verified
against the synthetic [examples/](examples/) (`tests/`). The bronze/silver
design and the gold **mapping** are signed off (§6); the **gold adapter
itself is not built yet**, blocked only by the gold code freeze (don't touch
`wealthdb/` adapters/enums/migrations while the concurrent AngelList/Carta
work is in flight) — not by any open design question.

A catch-all collector for **private holdings with no source UI at all** —
the bank/portal sources are all covered by the other twelve collectors;
what's left is illiquid private holdings tracked by hand:

- **Real estate** — directly-held residential / commercial property.
- **Convertible notes (CLAs)** — early-stage venture investments into private
  companies. Legally convertible loan agreements, but economically venture
  bets: typically **0% interest**, expected to convert to equity at the next
  funding round or be written to zero. Not debt in any meaningful sense.
- **Direct private-company equity** — e.g. a stake in a GmbH / AG.
- **Fund LP interests & SPVs** — limited-partner commitments in a venture/PE
  fund (capital calls + distributions) and single-deal SPVs.
- **Other illiquid positions** — anything without a cleaner home, e.g. a
  receivable or a private loan.

Volume is a few transactions per year, valuations updated occasionally.
The design goal is to be the **simplest collector in the repo**.

Sections:

1. [The unique shape — no source](#1-the-unique-shape--no-source)
2. [Bronze: the CSV schema](#2-bronze-the-csv-schema)
3. [Silver schema](#3-silver-schema)
4. [Why SQLite](#4-why-sqlite)
5. [Validation](#5-validation)
6. [Gold mapping (signed off — adapter pending the freeze)](#6-gold-mapping-signed-off--adapter-pending-the-freeze)
7. [Status of the decisions](#7-status-of-the-decisions)

## 1. The unique shape — no source

Every other collector has the lifecycle `login → download → load`, driving
a privileged read-only session against a source. **`manual` has no source.**
Three CSVs are maintained in `~/wealthdb/manual/`; there is no auth, no
MFA, no Docker, no browser, no `~/.secrets/manual.env`. Only `load` exists
(the `manual` wrapper accepts `download`/`login` as friendly no-ops). The
runtime is **host-venv** (like `schwab-api` / `ubs-psn`) minus the network
half — there is no third-party runtime dependency at all (SQLite is stdlib;
the only install is the shared `collectorkit`, editable). CSV parsing +
validation use the stdlib so the loader reports precise `file:row:column`
errors.

Because the CSVs *are* the source of truth (not a captured snapshot of a
remote state), `load` **fully rebuilds** silver from the current CSVs each
run, inside one transaction. Same CSVs in ⇒ same silver out. There are no
timestamped bronze run-dirs and no per-dump idempotency gate — the model
that fits a remote source that emits successive dumps doesn't apply to a
hand-edited spreadsheet.

## 2. Bronze: the CSV schema

Three CSVs, one row per thing. The **stable columns are the same across all
asset kinds**; everything kind-specific lives in a JSON `payload` column —
not sparse per-kind columns. This is the repo's silver convention (promote
stable filter/join fields, absorb drift in `payload`) applied one layer
earlier, at bronze, because here bronze *is* a hand-typed schema.
One file across kinds (a `kind` discriminator + `payload`), never a file per
kind, so adding another asset kind (a crypto cold-wallet, a collectible)
needs no new file and no schema change.

**positions.csv** — one row per held asset.
`id, kind, display_name, currency, acquired_at, closed_at, notes, payload`
- `kind` ∈ `real_estate | private_equity | convertible_note | private_fund`
  (extensible — lives in `load.py`'s `POSITION_KINDS`, not a DB constraint).
  The kind is **deliberately identical to the canonical gold `asset_class`**
  (so the gold classmap is an identity and the CSV self-documents the class).
- `closed_at` (nullable) — set when the asset stops existing (full disposal,
  or a convertible note that converted). Explicit rather than inferred from
  transactions, so position-liveness doesn't depend on transaction parsing;
  gold drops the position from as-of queries after this date.
- `payload` — e.g. real_estate `{property_type, ownership_pct, city,
  country}`; convertible_note `{principal, interest_rate, cap, maturity_date,
  conversion_terms, counterparty}` (`interest_rate` is 0 for these venture
  notes; the principal is also captured as the `acquisition` transaction +
  the cost-basis valuation); private_equity `{ownership_pct, share_cnt,
  fiduciary, converted_from_position_id?}`; private_fund `{role, commitment}`
  (an LP interest — capital calls are `contribution` txns, distributions are
  `distribution` txns).

**valuations.csv** — the periodic mark-to-market series.
`position_id, as_of_date, value, currency, notes, payload`
- One row per (position, as-of date). This is the **per-date valuation
  series** — borrowed from carta's per-date valuation-override pattern
  (carta DESIGN §5.1), but first-class here rather than a side-loaded
  override. The value as of a date D is the latest row with `as_of_date ≤ D`;
  gold forward-fills. Lets an illiquid asset's value move correctly over
  time (a property re-appraised every few years, a convertible note re-marked at a round).
- `payload` — kind-specific provenance, e.g. real_estate
  `{appraisal_source}`, convertible_note `{mark_source}`, spv/private_fund
  `{post_money_valuation}` / NAV provenance.

**transactions.csv** — dated cash flows / position-level events.
`id, position_id, occurred_at, kind, amount, currency, notes, payload`
- `kind` ∈ `acquisition | disposal | contribution | distribution | fee |
  conversion`.
- `amount` is a **positive magnitude**; canonical direction comes from
  `kind` (gold applies the sign — mirrors how carta stores its cash-flow
  ledger as positive magnitudes and lets `ApplyCanonicalSign` pin direction).
- `payload` — counterparty / transfer-reference / funding-account / etc.
  For `conversion`: `{converts_to_position_id}`.

**Conversion (convertible_note → equity)** is modelled as a **position close
+ position open, linked both ways**:
- the convertible_note position gets `closed_at` = the conversion date;
- a new `private_equity` position is opened with `acquired_at` = that date and
  `payload.converted_from_position_id` = the note's id;
- a single `conversion` transaction on the note carries the converted basis
  (the principal — these notes are 0%, so there's no accrued interest) as
  `amount` and `payload.converts_to_position_id` = the new equity position.
  It exists for provenance — **no external cash moves** at conversion. `load`
  enforces the link (a `conversion` with no / dangling target fails). The
  exact gold treatment is **deferred** (no real conversion has happened yet —
  §6/§7); the provisional plan is a sign-neutral `TxKindOther`, not a cash
  leg.

## 3. Silver schema

SQLite + JSON1, mirroring the bronze CSV shape one-to-one. Realized in
[migrations/0001_initial.sql](migrations/0001_initial.sql).

| Table | Grain | Notes |
|---|---|---|
| `positions` | `id` | `kind`, `display_name`, `currency`, `acquired_at`, `closed_at` (NULL = held), `notes`, `payload`. |
| `valuations` | (`position_id`, `as_of_date`) | `value`, `currency`, `notes`, `payload`. The per-date mark series. |
| `transactions` | `id` | `position_id`, `occurred_at`, `kind`, `amount` (positive magnitude), `currency`, `notes`, `payload`. |
| `load_runs` | — (append-only) | Audit log: `load_at`, schema version, `bronze_dir`, row counts, `payload`. No idempotency gate (full rebuild each run). |
| `schema_meta` | `silver_schema_version` | collectorkit migration bookkeeping. |

Storage (carta conventions): money (`value`, `amount`) is a decimal STRING
(TEXT) verbatim — exact, no float rounding; dates are ISO `'YYYY-MM-DD'`
TEXT; `payload` is TEXT JSON; `load_at` is Unix-seconds INTEGER. Rates /
ownership_pct / other ratios stay inside `payload`, not money columns.
Referential integrity (a valuation/transaction's `position_id`, a
conversion's target) is enforced in `load.py` (precise errors), **not** by
SQLite FK constraints — the loader truncates and rebuilds all three tables
each run, so hard FKs would only complicate delete/insert ordering. The
accepted `kind` vocabularies live in `load.py` so adding a kind needs no
migration.

## 4. Why SQLite

SQLite + JSON1 — the repo default. The one prior DuckDB exception
(`cointracking`) was driven by a window-function holdings replay and
`DECIMAL(38,18)` crypto amounts; **neither applies here** — this is a tiny
shape transformation, a few rows a year, no computation and no
arbitrary-precision need. DuckDB is for high-volume / complex-query stores,
which this is not. (The Phase-0 brief sketched a DuckDB silver; that was
overridden to stay on the documented default — same call carta made.) Money
is stored as decimal STRINGS (TEXT) verbatim to avoid float rounding, dates
as ISO TEXT, `payload` as TEXT JSON — exactly the carta silver conventions,
so the gold adapter reads it with the same `modernc.org/sqlite` driver every
other SQLite-silver adapter uses.

## 5. Validation

The collector's whole value is that hand-entered data is **checked**, since
there's no source system enforcing anything. `load` fails the entire load
(one transaction → rollback; non-zero exit; `file:row:column` message) on:
duplicate id; unknown `kind`; unparseable date / non-3-letter currency /
non-numeric or negative `amount`; a `value`/`amount` currency that disagrees
with the position's; a `valuations`/`transactions` `position_id` absent from
`positions.csv`; malformed or non-object JSON `payload`; a `conversion` with
a missing/dangling target; an unexpected (typo'd) column. It **warns** but
loads when a valuation/transaction predates the position's `acquired_at`.

## 6. Gold mapping (signed off — adapter pending the freeze)

The mapping below is **signed off by the user (2026-06-10)**; the gold
adapter (`wealthdb/internal/silver/manual/`) is not built yet only because of
the gold code freeze. It follows the carta adapter's structure.

**Account grain + taxonomy — DECIDED.** ONE gold account for the whole
`manual` source, every position under it (carta's one-account-per-portfolio
shape):
- `account_kind = other` — these are directly-held assets with **no
  institutional container** (not brokerage/cash/custody/crypto); `other` is
  the honest technical-container value. (Carta uses `custody` because Carta
  administers the holdings; here nobody does.)
- `tax_wrapper = taxable_personal`, `management_style = self_directed`
  (overridable via `account_overrides`). The asset-family split rides on each
  position's `asset_class`, not on the account.
- A sentinel `manual-funding` cash account carries the transaction pairs
  (below), distinct from this holding account.

**Instruments + positions — DECIDED.** One instrument per position (private
assets have no ISIN/CUSIP/symbol → adapter-scoped, like carta). One position
per asset:
- `market_value` = latest `valuations.value` with `as_of_date ≤` the query
  date (forward-filled); position drops out after `closed_at`.
- `book_value` = net cost basis from the position's `acquisition` +
  `contribution` − `disposal` transactions.
- `quantity` = NULL — real estate / a convertible note / a whole-company
  stake aren't unit-denominated; they're valued by amount, the way carta
  values a fund LP interest by NAV rather than units.

**Asset class — DECIDED.** Bronze `kind` **is** the canonical `asset_class`
(identity classmap), so the CSV self-documents the class:

| position `kind` = `asset_class` | status | rationale |
|---|---|---|
| `real_estate` | **NEW** | Directly-held property is a first-class asset class with no existing fit; `other` would erase it from portfolio queries. |
| `convertible_note` | **NEW** | 0%-interest early-stage venture bets expected to convert to equity (or go to zero) — **not** debt. Calling them `private_debt`/`bond` would be technically arguable but actively misleading. |
| `private_equity` | exists | Direct private-company equity (e.g. a GmbH/AG stake) — what carta's classmap folds into `private_equity`. |
| `private_fund` | exists | LP interest in a venture/PE fund (carta/angellist). Capital calls → `contribution`, distributions → `distribution`. |
| `spv` | exists | LP interest in a single-company SPV (equityzen). The one-shot buy-in → `acquisition`. |
| `other` | exists | Catch-all for holdings without a fitting class — e.g. a receivable or a private loan. Semantics ride in `display_name` + `payload`. |

Only `real_estate` + `convertible_note` are new enum values;
`private_equity` / `private_fund` / `spv` already exist (added with carta /
angellist / equityzen). `asset_class` carries **no SQL CHECK** (Go-validated
only), so the two new values touch just `internal/canonical/enums.go` + its
`assetClassValues` map — no gold migration for the enum itself.

**Transactions — funding sentinel (carta/equityzen pattern).** The user
records ONE real event per row (a positive magnitude + a kind); the user
**never** hand-enters double-entry. The gold adapter projects each into a
**balanced double-entry pair on the `manual-funding` sentinel**, exactly like
carta/equityzen, so the sentinel is a pass-through clearing account whose
derived balance is always 0:

| bronze `kind` | gold pair (signed) |
|---|---|
| `acquisition` | `deposit` (+) + `buy` (−, lot if known) |
| `disposal` | `sell` (+, lot if known) + `withdrawal` (−) |
| `contribution` | `deposit` (+) + `contribution` (−) |
| `distribution` | `distribution` (+) + `withdrawal` (−) — rent / dividend |
| `fee` | `deposit` (+) + `fee` (−) — property tax / mgmt fee |
| `conversion` | non-cash — **deferred** (see below) |

The external bank leg (`deposit`/`withdrawal`) and the holding leg net to
zero on the sentinel; the user's *real* bank movement still lives in the
UBS/Schwab/etc. collectors, but that's fine — the sentinel is a synthetic
clearing account, not a claim about a real balance (no `cash_balance` row),
identical to how carta's `carta-funding` works. All TxKinds used already
exist (`TxKindContribution`/`TxKindDistribution` joined the fixed-sign set
with the private-market work — `internal/canonical/sign.go`); the adapter
calls `ApplyCanonicalSign` per leg.

**Conversion — deferred.** No real conversion has happened yet, so the exact
treatment is left open (§7). Provisional plan: close the note + open the
linked equity position (handled by the positions projection via `closed_at` /
`converted_from_position_id`), and emit the `conversion` row as a sign-neutral
`TxKindOther` for provenance rather than a sentinel cash pair (it moves no
external cash). Revisit when the first conversion lands.

**Gold registration (later, not now — blocked by the freeze).** When the
freeze lifts: add a `0016_silver_sources_manual.sql` widening the
`silver_sources` `silver_kind` whitelist (the 0007–0015 rename-recreate
pattern); add `real_estate` + `convertible_note` to `enums.go`; add the
`wealthdb.cfg` source entry; build `wealthdb/internal/silver/manual/`. **None
of that is done in this scaffold.**

## 7. Status of the decisions

Decided (2026-06-10):

1. **Bronze `kind` = canonical `asset_class`** (identity classmap). The
   real data uses `real_estate` / `private_equity` / `convertible_note` /
   `private_fund` / `spv` directly as kinds. ✔
2. **New enum values**: `real_estate` + `convertible_note` (the latter not
   `private_debt`/`bond` — these are 0% venture notes; calling them debt would
   mislead). `private_equity` / `private_fund` / `spv` already exist. ✔
3. **Transactions** → carta-style `manual-funding` double-entry sentinel; the
   collector synthesizes both legs, the user enters only the single real
   event. (Asset-kind nuance: a fund LP capital call is a `contribution`; a
   single-deal SPV buy-in is an `acquisition`.) ✔
4. **Account** → one `manual` account, `account_kind = other`. ✔
5. **Silver engine** → SQLite (the repo default; DuckDB is for
   high-volume / complex-query stores, neither of which this is). ✔

Still open (non-blocking; safe to defer):

- **Conversion (note → equity) gold treatment** — provisional plan in §6;
  finalize when the first real conversion occurs. There are none yet.

The only thing between this design and a working gold adapter is the gold
code freeze.
