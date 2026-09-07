# ubs-web

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to UBS with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and UBS
offers no read-only sub-scope, so nothing but this codebase's own
discipline restricts the session to reading. If malicious code were ever
introduced into this repository, its dependency chain, or the container
images it runs, it could act on your accounts with your full authority and
cause **irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach UBS's terms of
service; verifying that your use is permitted is likewise your
responsibility.

**No warranty; no liability.** This software is provided “AS IS”, without
warranty of any kind, express or implied, including but not limited to the
implied warranties of merchantability, fitness for a particular purpose,
title, and non-infringement. To the maximum extent permitted by applicable
law, **SmartPointer AG and the contributors accept no responsibility for,
and shall not be liable for, any claim, damages, or other liability** —
whether in an action of contract, tort, or otherwise — arising from, out
of, or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds or other
assets, credential or data compromise, account suspension or termination,
and any direct, indirect, incidental, special, consequential, or punitive
damages. Your use is entirely at your own risk. See
[LICENSE](../../LICENSE) for the governing terms. This software is not
affiliated with, endorsed by, or sponsored by UBS or any other financial
institution; nothing in this repository is financial, legal, or tax
advice.

A toolkit for ingesting UBS Switzerland retail e-banking data the
PSN feed does not cover: driving the UBS netbanking web UI under
Playwright to export historic account statements, custody/portfolio
statements, transaction reports, and other eDocuments, then parsing
the raw downloads into a queryable SQLite silver database.

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions. The companion UBS collector is
[ubs-psn](../ubs-psn/) (see [Relationship to ubs-psn](#relationship-to-ubs-psn)
below).

## Why this design

[`ubs-psn`](../ubs-psn/) already
ingests UBS Private Standard Network (PSN) SFTP feeds, which carry
the canonical trade confirmations, MT940 cash statements, MT535
holdings, FX rates, and master data. PSN is the right channel for
going-forward data. It has one inflexible limitation: **UBS only
publishes to PSN going forward from the day the agreement is
activated.** Historic data — months or years of statements,
transactions, and tax PDFs already accumulated in the customer's
netbanking archive — is not available over PSN and never will be.

The remaining channel for that historic backfill is the UBS
Switzerland retail netbanking web UI, which exposes:

- **eDocuments archive** — multi-year PDF history of account
  statements, custody/portfolio statements, tax statements,
  trade confirmations, fee notes, corporate-action notices.
- **Transaction history exports** — CSV/PDF exports of cash-account
  transactions for arbitrary date ranges.
- **Position / portfolio statements** — periodic custody-account
  PDFs.

UBS does not expose any retail-accessible read API for any of this:

- **PSD2 / Open Banking** — Switzerland is outside the EU PSD2
  regime; UBS's published APIs target External Asset Managers and
  corporate treasury (OpenWealth, OpenBanking.swiss), not retail
  customers reading their own data.
- **PSN SFTP Pull** — see above; covers going-forward only.
- **PSD2 aggregators (Plaid, TrueLayer, Tink, Powens, etc.)** — no
  meaningful UBS Switzerland retail coverage.
- **Email feeds** — UBS sends "new document available" notifications
  with no payload; the parseable detail is only inside the PDF
  behind the netbanking login.

This toolkit automates the netbanking-web channel under Playwright
so a one-off historic backfill is feasible without manually
clicking through years of archive listings.

A second-factor approval (UBS Access App push or alternative MFA
factor) is required on every fresh login. Unattended cron is
therefore impossible; this toolkit is human-triggered (one
biometric tap per fresh session) but reuses the persisted session
cookie across runs until UBS invalidates it.

## Tools

The architecture follows the `swissquote` template; subcommand
names and roles are the same:

| Script | Purpose |
| --- | --- |
| [`login.py`](login.py) | Drive headless Chromium through the UBS Nevis login dialog and the Access App QR challenge: fill the contract number, advance through the optional "Login starten" interstitial, fetch the QR PNG from the rendered `<img>` data URL, render it both to the terminal (Unicode half-blocks; Access App scans this directly) and as an upscaled PNG (6×; for SFTP-then-scan on truly headless hosts), watch for QR rotations, poll for the post-auth URL transition (`/workbench/?login` → `/app/OQJ/<N>/ebanking/spa.html`), then persist `storageState.json` at `--state-path` (default `/secrets/ubs-web-state.json`, chmod 0600; an older `ubs_web_state.json` is read if the canonical file is absent). `--check` validates an existing state file without a new QR push. |
| [`download.py`](download.py) | Reuse the persisted session to enumerate **cash** accounts from the homepage, then for each: export the transactions list as CSV (one file per account per window) and SWIFT MT940 enriched (one or more files per account; bisected on the 1000-trx export cap). Export `positions.csv` per portfolio (enumerated from the homepage; one CSV per `portfolioUid`). Walk the documents archive in adaptive windows (bisected on UBS's 999-row display cap) fetching each PDF via the `/api/v1/digital-banking/files/` endpoint. Then capture the card surface via [`cards.py`](cards.py). Writes a `run.json` manifest. Read-only — see [CLAUDE.md](CLAUDE.md) §1. |
| [`explore.py`](explore.py) | Discovery harness. Launches headed Chromium on the container's Xvfb display and serves it over VNC, so a session can be driven by hand while everything it produces is recorded: the network (requests, responses and bodies, line-flushed so a crash keeps the log), the clicks, one DOM snapshot plus screenshot per structurally distinct screen, and every file downloaded. Artefacts land under `--debug-dir`, never bronze. The harness itself never navigates and never clicks — it opens the login page and records from there, so the read-only surface in [CLAUDE.md](CLAUDE.md) §1 binds whoever drives. An existing session is reused and saved back on exit, so a sign-in here is not paid for twice. |
| [`cards.py`](cards.py) | The credit-card surface, read from the SPA's own REST API rather than scraped (see [DESIGN.md §5](DESIGN.md)): the roster, each card account's paged ledger, its billing periods with their reconciling totals, and each period's statement PDF. Read-only and enforced — an allow-list of read endpoints gates every request, including the paging cursor the ledger hands back, so a link the API advertises is never followed for being advertised. Driven by `download.py`; not a verb of its own. |
| [`card_parsers.py`](card_parsers.py) | Bronze → silver for the card surface: pure functions from the captured JSON to the rows `load.py` writes, with no database handle, so each is testable against a synthetic payload. Holds the three readings that are easy to get wrong — the row key is the API's opaque `_id` and never `transactionNr`, a `RESERVED` row is unposted activity rather than a transaction, and `merchantName` is the category while `details` is the merchant. |
| [`load.py`](load.py) | Parse bronze artefacts into a queryable SQLite silver database using the schemas in [migrations/](migrations/). Applies pending migrations on startup; each dump loads atomically (compound-key UPSERT on transactions, content-hash dedup for documents, skip on `dump_runs` for idempotency). Parses the card surface via [`card_parsers.py`](card_parsers.py). Also walks the documents archive and reconstructs historical position + cash snapshots from "Statement of assets" and "Account Statement" PDFs via [`pdf_parsers.py`](pdf_parsers.py) (uses `pdfplumber`, bundled in the image). |

### Why both CSV and MT940?

UBS offers three transactions exports per cash account: CSV, PDF
(server-rendered, bronze-only), and SWIFT MT940 (light + enriched
variants). This toolkit captures **CSV and the enriched MT940**.
They overlap but neither is a strict superset:

- **CSV gives more per-line detail.** Full beneficiary name +
  address, payment reference (RF/SCOR), counterparty IBAN, cost
  code, four distinct timestamps (Trade date, Trade time, Booking
  date, Value date), clean UTF-8.
- **MT940 carries opening + closing balances.** The CSV does not
  expose statement balances per chunk; MT940's `:60F:` / `:62F:`
  lines do, which is useful as an integrity check.
- MT940 truncates the `:86:` narrative and uses a Latin-1-ish
  encoding (mojibake on non-ASCII names).

Both are a **cash-account** concern: SWIFT MT940 is not defined for
card transactions, and the card surface is not fetched as an export at
all (below).

### Why the card surface is fetched as JSON, not as an export

The card toolbar offers the same CSV and PDF exports the cash surface
does, and `download.py` fetches **neither**. The SPA reads its card area
from a REST API, and that JSON is strictly richer than what the export
renders from it: it carries a stable per-row
identity (`_id`), which the CSV has no column for at all, plus the
purchase and booking dates, the posted and original amounts with their
currencies, the row's billing state, and the merchant category. Fetching
the export too would cost a request per account for a lossy copy of what
is already on disk.

Statement PDFs are the exception, and are fetched: the eDocuments
archive carries no card category, so a card statement exists nowhere
else. `--no-card-statements` keeps the periods and their figures while
skipping the rendered documents; `--no-cards` skips the surface
entirely.

The window is driven by `--lookback` like every other surface, with one
source-specific choice: the API filters on either the purchase date or
the booking date, and the collector asks for **booking date** — the date
the balance moved and the date a billing period is drawn on, so a
window's edges line up with the invoices captured beside it. Each row
carries both dates regardless, so nothing is lost to the choice.

The ledger pages at 300 rows behind a cursor, which the collector
follows to exhaustion. Both the ledger and the invoice archive reach
back about two years and no further — there is no deeper channel and no
statement backfill (DESIGN.md §5.5).

### The two UBS export caps

UBS enforces two hidden caps that `download.py` works around by
bisecting the request window:

| Surface | Cap | Behaviour at the cap | Workaround |
| --- | --- | --- | --- |
| Documents list (`#/documents/bank-documents`) | 999 rows displayed | Banner "Not all documents are displayed right now" | `_walk_window` bisects `[--lookback..today]` recursively until each leaf window has < 999 docs |
| MT940 export dialog | 1000 transactions | Info-only dialog "A maximum of 1000 transactions can be exported" with no Export button | `_export_mt940_with_split` reads the rendered trx count for the current period; if > 1000, bisects in half and emits one MT940 file per leaf window. Output filename includes the window: `<kind>_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.mt940` |

Both bisections cap at `WINDOW_MAX_DEPTH = 20` and won't sub-divide
below `WINDOW_MIN_DAYS = 1`.

### Bronze layout

```
<bronze-dir>/
└── 20260518T220332Z/                                              one run = one UTC-timestamped dir
    ├── run.json                                                   manifest (status, accounts, windows, file inventory)
    ├── transactions/
    │   ├── cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.csv         one per account per window
    │   └── cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.mt940       one or more per cash account
    ├── documents/
    │   └── <sha256>.pdf                                           content-addressed by the PDF bytes
    └── cards/
        ├── accounts.json                                          the card roster, whole
        ├── transactions_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.json  every ledger page for one card account
        ├── invoices_<sha256-prefix>.json                          that account's billing periods
        ├── invoice-details_<sha256-prefix>.json                   each period's reconciling totals
        ├── statements_<sha256-prefix>.json                        which statement file came from which period
        └── statements/
            └── <sha256>.pdf                                       content-addressed by the PDF bytes
```

The `<sha256-prefix>` collapses the opaque UBS account-id token to
16 hex chars. UBS account-ids all share a ~34-char per-customer
prefix, so a naive slice would collide silently — hashing avoids it.

`download` writes `run.json` twice: `{"status": "in-progress"}` when
it creates the run dir, then an atomic overwrite with the terminal
manifest carrying `"status": "complete"` once the walk finishes. A
`--dry-run` walk writes nothing to bronze at all. A run dir left with
`status: "in-progress"` (or none at all) is therefore a crashed walk;
`prune` reclaims it, along with legacy `"dry-run"` shells (manifests
carrying `"status": "dry-run"`). Dumps that predate the status field
are statusless but complete iff their manifest is present and not a
legacy `--dry-run` shell (`dry_run: false`).

### Silver schema and gold-merge contract

Silver lives at `$XDG_DATA_HOME/wealthdb/ubs-web/ubs-web.db` by default;
companion PSN silver is at `$XDG_DATA_HOME/wealthdb/ubs-psn/ubs-psn.db` (from
`ubs-psn`). Schemas in [migrations/](migrations/):

- [`0001_initial.sql`](migrations/0001_initial.sql) — live-fetch
  tables: `banking_relationships`, `portfolios`, `accounts`,
  `positions`, `transactions`, `documents`. Driven by the
  positions.csv / MT940 / CSV / document-API artefacts.
- [`0002_historical_snapshots.sql`](migrations/0002_historical_snapshots.sql)
  — `historical_position_snapshots` (semi-annual full snapshots
  reconstructed from "Statement of assets" PDFs) and
  `historical_cash_balances` (monthly cash deltas reconstructed
  from "Account Statement" PDFs). Kept separate from the live-
  fetch tables because the identity model and cadence differ.
- [`0003_backfill_historical_portfolio_id.sql`](migrations/0003_backfill_historical_portfolio_id.sql)
  — one-shot backfill: prepends a leading zero to any pre-existing
  15-char `historical_position_snapshots.portfolio_external_id`
  values so they line up with PSN's 16-char canonical form.
  Idempotent; the parser fix prevents any new 15-char rows.
- [`0004_mortgages.sql`](migrations/0004_mortgages.sql) —
  `mortgages`: the per-mortgage liability rows positions.csv
  carries under "Pro memoria - Mortgages" (UBS-internal mortgage
  number as `account_external_id`; term, rate type, collateral in
  named columns; negative outstanding balance).
- [`0005_historical_mortgages.sql`](migrations/0005_historical_mortgages.sql)
  — `historical_mortgages`: per-quarter outstanding principal
  reconstructed from the "Maturity notice" PDFs, keyed
  `(as_of_date, account_external_id)`.
- [`0006_single_window.sql`](migrations/0006_single_window.sql) —
  `dump_runs` rebuild collapsing the per-facet window columns into
  one `window_*` pair (one `--lookback` window per run).
- [`0007_cards.sql`](migrations/0007_cards.sql) — the credit-card
  surface: `card_accounts` (a snapshot series, carrying the balance,
  the limit and the magnitude of unposted activity), `card_transactions`
  (booked rows keyed on the API's own opaque row id), `card_invoices`
  (billing periods with their opening balance, turnover and settlement
  date, plus whether the four figures reconcile) and `card_statements`
  (the statement PDFs, indexed). Separate tables rather than columns on
  `accounts` / `transactions`, because a card is keyed by an opaque
  token where a cash account is keyed by IBAN and carries the columns
  that join it to the PSN feed.

Full design notes including the per-entity gold-merge contract,
identifier conventions, IBAN ↔ PSN AcctId conversion, the
transaction-splice strategy, and the historical-snapshot
reconstruction approach in [DESIGN.md](DESIGN.md).

## Container build

Playwright + Chromium + PDF tooling is heavy; running it directly
on the host pollutes the OS. This toolkit ships as a Docker image
and runs entirely inside the container.

Base image: `mcr.microsoft.com/playwright/python:v1.62.0-noble`
(Ubuntu Noble, Chromium browser binary and all OS-level deps
pre-installed at `/ms-playwright/`). The Playwright Python package
itself is installed via `requirements.txt`, version-pinned to match
the base-image tag — bump both in lockstep. The Dockerfile pins to
a specific tag so reruns are deterministic.

### Build

```sh
git clone <this repo>
cd collectors/ubs-web
./ubs-web build         # one-time, ~10 min on first build
```

### Run

The repo ships a thin `ubs-web` shell wrapper around
`docker run`; the standard `~/.secrets → /secrets` and
`$XDG_DATA_HOME/wealthdb/<source> → /data` bind-mounts and the run lifecycle are
described in [collectors/README.md](../README.md#conventions-shared-across-collectors).
Credentials go in `~/.secrets/ubs-web.env` (the bank-level `~/.secrets/ubs.env`
also works as a fallback); see
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

In addition to the two standard mounts, this tool adds a third,
tool-specific `/debug` mount for opt-in screenshots / Playwright
traces / ad-hoc QR PNGs:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/debug` | `~/.cache/wealthdb/debug/ubs-web` | opt-in screenshots / Playwright traces / ad-hoc QR PNGs, and the whole `explore` recording |

Pass any debug-flag value as `/debug/...` so debug artefacts stay
out of the bronze/silver tree.

```sh
./ubs-web login --check
./ubs-web login --qr-png /debug/qr.png
./ubs-web download --dry-run --screenshot-dir /debug/download
./ubs-web download --lookback 1y        # explicit wider window (default = 90 days)
./ubs-web load                          # defaults under the /data mount
./ubs-web explore                       # VNC-driven capture into /debug
./ubs-web prune --dry-run               # print the deletion plan, delete nothing
./ubs-web prune                         # reclaim non-complete dumps
```

Override any of the host paths via env vars:
`UBS_WEB_SECRETS_DIR`, `UBS_WEB_DATA_DIR`, `UBS_WEB_DEBUG_DIR`.

#### Driving an `explore` session

`explore` is the one verb that runs the browser **headed**. It starts
Chromium on a virtual X11 display inside the container and serves that
display over VNC; the wrapper forwards a free host port (5900–6000) and
the container prints the port and a single-use password at startup:

```
explore: VNC ready on 127.0.0.1:5901
explore: password (single-use):  <generated per run>
```

Connect a VNC client to that address (macOS: `open vnc://localhost:5901`;
from a remote host, tunnel first with
`ssh -L 5901:127.0.0.1:5901 <host>`), then sign in and navigate. Nothing
in the session is scripted — the harness records what is driven, and the
read-only surface in [CLAUDE.md](CLAUDE.md) §1 applies to the person
driving it.

**Stop with Ctrl-C** rather than by closing the browser window. Both end
the recording and flush the artefacts, but only Ctrl-C leaves the session
readable long enough to save it back to the state file — which is what
lets `download` reuse a sign-in made here instead of challenging again.

The recording lands in a UTC-stamped subdir of the `/debug` mount:

| Artefact | What it is for |
| --- | --- |
| `network.jsonl` | every request and response with headers and text bodies, written line by line so a crash keeps what was seen. The endpoint shapes behind each screen are read off this. |
| `network.har` | the same traffic for a HAR viewer; flushed only on a clean exit, and rewritten with its credentials out once it is. |
| `clicks.jsonl` | one record per click with the attributes a selector is built from, plus navigation, download and lifecycle events. |
| `dom/<NNN>/` | each structurally distinct screen's DOM (one file per UBS frame) plus a screenshot and the page URLs. |
| `downloads/` | every file fetched during the session, sequence-prefixed so a reused filename cannot overwrite an earlier one. |
| `trace.zip`, `trace-chunks/` | opt-in with `--trace`. Unredacted: a trace is a zip of driver-written blobs whose DOM snapshots carry every input's value, so nothing masks it afterwards. |

These artefacts hold real account data and unredacted identifiers, and
the trace holds the credential itself: a trace cannot be rewritten after
the fact, so nothing redacts it
(collectors/README.md, "Captures carry credentials"). They live outside
bronze and outside the repo, and nothing derived from them belongs in a
tracked file (root [CLAUDE.md](../../CLAUDE.md) §4). `prune` reclaims
them by age along with the rest of the debug dir.

#### Reclaiming disk

`prune` removes whole run dirs that are **not** complete dumps: a
`--dry-run` shell (a manifest and no exports), or a walk that crashed
before finalising (an `in-progress` marker, or no `run.json` at all).
From a complete dump it strips one thing: `screenshots/`, the landmark
DOM + screenshot captures `download --debug` writes, which `load` never
reads. A complete dump's load inputs (`positions/`, `transactions/`,
`documents/`, `run.json`) are always kept and silver stays reproducible.
(ubs-web's other diagnostics — screenshots, Playwright traces and QR
PNGs — land in the external `--screenshot-dir` / `--trace` / `--qr-png`
outputs under the `/debug` mount, never in bronze, so prune never sees
them.) Deleting a non-complete dump surfaces on the next `load --force`
rebuild. An in-flight guard (`--min-age-hours`, default 1, keyed on
recent write activity) keeps it from removing a multi-window backfill
that is still running.

`download --debug` captures three landmarks into `<run>/screenshots/`:
`10-home` (the homepage the account and portfolio anchors are scraped
from — both misses only warn, so this DOM is the only record of why),
`30-documents` (the documents list the window-bisect walk reads its
counts out of), and `20-txn-<short>-failed` for any account whose
transaction export raised. Off by default, and a no-op under `--dry-run`,
which persists nothing to bronze.

### Headless remote host

The toolkit is built to run on a headless remote Linux host (e.g.
an always-on home server or a small VPS). The bronze directory
is `scp`'d back to a workstation for analysis. Chromium
runs headless inside the container; the MFA approval happens on
the phone, not in any UI on the remote host.

**First-run device verification.** UBS does device fingerprinting
on retail netbanking. The first login from a new IP (i.e. the
remote host's IP) is likely to trigger an extra device-trust step
on top of the normal MFA factor. Plan to do the very first
`login.py` run with `--screenshot-dir` enabled so you can `scp` the
captured screenshots down and see what UBS is asking for.
Alternatively, do the device-trust step once via a manual browser
session on the same outbound IP (e.g. SSH port-forwarding), then
run scripted afterwards.

## Relationship to ubs-psn

UBS has two collectors in this repo: this one and the sibling
[ubs-psn](../ubs-psn/). They are separate collectors with separate
silver DBs — `ubs-psn` lands in `ubs-psn.db` (canonical PSN MT/XML),
`ubs-web` lands in `ubs-web.db` (web-scraped statements +
PDF-derived rows) — that the wealthdb UBS adapter consumes side by
side.

The two silver shapes overlap (both produce transactions, holdings,
account-level snapshots) but differ in fidelity: PSN is the
authoritative source for any date where both feeds carry the same
event; `ubs-web` is the only source for historic dates before the
PSN agreement was activated. The two feeds therefore splice on a
per-relationship cutover date — see [DESIGN.md](DESIGN.md) and
[the adapter doc](../../wealthdb/docs/adapters/ubs.md) for how gold
reconciles them.

## Session lifecycle

UBS netbanking sessions, like Swissquote's, have two layers:

- **Session cookie** — set by UBS's auth gateway after the login +
  MFA flow completes. `login.py` persists it into
  `storageState.json` (at `--state-path`); `download.py` reuses
  that file directly. Lifetime is policy-driven; a re-login at
  least once per working session is the practical norm.
- **MFA gate** — UBS Access App push (or the configured fallback
  factor) on every fresh login. Cannot be scripted away.

So the normal rhythm is: run `login.py` once, then `download.py`
freely during the cookie's lifetime. When
`download.py` reports the session is dead, re-run `login.py`.
