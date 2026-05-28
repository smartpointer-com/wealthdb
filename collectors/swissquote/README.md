# swissquote-dump

A toolkit for ingesting Swissquote Bank private-client portfolio data:
driving the e-banking web UI under Playwright to export positions,
transactions, and eDocuments, then (in subsequent scripts) parsing the
raw CSVs and PDFs into a queryable SQLite silver database for
downstream tools — e.g. local LLM-based agents and the `wealthdb`
gold layer — to consume.

Part of the **wealthdb** suite — see [the architecture
overview](../../ARCHITECTURE.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions.

## Why this design

Swissquote does not expose any retail-accessible API for reading your
own portfolio:

- **PSD2 / Open Banking** — TPP-only (eIDAS cert required).
- **OpenWealth / `bankingapi.swissquote.ch`** — B2B-only (External
  Asset Managers, family offices). Worth asking your relationship
  manager if you have the assets to justify it, but expect "no" for
  retail.
- **FIX API** — covers only the Forex/CFD margin sub-account, not the
  bank-account portfolio (equities, ETFs, bonds, funds).
- **PSD2 aggregators (Plaid, TrueLayer, Tink, etc.)** — no Swissquote
  coverage. Powens has it but is B2B.
- **Email feeds** — Swissquote sends "document available" notifications
  with no payload; the parseable detail is only inside the PDF behind
  the e-banking login.

The remaining channel is the e-banking web UI: CSV export for
transactions and positions, PDF download for eDocuments (trade
confirmations, statements, fee notes). This toolkit automates that
channel under Playwright.

A **Mobile Level 3** push to the user's phone is required on every
fresh login. Unattended cron is therefore impossible; this toolkit is
human-triggered (one biometric tap per fresh session) but reuses the
persisted session cookie across runs until it expires.

## Tools

| Script | Status | Purpose |
| --- | --- | --- |
| [`login.py`](login.py) | implemented | Drives headless Chromium through the F5 BIG-IP login form and the Mobile Level 3 MFA gate, scrapes the on-screen Operation No. (TAN) so the operator can compare against their phone, and persists the Playwright `storageState.json`. `--check` validates an existing state file without an MFA push. |
| [`download.py`](download.py) | implemented | Reuses the persisted session to export transactions (CSV), positions + list of assets (XLS), account overview (PDF), and per-document PDFs from eBanking into a timestamped bronze directory. Read-only — see [CLAUDE.md](CLAUDE.md) §1. |
| [`load.py`](load.py) | implemented | Parses bronze CSVs and XLSs into a queryable SQLite silver database. Applies pending migrations on startup; each dump loads atomically (window-DELETE-INSERT for transactions, content-hash dedup for documents). Idempotent — already-loaded dumps are skipped. Also parses **Portfolio Performance PDFs** in bronze to reconstruct historical position snapshots (one per year-end the bank issues), tagged with `source='pp:<doc_id>'` on the silver `positions` table. |

## Container build

Playwright + Chromium + PDF tooling is heavy; running it directly on
the host pollutes the OS. This toolkit ships as a Docker image and
runs entirely inside the container.

Base image: `mcr.microsoft.com/playwright/python:v1.59.0-noble`
(Ubuntu Noble, Chromium browser binary and all OS-level deps
pre-installed at `/ms-playwright/`). The Playwright Python package
itself is installed via `requirements.txt`, version-pinned to match
the base-image tag — bump both in lockstep. The Dockerfile pins to a
specific tag so reruns are deterministic.

### Build

```sh
git clone <this repo>
cd swissquote-dump
./swissquote-dump build         # one-time, ~10 min on first build
```

### Run

The `swissquote-dump` wrapper drives `docker run`; see
[collectors/README.md](../README.md) for the shared Docker
mount/wrapper conventions (`~/.secrets → /secrets`,
`~/wealthdb/swissquote → /data`).

```sh
./swissquote-dump login --check
./swissquote-dump download --dry-run
./swissquote-dump load --silver-db /data/swissquote.db --bronze-dir /data
```

### Headless remote host

The toolkit is built to run on a headless remote Linux host (e.g. an
always-on home server or a small VPS). The user `scp`s the bronze
directory back to their workstation for analysis. Chromium runs
headless inside the container; the MFA push is approved on the
user's phone, not in any UI on the remote host.

**First-run device verification.** Swissquote does device
fingerprinting. The first login from a new IP (i.e. the remote
host's IP) is likely to trigger an extra "verify this device" step
on top of Mobile Level 3 — typically an email link or SMS code.
Plan to do the very first `login.py` run with `--screenshot-dir`
enabled so you can `scp` the captured screenshots down and see what
Swissquote is asking for. Alternatively, do the device-trust step
once via a manual browser session on the same outbound IP (e.g. SSH
port-forwarding), then run scripted afterwards.

## Layout

```
<bronze-dir>/                       e.g. ~/wealthdb/swissquote/
├── 20260514T093122Z/               one bronze dump per run
│   ├── transactions_000.csv        single CSV covering --since..--until
│   ├── positions.xls               Trading Platform Positions export (.xls binary; securities only)
│   ├── position_details.json       DOM scrape: per-position long `name` + `isin` (joined into silver)
│   ├── list_of_assets.xls          Trading Platform List of Assets export (.xls binary; per-currency cash + FX)
│   ├── account_overview.pdf        Server-rendered portfolio-summary PDF (~35 KB)
│   ├── accounts.json               DOM scrape: per-account `{account_product, account_external_id}` from #accountOverview/main
│   ├── documents/
│   │   ├── <docid>.pdf             eDocuments (trade confirms, statements, tax statements, fee notes, ...)
│   │   └── ...
│   └── run.json                    metadata: customer ID, accounts entries, transaction + documents windows, per-doc metadata
├── 20260514T210105Z/
│   └── ...
├── manual/                         user-uploaded bronze artefacts
│   ├── tax_statement_2024.pdf      annual e-tax PDFs from prior years
│   └── tax_statement_2025.pdf
└── swissquote.db                   silver SQLite database (default name)
```

Bronze and silver paths are independently configurable; the layout
above is the path of least resistance for personal use.

The `manual/` directory is for bronze artefacts the user produces
out-of-band — most notably the annual Swissquote e-tax statement
PDF (CHF 91 each, contains full year of positions + income +
transactions and is useful as a year-end reconciliation against the
scraped silver data). `load.py` ingests `manual/` on every run
using the same dedup-by-hash mechanism as the auto-fetched eDocs.

## Session lifecycle

Swissquote sessions have two layers:

- **Session cookie** — set by F5 BIG-IP after the login + MFA flow
  completes. `login.py` persists it into `storageState.json`;
  `download.py` reuses that file directly (no refresh logic — when
  the cookie dies, F5 redirects to `/my.policy` and the script
  exits with a clear "run login.py" message). Lifetime is
  policy-driven by Swissquote and unconfirmed; expect to re-login
  at least once per working session in practice.
- **MFA gate** — Mobile Level 3 push to the user's phone. Required
  on every fresh login. Cannot be scripted away; the user must tap.
  Swissquote occasionally skips the push when the device
  fingerprint is recent enough — the script handles that case
  silently.

So the normal rhythm is: run `login.py` once, then `download.py` as
many times as you like during the cookie's lifetime. When
`download.py` reports the session is dead, re-run `login.py`.

## login.py

### How it works

`login.py` launches headless Chromium inside the container and hits
a protected eBanking URL (`/sqc-web-client-portal/`); the F5 BIG-IP
gateway redirects to `/my.policy` with the login form. The script
fills the form, waits for the Mobile Level 3 MFA page to appear, and
prints the on-screen Operation No. (TAN) to the terminal so the
operator can compare it against the value shown on their phone
before tapping approve. Once F5 redirects away from `/my.policy`,
the resulting browser context (cookies + localStorage) is persisted
to a JSON state file (chmod `0600`).

The script waits up to `--mfa-timeout` seconds (default: 300) for
F5 to leave `/my.policy`, polling the URL rather than sleeping a
fixed duration. If Swissquote bypasses MFA on a recent-enough device
fingerprint, the script completes silently in milliseconds.

### Usage

Initial mint (and re-mint when the session expires):

```sh
./swissquote-dump login \
    --state-path /secrets/swissquote_state.json \
    --username <USERNAME>
```

`--username` may also be provided via `SWISSQUOTE_USERNAME`. The
password is read from `SWISSQUOTE_PASSWORD` if set, otherwise
prompted interactively (echo disabled). Credentials go in
`~/.secrets/swissquote.env`; see
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

Check whether the current session cookie still authenticates (no
new MFA push, no fresh login):

```sh
./swissquote-dump login --state-path /secrets/swissquote_state.json --check
```

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--state-path` | _(required)_ | Path to read/write the Playwright `storageState.json` file. |
| `--username` | _(env `SWISSQUOTE_USERNAME`)_ | Swissquote login username / customer number. Falls back to env var. |
| `--check` | off | Validate the existing state file against a live landmark URL; print whether it's still authenticated. No new login, no MFA push. |
| `--mfa-timeout` | `300` | Seconds to wait for the user to approve the Mobile Level 3 push. |
| `--screenshot-dir` | _unset_ | If set, write a Playwright screenshot at each navigation landmark for offline debugging. Never use on a real account in tracked output — see [CLAUDE.md](CLAUDE.md) §4. |
| `--trace` | off | Capture a Playwright trace bundle. Requires `--screenshot-dir`; the bundle lands there alongside screenshots. Never auto-writes to the secrets dir. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

## download.py

### How it works

`download.py` loads the persisted session state, opens a Playwright
context, and walks a fixed sequence of e-banking pages to export
bronze artefacts. The first navigation is also the session-liveness
check — if Swissquote bounces to the login page, the script exits
with a "run login.py" message rather than triggering a new MFA push
mid-fetch.

Per run, the script:

1. Verifies the session is alive by navigating to the eBanking SPA
   root and checking that F5 hasn't bounced us to `/my.policy`.
   While there, scrapes the per-account list from
   `#accountOverview/main` (`<TYPE> <CUSTOMER_ID>` lines like
   `Trading 1234567`) into `accounts.json`.
2. Navigates to `#portfoliooverview` on the Trading Platform SPA and
   triggers three exports from that page:
   - **Positions** export (top-right of the Positions table) →
     `positions.xls`.
   - **List of Assets** export (next to the Assets table) →
     `list_of_assets.xls` (per-currency cash + securities valuation
     + FX rates against CHF).
   - **Export account overview** (top-right of the page, near
     "Buying power") → `account_overview.pdf` (server-rendered
     portfolio-summary PDF, ~35 KB).

   The two XLS files are the legacy CDFV2 binary Excel format (not
   modern `.xlsx`); `load.py` reads them via `xlrd==1.2.0`, the last
   `xlrd` line that supports `.xls`. The PDF is bronze-only at this
   stage — it is not parsed into silver.
3. Still on `#portfoliooverview`, scrapes per-position long names
   and ISINs into `position_details.json`. ISIN is the first path
   segment in each row's FullQuote link href; the long name is in a
   hover tooltip on the symbol cell. The Positions XLS only carries
   the ticker, so this DOM scrape is what gets `name` and `isin`
   into the silver `positions` table.
4. Navigates to `#transactions`, mutates the Period filter to the
   requested window via the React-native-setter trick (the inputs
   ignore plain Playwright `.fill()`), clicks Apply, then triggers
   the export dropdown and saves `transactions_000.csv`. Swissquote
   does not enforce a window cap, so the entire range goes in a
   single CSV — no chunking.
5. Navigates to the eBanking SPA's `#documents` route (loaded by
   mutating `location.hash` after the SPA bootstraps; direct
   navigation strips the hash), widens the Period filter to
   `--documents-since..--documents-until` (default: ~25 years), and
   waits for the `.LoadingTable` spinner to clear.
6. Scrapes the rendered DOM for `a[href*='getPdfDocument']` anchors,
   parses each URL into `(customer, doc_id, doc_type, contract_no,
   date, target_user)`, skips any IDs already present in prior bronze
   runs (filename-based dedup), and fetches each new PDF via
   Playwright's request API (cookie reused, no per-row clicking) into
   `documents/<doc_id>.pdf`.
7. Writes `run.json` with the customer ID, accounts entries,
   transaction window bounds, documents window bounds, and per-document
   metadata.

Every response is written to disk verbatim — no parsing, no
normalisation, no filtering happens at this stage. That's silver's
job.

The script must never navigate to a write surface — see
[CLAUDE.md](CLAUDE.md) §1.

### Prerequisites

- Docker (host OS).
- A Swissquote private-client account with e-banking enabled and
  Mobile Level 3 active.
- A valid `storageState.json` minted by `login.py`.

### Usage

Dry run — loads the session, verifies it's alive, walks the UI to
confirm landmark selectors match, exits without exporting any
artefacts:

```sh
./swissquote-dump download \
    --state-path /secrets/swissquote_state.json \
    --dest /data \
    --dry-run
```

Real download:

```sh
./swissquote-dump download \
    --state-path /secrets/swissquote_state.json \
    --dest /data
```

Files land in `/data/<UTC-timestamp>/` inside the container, which
maps to `~/wealthdb/swissquote/<UTC-timestamp>/` on the host.

| File | Source |
| --- | --- |
| `transactions_000.csv` | Trading Platform → `#transactions` → top-right download menu (CSV) |
| `positions.xls` | Trading Platform → `#portfoliooverview` → Positions export (legacy `.xls`, securities only) |
| `position_details.json` | Trading Platform → `#portfoliooverview` → DOM scrape (per-position long `name` from hover tooltip + `isin` from FullQuote link href) |
| `list_of_assets.xls` | Trading Platform → `#portfoliooverview` → List of Assets export (legacy `.xls`, per-currency cash + FX) |
| `account_overview.pdf` | Trading Platform → `#portfoliooverview` → Export account overview (server-rendered PDF) |
| `accounts.json` | eBanking `#accountOverview/main` → DOM scrape (per-account `<TYPE> <CUSTOMER_ID>` lines) |
| `documents/<docid>.pdf` | eBanking `#documents` → `getPdfDocument` REST endpoint (cookie reused) |
| `run.json` | metadata: customer ID, accounts entries, transaction window, documents window, per-doc metadata |

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--state-path` | _(required)_ | Path to the Playwright `storageState.json` file. |
| `--dest` | _(required)_ | Local destination directory (must be writable). |
| `--since` | _today - 90d_ | Earliest transaction date to fetch (YYYY-MM-DD). Swissquote does not enforce a window cap; for a one-off bulk backfill pass an older date explicitly (e.g. `--since 2010-01-01`). |
| `--until` | _today (UTC)_ | Latest transaction date to fetch (YYYY-MM-DD, inclusive). |
| `--documents-since` | _25 years ago_ | Earliest document date (YYYY-MM-DD). Defaults to a wide window so every run scans the full available document history; existing PDFs are skipped by filename, so the cost is just one extra listing scrape. Pass a closer date for a deliberately narrower window. |
| `--documents-until` | _same as `--until`_ | Latest document date (YYYY-MM-DD, inclusive). |
| `--dry-run` | off | Skip exports; only validate session and selectors. |
| `--screenshot-dir` | _unset_ | Write a screenshot at each landmark for offline debugging. |
| `--trace` | off | Capture a Playwright trace bundle. Requires `--screenshot-dir`; the bundle lands there alongside screenshots. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### Caveats

- **Session expiry.** Swissquote's session-cookie lifetime is
  policy-driven and not configurable. When `download.py` reports
  the session is dead (F5 redirect to `/my.policy` during the
  initial liveness check), re-run `login.py`.
- **UI churn.** Selectors will break when Swissquote redesigns the
  e-banking UI. Failure mode is "raise an explicit error pointing
  at the broken landmark" — silent "empty CSV" results are
  disallowed. Save a `--trace` bundle when reporting issues.
- **CSV header drift.** Swissquote occasionally renames CSV columns
  even when the UI stays put. `load.py` fails loud on unknown
  headers rather than skipping silently.
- **Currency.** Swissquote returns multi-currency data; every
  position and transaction in silver carries an explicit `currency`
  column. There is no implicit "main currency" assumption.
- **One trading account today.** The schema and code path are
  written to support multiple linked accounts under one login, but
  no second-account fixtures exist — multi-account paths are
  exercised but not verified.
- **No retry / resume / scheduling.** Run interactively when you
  want fresh data. The Mobile Level 3 gate makes unattended cron a
  non-starter.

## load.py

### How it works

Parses one or more bronze dump directories (as produced by
`download.py`) and inserts them into a SQLite silver database.
The schema is defined in `migrations/0001_initial.sql`; the loader
applies any pending migrations on startup before loading data, so
the silver database is always at the latest schema version.

Each dump is loaded atomically — a failure mid-load rolls back to
the prior state, and a re-run retries the whole dump. Already-loaded
dumps are detected via `dump_runs` and skipped, so the loader is
safe to point at a `--bronze-dir` containing a mix of new and
already-processed snapshots.

Reload semantics mirror the Schwab loader:

- **Snapshots** (`accounts`, `positions`, `currency_balances`) are
  append-only. Each dump produces a new row per (snapshot, entity).
  - `accounts` does content-dedup per `(account_external_id, account_product)`
    (only inserts when the canonical-JSON payload differs from the
    most recent row for that account). The `account_product` column
    is populated from `accounts.json`; older bronze dumps without it
    fall back to `account_product=''`. The bronze key was previously
    `account_type` (migration 0002); migration 0005 renamed the
    silver column to `account_product` for cross-bank gold-layer
    clarity, and `load.py` reads either bronze key for backward
    compatibility.
  - `positions` rows get their `name` and `isin` columns populated
    from `position_details.json` when present (joined on
    `(symbol, currency)`); older bronze dumps without the sidecar
    leave them `NULL` (forward-fill — see migration 0003).
  - `positions` rows carry a `source` column (see migration 0004):
    `'live'` for rows from the current Positions XLS export, and
    `'pp:<doc_id>'` for rows reconstructed from a Portfolio
    Performance PDF in bronze. The PP-sourced rows are how silver
    gets year-end historical snapshots — `download.py` only ever
    sees the current state. Re-parsing a PP doc is idempotent
    (DELETE-then-INSERT under the same source tag). Cash entries
    in the PP table are skipped; they belong in `currency_balances`.
    **Account Statement PDFs are *not* parsed**: they are cash-flow
    ledgers, not position snapshots. Account Statements remain
    indexed in the `documents` table for future use.
- **Events** (`transactions`) use window-DELETE-then-INSERT per
  `(account, time-window)`. Re-running a window converges to
  Swissquote's current truth even if dates/amounts were amended.
- **Documents** (PDFs) are tracked by content hash + Swissquote's
  GUID-style document ID in a `documents` table, but the binary
  itself stays on disk. Structured PDF parsing is deferred — the
  `payload` JSON records source filename, doc type, contract number,
  date, and target user from the URL query string.
- **`account_overview.pdf`** is regenerated every run (it's a live
  snapshot, not a stable document). It lands in bronze but is
  intentionally NOT indexed into the `documents` table.

The `manual/` directory under the bronze root (e.g. pre-Swissquote-era
e-tax statements brought forward, anything dropped in by the user out
of band) is treated as a parallel bronze input on every run: each
file's sha256 is compared against `documents.content_sha256`, and only
new files are recorded as `source = 'manual'`.

See [the architecture overview](../../ARCHITECTURE.md) for the
bronze → silver → gold model; this collector follows the shared
snapshot/event and semi-relational JSON1 conventions.

### Usage

```sh
./swissquote-dump load \
    --silver-db /data/swissquote.db \
    --bronze-dir /data
```

The loader scans `<bronze-dir>` for subdirectories whose names match
the dump-timestamp format `YYYYMMDDTHHMMSSZ` and processes each one
not already recorded in `dump_runs`. It also scans
`<bronze-dir>/manual/` for new files on every run.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--silver-db` | _(required)_ | Path to the silver SQLite database. Created if missing. Conventional name: `swissquote.db`. |
| `--bronze-dir` | _(required)_ | Directory containing bronze dump subdirectories and the `manual/` subdirectory. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### Schema migrations

To change the silver schema, add a new `migrations/NNNN_<slug>.sql`
file. The number must be strictly greater than any existing
migration. Each file:

- Contains the DDL and any data backfill needed.
- Ends with `INSERT INTO schema_meta (silver_schema_version, applied_at)
  VALUES (N, CAST(strftime('%s','now') AS INTEGER));` as the
  migration-complete marker the loader checks for.

The loader executes each new migration in numeric order and commits
between files. Silver databases must always be at the latest schema —
never write code that handles "if column X exists".

## Silver: `accounts.account_product`

`accounts.account_product` is scraped from the eBanking
`#accountOverview/main` listing, where each account is rendered as
a `<PRODUCT> <CUSTOMER_ID>` line ("Trading 1234567", "Säule 3a
1234567", etc.). The scraper handles German diacritics via Python's
Unicode-aware `\w`. A label the regex can't parse (e.g. one
starting with a lowercase letter like "ePrivate Banking") is logged
as a warning at scrape time and is absent from `accounts`, so
`account_product` falls back to `''`.

## Gold integration

The wealthdb Swissquote adapter consumes this silver and derives
the canonical columns (e.g. `tax_wrapper` from
`accounts.account_product`, instrument joins via `positions.isin`).
See [the adapter doc](../../wealthdb/docs/adapters/swissquote.md).
