# fidelity-web — design

Design document for the `fidelity-web` toolkit. The audience
is the engineer (current author, future contributor) implementing
and maintaining `download.py` and the silver loader
against the live `www.fidelity.com` UI. It is also the contract
between this silver and the wealthdb Fidelity adapter
([`wealthdb/internal/silver/fidelity/`](../../wealthdb/internal/silver/fidelity/)).

This document is the planning artefact, not the journal. Decisions
that get revised should be revised here, in place. See §11 for the
explicit punch list of unknowns that remain.

Part of the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. This document only covers what's Fidelity-specific.

## 1. Context and non-goals

### 1.1 Why a web scraper

Fidelity's OFX Direct Connect endpoint (`ofx.fidelity.com`) is
**NXDOMAIN as of 2026-05-15**. The historical channel that
Quicken / GnuCash / hobbyists used to read Fidelity data
programmatically has been retired; verification was a `dig` against
the hostname (empty) vs `www.fidelity.com` / `oltx.fidelity.com`
(both still resolving via Akamai), confirming the OFX hostname
specifically was removed rather than a transient outage. Customer-
service FAQ pages for "Quicken / Direct Connect" have been
replaced by a chat-search that dead-ends on the topic.

The official consumer-facing path for 2026 is **Fidelity Access /
Akoya** (FDX-JSON over OAuth). It is B2B-only: data-recipient
registration is geared toward fintechs and family offices, not
individual users reading their own data. Aggregators built on
Akoya (Plaid, MX, Empower) are similarly B2B.

That leaves the client-web channel — `www.fidelity.com` driven
under Camoufox (a stealth-patched Firefox; rung 3 of §6) — as the
only realistic surface for a self-service personal-archive
toolkit today.

### 1.2 Account-category model

Fidelity's account selector groups accounts under section
labels rendered as `<section aria-label="…">` blocks. The
toolkit captures the label at bronze time
(`account_dimensions[*].portfolio`) and silver classifies it
into a stable `portfolios.kind`:

| Section label                  | `portfolios.kind` | Notes |
| ---                            | ---               | --- |
| `Education`                    | `529`             | 529 College Investing Plan participant accounts. Statement PDFs are generated and surface in the document center's Statements sub-page. |
| `Authorized`                   | `trust_managed`   | Accounts under a trust agreement whose investments a third-party manager runs, with Fidelity as custodian (see §1.3). The web document center may serve no statement PDFs for this group; statements supplied out-of-band are ingested separately (see §4.5). Its tax forms surface in the read-through view. |
| `Fidelity Charitable® Giving`  | `daf`             | Donor-Advised Fund. Ingested by the DAF walk phase (§12) over the Fidelity Charitable JSON REST API, reached by an SSO hop; the retail phases still auto-exclude it by its 7-digit id length (it has no brokerage-side surfaces). Gold: `account_kind = donor_advised_fund`, `tax_wrapper = charitable`, `management_style = automated` (§12.3). |
| any other label                | `other`           | Fall-through so future Fidelity labels don't need a schema migration. |

### 1.3 Third-party managers and institutional feeds

A `trust_managed` account can be run by a third-party investment
manager, with Fidelity as custodian. Such a manager usually reads
Fidelity's institutional feed into its own system. A feed of that
kind belongs in its own collector, not in this one. The `manual/`
bronze subdirectory holds documents that arrive out-of-band.

### 1.4 Non-goals

- **Real-time / near-real-time pull.** MFA gates every truly-fresh
  login.
- **Trade execution, money movement, account configuration.** See
  [CLAUDE.md](CLAUDE.md) §1 — the contract is read-only.

- **Akoya / FDX / Plaid / SnapTrade.** B2B-only.
- **Prospectuses / fund supplements / disclosures.** Out of
  document-center scope (CLAUDE.md §1).
- **Cross-bank semantic alignment.** `wealthdb` gold's job.

## 2. Bronze layout

```
<bronze-root>/
├── 20260524T120000Z/
│   ├── run.json                                manifest (CLI config,
│   │                                           account_dimensions, per-phase results) — NOT compressed
│   ├── positions/
│   │   ├── positions_summary.csv.zst           Overview view (all accounts in one CSV)
│   │   └── positions_dividend.csv.zst          DividendView (ex-date, yield, est. annual income)
│   ├── activity/
│   │   └── activity_<YYYYMMDD>__<YYYYMMDD>.csv.zst one CSV per date-window (Custom-range mode),
│   │                                           or activity_past_90_days.csv.zst (preset mode);
│   │                                           consolidated across accounts, Account Number
│   │                                           column inside.
│   ├── documents/
│   │   ├── <fidelity-supplied-filename>.pdf    statement PDFs (where served) +
│   │   │                                       per-year, per-account tax-form PDFs
│   │   │                                       (Consolidated 1099, 1099-Q, etc.) — NOT compressed
│   │   └── …
│   ├── balances/
│   │   └── balances.html.zst                   full-page DOM (no CSV export available;
│   │                                           per-account totals in
│   │                                           data-testid$='-totalaccountvalue-label')
│   ├── performance/
│   │   └── performance.html.zst                full-page DOM (no structured export;
│   │                                           return % surface only via rendered text)
│   ├── daf/                                    Donor-Advised Fund phase (§12); only when the
│   │   │                                       account selector lists a Fidelity Charitable section
│   │   ├── accounts.json                       giving-account roster — NOT compressed
│   │   └── <account_key>/                      one per giving account (sha256(acctNbr)[:16])
│   │       ├── account.json                    balance + pending buckets + establish date
│   │       ├── pool_balances.json              investment-pool positions (today's snapshot)
│   │       ├── grants.json / contributions.json / gifts.json
│   │       ├── pool_exchanges.json / adjustments.json   (unioned across active years)
│   │       ├── documents_index.json            listing metadata for the PDFs below
│   │       ├── exports/*.csv.zst               server-side CSV exports (corroborating)
│   │       └── documents/*.pdf                 statements, grant/contribution confirmations,
│   │                                           Form 8283 — NOT compressed
│   └── screenshots/                            only with `download --debug` / `--explore`
│       └── <ts>-<label>.{html,png}             per-landmark diagnostics for selector drift
│                                               (+ <ts>-<label>.dominv.json with --explore) — NOT compressed
├── 20260525T120000Z/
│   └── …
├── manual/                                     hand-dropped artefacts (documents that arrive out-of-band)
├── supplied-statements/                        statement PDFs supplied out-of-band (→ historical_position_snapshots, §4.5)
└── fidelity-web.db                             silver SQLite (default location)
```

(HTML/CSV shown as `.zst`; pre-compression dumps carry the plain
names, and both forms load — see **Bronze compression** below.)

**Bronze compression.** Each HTML/CSV artefact is zstd-compressed in
place as it lands (`collectorkit.compress.compress_file`: atomic
tmp+rename, decompress-and-sha256-verify before the plain file is
unlinked, mtime carried over). HTML/CSV-shaped bronze compresses to a
small fraction of its raw size. The list of compressed forms is exactly
the load inputs that are text: `positions/*.csv`, `activity/*.csv`,
`balances/balances.html`, `performance/performance.html`, and any
`documents/Statement*.csv` companions. **PDFs are never compressed**
(already internally compressed; excluding them avoids spending CPU to
grow the file), nor is `run.json` (it must stay greppable — it is the
status-lifecycle handshake `prune` keys on), nor the `--debug`
`screenshots/` tree (debug artefacts `prune` owns).

Compression failures downgrade to a warning and leave the plain file in
place: every reader resolves the on-disk variant via
`compress.resolve_variant` (plain wins when both forms coexist), so a
half-adopted tree is a valid tree, not an error state.

Unlike cointracking — whose DuckDB loader streams `.csv.zst` natively,
so nothing is ever materialised — fidelity-web's silver is SQLite and
the loader parses HTML/CSV **in Python**, so the loader itself
decompresses (`compress.open_text`). That is why fidelity-web is a
**hybrid** collector (Docker image for the Camoufox `download`; a host
venv carrying `zstandard` for the host-side `load` / `recompress`; see
§5). The convergence guarantee: a `documents` row is keyed on the
DECOMPRESSED content (`content_sha256`, `size_bytes`) and the LOGICAL
name (`balances.html`, never `balances.html.zst`), and the activity
`source_sha256` is the decompressed hash — so `load --force` on a
compressed tree yields byte-identical silver to `load --force` on the
same tree uncompressed, and the `content_sha256` dedup still collapses
the same artefact across runs regardless of compression state.

Pre-compression run dirs are converted by the manual `recompress` verb
(thin wrapper over `collectorkit.recompress`: prune-grade safety
envelope, verify-then-unlink per file, byte accounting; reuses prune's
completeness predicate; never scheduled) — after a sweep, a
`load --force` rebuild must produce identical silver.

Everything in a run dir except `screenshots/` is a `load` input.
The captures are diagnostic-only and opt-in (`--debug`, implied by
`--explore`): a full-page DOM dump plus PNG at every navigation
landmark is far larger than the structured load inputs of the same
run, so leaving them on for every nightly run makes the debug
artefacts, not the data, the bulk of the bronze tree. The `prune`
verb deletes them wholesale, along with non-complete dumps
(missing/unreadable `run.json`, or `status` ≠ `complete`, e.g.
`--dry-run` shells and crashed walks) older than an in-flight guard
window; `--dry-run` prints the plan first. Pruning captures leaves
silver byte-identical; pruning non-complete dumps surfaces in
silver on the next `load --force` rebuild.

Filename conventions:

- Activity / positions CSV filenames are content-keyed (date
  window or view name) — no account discriminator in the name.
  `run.json` keys its `account_dimensions` block by
  `sha256(account_external_id)[:16]` so that `ls`-ing a bronze
  dir + a glance at the manifest doesn't expose the 9-digit
  Fidelity account number; the canonical mapping lives inside
  each CSV's `Account Number` column.
- Positions: TWO CSVs, one per view, **each containing all
  in-scope accounts**. Fidelity's positions Download exports the
  consolidated all-accounts view; per-account navigation does not
  filter the output. Account dimension lives in the CSV's
  `Account Number` column.
- Activity: one CSV per date-window, **each containing all
  in-scope accounts** (consolidated; account-selector state does
  NOT scope the export). Account dimension lives in the CSV's
  `Account Number` column, same shape as positions. With a
  `--lookback` window, the request is bisected into ≤93-day
  chunks (Fidelity's per-export cap, configurable via
  `MAX_ACTIVITY_WINDOW_DAYS`); one CSV per chunk, named
  `activity_<YYYYMMDD>__<YYYYMMDD>.csv`. With no window, the
  rolling preset (default `Past 90 days`) produces a single
  `activity_past_90_days.csv`. Retention upper-bound is set by
  Fidelity's Custom-tab `min` attribute on the date input
  (currently ~4 years back from today; backfill calls below that
  are clamped, logged, and continue).
- Documents: Fidelity's own filenames preserved. A tax-form PDF
  can carry an account or agreement number in the filename
  (Fidelity-supplied; we don't redact, but bronze is gitignored).

`run.json` records the CLI config, account inventory, and
per-phase results (success + file paths, or per-failure error
strings).

### 2.1 Coverage, status and the exit code — three different questions

A run answers three questions that must not be conflated, because
conflating two of them is how the activity phase failed on 55
consecutive nightly runs while every signal read healthy.

**`coverage` — did each phase get what it went for?** One entry per
phase the run *attempted*, always written, each with a `complete`
flag and a `gaps` list. The gaps list is written even when empty, so
an empty list is a positive statement of coverage rather than the
absence of a field — ubs-web's `csv_gaps` / `mt940_gaps` convention,
applied per phase. A gap is named the way ubs-web names one:
`YYYY-MM-DD..YYYY-MM-DD` for a window, otherwise whatever identifies
the attempt. A phase the run never requested has **no entry at all**,
so "not asked for" can never be read as "asked for and came back
empty".

**`status` — did the walk finish?** `in-progress` at run-dir
creation, `complete` at the end, and it stays `complete` however the
phases fared. That is deliberate. `complete` is what keeps a dump
loadable and out of `prune`'s delete path, and a dump whose activity
phase failed still holds good positions, balances, documents and DAF
artefacts. Downgrading the status would hand all of that to prune and
block the positions load besides — chase records the same lesson at
its `download.py` head and in `load.py`'s "DO NOT re-add a coverage
gate here". Phase completeness and *dump* completeness are different
axes and live in different fields.

**The exit code — should a human be told?** `download` exits
`EXIT_PHASE_INCOMPLETE` (4) when any attempted phase has
`complete: false`, and 0 when every one of them covered its ground —
including a phase that legitimately had nothing to fetch. It is
emitted *after* the manifest is finalised, so the alarm never costs
the dump its loadability, and it is distinct from the credential (2)
and login (3) codes so a nightly summary tells "the session died"
apart from "a phase quietly died". The propagation chain carries it
unchanged: `entrypoint.sh` execs python, the wrapper execs `docker
run`, `wealthdb-collect` execs the wrapper.

The loader adds two guards of its own on the same reasoning: it skips
a dump whose `status` says it never finished (the check eleven of the
fleet's loaders already make), and `validate` warns when
activity-covering dumps keep landing while the newest transaction
stops moving — the second line of defence that catches the one case
an exit code cannot, an export that succeeds but parses to nothing.
It is a warning and never a gate, because a quiet account
legitimately produces no transaction for weeks.

## 3. Identifier strategy

### 3.1 `account_external_id`

Fidelity uses **9-digit account numbers** for brokerage / trust /
529 accounts and a 7-digit number for the DAF. Surfaces:

| Surface | Format | Notes |
| --- | --- | --- |
| Account-selector `data-testid` | `NNNNNNNNN` (no dash) | `[data-testid="ap143528-accounts-selector-account-link-NNNNNNNNN"]` |
| CSV exports (`Account Number` column) | `NNNNNNNNN` (no dash) | |
| Statement PDF header | `NNN-NNNNNN` (with dash) | Parser strips the dash on the way into silver |

**Canonical form: 9-digit, no-dash.** Promoted as
`account_external_id`; the dashed form lives only in PDF payloads.

The DAF's 7-digit id is the in-built exclusion criterion: walk()
auto-excludes any account whose id isn't exactly 9 characters.

### 3.2 `instrument_external_id`

CUSIP when present, ticker otherwise. Fidelity's CSVs surface
CUSIP for bonds (9-char alphanumeric in the `Symbol` column) and
ticker for equities / ETFs / mutual funds. Money-market core
positions use Fidelity-internal codes (`CORE_X**` and similar; the
asterisks are footnote markers, not part of the ticker). 529-plan
positions use plan-internal codes (e.g. `XXX######` for state
529-plan target-date sleeves).

Silver should discriminate via an `instrument_kind` column
(`cusip` / `ticker` / `plan_code`) so gold can route plan-internal
codes to a different lookup than CUSIPs.

### 3.3 `transaction_external_id`

Fidelity activity CSVs do NOT carry a stable per-row identifier
across exports. `activity_id` is a deterministic SHA-256 prefix
over the row's structural columns (account, timestamp, kind,
symbol, quantity, price, amount, settlement date) plus a per-file
occurrence index. Free-text columns and the source-file hash are
deliberately excluded — they vary between exports and would break
cross-window dedup (migration 0005). Silver-internal only; gold
does not attempt cross-source per-row matching on it.

### 3.4 `owner` dimension

Two values, with sub-trust extensibility:

- `self` — personal accounts (retail, brokerage, 529).
- `trust` — accounts under a trust agreement.

The owner discriminator is derived at silver-load time from the
`portfolio` field captured at bronze time by
`enumerate_account_dimensions()` (see §3.5). Fidelity's
account-selector groups every account under a portfolio label
that is stable across logins:
- `Authorized` → `trust`
- `Education` → `self`
- `Fidelity Charitable® Giving` → out-of-scope (DAF, auto-
  excluded by account-id length before walk reaches it)

### 3.5 `account_dimensions` capture

At the start of every walk, before any phase runs, we read the
account-selector's `<section aria-label="<portfolio>">` blocks
to capture three dimensions per account:

- `portfolio` — the section's `aria-label` (e.g. `Authorized`,
  `Education`); maps directly to the `owner` discriminator at
  silver-load time. Mirrors UBS PSN's `relationship_id`
  dimension: a single login, multiple parallel ownership
  contexts.
- `nickname` — the user-visible link text inside each
  `apex-kit-web-link`, with the account-id digit-sequence
  stripped (so e.g. a display name
  `<Nickname> NNNNNNNNN , balance: $...` becomes just `<Nickname>`).
  Accounts under a trust agreement can share one generic label;
  only the agreement number tells them apart, and it is not
  promoted.
- `account_id` — the 9-digit account number from the
  `data-testid` (`ap143528-accounts-selector-account-link-
  <id>`).

Persisted in `run.json` under `account_dimensions`, keyed by
`account_key(account_id)` so the run.json doesn't itself leak
raw 9-digit ids if viewed standalone. The mapping back to
canonical id lives inside the per-phase CSVs.

## 4. Silver schema

Materialised in `migrations/0001_initial.sql`, evolved by numbered
migration files in the same directory. The loader (`load.py`)
applies any pending numbered migration on every run, so silver
databases always conform to the latest schema.

### 4.1 Tables

| Table | Archetype | PK | Promoted columns |
| --- | --- | --- | --- |
| `schema_meta` | meta | `silver_schema_version` | `applied_at` |
| `dump_runs` | meta | `snapshot_at` | `silver_schema_version`, `run_dir`, `mode`, `activity_since`, `activity_until`, `positions_present`, `activity_present`, `documents_present` |
| `portfolios` | snapshot | `(snapshot_at, portfolio_external_id)` | `kind` (`529` / `trust_managed` / `other`); rest in `payload` |
| `accounts` | snapshot | `(snapshot_at, account_external_id)` | `portfolio_external_id`, `nickname`, `management_style`; rest in `payload` |
| `positions` | snapshot | `(snapshot_at, account_external_id, instrument_key)` | `description`, `quantity`, `last_price`, `current_value`, `cost_basis_total`, `average_cost_basis`, `type`, `currency`, `asset_class`, `is_core_position`; dividend-view fields (`ex_date`, `amount_per_share`, `pay_date`, `distribution_yield`, `sec_yield`, `est_annual_income`); rest in `payload` |
| `transactions` | event | synthetic `activity_id` (SHA-256 prefix over the row's structural columns + a per-file occurrence index; §3.3 — file-independent so overlapping windows collapse) | `timestamp`, `account_external_id`, `kind`, `instrument_key`, `quantity`, `price`, `amount`, `settlement_date`, `currency`, `source_sha256` |
| `documents` | event | `content_sha256` | `snapshot_at` (first observation), `file_path`, `file_name`, `size_bytes`, `doc_kind` (`statement` / `tax_form` / `balances_html` / `performance_html`), `file_format`, `tax_year`, `account_external_id` |
| `historical_position_snapshots` | snapshot | `(as_of_date, account_external_id, description)` | `instrument_key` (cross-walked from `positions.description` when available; NULL otherwise), `quantity`, `price`, `market_value`, `percent_of_total`, `currency`, `source_sha256`; rest in `payload`. Populated from two PDF archives — scraped 529 statements (`pdf_parsers.parse_statement_pdf()`) and statements supplied out-of-band under `<bronze-dir>/supplied-statements/` (`pdf_parsers_supplied.parse_supplied_statement_pdf()`). See §4.5. |

Notes:
- `currency` defaults to `'USD'` on both `positions` and
  `transactions`. Fidelity US is USD-only today; the column is
  load-bearing the day a foreign-fund holding shows up.
- `asset_class` is the loader's best-effort classification from
  the Symbol shape: `money_market` / `bond` / `plan_fund` /
  `mutual_fund` / `equity`. See §4.3.
- `is_core_position` is `1` for money-market core sweep funds.
  Bronze emits these with a `*` / `**` suffix on the Symbol
  column; the loader strips the suffix so `instrument_key` joins
  cleanly across `positions` and `transactions` and promotes the
  channel signal to this flag. See §4.3.

### 4.2 Snapshots vs events

`portfolios`, `accounts`, `positions` follow the snapshot
archetype: PK begins with `snapshot_at`, INSERT OR REPLACE per
load. Every dump's view of the master data is preserved.

`transactions` follows the event archetype: idempotent INSERT
OR REPLACE on the synthetic `activity_id`. The ID is derived
purely from the row's structural columns (§3.3) plus a per-file
occurrence index — **not** from the
source-file sha256 or the CSV row index, both of which vary
between download windows. This is what lets the same transaction
re-downloaded across overlapping windows / repeated runs collapse
onto one row. The per-file occurrence index preserves genuinely-
repeated identical rows within a single export (e.g. two same-day,
same-amount fills): every file covering a given day sees that
day's complete row set, so the Nth identical copy is assigned the
same occurrence index in every file.

`documents` follows the event archetype: PRIMARY KEY on
`content_sha256` so the same PDF / HTML across multiple dumps
collapses to one row whose `snapshot_at` is the first dump that
observed it. PDF regeneration (same logical doc, different bytes)
is the open question in §11.1.

### 4.3 Cash routing + asset_class

Fidelity surfaces money-market core positions (`CORE_X`, `CORE_Y`,
`CORE_Z`, similar) as ordinary rows on the positions CSV with a
`*` or `**` suffix on the Symbol column — a channel signal
marking the cash-sweep core. The loader **strips** the suffix
on the way into silver and promotes the signal to a separate
`is_core_position INTEGER` column. Without this normalisation the
same fund would have two identities across tables (`CORE_X**` in
positions, `CORE_X` in transactions); with it, `instrument_key`
joins cleanly.

`asset_class` is the loader's best-effort classification of the
Symbol shape into a small enum:

| `asset_class`  | Heuristic                                        | Example          |
| --- | --- | --- |
| `money_market` | `is_core_position = 1`                           | `CORE_X`          |
| `plan_fund`    | `[A-Z]{3}[0-9]{6}` (529 plan investment option)  | (3-letter prefix + 6-digit code) |
| `bond`         | 9-char alphanumeric, trailing digit (CUSIP-9)    | (8-char base + check digit) |
| `mutual_fund`  | 5-char ticker ending in `X` (industry convention) | `FXAIX`         |
| `etf`          | word-boundary `ETF` in the Description (Fidelity groups ETFs with stocks; the security name is the only signal — the boundary keeps N-ETF-LIX out) | `SPY` "S&P 500 ETF" |
| `equity`       | default fall-through (stocks, ADRs, name-shy ETPs) | `AAPL`          |

The classifier is order-sensitive: `plan_fund` is checked before
`bond` because the plan-fund shape is a stricter subset of
CUSIP-9. Anything Fidelity adds in the future that doesn't match
the above falls into `equity` — the wealthdb config's
`instrument_overrides` pins the class of any holding the shapes
misjudge (e.g. an exchange-traded commodity trust whose name
never says "ETF"). The gold adapter further refines `etf` rows
by underlying exposure (crypto / metal / bond ETFs leave the
`etf` bucket — see wealthdb docs/DESIGN.md §6.8).

The `Type` column on `positions` carries `Cash` / `Margin` — that's
the account margin bucket, NOT an instrument category; it stays
promoted separately on `positions.type`. The `wealthdb` gold
adapter re-routes money-market positions per its per-broker
convention.

### 4.4 Portfolio classification

Fidelity's account selector renders accounts under labelled
groups (`<section aria-label="…">` blocks). `download.py`
captures the label as `portfolio` on each account_dimensions
entry. The loader maps the literal label to a stable `kind`:

| Selector label | `portfolios.kind` |
| --- | --- |
| `Education` | `529` |
| `Authorized` | `trust_managed` |
| anything else | `other` |

`accounts.portfolio_external_id` is the literal label (so gold
can re-derive the mapping if it wants a different taxonomy);
`portfolios.kind` is the loader's normalised classification.
Any future Fidelity group label drops cleanly into `other`
without a schema change.

`accounts.management_style` is derived from the same `kind` —
silver pins what's structurally implied without needing per-
account UI signals (Fidelity emits none — see §11.6):

| `portfolios.kind` | `accounts.management_style` |
| --- | --- |
| `529`            | `automated`                 |
| `trust_managed`  | `discretionary`             |
| `other` / NULL   | NULL                        |

A Fidelity 529 is `automated`, not `self_directed`: the plan offers
only percentage-wise allocation across a small menu of funds and
age-based strategies — a model portfolio, not free security
selection. Migration 0003 originally classified it `self_directed`;
migration 0006 corrected it in place (the loader writes the corrected
value on every fresh/`--force` load).

### 4.5 Historical reconstruction — three PDF sources

`historical_position_snapshots` is populated from **three distinct
statement-PDF archives**, each with its own parser, all feeding
the same silver table — the two below, plus the Donor-Advised
Fund's Giving Account statements (`pdf_parsers_daf`; see §12.3):

**(a) Document-center statements — auto-scraped.** Fidelity's *web
document center* exposes monthly / quarterly statement PDFs for the
account groups it serves statements for, such as 529 plan accounts.
`download.py` scrapes these into
`<dump>/documents/Statement<MMDDYYYY>.pdf`, parsed by `pdf_parsers.parse_statement_pdf()` and wired into
`load.py` via a `ProcessPoolExecutor` worker pool (PDF text
extraction is CPU-bound; SQLite insert stays on the main thread).
The cross-walk from the human-readable fund description to an
`instrument_key` queries the live `positions` table per (account,
description); when the fund hasn't appeared in any live snapshot
yet the row lands with `instrument_key` NULL and the description
preserved for downstream resolution.

**(b) Supplied statements — sourced out-of-band.** An account group
the web document center serves no statements for can still have
statement PDFs supplied out-of-band and dropped in. These use a
different layout (`pdf_parsers_supplied.py`) and are loaded from a
directory rather than the scraped dump tree.

> **Reproducible-from-bronze.** The supplied PDFs default to
> **`<bronze-dir>/supplied-statements/`** — *under* the bronze tree —
> precisely so `load --force` (which wipes silver and rebuilds from
> bronze) re-ingests them automatically. Do not move the default
> outside bronze: sourcing them from an arbitrary external dir
> reachable only via `--supplied-statements-dir` means every `--force`
> rebuild or flag-less nightly reload silently drops that entire
> history, since it would live only in silver with nothing under
> bronze able to rebuild it. Bronze residency is what upholds the
> "silver is reproducible from bronze alone" invariant that
> `silver.reset()` depends on. The flag still works as an override.
>
> A misfiled PDF (a statement for an unrelated account) is rejected by
> a page-1 **signature** check — a substring (the account registration)
> that must appear in the document. The signature is read from `--statement-signature` or, when
> omitted, the first line of **`<supplied-statements-dir>/signature.txt`**
> so the registration (PII) stays in the local data dir, never in
> argv / shell history / a committed orchestration script.

An account that appears in historical statements but in no live
download gets a placeholder `accounts` (+ `portfolios`) master row.
The loader synthesises it at the account's last historical
`as_of_date`, so gold's historical-account projection has something
to join against. Gold's per-source latest-snapshot semantics then
zero the account out automatically for every date after its final
statement (no zombie balances).

### 4.6 Tax-form structured data

Fidelity's Consolidated 1099 is available as **PDF only** from
the Tax forms sub-page. The download anchors carry aria-labels
ending in ` (pdf)` exclusively; an early DOM snapshot suggested
CSV / XML variants may exist for some 1099 types, but live
testing found no such variants surface in practice. Silver
stores the PDFs in `documents` keyed by
`content_sha256` with `doc_kind='tax_form'` and the parsed
`tax_year`. Structured per-lot extraction (1099 detail into a
`tax_form_rows` table) is a follow-up; a parser would mirror
the 529-statement reconstruction implemented in §4.5
(pdfplumber + per-line layout heuristics).

### 4.7 Balances + Performance pages

Two additional Fidelity surfaces beyond positions / activity /
documents:

- **Balances** (`/portfolio/balances`) — no direct CSV / Excel
  / PDF export. The actions menu (`balwebex-actions-menu` in
  some builds, `more-action-menu` in others) offers 'Create
  Balance Letter', 'Print', and 'Glossary of terms'. 'Create
  Balance Letter' opens a multi-step wizard (select letter
  type: 'sample letter balance' or 'sample letter balance in
  excess' → select account(s) → Generate → PDF), with shapes
  that aren't a plain snapshot of the on-screen balance table.
  We persist the rendered `balances.html` instead; per-account
  totals are in `data-testid$='-totalaccountvalue-label'`
  elements (and accessible to the silver loader without
  driving the wizard). The Balance Letter wizard is left as
  a follow-up if a formal as-of PDF is ever required.

- **Performance** (`/portfolio/performance`) — no structured
  export of any kind. The page is a Highcharts SVG plus
  collapsible info tiles (return percentages by period,
  benchmark deltas). The only download-shaped action is
  "Print"; there is no kebab/menu CSV option. The full
  rendered DOM is captured as `performance/performance.html`
  so silver can extract metrics by parsing the rendered text;
  most return metrics are also derivable from positions +
  activity time-series, so the gap is acceptable.

## 5. Architecture: one-shot model

**Fidelity binds its session to the Firefox-process lifetime.**
Confirmed empirically: after a successful login that writes the
profile dir to disk, closing the Camoufox process and reopening
with the same profile dir lands on a signin redirect — Fidelity
treats the cookie as dead even though the file persists. This
matches the [schwab-web session model](../schwab-web/README.md#session-lifecycle)
exactly.

Consequence: **login and scrape share one continuous Camoufox
process.** The sibling-tool "login mints `storageState.json`,
download reuses it across runs" pattern (UBS, Swissquote) does
not apply.

`./fidelity-web download` is a one-shot:

```
./fidelity-web download [flags]
    → docker spawn → Camoufox launch
    → IUA accept (if non-US locale) → signin
    → MFA auto-skip via device-trust cookie, or stdin prompt
    → walk(): positions / activity / documents / balances / performance
    → logout (best-effort) → Camoufox teardown → exit
```

The wrapper mounts `$HERE:/app` so edits to `download.py` on the
host land in the next container spawn — no image rebuild during
iteration. The device-trust cookie suppresses MFA across spawns
for ~30 days, so iteration on the walk phases doesn't burn MFA
pushes.

**Hybrid: Docker `download`, host-venv `load` / `prune` /
`recompress`.** `download` needs Camoufox + Xvfb, so it runs in the
image. The file-only verbs are pure bronze walks that never touch a
browser, and they run host-side on a venv the `.host-venv` marker tells
the Makefile to build (from `requirements.txt`, with `collectorkit`
editable-installed). Running host-side saves the docker-spin overhead
and bypasses the single-writer safety guard, so a `load` can run while a
`download` container is mid-flight. The host venv exists (rather than
bare `python3`, as it did before bronze compression) because `load`
decompresses zstd bronze **in Python** — fidelity-web's silver is
SQLite, with no engine to stream `.csv.zst` natively the way
cointracking's DuckDB does — so the host interpreter needs the
`zstandard` package (and it already needed `pdfplumber` for the
statement-PDF parsing). The wrapper runs these via `host_python`
(`shared/wrappers/host-lib.sh`).

### 5.1 Session timeout

Empirically observed: **Fidelity's idle timeout is ~15 minutes**.
Since each run is bounded by login + walk + logout, this is only
relevant when the walk itself exceeds 15 min idle (e.g. waiting
for human stdin at the 2FA prompt). The walk functions
defensively check the post-auth URL prefix after navigation and
abort the affected phase cleanly if Fidelity has redirected to a
session-timeout modal.

## 6. Anti-bot configuration (Camoufox + behavioural mimicry)

The working config: **Camoufox + `os="macos"` + `humanize=True` +
`geoip=True`**, headed against Xvfb. This combination gets the
login flow past Akamai Bot Manager (Fidelity's bot-detection
vendor). The empirical diagnostic chain is below.

The escalation history:

| Rung | Configuration | Result |
| --- | --- | --- |
| 1 | Vanilla Playwright Chromium | Akamai-blocked at credential submit (`dom-system-error-header` "Sorry, we can't complete this action right now"). |
| 3a | Camoufox + `os="macos"`, no behavioural mimicry | Same block. The static fingerprint stack alone was not enough. |
| 3b | + `humanize=True` (cursor trajectories) + `geoip=True` (locale/tz match egress IP) + **one initial VNC-driven login** to seed the Akamai trust cookie | Works. Subsequent auto-driven logins (CLI-MFA or auto-MFA-skip via device-trust cookie) succeed. |

The first-ever login on a fresh profile dir requires the VNC
handoff — clicking "Log in" by hand in a VNC client generates
real Firefox-sourced mousedown/mouseup events that satisfy
Akamai's behavioural-score gate. Once Akamai's trust cookie is
in the profile dir, automated clicks on the button work fine.

The `--vnc` subcommand drives this: pre-fills the credentials,
prints a READY banner, polls for the post-auth URL while the
login + 2FA are completed by hand over VNC.

## 7. Login + MFA flow

Default flow (`./fidelity-web download`), assuming a profile
dir that already has Akamai trust + Fidelity device-trust
cookies:

1. Launch Camoufox (rung 3b).
2. Navigate to `https://digital.fidelity.com/prgw/digital/signin/`.
3. Detect either signin form (US locale) or International Usage
   Agreement interstitial (non-US locale, when geoip=True trips
   it); click "I Accept" on the IUA if served.
4. Fill `#dom-pswd-input` with `FIDELITY_WEB_PASSWORD`. Username field
   varies by device-known state (text input vs `<select>`); the
   script tries both paths.
5. Click `#dom-login-button`.
6. Either:
   - Device-trust cookie suppresses MFA → straight to post-auth.
   - 2FA prompt (TOTP-style 6-digit code in
     `#dom-totp-security-code-input`) → prompt on stdin, fill,
     submit.
7. Wait for the post-auth URL prefix
   `https://digital.fidelity.com/ftgw/digital/portfolio/` to
   appear (using `live_url()` via `location.href` — see workaround
   note in §6).
8. Run `walk()` for the requested mode(s).
9. Best-effort `logout()` (click visible Log Out link).
10. Camoufox context teardown → process exit.

`--no-trust-this-browser` skips ticking the "Trust this browser"
checkbox at the 2FA page (useful for repeatedly exercising the
MFA flow during development).

`--check` loads the profile dir and navigates to the post-auth
landing without re-logging — reports session ALIVE / DEAD,
skips walk + logout.

`--vnc` (via the `vnc-login` subcommand, or directly) pre-fills
the credentials but waits for Log In to be clicked by hand over
VNC. After the post-auth URL lands, the walk runs as normal (or
`--mode none` skips it for a profile-dir-seed-only flow).

## 8. UI surface map

URLs and selectors anchored to the live DOM as of 2026-05-24/25.

### 8.1 URLs

| Page | URL |
| --- | --- |
| Signin SPA | `https://digital.fidelity.com/prgw/digital/signin/` |
| Post-auth landing | `https://digital.fidelity.com/ftgw/digital/portfolio/summary` |
| Positions SPA | `https://digital.fidelity.com/ctgw/digital/positions/poswebex/client/` (we use the wrapper URL at `/ftgw/digital/portfolio/positions` which routes through) |
| Activity & Orders | `https://digital.fidelity.com/ftgw/digital/portfolio/activity` |
| Documents hub | `https://digital.fidelity.com/ftgw/digital/portfolio/documents` — now 302s to the Enterprise Document Center at `https://digitalservices.fidelity.com/navigate/ent-documentcenter/statements` (a **different host**). The documents phase accepts that host as a valid landing (`DOCCENTER_PREFIX`); only a redirect to `/prgw/digital/signin` is treated as a session timeout. |
| GraphQL endpoint | `https://digital.fidelity.com/ftgw/digital/portfolio/api/graphql` (not used; flagged as a future-iteration alternative to HTML scraping) |

### 8.2 Login

| Element | Selector |
| --- | --- |
| Username (returning-device dropdown) | `#dom-select-username` (native `<select>` with `value="default"` = "Enter different username") |
| Username (fresh-device text input) | `input#dom-username-input`, then `input[autocomplete~=username]`, then the older ids. **`~=`, not `=`** — the attribute is a space-separated token list (`autocomplete="username webauthn"`), so an exact match misses it the moment Fidelity adds a token |
| Password | `#dom-pswd-input` |
| Submit | `#dom-login-button` |
| 2FA code | `#dom-totp-security-code-input` |
| Trust-this-browser checkbox | `<input type=checkbox id="dom-trust-device-checkbox">` — must click the `<label for="dom-trust-device-checkbox">` (input is intercepted by the PVD label overlay) |
| IUA accept | `a.accept-link` (calls `javascript:acceptAgreement()`) |

**A credential that does not land is never submitted.** Both fields
are filled and then READ BACK, and re-filled if the value did not
stick: these are PVD web components, and the framework can overwrite a
value when it hydrates, leaving the field empty while `Locator.fill`
reports success. `click_login` checks both fields once more before
submitting, because the re-render that empties one can happen between
the two fills.

That guard is a safety rule, not tidiness. A blank submit is not a
no-op — Fidelity counts it as a failed sign-in attempt, so a run that
makes one nightly walks the account towards a lockout while reporting
only that no post-auth URL appeared within 15s. When the username
selector drifted, that is exactly what happened: every candidate
missed, a last-resort "first visible text input" filled without
verifying, and the run's own diagnosis pointed at anti-bot blocking.
The last-resort branch still exists and still names itself in the log
— a run filling through it has a drifted selector and is one render
away from filling the wrong box.

### 8.3 Positions

| Element | Selector |
| --- | --- |
| Account selector container | `[data-testid="ap143528-accounts-selector-container"]` |
| Per-account link | `[data-testid="ap143528-accounts-selector-account-link-<ID>"]` |
| All-accounts toggle | `.acct-selector__all-accounts` |
| Preset-view dropdown (native `<select>`) | `[data-testid="preset-views-dropdown"] select` — values: `Overview`, `DividendView`, `FundPerfView`, `ClosedPositionsView`, `MyView`, `EditMyView` |
| Kebab menu (download trigger) | `[data-testid="kebab-menu"]` |
| Download menuitem | matched via `role=menuitem[name=/download/i]` after kebab open |

**Per-account navigation does NOT filter the positions CSV.**
The all-accounts view's Download exports a consolidated CSV with
rows for every visible account. Two CSVs per scrape (one per
preset view); account dimension lives in the `Account Number`
column.

**CSV header drift (observed 2026-07-10, diagnosed 2026-09-02).**
Fidelity re-cased every positions-CSV header from title case to
sentence case (`Account Number` → `Account number`, `Last Price` →
`Last price`, …) and renamed the dividend view's `Dist. yield` /
`Distribution yield as of` to the `rate` spellings. The download side
was unaffected (files kept landing, `run.json` reported ok) but the
loader's exact-name lookups matched nothing, silently zeroing the
positions load for ~8 weeks until the DAF work surfaced it. The
loader now resolves columns case-insensitively (`_row_ci`) and
accepts both yield/rate spellings, `validate()` warns loudly whenever
positions-covering dumps keep landing while the retail positions
table stops advancing, and a `load --force` rebuild recovers the
whole gap from bronze. The activity CSV headers were spared this
round; their lookups are case-insensitive now too.

### 8.4 Activity & Orders

Fidelity rebuilt this page on the **`fds-*` design system** (Angular
host, Lit components, AG Grid table) in 2026-07. Every selector below
carries the current generation first and the ones before it as
fallbacks, because the rebuild has rolled forward and back before.
Three properties of the new page are load-bearing and easy to miss:

- **ids are generated per render** (`segment-444046273391`,
  `button-968185178406`, `form-7107669447`), so nothing may key on
  one. The stable handles are `data-testid`, a custom element's own
  light-DOM id, and the value of a radio.
- **native controls live in shadow roots.** An `fds-button`'s real
  `<button>` is inside one, so a CSS match for the inner element can
  miss it and an actionability click can read it as hidden. Clicks go
  to the native button *through* the shadow root.
- **the label lies about the data.** The time filter's label updates
  the instant Apply is pressed; the rows behind it arrive later, and
  the CSV is generated from whatever the table holds. See §8.4.1.

| Element | Selector |
| --- | --- |
| Filter dialog trigger | `button:has-text('Filter')` (also `[aria-label='Filter']`) |
| Time-period filter pill | `[data-testid='filter-by-time-button']` (expands panel `[id^='time-filter-panel']`; legacy `[data-testid='ap143528-timeperiod-filter']`) |
| Picker open state | `aria-expanded='true'` on the pill (legacy: the Recent/Custom radios existing at all) |
| Preset day-count radios (Recent tab) | `input.fds-radio__radio[name='recent-options']` (legacy `helios-radio[pvd-value='<N>']`) |
| Custom tab | `input.fds-segment__radio[type='radio'][value='custom' i]` — matched case-INSENSITIVELY because the generation before spelled the same value `Custom`; then `input#Custom[type='radio']`, then legacy `apex-kit-segment[pvd-value='Custom']` |
| Custom-tab date inputs | `#input-from-date` / `#input-to-date` (legacy `#customized-timeperiod-from-date` / `-to-date`). HTML5 `<input type="date">`, ISO YYYY-MM-DD. The current generation sets **no `min`/`max`** — the retention floor is enforced by the panel's own validation instead, so the bounds probe finds nothing to clamp to |
| Apply (Custom tab) | `form:has(#input-from-date) button[type='submit']` — the button is unlabelled and its id is per-render, so it is addressed through the form the date inputs sit in (legacy `button[aria-label='Apply Customized Time Period']`) |
| Download dropdown trigger | `button[aria-label='Download']`. Disabled while the SPA re-fetches; the busy state is now `fds-disabled` on the wrapping custom element, NOT the native `disabled` attribute — reading only the native one reports a busy button as ready |
| CSV item inside popover | `#download-csv-button` — the custom element's own light-DOM id, the one stable handle (legacy `#downloadContent button:has-text('CSV')`) |

#### 8.4.1 The table lags the filter by a whole apply

A freshly-applied Custom range hands back the **previous** range's
rows — exactly, repeatably, one apply behind. Three windows requested
in sequence exported the default 30 days, then window 1's rows, then
window 2's.

Nothing on the page reports this. The filter's label updates the
instant Apply is pressed, `networkidle` is satisfied while the old
rows are still loaded, and the XHR that Apply fires completes before
the grid re-renders. Each was tried and each passed while the export
was stale.

So the export is judged on its CONTENT, which is the only thing here
that cannot lie about which filter produced it:
`activity_export_matches` requires an export's own rows to fall inside
the window it was asked for, and a window that fails is pulled again
after a short settle (`ACTIVITY_EXPORT_ATTEMPTS`). In practice the
second pull always lands. A window that never comes back with its own
rows is recorded `ok: false` with the span it actually held — the rows
themselves are real and would load, but the window is not covered, and
saying otherwise is how a hole hides.

A window with no rows matches by default: a quiet window is
legitimate, and treating it as stale would retry for ever.

#### 8.4.2 Why windows are capped at 30 days, not 93

Fidelity caps a Custom range at 93 days per export, but
`MAX_ACTIVITY_WINDOW_DAYS` is **30**, and that is a correctness rule.

The CSV is generated from whatever the table currently holds. The
filter's label updates the moment Apply is pressed — well before the
rows behind it arrive — so an export taken in that gap is the
*previous* filter's data under the new window's name. Waiting on
`networkidle`, on the label, or on the request Apply fires all proved
insufficient: each can be satisfied while the table is still the old
one.

Asking only for windows no wider than the page's own default filter
("Past 30 days") makes the stale answer harmless. A stale export is
then always a **superset** of what was asked for, never a truncation:
extra rows are a no-op because an activity row's id is derived from
its content and reloading it replaces itself, whereas missing rows are
a hole nothing reports. A 93-day window had the opposite property and
came back silently holding 30 days.

Every window's result also records `rows` and the `covers` span the
CSV actually holds, so an export that came back short — or empty,
which a header-only CSV looks exactly like from the outside — is
visible in `run.json` rather than reading as a success with a file
beside it.

**Activity export is consolidated.** Clicking the account-selector
does NOT scope the resulting CSV — the same CSV comes down
regardless of selector state. One CSV per window, all accounts
inside (Account Number column as the per-row discriminator). The
silver loader fans out per-account from there.

#### 8.4.3 `--lookback all` and the missing bounds

`_probe_activity_date_bounds` clamps a backfill to the retention the
page publishes on its date inputs' `min`/`max`. The current
generation sets NEITHER, so the probe comes back empty as a matter of
course rather than as a fault, and `ACTIVITY_RETENTION_FLOOR_DAYS` is
the fallback floor. Without it `--lookback all` asks for thirty years
and chunks every one of them into 30-day windows, most of them past
anything Fidelity holds.

### 8.5 Documents

Fidelity migrated the document center to the **Enterprise Document
Center** (`digitalservices.fidelity.com/navigate/ent-documentcenter`),
a Stencil/PVD component SPA. Statements and Tax forms share one
shape; both are driven by `_doccenter_*` helpers in `download.py`.

| Element | Selector |
| --- | --- |
| Document-type rail link | `a[href='statements']` (= "Personal" statements), `a[href='tax-forms']`, `a[href='ip-statements']` (interested-party — often empty). The default landing is `ip-statements`, which is empty, so the type MUST be switched. |
| Document-type `<select>` | `select[aria-label='Document selector']` — present but **ignored**: the Stencil component doesn't react to programmatic value writes, which is why the rail link is used instead. |
| Date filter | `#options-select-TimeFilter` (native `<select>`; "Last 3/6 months" + concrete years). Driven via Playwright `select_option`, then Apply. |
| Apply | `button.pvd-button--primary:has-text('Apply')` |
| Document row | `ent-ds-link` whose text ends in `(pdf)` (e.g. `"Jan-March 2026 — Statement (pdf)"`, `"Consolidated Form 1099 (pdf)"`). No stable `data-testid`/href — the href is `javascript:void(0)`. |

**Download mechanism (API-driven).** The center is backed by a JSON
API. Clicking a row fires an authenticated **POST** to
`.../retail-am-financialdoc/.../financial-documents/download` whose
body identifies the document (`{id, formatType:"PDF", docType, …}`;
statements use the `digitalservices` host + `/v2`, tax forms use
`dpservice.fidelity.com` + `/v1` and add `acctNum`/`requestor`). The
response is JSON carrying the PDF as **base64 in
`document.docDetail.content`**; the SPA decodes it to an in-memory
`blob:` and opens that in a viewer tab. Chasing the rendered blob is
fragile (revoked URLs, viewer-context fetch errors), so
`_doccenter_download_row` instead waits for the JSON response via the
canonical `page.expect_response`, then base64-decodes the content
(`_pdf_from_docapi_body`). No popup/blob/`context.request` dance.

**Dedup.** Within a type, every visible `(pdf)` row is downloaded by
index and deduped on **PDF content hash**, not label: tax forms
repeat one label (`"Consolidated Form 1099 (pdf)"`) across accounts
(distinct documents, distinct bytes), while a statement reappearing
under several year filters is identical bytes. Statement-row labels
carry a year used for the `min_year` floor; tax-form labels don't and
are always kept.

**Scope filter.** Only the personal Statements + Tax-forms types are
walked; interested-party / prospectus / proxy categories are out of
scope (per CLAUDE.md §1).

**Householded statements → 529 parsing.** The Statements type returns
**householded** combined Investment Reports (`isHouseholded:true`):
one PDF per period that can cover several accounts of one household,
each in its own section, including **EDUCATION (529)** sections.
`_doc_stem` therefore names statement downloads
`Statement_<period>.pdf` so the silver loader's 529 historical path
(`Statement*.pdf` glob → `pdf_parsers.parse_statement_pdf`) parses the
529 holdings into `historical_position_snapshots` (§4.5). An account
outside the household surfaces as interested-party instead, which the
walk leaves alone; its history can come from
`<bronze-dir>/supplied-statements/`.

### 8.6 Balances + Performance

Two surfaces with no structured export. `download.py` persists
the rendered HTML; silver scrapes from there.

| Page | URL | Surface notes |
| --- | --- | --- |
| Balances | `/ftgw/digital/portfolio/balances` | Per-account totals in `[data-testid$='-totalaccountvalue-label']`. Actions menu (`balwebex-actions-menu` in some builds, `more-action-menu` in others) offers a multi-step "Create Balance Letter" wizard (select letter type → select account → Generate → PDF), 'Print' (browser dialog), and 'Glossary of terms' — none are plain snapshots, so we skip the menu entirely. |
| Performance | `/ftgw/digital/portfolio/performance` | Highcharts SVG + collapsible info tiles. No kebab/menu Download item; the only data path is parsing the rendered DOM (return percentages by period, benchmark deltas). |

## 9. What we do NOT do

- OFX — dead (§1.1).
- Akoya / FDX / Plaid / SnapTrade — B2B-only.
- Prospectuses / supplementary documents.
- Trade / transfer / config writes — see [CLAUDE.md](CLAUDE.md) §1.
- MFA automation — human-in-the-loop on every truly-fresh login.
- Cross-bank semantic alignment — gold's job.
- Third-party investment-manager data sources — out-of-band; their own future collector when needed.

## 10. Implementation status

| Step | Status |
| --- | --- |
| Container scaffolding + design docs | done |
| `download.py` — IUA gate, MFA, trust-device, profile dir, one-shot login → walk → logout | done |
| `download.py` — positions Overview + DividendView (consolidated CSVs) | done |
| `download.py` — activity preset 'Past 90 days' (page-level pill → radio → Apply Recent → networkidle) | done |
| `download.py` — activity Custom-range backfill (Custom tab, ISO date inputs, retention-clamped, bisected into ≤93-day windows) | done |
| `download.py` — documents: statements + tax forms via the Enterprise Document Center (rail-link type switch, year filter, row click → `financial-documents/download` JSON → base64 PDF; content-hash dedup) | done — see §8.5 |
| `download.py` — `--explore`: shadow-/iframe-piercing DOM inventory for doc-center UI-drift debugging | done |
| `download.py` — `--debug` gate on walk-phase captures (off by default; `--explore` implies it) | done |
| `download.py` — balances + performance HTML capture (no structured export available on either surface) | done |
| `prune.py` — delete debug captures + non-complete dumps from bronze (`--dry-run` plan mode, in-flight age guard) | done |
| `migrations/0001_initial.sql` + `load.py` (positions, transactions, portfolios, accounts, documents; validation pass) | done |
| `migrations/0002_*.sql` (currency + asset_class + is_core_position; drop cosmetic `*_present` flags) | done |
| Statement-PDF parser (529 historical reconstruction) | done — `pdf_parsers.py` + migration 0004 populate `historical_position_snapshots` |
| Per-account `account_registration` | deferred — see §11.5 |
| `migrations/0003_*.sql` (`accounts.management_style` derived from `portfolios.kind`: 529 → `automated` (via 0006; 0003 first wrote `self_directed`), trust_managed → `discretionary`) | done — see §4.4 / §11.6 |
| `migrations/0006_*.sql` (correct 529 `management_style` → `automated`) | done — see §4.4 |
| `download.py` — Donor-Advised Fund phase (SSO hop → JSON REST API → CSV exports + PDF documents; rides modes `all`/`positions`) | done — see §12 |
| `migrations/0007_*.sql` + `load._load_daf` (DAF bronze → shared silver tables; `portfolios.kind='daf'`) | done — see §12.3 |
| `wealthdb` DAF taxonomy (`donor_advised_fund` kind + `charitable` wrapper, gold migration 0036, adapter mapping) | done — see §12.3 |
| `pdf_parsers_daf.py` + `load._load_daf_historical` (Giving Account statement PDFs → `historical_position_snapshots`, reconciliation-gated) | done — see §12.3 |
| `wealthdb` Fidelity adapter | sibling component (`wealthdb/`) |

## 11. Open questions

### 11.1 PDF regeneration
Does Fidelity regenerate statement / 1099 PDFs per request
(different sha256 each time, same logical content) like Schwab
does? Resolve by downloading the same statement twice and diffing
sha256s. The silver loader dedups on `content_sha256`, which
assumes stable hashes; if they turn out to be regenerated, switch
to `(account, doc_date, doc_kind, filename)` dedup à la
schwab-web.

### 11.2 GraphQL endpoint
`https://digital.fidelity.com/ftgw/digital/portfolio/api/graphql`
is reachable from the post-auth session. Schema unknown; could
provide cleaner / more stable access than HTML scraping for
positions and activity. Out of scope for v1; flagged for a
future-iteration alternative.

### 11.3 Balance Letter wizard
The Balances actions menu's 'Create Balance Letter' opens a
multi-step wizard (select letter type → select account →
Generate). The letter types (`sample letter balance`,
`sample letter balance in excess`) are compliance / eligibility
artefacts, not a plain snapshot of the on-screen balance table,
so we skip the wizard. Deferred unless a formal as-of PDF is
later needed — the per-account totals are already accessible
from the persisted `balances.html`.

### 11.4 Third-party-manager institutional feed
Out-of-band; such a feed would live in its own collector
(see §1.3), so it blocks nothing here.

### 11.5 Per-account registration label (`account_registration`)
The wealthdb gold layer's three-column account taxonomy wants a
promoted `account_registration` on `accounts` — the Fidelity-
exposed wrapper label (`Roth IRA`, `Traditional IRA`,
`Coverdell ESA`, `Health Savings Account`, `Joint WROS`,
`Individual TOD`, etc.).

**Finding (2026-05):** Fidelity does NOT emit a per-account
registration label on any surface we currently scrape. Sweeping
the captured DOM for keyword shapes (`Roth IRA`, `Coverdell`,
`HSA`, `TOD`, `WROS`, `Joint`, `Individual`, `Trust`, …) across:

* account-selector sidebar (`balances.html`, `performance.html`,
  every `screenshots/*-positions-*.html`)
* per-account balances cards (`<acct>-totalaccountvalue-label`,
  `<acct>-currentvalue-label`, … no `-registration-label` or
  similar)
* positions / activity / documents page bodies
* the consolidated positions CSV (`Account Name` carries only
  the account nickname)

…surfaces no structured registration tag. The keyword hits we DO
find ("Health Savings Account", "Strategic Disciplines",
"Fidelity Wealth Services") are all in page-wide disclosure /
footer text, not per-account tags.

For the account categories silver currently models, the
registration is implied by `portfolios.kind`:

| `portfolios.kind` | Implied registration                         |
| --- | --- |
| `529`            | 529 College Savings Plan participant account |
| `trust_managed`  | Trust account, managed                        |

Other account types (retirement, brokerage) would extend this
table. The most likely place a registration label
surfaces is the dedicated `/portfolio/accounts/<account-id>`
detail page, which `download.py` does NOT currently visit. The
path forward when that signal materialises:
1. Extend `download.py` to nav each in-scope account-id detail
   page; capture `account-registration` (or similar testid)
   into `run.json/account_dimensions[*].registration`.
2. Migration to add `accounts.account_registration TEXT`.
3. Loader populates the column from the captured value.

Until then, gold treats `portfolios.kind` as the registration
proxy and the column stays unimplemented.

### 11.6 Advisory vs discretionary within `trust_managed`
Migration 0003 promotes `accounts.management_style`, derived
from `portfolios.kind` (see §4.4; 0006 corrected the 529 value):
- `529` → `automated`
- `trust_managed` → `discretionary`
- other / unknown kind → NULL

What's NOT derivable: the advisory-vs-discretionary distinction
inside `trust_managed`. Fidelity's relevant product taxonomy
(`Fidelity® Wealth Services`, `Fidelity® Strategic Disciplines`,
`Portfolio Advisory Services`, `Fidelity Personal and Workplace
Advisors`, the generic `Managed Accounts` umbrella) appears
ONLY in page-wide disclosure / footer / marketing-sidebar text
across every captured surface — none of it surfaces as a
per-account tag. The current default of `discretionary` is the
common case for the trust-account pattern silver models; gold
can override per-account if it has out-of-band knowledge that
a specific trust agreement is an advisory rather than
discretionary arrangement.

If a future Fidelity build emits a structured indicator —
candidates: a `data-testid$='-managed-by-label'` on the
account-detail page, or an `aria-label` on the section header
that names the advisory product — the path mirrors §11.5:
extend `download.py`, schema migration, loader populates.

## 12. Donor-Advised Fund extension

A Fidelity Charitable Donor-Advised Fund appears in the account
selector under `Fidelity Charitable® Giving`. It sits behind a
distinct web UI, so this surface followed the fleet discovery
playbook (NEW-COLLECTOR-PROMPT.md): a human-driven `explore` session
(§12.1) mapped it, and the DAF walk phase (§12.2) was built from the
captures. The retail phases auto-exclude the DAF by account-id length
(§1.2/§3.1; it has no brokerage-side surfaces); the DAF phase enters
it via the charitable API.

`explore.py` is the discovery harness — copy-adapted from the firstcitizens
sibling (the tracked convention: per-collector copies, no shared
library). It opens the standard signin on the shared
`/secrets/fidelity-web-profile` (so Akamai + device trust carry over
and the session costs at most a 2FA code), pre-fills the known login
form (§8.2; fill gated to fidelity.com hosts), and records the
VNC-driven session: crash-safe `network.jsonl` with text response
bodies, `clicks.jsonl`, browser downloads, and structure-deduped DOM
snapshots for every fidelity.com / fidelitycharitable.org|com frame.
Distinct from `download --explore`, which only adds DOM inventories
along the scripted walk. Allow/forbid surface: CLAUDE.md §1a —
Grant / Contribute / Exchange are the DAF's money-movement controls
and are never clicked, in discovery or ever.

### 12.1 Surface map

**Entry / SSO.** The portfolio's account-selector DAF link lands on
`https://charitablegift.fidelity.com/cgfweb/CGFLogon.cgfdo?Ref_at=ng`,
which consumes the existing `.fidelity.com` session cookie — no
second credential, no extra challenge — and redirects into the donor
SPA at `/cgfweb/fc-donor/?Ref_at=ng`. The SPA routes by URL hash
(`#/Dashboard/summary`, `#/History/grant-history`,
`#/History/statements-confirmations`, `#/Invest/view-investment-
selections`, …).

**Technology + bot defense.** An Angular SPA over a clean JSON REST
API rooted at `/fc-services/api/v1/`. Akamai sensor beacons are
present on the host (the random-path `POST`s of Bot Manager), so the
browser-everywhere runtime stays; data calls themselves are plain
cookie-authenticated GETs — `page.request` territory, no DOM
scraping needed. Every observed API call returned 2xx; no challenge
fired mid-session.

**Identity bootstrap.** `POST /fc-services/api/v1/identity/self`
returns `{loginKeyId, partyId}`; `GET /user/<partyId>/accounts` is
the giving-account roster (per-account `accountNbr`, `gaName`,
`gaBalance`, `role`, `privileges`, `asOfDate`). The giving-account
number is 7 digits, confirming §3.1's exclusion criterion.

**Data endpoints** (all GET, query-param-driven):

| Surface | Endpoint | Notes |
| --- | --- | --- |
| Account master | `givingAccounts/<acctNbr>` | balance + `balanceDate`, pending buckets (grant / contribution / adjustment / …), YTD + two prior-year contribution and YTD grant totals, program-membership flags, registration code |
| Pool positions | `poolBalances?accountNumber&endDate&numberOfDays=1` | per pool: `poolId`, `poolName`, `poolCategory`, `unitQuantity`, `poolUnitPrice`, `marketValue`, `percentage`; plus `poolPriceDate` + `totalMarketValue`. `numberOfDays` suggests a history series — unverified (§12.4) |
| Grant history | `transactionHistory/grants?accountId&page&size` (+`fromDate`/`endDate` or `viewingFilter=SINCE_INCEPTION`) | paginated envelope (`totalItems`, `items[]`); each item carries `grantId`, `charityId`, `charityName`, `taxId`, `amount`, submit/approval/settlement/check dates, check status, EFT flag, `designation`, `acknowledgement` |
| Grant detail | `transactionHistory/grants/<grantId>` | |
| Contribution history | `transactionHistory/contributions?accountId&…` | same envelope; detail at `/contributions/<id>-1` |
| Pool exchanges | `poolExchange?accountNumber&year` | year-scoped |
| Gift4Giving | `gift?donorAccountNumber&page&size&year` | paginated envelope |
| Adjustments | `transactionHistory/adjustment?accountNumber&year&page&size` | year-scoped ("all other transactions") |
| Document listing | `document?accountNumber&documentType&fromDate&endDate` | types observed: `STATEMENT` (quarterly + year-end), `GRANT` (donor grant confirmations), `CONTRIBUTION`, `FORM_8283` (IRS form). Rows carry `id`, `documentName`, `correspondenceDate`, `generatedDate`, and the composite `legacyDocumentKey` the download needs |
| Document fetch | `document/download?accountNumber&legacyDocumentKey&documentType` | returns `application/pdf` directly — no base64-in-JSON dance (unlike the retail document center, §8.5) |

**Server-side CSV exports.** Every history surface also has a
`…/download?format=csv` twin: `transactionHistory/grants/download`
and `…/contributions/download` (both accept
`viewingFilter=SINCE_INCEPTION` — full history in one GET),
`poolExchange/download?year`, `gift/download?asOfDate`,
`transactionHistory/adjustment/download?year`, and
`poolBalances/download`. The UI serves them as `blob:` downloads;
the underlying response is the CSV. Shape: a title line, preamble
rows (account name, established date, view, as-of), then the header
row. The JSON is richer than the CSVs (ids, charity ids, status
detail), so per fleet lessons JSON is the primary silver source and
the CSVs are corroborating bronze.

**Session model.** The hop rides the one Camoufox process; nothing
suggests a DAF-side session independent of the retail one
(browser-lifetime binding, §5, presumed to apply across the hop).

### 12.2 Walk phase (built)

`scrape_daf` in `download.py` — runs whenever the account enumeration
carries a Fidelity Charitable relationship (a retail-only login skips
the SSO hop and writes nothing): the full phase in `--mode all`, and
a master + pool-balances slice (`positions_only`) in
`--mode positions`. There is deliberately **no DAF-only mode** — the
completeness invariant (§12.3) requires every positions-bearing dump
to cover both channels. The full phase:

1. Takes the SSO hop (`_daf_navigate_sso`): navigates
   `CGFLogon.cgfdo`, waits for a `charitablegift.fidelity.com`
   landing, and returns `no-daf` if it bounces to signin instead
   (session timeout, or a login with no charitable relationship).
2. Bootstraps auth (`_daf_bootstrap`): `POST identity/self` mints the
   `fid-cgf-auth-jwt` session token (read from the response header)
   and returns the donor `partyId`. Every subsequent call is a
   cookie-authenticated `page.request` GET carrying that header —
   no DOM scraping (the firstcitizens REST pattern).
3. Reads the roster (`GET user/<partyId>/accounts`) and **iterates
   every giving account** it returns. Per account (`_daf_scrape_account`):
   the JSON surfaces (account master, pool balances, grants,
   contributions, gifts — paged to `totalItems`; pool exchanges +
   adjustments unioned across the account's active years), the
   server-side CSV exports (zstd-compressed), and the PDF document
   set. The window honours `--lookback` exactly like the retail
   phases (default 90 days; `--lookback all` or an ISO date backfills
   full history); silver is cumulative, so nightly windows keep
   recent events fresh while the initial backfill captures history.

Bronze layout (see §2): `daf/accounts.json` (roster) + one
`daf/<account_key>/` per giving account, holding `account.json`,
`pool_balances.json`, `grants.json`, `contributions.json`,
`gifts.json`, `pool_exchanges.json`, `adjustments.json`,
`documents_index.json`, `exports/*.csv.zst`, and `documents/*.pdf`.
JSON is the primary silver source (richer than the CSVs — stable
ids, charity ids, status detail); CSVs are corroborating; PDFs are a
document archive. Documents download as raw `application/pdf` (no
base64-in-JSON dance) and dedup on content hash, with a `%PDF`
magic-byte guard rejecting a non-PDF body.

**Dry-run.** `--dry-run` still exercises the DAF surface (unlike the
retail phases, whose export surfaces the account enumeration already
reaches, the DAF sits behind its own SSO hop + API): the dry-run hops,
bootstraps, reads the roster, and reads the per-account JSON counts +
document listing — but fetches no CSV export or PDF and writes no
bronze (root CLAUDE.md §2 "export nothing"). So a cheap
`download --dry-run` validates reachability + auth before a full run,
and its plan log carries the real per-account counts.

**Validation.** A full `--lookback all` DAF walk exercises the SSO
hop, the JWT bootstrap, the roster read, pagination, and the
per-account JSON, CSV and PDF paths. Two checks confirm its bronze:
every PDF is a valid `%PDF`, and the JSON grant count matches the CSV
export. The `--dry-run` path hops, bootstraps and counts while writing
nothing.

**Several giving accounts.** The roster loop iterates any number of
giving accounts. The year-union and pagination logic is uniform per
account. One detail may still differ on a login with several DAFs:
whether the party scope is shared or per account.

### 12.3 Silver + gold model (implemented)

**Silver** (migration 0007 + `load._load_daf`): the DAF bronze under
`<dump>/daf/` lands in the shared silver tables so the adapter
composes uniformly — a `portfolios` row (`kind='daf'`, external id =
the `Fidelity Charitable® Giving` label), one `accounts` row per
giving account (`management_style='automated'`), pool positions
(`instrument_key` = poolId, `asset_class='daf_pool'`), transactions
from grants / contributions / gifts / pool exchanges / adjustments
(PK `daf-<sha>` over the source's own stable id — grantId,
contributionId, … — so re-downloads collapse; grants and gifts signed
negative, contributions positive), and the PDF archive in `documents`
under namespaced kinds (`daf_statement`, `daf_tax_form`,
`daf_grant_confirmation`, `daf_contribution_confirmation`).

**Historical reconstruction.** Giving Account statements come
quarterly and at year-end, and page 1 of each carries a per-pool
holdings table (units, unit price, period-begin/-end market value)
plus a beginning →
ending value reconciliation. `pdf_parsers_daf` parses them (pure
text-level parsers, pdfplumber extraction — the §4.5 architecture) and
`load._load_daf_historical` back-fills `historical_position_snapshots`
with the period-END pool rows, gated on the fleet reconciliation rule:
the pool values must sum to the statement's own stated total and
ending value, or the statement is skipped and logged. Pool names
cross-walk to the live poolId via `positions.description`; a retired
pool's rows keep the description with a NULL key (gold synthesises
identity, as for sold 529 funds). The gold adapter classifies
DAF-account historical rows as (multi_asset, fund) — pool names carry
none of the shape signals the 529 heuristics key on — and the
back-projected account master carries the §12.3 taxonomy, so the
DAF's value spine (and its returns inception) starts at its first
statement quarter-end rather than the toolkit's first live run.

**Gold.** The DAF flows through the single `fidelity` source (the
1:1 silver:source relationship is preserved). A DAF maps to

- `account_kind` = `donor_advised_fund` (canonical enum + gold
  migration 0036)
- `tax_wrapper` = `charitable` (renamed from the never-emitted `daf`
  value in 0036 — the wrapper names the tax treatment, the kind
  carries the DAF-ness)
- `management_style` = `automated`, from the silver column (Fidelity
  DAF pools are model portfolios — allocation across a fixed pool
  menu, not free selection)

The `daf_pool` asset class projects as (multi_asset, fund), like the
529 plan funds; grants/gifts map to `withdrawal` and contributions to
`deposit`, so the standard bank returns policy counts them as
external flows. A DAF balance is irrevocably donated, so gold
surfaces it under its own kind + wrapper (and its own portfolio row)
and leaves the include/exclude-from-net-worth choice to the
deployment's queries and config rather than dropping or
force-including it.

**The completeness invariant (fleet lesson).** Gold's report macros
anchor holdings on the latest positions-bearing snapshot **per
source**, treating every such snapshot as a complete observation of
that source. A partial dump therefore corrupts: the retired
`--mode daf` produced a one-account positions snapshot that became
the fidelity anchor and zeroed every retail holding. The invariant
is enforced at both ends:

- **Download**: every positions-bearing mode covers both channels —
  `all` runs the full DAF phase, `positions` runs the master + pool
  slice, and a DAF-only mode does not exist. When the account
  selector enumerated a DAF but the charitable walk comes back
  empty, the walk records `daf_results.status='error'` rather than
  `no-daf`, so the failure is legible downstream.
- **Load** (`_positions_completeness_gate`): positions are
  all-or-nothing per dump. A dump whose observation is partial — a
  legacy `mode='daf'` dump, a DAF-phase failure alongside landed
  retail CSVs, or retail rows parsing to zero while the pool landed —
  contributes NO positions at all, with a loud warning. Stale beats
  partial: the anchor stays on the last complete dump. Events,
  documents, and master data still load (keyed rows, not
  snapshots), and the loader skips the retail master rows of a
  legacy `mode='daf'` dump (its `account_dimensions` is an
  enumeration-only capture).

### 12.4 Remaining open questions

1. Whether `poolBalances?numberOfDays=N` (N > 1) returns a NAV/value
   time series. The quarterly statement reconstruction (§12.3) now
   covers history, so this would only add intra-quarter granularity;
   one read-only probe with a larger N answers it.
2. PDF byte stability across repeated downloads (dedup key choice —
   content hash vs logical identity; same question as §11.1). The
   walk dedups within a run by content hash; cross-run silver dedup
   is the silver loader's concern.
3. Retail idle-timeout behaviour across the hop for long walks
   (~15 min, §5.1) — matters only if the DAF phase runs late in a
   long dump.
4. Gold treatment beyond the wrapper: the include/exclude default in
   the user's config (§12.3).
