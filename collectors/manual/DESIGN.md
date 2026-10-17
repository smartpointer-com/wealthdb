# manual — design notes

`load.py` validates a few hand-maintained CSVs into a SQLite silver. The
gold adapter
([`wealthdb/internal/silver/manual/`](../../wealthdb/internal/silver/manual/),
§6) loads it into gold. [README.md](README.md) documents the CSV columns,
the maintenance recipes and the validation rules.

This is a catch-all collector for **private holdings with no source UI at
all**. The bank and portal sources have their own collectors. What is
left is illiquid private holdings tracked by hand:

- **Real estate** — directly-held residential / commercial property.
- **Convertible notes (CLAs)** — early-stage venture investments into private
  companies. Legally convertible loan agreements, but economically venture
  bets: typically **0% interest**, expected to convert to equity at the next
  funding round or be written to zero. Not debt in any meaningful sense.
- **Direct private-company equity** — e.g. a stake in a GmbH / AG.
- **Fund LP interests & SPVs** — limited-partner commitments in a venture/PE
  fund, paid in over several capital calls, and single-deal SPVs.
- **Other illiquid positions** — anything without a cleaner home, e.g. a
  receivable or a private loan.

The expected volume is a few new rows a year. The design goal is to be
the **simplest collector in the repo**.

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
A few hand-maintained CSVs live in the data dir
(`$XDG_DATA_HOME/wealthdb/manual/`). There is no auth, no MFA, no Docker,
no browser and no `~/.secrets/manual.env`. Only `load` does work. The
wrapper accepts `download` and `login` as no-ops. The runtime is
**host-venv** with no third-party dependency: SQLite and CSV parsing are
stdlib, and the only install is the shared `collectorkit`. The loader
reports precise `file:row:column` errors.

Because the CSVs *are* the source of truth (not a captured snapshot of a
remote state), `load` **fully rebuilds** silver from the current CSVs each
run, inside one transaction. Same CSVs in ⇒ same silver out. There are no
timestamped bronze run-dirs and no per-dump idempotency gate — the model
that fits a remote source that emits successive dumps doesn't apply to a
hand-edited spreadsheet.

The fleet-wide **`prune`** verb follows from this shape. There are no run
dirs and no debug artefacts, so there is nothing to reclaim. The shared
prune engine
([`collectorkit.prune`](../../shared/collectorkit/collectorkit/prune.py))
walks only timestamped run dirs, so it would always be empty-handed here.
A sweeper for the root files is ruled out, because the CSVs are the only
copy of the source of truth ([AGENTS.md](AGENTS.md) §1–2). So `prune` is
a **documented no-op**. It explains why and exits 0, because the
`wealthdb-collect` dispatcher forwards the verb and it must not fail. It
never touches the CSVs or the silver DB.

## 2. Bronze: the CSV schema

Five CSVs, one row per thing. [README.md](README.md#the-csv-schema-bronze)
lists their columns. The **stable columns are the same across all asset
kinds**. Everything kind-specific lives in a JSON `payload` column, not in
sparse per-kind columns. This is the repo's silver convention (promote
stable filter and join fields, absorb drift in `payload`), applied one
layer earlier. Here bronze *is* a hand-typed schema. There is one file
across kinds (a `kind` discriminator + `payload`), never a file per kind.
So a new asset kind (a crypto cold wallet, a collectible) needs no new
file and no schema change.

**accounts.csv** is optional. A book without it puts every position in
one default account.

- An account is a **declaration, not something fetched**. It exists
  because a holding inside a trust or a company is not the holder's own
  taxable property. Rolling both into one account makes every
  wrapper-grained report wrong.
- `account_kind`, `tax_wrapper` and `management_style` are the canonical
  gold vocabularies. `load.py` checks them against literal sets that
  mirror `internal/canonical/enums.go`. A value gold would reject fails at
  load, where the CSV row number is still in hand. The two optional ones
  default to the default account's values, so a row that only names a
  wrapper need not restate the rest.
- The alternative is a **second silver source per sleeve**: a directory,
  a silver DB, a config entry and an `account_overrides` entry each time.
  One table replaces all of that. `account_overrides` still wins on
  overlap, because the loader applies config after the adapter stamps.

**positions.csv** holds one row per held asset.

- A position naming an account that `accounts.csv` does not declare is a
  load error, not a silently synthesised sleeve.
- `kind` is the coarse classification. Its vocabulary lives in
  `load.py`'s `POSITION_KINDS`, not in a DB constraint, so a new kind
  needs no migration.
- `vehicle` is the wrapper dimension of the 2-D taxonomy
  ([TAXONOMY.md](../../wealthdb/docs/TAXONOMY.md)). A blank one defaults
  from `kind`. The gold adapter derives the exposure from (`kind`,
  `vehicle`) (§6).
- `closed_at` is set when the asset stops existing: a full disposal, or a
  convertible note that converted. Gold drops the position from as-of
  queries after that date.

**valuations.csv** is the **per-date valuation series**. It follows
carta's per-date valuation-override pattern
([carta DESIGN](../carta/DESIGN.md) §5.1), but as a first-class series.
The value as of a date D is the latest row with `as_of_date ≤ D`, and gold
forward-fills. So an illiquid asset's value moves correctly over time: a
property re-appraised every few years, a note re-marked at a round.

**cost_basis.csv** is an optional paid-in series, for a holding whose
cost is not its first valuation. The rows are running totals, not cash
events. The calls themselves are bank wires.

**exits.csv** holds what a position was sold for. It carries the one
datum the bank wires cannot: which position the money came from. The
gold adapter projects it as a `sell` (§6), so the lot engine can realize
the gain against the cost basis.

**No cash-flow file.** Every other event is a real wire in the bank
accounts, already captured by the bank collectors: a purchase, a capital
call, a fee, a distribution, the proceeds landing. A file here would only
duplicate them.

**Conversion (convertible_note → equity)** is modelled **purely
position-side**, with no exit and no transaction:

- the note gets `closed_at` = the conversion date;
- a new `private_equity` position opens with `acquired_at` = that date
  and `payload.converted_from_position_id` = the note's id. `load`
  enforces that back-reference.

No cash moves at a conversion, so there is nothing more to record.

## 3. Silver schema

SQLite + JSON1, mirroring the bronze CSV shape one-to-one. The schema is
[migrations/0001_initial.sql](migrations/0001_initial.sql), extended by
[0002_positions_vehicle.sql](migrations/0002_positions_vehicle.sql),
[0003_accounts.sql](migrations/0003_accounts.sql),
[0004_cost_basis.sql](migrations/0004_cost_basis.sql) and
[0005_exits.sql](migrations/0005_exits.sql).

| Table | Grain | Notes |
|---|---|---|
| `accounts` | `id` | `display_name`, `account_kind`, `tax_wrapper`, `management_style`, `notes`, `payload`. Optional (migration 0003): the sleeve a position is held in. |
| `positions` | `id` | `account_id` (NULL → the default account), `kind`, `vehicle`, `display_name`, `currency`, `acquired_at`, `closed_at` (NULL = held), `notes`, `payload`. |
| `valuations` | (`position_id`, `as_of_date`) | `value`, `currency`, `notes`, `payload`. The per-date mark series; the row dated at the position's `acquired_at` is the cost basis, unless `cost_basis` covers the position. |
| `cost_basis` | (`position_id`, `as_of_date`) | `amount`, `currency`, `notes`. Optional (migration 0004): the capital paid in as of a date, gross of any paid back. |
| `exits` | `position_id` | `exit_date`, `proceeds`, `currency`, `notes`. Optional (migration 0005): what the position brought on the day it left. |
| `load_runs` | — (append-only) | Audit log: `load_at`, schema version, `bronze_dir`, row counts, `payload`. No idempotency gate (full rebuild each run). |
| `schema_meta` | `silver_schema_version` | collectorkit migration bookkeeping. |

Storage (carta conventions): money (`value`, `amount`) is a decimal STRING
(TEXT) verbatim — exact, no float rounding; dates are ISO `'YYYY-MM-DD'` TEXT;
`payload` is TEXT JSON; `load_at` is Unix-seconds INTEGER. Rates /
ownership_pct / other ratios stay inside `payload`, not money columns.
Referential integrity (a valuation's `position_id`; a conversion's
`converted_from_position_id` back-reference) is enforced in `load.py`
(precise errors), **not** by SQLite FK constraints — the loader truncates and
rebuilds every table each run, so hard FKs would only complicate
delete/insert ordering. The accepted `kind` vocabulary lives in `load.py` so
adding a kind needs no migration.

## 4. Why SQLite

SQLite + JSON1 is the repo default. The one DuckDB exception
(`cointracking`) is driven by a window-function holdings replay and
`DECIMAL(38,18)` crypto amounts; **neither applies here** — this is a small
shape transformation with no computation and no
arbitrary-precision need. DuckDB is for high-volume / complex-query stores,
which this is not. Money
is stored as decimal STRINGS (TEXT) verbatim to avoid float rounding, dates
as ISO TEXT, `payload` as TEXT JSON — exactly the carta silver conventions,
so the gold adapter reads it with the same `modernc.org/sqlite` driver every
other SQLite-silver adapter uses.

## 5. Validation

The collector's whole value is that hand-entered data is **checked**,
since no source system enforces anything. A bad row fails the entire
load: one DB transaction rolls back, the exit is non-zero, and the
message carries `file:row:column`. [README.md](README.md#validation)
lists the checks. The taxonomy sets in `load.py` mirror
`internal/canonical/enums.go` (account kinds, tax wrappers, management
styles) and `taxonomy.go` (vehicles). A collectorkit test
(`test_canonical_enum_mirrors.py`) pins the `enums.go` sets to their Go
source.

## 6. Gold mapping

The gold adapter is in [`wealthdb/internal/silver/manual/`](../../wealthdb/internal/silver/manual/),
registered in `cmd/wealthdb/main.go`. It follows the carta/equityzen
structure, with an exit as its only transaction.

**Accounts — one per declared sleeve**, and one default account for every
position that names none. A book with no `accounts.csv` therefore projects
exactly one account.

- The DEFAULT account has `account_kind = other` — directly-held assets
  with **no institutional container** (not
  brokerage/cash/custody/crypto); the honest value (Carta uses `custody`
  because Carta administers the holdings; here nobody does) — plus
  `tax_wrapper = taxable_personal`, `management_style = self_directed`.
  Its id is fixed, so a book without `accounts.csv` always projects the
  same account.
- A DECLARED account carries its own `account_kind`, `tax_wrapper` and
  `management_style` from `accounts.csv`, which is what lets one book span
  tax sleeves — holdings under one wrapper beside personally-held ones.
- Either way, `account_overrides` still has the last word. No
  `base_currency`, since a book can span currencies. The asset-family split
  rides on each position's `asset_class`, not the account.
- An account is emitted at a snapshot date only while it holds something
  then, so a sleeve that is fully closed stops appearing rather than
  lingering as an empty shell.
- There is **no funding sentinel** — the adapter projects no cash flow
  (below), so there is nothing to balance.

**Instruments + positions.** One instrument per position (private assets have
no ISIN/CUSIP/symbol → adapter-scoped). One position per asset, emitted as a
COMPLETE forward-filled snapshot at **every event date** (any date a position
is acquired, re-valued, or closed) so gold's as-of query — which reads the
latest `snapshot_at ≤` the query date per source, then all its positions — is
correct at any historical date:
- `market_value` = latest `valuations.value` with `as_of_date ≤` the snapshot
  date (forward-filled); the position drops out after `closed_at`.
- `book_value` = for a position `cost_basis` covers, its latest entry with
  `as_of_date ≤` the snapshot date (none before the first entry). A fund
  commitment shows the capital called so far, not the commitment.
  Otherwise it is the valuation dated at the position's `acquired_at`,
  constant while the market moves. Real estate shows purchase price
  against the current appraisal; a note, loan or escrow held at par shows
  book equal to market. Every `cost_basis` date is a snapshot date too.
  The figure is stamped `stated`, with method `paid_in` or
  `acquisition_value` and fees `unknown`
  ([DESIGN.md](../../wealthdb/docs/DESIGN.md) §7.4): a figure entered by
  hand says nothing about fees.
- A `mortgage` is entered as a positive outstanding balance. The adapter
  negates its market and book value, so the liability nets against the
  property.
- `quantity` = NULL — none of these are unit-denominated; they're valued by
  amount (the way carta values a fund LP interest by NAV, not units).
- `acquisition_date` = `acquired_at`.

**Asset class and vehicle.** The adapter
([`classmap.go`](../../wealthdb/internal/silver/manual/classmap.go))
derives the exposure (`asset_class`) from `kind`, and for `other` also
from `vehicle`. The `vehicle` comes straight from silver, with the
kind's default when blank.

| position `kind` | `asset_class` | default `vehicle` | rationale |
|---|---|---|---|
| `real_estate` | `real_estate` | `physical` | Directly-held property is a first-class class; `other` would erase it from portfolio queries. |
| `mortgage` | `real_estate` | `mortgage` | A liability secured on a property, netted against it (above). |
| `private_equity` | `private_equity` | `stock` | Direct private-company equity, such as a GmbH or AG stake. |
| `private_fund` | `private_equity` | `fund` | An LP interest in a venture or PE fund. The capital paid in rides in `cost_basis.csv`; calls and distributions are bank wires. |
| `spv` | `private_equity` | `spv` | An LP interest in a single-company SPV. The buy-in is the valuation at `acquired_at`. |
| `convertible_note` | `private_debt` | `convertible_note` | A 0%-interest venture note that converts to equity or goes to zero. The vehicle keeps it apart from ordinary lending. |
| `other` | `private_debt` with vehicle `loan` or `escrow`, else `other` | `other` | The catch-all, such as a receivable or a private loan. Set `vehicle` so gold gets the right wrapper. |

**One transaction: the exit.** `Transactions()` yields a `sell` per
`exits` row, on the exit date, on the position's account, for the
proceeds (id `exit:<position_id>`). It is the internal half of the sale:
the cash lands in a bank account the banks book. The lot engine's
`stated` mode realizes it against the position's book value as of the
last snapshot before the sale ([LOTS.md](../../wealthdb/docs/LOTS.md)). There is no
`manual-funding` sentinel and no other transaction: a purchase wire,
rent, a fee, the proceeds landing are real movements in the bank
accounts, already captured by the bank collectors; re-representing them
on a sentinel only duplicates them. carta/equityzen *need* their
double-entry ledgers (booked on their custody accounts) because those
sources' cash is invisible to everything else; manual's is not.

**One exception — value that arrives from another tracked vehicle, not from a
bank.** Another source can book a withdrawal whose value reappears here as a
claim, with no bank in between: sale proceeds an agent holds back, for
example, as an `other`/`escrow` position. The position alone would make the
returns engine read the arrival as performance — a mark stepping up with
nothing to fund it — and a later release as a loss of the whole claim. The
missing half is supplied by a row in the config-side equity-transfer ledger
([DESIGN.md](../../wealthdb/docs/DESIGN.md) §13.9) written against this source's single
account; the loader injects it as a `transfer_in` / `transfer_out`, and the
source's returns policy admits exactly those two kinds. The invariant, for
every date something happens to such a claim:

- the `valuations.csv` row says what the claim is **still worth after** the
  event;
- the ledger row says what **cash actually crossed** the claim's boundary,
  and in which direction;
- the engine books (Δvalue + cash) as the return.

So the arrival is a `direction=in` leg against a valuation of the same
amount (return 0). A release tranche is a `direction=out` leg dated the day
cash reaches the bank, against a mark-down of the same amount (return 0);
the bank's deposit is its own external inflow, and the two do not need to
pair. Interest is a mark above the cash that eventually comes out (a gain).
A claim paid out to a third party is a mark-down with **no ledger row**
(a loss); forfeiture is the mark going to zero the same way.

**Gold registration.** `internal/gold/migrations/0016_silver_sources_manual.sql`
adds `manual` to the `silver_sources` `silver_kind` whitelist. The adapter
registers in `cmd/wealthdb`. The `wealthdb.cfg` entry is
`{"id":"manual","kind":"manual","path":"…/manual.db"}`.
