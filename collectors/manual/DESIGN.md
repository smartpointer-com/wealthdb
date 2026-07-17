# manual — design notes

`load.py` validates two hand-maintained CSVs into a SQLite silver against the
synthetic [examples/](examples/) (`tests/`); the gold adapter
([`wealthdb/internal/silver/manual/`](../../wealthdb/internal/silver/manual/),
§6) loads the manual source into gold, so it shows up in `wealthdb holdings positions`.

A catch-all collector for **private holdings with no source UI at all** —
the bank/portal sources are all covered by the other collectors;
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

Volume is a few new holdings per year, valuations updated occasionally.
The design goal is to be the **simplest collector in the repo**.

Sections:

1. [The unique shape — no source](#1-the-unique-shape--no-source)
2. [Bronze: the CSV schema](#2-bronze-the-csv-schema)
3. [Silver schema](#3-silver-schema)
4. [Why SQLite](#4-why-sqlite)
5. [Validation](#5-validation)
6. [Gold mapping](#6-gold-mapping)

## 1. The unique shape — no source

Every other collector has the lifecycle `login → download → load`, driving
a privileged read-only session against a source. **`manual` has no source.**
Two hand-maintained CSVs live in `$XDG_DATA_HOME/wealthdb/manual/`; there is no auth, no
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

The fleet-wide **`prune`** verb follows from this shape: with no run-dirs and no
debug/diagnostic artefacts (nothing writes a screenshot, trace, or DOM dump into
bronze), there is nothing for it to reclaim. The shared prune engine
([`collectorkit.prune`](../../shared/collectorkit/collectorkit/prune.py)) walks
only timestamped run-dirs, so it would be permanently empty-handed here; a
bespoke root-file sweeper is expressly ruled out because the two CSVs are the
irreplaceable source of truth (§2, [CLAUDE.md](CLAUDE.md) §1–2). So `manual`
accepts `prune` — the `wealthdb-collect` dispatcher forwards it, so the wrapper
must not crash on it — as a **documented no-op**: it explains why nothing is
reclaimed and exits 0, never touching the CSVs or the derived silver DB.

## 2. Bronze: the CSV schema

Two CSVs, one row per thing. The **stable columns are the same across all
asset kinds**; everything kind-specific lives in a JSON `payload` column —
not sparse per-kind columns. This is the repo's silver convention (promote
stable filter/join fields, absorb drift in `payload`) applied one layer
earlier, at bronze, because here bronze *is* a hand-typed schema.
One file across kinds (a `kind` discriminator + `payload`), never a file per
kind, so adding another asset kind (a crypto cold-wallet, a collectible)
needs no new file and no schema change.

**positions.csv** — one row per held asset.
`id, kind, vehicle, display_name, currency, acquired_at, closed_at, notes, payload`
- `kind` ∈ `real_estate | private_equity | convertible_note | private_fund
  | spv | mortgage | other` (extensible — lives in `load.py`'s
  `POSITION_KINDS`, not a DB constraint). The kind is the coarse 1-D
  classification; the gold adapter maps (`kind`, `vehicle`) to the
  (`asset_class`, `vehicle`) pair.
- `vehicle` (optional) — the wrapper dimension of the 2-D taxonomy (wealthdb
  docs/TAXONOMY.md), e.g. `physical | stock | fund | spv | convertible_note
  | loan | escrow | mortgage`. Blank defaults from `kind`
  (real_estate→physical, private_equity→stock, spv→spv, private_fund→fund,
  convertible_note→convertible_note, mortgage→mortgage, other→other). Set it
  explicitly on a `kind=other` row so gold gets the right wrapper — an
  escrow receivable is `escrow`, a private loan is `loan`. The gold adapter
  derives the exposure (`asset_class`) from (`kind`, `vehicle`). Full
  vocabulary is `load.py`'s `POSITION_VEHICLES`.
- `closed_at` (nullable) — set when the asset stops existing (full disposal,
  or a convertible note that converted). Gold drops the position from as-of
  queries after this date.
- `payload` — e.g. real_estate `{property_type, ownership_pct, city,
  country}`; convertible_note `{principal, interest_rate, cap, maturity_date,
  conversion_terms, counterparty}` (`interest_rate` is 0 for these venture
  notes; the principal is the cost-basis valuation at `acquired_at`);
  private_equity `{ownership_pct, share_cnt,
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

**No transactions file.** The collector is positions + valuations only
(§6). A cash-flow file would add nothing but duplication: every such event
is a real wire in the bank accounts, already captured by the bank
collectors. The one datum it would carry that positions/valuations don't —
the acquisition date — already lives on the position (`acquired_at`).

**Conversion (convertible_note → equity)** is therefore modelled **purely
position-side**, no transaction:
- the convertible_note position gets `closed_at` = the conversion date (it
  drops out of as-of queries after that date);
- a new `private_equity` position is opened with `acquired_at` = that date and
  `payload.converted_from_position_id` = the note's id (the whole record of
  the conversion). `load` enforces the back-reference (a dangling
  `converted_from_position_id` fails). No cash moves at conversion, so there
  is nothing to record beyond the close + open.

## 3. Silver schema

SQLite + JSON1, mirroring the bronze CSV shape one-to-one. Realized in
[migrations/0001_initial.sql](migrations/0001_initial.sql).

| Table | Grain | Notes |
|---|---|---|
| `positions` | `id` | `kind`, `display_name`, `currency`, `acquired_at`, `closed_at` (NULL = held), `notes`, `payload`. |
| `valuations` | (`position_id`, `as_of_date`) | `value`, `currency`, `notes`, `payload`. The per-date mark series; the row dated at the position's `acquired_at` is the cost basis. |
| `load_runs` | — (append-only) | Audit log: `load_at`, schema version, `bronze_dir`, row counts, `payload`. No idempotency gate (full rebuild each run). |
| `schema_meta` | `silver_schema_version` | collectorkit migration bookkeeping. |

Storage (carta conventions): money (`value`) is a decimal STRING (TEXT)
verbatim — exact, no float rounding; dates are ISO `'YYYY-MM-DD'` TEXT;
`payload` is TEXT JSON; `load_at` is Unix-seconds INTEGER. Rates /
ownership_pct / other ratios stay inside `payload`, not money columns.
Referential integrity (a valuation's `position_id`; a conversion's
`converted_from_position_id` back-reference) is enforced in `load.py`
(precise errors), **not** by SQLite FK constraints — the loader truncates and
rebuilds both tables each run, so hard FKs would only complicate
delete/insert ordering. The accepted `kind` vocabulary lives in `load.py` so
adding a kind needs no migration.

## 4. Why SQLite

SQLite + JSON1 — the repo default. The one prior DuckDB exception
(`cointracking`) was driven by a window-function holdings replay and
`DECIMAL(38,18)` crypto amounts; **neither applies here** — this is a tiny
shape transformation, a few rows a year, no computation and no
arbitrary-precision need. DuckDB is for high-volume / complex-query stores,
which this is not. (An early sketch used a DuckDB silver; it was overridden to
stay on the documented default — the same call carta made.) Money
is stored as decimal STRINGS (TEXT) verbatim to avoid float rounding, dates
as ISO TEXT, `payload` as TEXT JSON — exactly the carta silver conventions,
so the gold adapter reads it with the same `modernc.org/sqlite` driver every
other SQLite-silver adapter uses.

## 5. Validation

The collector's whole value is that hand-entered data is **checked**, since
there's no source system enforcing anything. `load` fails the entire load
(one DB transaction → rollback; non-zero exit; `file:row:column` message) on:
duplicate id; unknown `kind`; unparseable date / non-3-letter currency /
non-numeric or negative `value`; a `value` currency that disagrees with the
position's; a `valuations` `position_id` absent from `positions.csv`; a
`converted_from_position_id` that references a position absent from
`positions.csv`; malformed or non-object JSON `payload`; an unexpected
(typo'd) column. It **warns** but loads when a valuation predates the
position's `acquired_at`.

## 6. Gold mapping

The gold adapter is in [`wealthdb/internal/silver/manual/`](../../wealthdb/internal/silver/manual/),
registered in `cmd/wealthdb/main.go`. It follows the carta/equityzen
structure, minus the transaction half.

**Account — ONE gold account** for the whole `manual` source, every position
under it:
- `account_kind = other` — directly-held assets with **no institutional
  container** (not brokerage/cash/custody/crypto); the honest value. (Carta
  uses `custody` because Carta administers the holdings; here nobody does.)
- `tax_wrapper = taxable_personal`, `management_style = self_directed`
  (overridable via `account_overrides`). No `base_currency` — the book spans
  CHF / EUR / USD. The asset-family split rides on each position's
  `asset_class`, not the account.
- There is **no funding sentinel** — the adapter projects no transactions
  (below), so there is nothing to balance.

**Instruments + positions.** One instrument per position (private assets have
no ISIN/CUSIP/symbol → adapter-scoped). One position per asset, emitted as a
COMPLETE forward-filled snapshot at **every event date** (any date a position
is acquired, re-valued, or closed) so gold's as-of query — which reads the
latest `snapshot_at ≤` the query date per source, then all its positions — is
correct at any historical date:
- `market_value` = latest `valuations.value` with `as_of_date ≤` the snapshot
  date (forward-filled); the position drops out after `closed_at`.
- `book_value` = the valuation dated at the position's `acquired_at` (the cost
  basis). Constant while market moves — real estate shows purchase price vs.
  current appraisal; a note / loan / escrow held at par shows book == market.
- `quantity` = NULL — none of these are unit-denominated; they're valued by
  amount (the way carta values a fund LP interest by NAV, not units).
- `acquisition_date` = `acquired_at`.

**Asset class.** Bronze `kind` **is** the canonical `asset_class`
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

**No transactions.** The adapter's `Transactions()` returns an empty stream;
there is no `manual-funding` sentinel. Every cash flow a manual holding could
record — a purchase wire, rent, a fee, sale proceeds — is a real movement in
the bank accounts, already captured by the bank collectors; re-representing it
on a sentinel only duplicates them. The one datum the acquisition transaction
carried that positions/valuations don't (the acquisition date) already rides on
the position. carta/equityzen *need* their funding sentinel because those
sources' cash is invisible to everything else; manual's is not.

**Gold registration.** `internal/gold/migrations/0016_silver_sources_manual.sql`
widens the `silver_sources` `silver_kind` whitelist (the 0007–0015
rename-recreate pattern); `real_estate` + `convertible_note` are added to
`internal/canonical/enums.go`; the adapter is built + registered in
`cmd/wealthdb`. The `wealthdb.cfg` entry is
`{"id":"manual","kind":"manual","path":"…/manual.db"}`.
