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

Every fresh login needs a UBS Access App scan of a QR challenge.
Unattended cron is therefore impossible. The toolkit is
human-triggered: one scan per fresh session. Later runs reuse the
persisted session cookie until UBS invalidates it.

## Tools

| Script | Purpose |
| --- | --- |
| [`login.py`](login.py) | Drive headless Chromium through the UBS Nevis login dialog and the Access App QR challenge. It fills the contract number and renders the QR to the terminal (Unicode half-blocks the Access App scans directly). `--qr-png` also writes an upscaled PNG for a host with no usable terminal. It follows QR rotations, waits for the post-auth SPA URL, then saves the Playwright storage state at `--state-path` (default `/secrets/ubs-web-state.json`, chmod 0600; `ubs_web_state.json` is read when that file is absent). `--check` probes the stored session without a new QR challenge. |
| [`download.py`](download.py) | Reuse the persisted session and run five passes. **positions**: one `positions.csv` for the consolidated default view, plus one per `portfolioUid` found on the homepage. **transactions**: for each **cash** account on the homepage, the transaction list as CSV and as enriched SWIFT MT940, both bisected on the 1000-transaction export cap. **portfolio-transactions**: each managed portfolio's securities-transaction CSV, reached through the portfolio switcher ([DESIGN.md](DESIGN.md) §3.7b). **documents**: the eDocuments archive in adaptive windows (bisected on the 999-row display cap), each PDF fetched from `/api/v1/digital-banking/files/`. **cards**: the card surface via [`cards.py`](cards.py). `--only PASS` runs one pass and marks the dump `partial`. Writes a `run.json` manifest. Read-only — see [AGENTS.md](AGENTS.md) §1. |
| [`explore.py`](explore.py) | Discovery harness. Launches headed Chromium on the container's Xvfb display and serves it over VNC, so a session can be driven by hand while everything it produces is recorded: the network (requests, responses and bodies, line-flushed so a crash keeps the log), the clicks, one DOM snapshot plus screenshot per structurally distinct screen, and every file downloaded. Artefacts land under `--debug-dir`, never bronze. The harness itself never navigates and never clicks — it opens the login page and records from there, so the read-only surface in [AGENTS.md](AGENTS.md) §1 binds whoever drives. An existing session is reused and saved back on exit, so a sign-in here is not paid for twice. |
| [`cards.py`](cards.py) | The credit-card surface, read from the SPA's own REST API rather than scraped (see [DESIGN.md §5](DESIGN.md)): the roster, each card account's paged ledger, its billing periods with their reconciling totals, and each period's statement PDF. Read-only and enforced — an allow-list of read endpoints gates every request, including the paging cursor the ledger hands back, so a link the API advertises is never followed for being advertised. Driven by `download.py`; not a verb of its own. |
| [`card_parsers.py`](card_parsers.py) | Bronze → silver for the card surface: pure functions from the captured JSON to the rows `load.py` writes, with no database handle, so each is testable against a synthetic payload. Holds the three readings that are easy to get wrong — the row key is minted from row content, because the API's `_id` is re-minted at every login and `transactionNr` is not a key, a `RESERVED` row is unposted activity rather than a transaction, and `merchantName` is the category while `details` is the merchant. |
| [`prune.py`](prune.py) | Reclaim disk from the bronze tree and the `/debug` mount; see [Reclaiming disk](#reclaiming-disk). |
| [`load.py`](load.py) | Parse bronze artefacts into a queryable SQLite silver database using the schemas in [migrations/](migrations/). Applies pending migrations on startup; each dump loads atomically (compound-key UPSERT on transactions, content-hash dedup for documents, skip on `dump_runs` for idempotency). Parses the card surface via [`card_parsers.py`](card_parsers.py). Also walks the documents archive — the scraped one plus any bank-delivered PDFs under `supplied-documents/` — and reads it via [`pdf_parsers.py`](pdf_parsers.py) (uses `pdfplumber`, bundled in the image): positions with their cost, and the transaction list, from each "Statement of assets"; cash balances and movements from each "Account Statement"; mortgage balances from maturity notices; payments from credit and debit advices; and capital calls and contract notes ([DESIGN.md](DESIGN.md) §3.6–§3.10). |

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

`load` parses the CSV. The MT940 files stay in bronze as the
balance record; no loader reads them.

Both are a **cash-account** concern: SWIFT MT940 is not defined for
card transactions, and the card surface is not fetched as an export at
all (below).

### Why the card surface is fetched as JSON, not as an export

The card toolbar offers the same CSV and PDF exports the cash surface
does, and `download.py` fetches **neither**. The SPA reads its card area
from a REST API, and that JSON is strictly richer than what the export
renders from it: it carries a per-row `_id`, which the CSV has no
column for at all (a per-session handle, not a key; DESIGN.md §5.3),
plus the
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
| Transaction export, CSV and MT940 | 1000 transactions | MT940: an info-only dialog "A maximum of 1000 transactions can be exported" with no Export button. CSV: a dialog instead of a file | `_export_with_split` reads the rendered transaction count for the window; above 1000 it bisects and writes one file per leaf window, named `cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.<csv\|mt940>` |

Both bisections cap at `WINDOW_MAX_DEPTH = 20` and won't sub-divide
below `WINDOW_MIN_DAYS = 1`.

### Bronze layout

```
<bronze-dir>/
├── supplied-documents/                                            bank-delivered PDFs; no run, no manifest
│   └── <anything>.pdf                                             identified from the text it prints
└── 20260101T120000Z/                                              one run = one UTC-timestamped dir
    ├── run.json                                                   manifest (status, window, accounts, file inventory)
    ├── positions/
    │   ├── positions.csv                                          the consolidated default view
    │   └── positions_<sha256-prefix>.csv                          one per portfolio
    ├── transactions/
    │   ├── cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.csv         one or more per cash account
    │   └── cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.mt940       one or more per cash account
    ├── portfolio_transactions/
    │   └── transactions_<sha256-prefix>.csv                       one per managed portfolio
    ├── documents/
    │   └── <sha256>.pdf                                           content-addressed by the PDF bytes
    ├── cards/
    │   ├── accounts.json                                          the card roster, whole
    │   ├── transactions_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.json  every ledger page for one card account
    │   ├── invoices_<sha256-prefix>.json                          that account's billing periods
    │   ├── invoice-details_<sha256-prefix>.json                   each period's reconciling totals
    │   ├── statements_<sha256-prefix>.json                        which statement file came from which period
    │   └── statements/
    │       └── <sha256>.pdf                                       content-addressed by the PDF bytes
    └── screenshots/                                               only with `download --debug`; never read by load
```

`supplied-documents/` holds the documents UBS produces only on request:
they appear in no e-banking listing, so `download` cannot reach them and
no manifest describes them. Each is identified from its own text rather
than its filename, keyed by its content hash, and skipped if the scraped
archive already holds the same bytes. The directory is not a run slug, so
`load` never walks it as a dump and `prune` never deletes it; it lives
inside the bronze tree so that `load --force` still rebuilds silver from
bronze alone. See [DESIGN.md](DESIGN.md) §3.7a.

The `<sha256-prefix>` collapses the opaque UBS account-id token to
16 hex chars. UBS account-ids all share a ~34-char per-customer
prefix, so a naive slice would collide silently — hashing avoids it.

The `run.json` status lifecycle is the fleet's
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
Specific to ubs-web:

- `--dry-run` writes nothing to bronze, not even a `run.json`.
- `--only PASS` finalises the dump as `partial`, not `complete`.
- `load` reads `complete`, `partial` and statusless (legacy) dumps.
  It skips every other dump, and `prune` keeps exactly what `load`
  reads.

[DESIGN.md](DESIGN.md) §4 has the detail.

### Silver schema and gold-merge contract

Silver lives at `$XDG_DATA_HOME/wealthdb/ubs-web/ubs-web.db` by default;
companion PSN silver is at `$XDG_DATA_HOME/wealthdb/ubs-psn/ubs-psn.db` (from
`ubs-psn`). The schema is built by the files in
[migrations/](migrations/), and each file's header says what it adds.
Silver holds:

- what the live fetch reads: `banking_relationships`, `portfolios`,
  `accounts`, `positions`, `mortgages`, `transactions` and `documents`;
- a managed portfolio's trades from the portfolio export:
  `portfolio_transactions` (DESIGN.md §3.7b);
- the credit-card surface: `card_accounts`, `card_transactions`,
  `card_invoices` and `card_statements` (DESIGN.md §5.9);
- what the PDF archive states: `historical_position_snapshots`,
  `historical_cash_balances` and `historical_mortgages` (DESIGN.md
  §3.8), `advices` (§3.9) and `statement_trades` (§3.10). The
  statement-era cash movements land in `transactions` (§3.6);
- bookkeeping: `dump_runs`, `parser_generations` and `schema_meta`.

[DESIGN.md](DESIGN.md) holds the design notes: identifier
conventions, IBAN ↔ PSN AcctId conversion, the transaction splice and
the historical-snapshot reconstruction.

## Container build

Playwright, Chromium and the PDF tooling are heavy, so the toolkit
ships as a Docker image and runs inside the container. The Dockerfile
builds on the shared `wealthdb/base-playwright` image, which pins
`mcr.microsoft.com/playwright/python:v1.64.1-noble` (Chromium and its
OS deps under `/ms-playwright/`). `requirements.txt` pins the
Playwright Python package to the same version; bump both in lockstep.

### Build

```sh
make build-ubs-web      # from the repo root; builds the base images first
```

### Run

The `ubs-web` wrapper drives `docker run`. The `/secrets` and `/data`
mounts, the directory overrides and the env-file rules are the fleet's
([collectors/README.md](../README.md#conventions-shared-across-collectors)).
Credentials go in `~/.secrets/ubs-web.env`, as one line
`UBS_CONTRACT_NUMBER=<contract-number>`. The bank-level
`~/.secrets/ubs.env` is read when `ubs-web.env` does not exist.

The tool adds a `/debug` mount for opt-in diagnostics:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/debug` | `~/.cache/wealthdb/debug/ubs-web` (`UBS_WEB_DEBUG_DIR`) | opt-in screenshots / Playwright traces / ad-hoc QR PNGs, and the whole `explore` recording |

Pass any debug-flag value as `/debug/...` so debug artefacts stay
out of the bronze/silver tree.

```sh
./ubs-web login --check
./ubs-web login --qr-png /debug/qr.png
./ubs-web download --dry-run --screenshot-dir /debug/download
./ubs-web download --lookback 1y        # explicit wider window (default = 90 days)
./ubs-web download --only cards         # one pass; the dump is marked partial
./ubs-web load                          # defaults under the /data mount
./ubs-web explore                       # VNC-driven capture into /debug
./ubs-web prune --dry-run               # print the deletion plan, delete nothing
./ubs-web prune                         # reclaim non-complete dumps and old debug captures
```

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
read-only surface in [AGENTS.md](AGENTS.md) §1 applies to the person
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
tracked file (root [AGENTS.md](../../AGENTS.md) §4). `prune` reclaims
them by age along with the rest of the debug dir.

#### Reclaiming disk

`prune` follows the fleet's safety envelope
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
For ubs-web it reclaims:

- whole run dirs that `load` skips: a crashed walk (`in-progress` or
  no `run.json`), or a legacy `dry-run` shell;
- `screenshots/` inside a dump it keeps;
- aged entries of the `/debug` mount, including `explore` recordings.

A `partial` dump from `--only` is a load input, so `prune` keeps it.

`download --debug` captures these landmarks into `<run>/screenshots/`:

- `10-home`: the homepage the account and portfolio anchors are scraped
  from. Both misses only warn, so this DOM is the only record of why.
- `20-txn-<short>-failed`: the page an account's transaction export
  raised on.
- `25-portfolio-txn-failed`: the page the portfolio-transaction pass
  raised on.
- `30-documents`: the documents list the window-bisect walk reads its
  counts from.

They are off by default, and `--dry-run` writes none.

### Headless remote host

The toolkit runs on a headless remote Linux host, such as an
always-on home server or a small VPS. Chromium runs headless inside the
container. The Access App scan happens on the phone, against the QR
the terminal shows.

**First-run device verification.** UBS fingerprints devices on retail
netbanking. The first login from a new IP is likely to add a
device-trust step on top of the QR challenge. A first `login` with
`--screenshot-dir` records what UBS asks for. The other route is one
manual browser session from the same outbound IP (for example over SSH
port forwarding) before the first scripted login.

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

A UBS netbanking session has two layers:

- **Session cookie.** UBS's auth gateway sets it once the login and
  the QR challenge complete. `login` saves it to the state file at
  `--state-path`, and `download` and `explore` reuse that file. UBS
  sets its lifetime by policy.
- **Access App QR challenge.** Every fresh login raises one. It cannot
  be scripted away.

So `login` runs once, and `download` runs freely while the cookie
lives. When `download` reports the session is dead, `login` runs
again.
