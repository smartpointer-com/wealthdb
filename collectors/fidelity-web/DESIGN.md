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
overview](../../ARCHITECTURE.md) for the bronze → silver → gold
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
| `Fidelity Charitable® Giving`  | (auto-excluded)   | Donor-Advised Fund. Fidelity uses a shorter account-id length for DAFs than for brokerage / trust / 529 accounts; `download.py` auto-excludes by id length so DAFs never enter silver. |
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
- **The Fidelity Charitable DAF.** Excluded by id length.
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
│   └── screenshots/                            only with `download --debug` / `--explore`
│       └── <ts>-<label>.{html,png}             per-landmark diagnostics for selector drift
│                                               (+ <ts>-<label>.dominv.json with --explore) — NOT compressed
├── 20260525T120000Z/
│   └── …
├── manual/                                     user-uploaded artefacts (documents that arrive out-of-band)
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
  `--since/--until` window, the request is bisected into ≤93-day
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
across exports. Plan: synthesise a deterministic SHA-256 prefix
over the row's promoted columns (`account_external_id |
timestamp | amount | description | symbol | source_sha256`),
mirroring `schwab-web`'s pattern. Silver-internal only;
gold does not attempt cross-source per-row matching on it.

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
| `transactions` | event | synthetic `activity_id` (SHA-256 prefix over the row's full normalized payload + a per-file occurrence index; file-independent so overlapping windows collapse) | `timestamp`, `account_external_id`, `kind`, `instrument_key`, `quantity`, `price`, `amount`, `settlement_date`, `currency`, `source_sha256` |
| `documents` | event | `content_sha256` | `snapshot_at` (first observation), `file_path`, `file_name`, `size_bytes`, `doc_kind` (`statement` / `tax_form` / `balances_html` / `performance_html`), `file_format`, `tax_year`, `account_external_id` |
| `historical_position_snapshots` | snapshot | `(as_of_date, account_external_id, description)` | `instrument_key` (cross-walked from `positions.description` when available; NULL otherwise), `quantity`, `price`, `market_value`, `percent_of_total`, `currency`, `source_sha256`; rest in `payload`. Populated from two PDF archives — scraped 529 statements (`pdf_parsers.parse_statement_pdf()`) and user-supplied trust statements under `<bronze-dir>/supplied-statements/` (`pdf_parsers_supplied.parse_supplied_statement_pdf()`). See §4.5. |

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
purely from the row's content fingerprint (its full normalized
payload) plus a per-file occurrence index — **not** from the
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
| `529`            | `self_directed`             |
| `trust_managed`  | `discretionary`             |
| `other` / NULL   | NULL                        |

### 4.5 Historical reconstruction — two PDF sources

`historical_position_snapshots` is populated from **two distinct
statement-PDF archives**, each with its own parser, both feeding
the same silver table:

**(a) 529 statements — auto-scraped.** Fidelity's *web document
center* exposes monthly / quarterly statement PDFs for the
account groups it serves statements for, such as 529 plan accounts. `download.py`
scrapes these into `<dump>/documents/Statement<MMDDYYYY>.pdf`,
parsed by `pdf_parsers.parse_statement_pdf()` and wired into
`load.py` via a `ProcessPoolExecutor` worker pool (PDF text
extraction is CPU-bound; SQLite insert stays on the main thread).
The cross-walk from the human-readable fund description to an
`instrument_key` queries the live `positions` table per (account,
description); when the fund hasn't appeared in any live snapshot
yet the row lands with `instrument_key` NULL and the description
preserved for downstream resolution.

**(b) Supplied statements — sourced out-of-band.** An account group
the web document center serves no statements for can still have
statement PDFs supplied out-of-band and dropped in.
These use a different layout (`pdf_parsers_supplied.py`) and
are loaded from a directory rather than the scraped dump tree.

> **Reproducible-from-bronze.** The trust PDFs default to
> **`<bronze-dir>/supplied-statements/`** — *under* the bronze tree —
> precisely so `load --force` (which wipes silver and rebuilds from
> bronze) re-ingests them automatically. An earlier design sourced
> them from an arbitrary external dir reachable only via
> `--supplied-statements-dir`; every `--force` rebuild or flag-less
> nightly reload then silently dropped the entire trust history,
> since it lived only in silver and nothing under bronze could
> rebuild it. Keeping the PDFs bronze-resident restores the "silver
> is reproducible from bronze alone" invariant that `silver.reset()`
> depends on. The flag still works as an override.
>
> A misfiled PDF (a statement for an unrelated account that Fidelity
> bundled into the batch) is rejected by a page-1 **signature** check
> — a substring (the account registration) that must appear in the document.
> The signature is read from `--supplied-statement-signature` or, when
> omitted, the first line of **`<supplied-statements-dir>/signature.txt`**
> so the registration (PII) stays in the local data dir, never in
> argv / shell history / a committed orchestration script.

An account that appears in historical statements but in no live
download gets a placeholder `accounts` (+ `portfolios`) master row at the account's
last historical `as_of_date` so gold's historical-account projection
has something to join against. Gold's per-source latest-snapshot
semantics then zero the account out automatically for every date
after its final statement (no zombie balances).

### 4.6 Tax-form structured data

Fidelity's Consolidated 1099 is available as **PDF only** from
the Tax forms sub-page. The download anchors carry aria-labels
ending in ` (pdf)` exclusively; an early DOM snapshot suggested
CSV / XML variants may exist for some 1099 types, but live
testing found no such variants surface in practice. Silver
stores the PDFs in `documents` keyed by
`content_sha256` with `doc_kind='tax_form'` and the parsed
`tax_year`. Structured per-lot extraction (1099 detail into a
`tax_form_rows` table) is a follow-up; parser TBD (pdfplumber
+ per-line layout heuristics, mirroring the planned 529-
statement reconstruction in §4.5).

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
matches the [schwab-web session model](../schwab-web/DESIGN.md#5-login--mfa-flow)
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
handoff — the operator clicks "Log in" manually in their VNC
client, generating real Firefox-sourced mousedown/mouseup events
that satisfy Akamai's behavioural-score gate. Once Akamai's
trust cookie is in the profile dir, automated clicks on the
button work fine.

The `--vnc` subcommand drives this: pre-fills the credentials,
prints a READY banner, polls for the post-auth URL while the
operator drives the login + 2FA via VNC.

## 7. Login + MFA flow

Default flow (`./fidelity-web download`), assuming a profile
dir that already has Akamai trust + Fidelity device-trust
cookies:

1. Launch Camoufox (rung 3b).
2. Navigate to `https://digital.fidelity.com/prgw/digital/signin/`.
3. Detect either signin form (US locale) or International Usage
   Agreement interstitial (non-US locale, when geoip=True trips
   it); click "I Accept" on the IUA if served.
4. Fill `#dom-pswd-input` with `FIDELITY_PASSWORD`. Username field
   varies by device-known state (text input vs `<select>`); the
   script tries both paths.
5. Click `#dom-login-button`.
6. Either:
   - Device-trust cookie suppresses MFA → straight to post-auth.
   - 2FA prompt (TOTP-style 6-digit code in
     `#dom-totp-security-code-input`) → prompt operator on stdin,
     fill, submit.
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
the credentials but waits for the operator to click Log In via a
VNC client. After the post-auth URL lands, the walk runs as
normal (or `--mode none` skips it for a profile-dir-seed-only
flow).

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
| Username (fresh-device text input) | fall-back candidates; `input[aria-labelledby=dom-username-label]` etc. |
| Password | `#dom-pswd-input` |
| Submit | `#dom-login-button` |
| 2FA code | `#dom-totp-security-code-input` |
| Trust-this-browser checkbox | `<input type=checkbox id="dom-trust-device-checkbox">` — must click the `<label for="dom-trust-device-checkbox">` (input is intercepted by the PVD label overlay) |
| IUA accept | `a.accept-link` (calls `javascript:acceptAgreement()`) |

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

### 8.4 Activity & Orders

| Element | Selector |
| --- | --- |
| Filter dialog trigger | `button:has-text('Filter')` (also `[aria-label='Filter']`) |
| Time-period filter pill | `[data-testid='ap143528-timeperiod-filter']` (opens `#timeperiod-select-container`) |
| Preset day-count radios (Recent tab) | `helios-radio[pvd-value='<N>']` (`N` ∈ {10, 30, 60, 90}; default 30, preference 90) |
| Apply (preset tab) | `button[aria-label='Apply Recent Time Period']` |
| Custom tab | `input#Custom[type='radio']` (PVD radio group `time-period-group-hsa`; the legacy `apex-kit-segment[pvd-value='Custom']` web component is gone — `_click_custom_timeperiod_tab` tries the radio first, then the legacy selector as fallback) |
| Custom-tab date inputs | `#customized-timeperiod-from-date` / `#customized-timeperiod-to-date` (HTML5 `<input type="date">`, ISO YYYY-MM-DD; `min`/`max` attrs bound retention) |
| Apply (Custom tab) | `button[aria-label='Apply Customized Time Period']` |
| Download dropdown trigger | `button[aria-label='Download']` (opens `#downloadContent` popover; the SPA disables it while re-fetching after Apply — we poll on `disabled` clearing before clicking) |
| CSV item inside popover | `#downloadContent button:has-text('CSV')` |

**Activity export is consolidated.** Clicking the account-selector
does NOT scope the resulting CSV — the same CSV comes down
regardless of selector state. One CSV per window, all accounts
inside (Account Number column as the per-row discriminator). The
silver loader fans out per-account from there.

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
walk leaves alone; its history can come from `<bronze-dir>/supplied-statements/`.

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
- The Fidelity Charitable DAF — auto-excluded.
- Prospectuses / supplementary documents.
- Trade / transfer / config writes — see [CLAUDE.md](CLAUDE.md) §1.
- MFA automation — human-in-the-loop on every truly-fresh login.
- Cross-bank semantic alignment — gold's job.
- Trust statement reconstruction from PDFs — no statements exist.
- Outside investment-manager data sources — out-of-band; its own future collector when needed.

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
| `migrations/0003_*.sql` (`accounts.management_style` derived from `portfolios.kind`: 529 → `self_directed`, trust_managed → `discretionary`) | done — see §11.6 |
| `wealthdb` Fidelity adapter | sibling component (`wealthdb/`) |

## 11. Open questions

### 11.1 PDF regeneration
Does Fidelity regenerate statement / 1099 PDFs per request
(different sha256 each time, same logical content) like Schwab
does? Resolve by downloading the same statement twice and diffing
sha256s. The silver loader currently plans to dedup on
`content_sha256` (assumes stable hashes); if regenerated, we
switch to `(account, doc_date, doc_kind, filename)` dedup à la
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
table. The most likely place a registration label surfaces
is the dedicated `/portfolio/accounts/<account-id>` detail page,
which `download.py` does NOT currently visit. The path forward
when that signal materialises:
1. Extend `download.py` to nav each in-scope account-id detail
   page; capture `account-registration` (or similar testid)
   into `run.json/account_dimensions[*].registration`.
2. Migration to add `accounts.account_registration TEXT`.
3. Loader populates the column from the captured value.

Until then, gold treats `portfolios.kind` as the registration
proxy and the column stays unimplemented.

### 11.6 Advisory vs discretionary within `trust_managed`
Migration 0003 promotes `accounts.management_style`, derived
from `portfolios.kind` (see §4.4):
- `529` → `self_directed`
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
