# schwab-web

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Charles Schwab with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Charles Schwab offers no read-only sub-scope, so nothing but this
codebase's own discipline restricts the session to reading. If malicious
code were ever introduced into this repository, its dependency chain, or
the container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Charles Schwab's
terms of service; verifying that your use is permitted is likewise your
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
affiliated with, endorsed by, or sponsored by Charles Schwab or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md) for the bronze → silver → gold model and [collectors/README.md](../README.md) for shared collector conventions.

A toolkit for ingesting Charles Schwab data the Trader API does
not cover: driving the Schwab client UI (`client.schwab.com`)
under Playwright to export historic account statements, position
snapshots, transaction history exports, and other archived
documents, then (in subsequent scripts) parsing the raw downloads
into a queryable SQLite silver database.

Sibling collectors:
[schwab-api](../schwab-api/),
[ubs-psn](../ubs-psn/),
[ubs-web](../ubs-web/),
[swissquote](../swissquote/).

## Why this design

[`schwab-api`](../schwab-api/) already
ingests Schwab Trader API data, which carries current account
metadata, positions, transactions, and open orders over a clean
read-only REST surface. The Trader API is the right channel for
going-forward data and for the live transaction history Schwab
exposes per account (one year per request).

It has one inflexible limitation: **the Trader API does not
expose a history of past account snapshots.** Positions and
balances are returned as-of-now only; there is no `as-of date`
parameter, no archive of yesterday's NAV, no monthly statement
endpoint. The transactions endpoint covers cash and trade events
but is not a substitute for the per-statement marked-to-market
snapshot a tax / NAV-history workflow needs.

The remaining channel for that historic backfill is the Schwab
client web UI, which exposes:

- **Statements archive** — multi-year PDF history of monthly
  account statements (positions, market values, cash activity,
  realised gains as of each month-end).
- **Tax forms archive** — annual 1099 composites, year-end
  realised gain/loss reports, supplemental tax forms.
- **Trade confirmations** — per-trade PDF confirmations.
- **Transaction history exports** — CSV / Excel exports of
  positions and transactions for arbitrary date ranges, beyond
  the Trader API's 1-year window.

Schwab does not expose any retail-accessible read API for the
historic snapshot data:

- **Trader API (Accounts and Trading – Individual)** — covers
  current state and ≤1-year transactions only; no historic
  snapshots, no statement PDFs (see above).
- **Schwab Open Banking / Aggregators (Plaid, MX, Finicity, etc.)**
  — aggregator coverage of Schwab brokerage data is shallow,
  USD-balance-of-the-day at best, and the data terms forbid
  redistribution into a personal tool.
- **Email feeds** — Schwab sends "new document available"
  notifications with no payload; the parseable detail is only
  inside the PDF behind the client login.

This toolkit automates the client-web channel under Playwright so
a one-off historic backfill is feasible without manually clicking
through years of statements.

A second-factor approval (SMS code / voice call / push / security
question — depends on the configured factor) is required
on every fresh login. Unattended cron is therefore impossible;
this toolkit is human-triggered (one code entry or biometric tap
per fresh session). Login and scrape run in one Firefox session —
Schwab invalidates the session when Firefox closes, so each
`download` pays one fresh MFA challenge.

## Tools

The architecture follows the `ubs-web` / `swissquote`
template; subcommand names and roles are the same, except login is
folded into `download` (one continuous Firefox session — no
standalone `login` verb):

| Script | Purpose |
| --- | --- |
| [`login.py`](login.py) | One-shot: pre-fill the login form from `SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD`, auto-click Log In, prompt for the 2FA code on stdin, fill, click Continue, then hand off to `download.walk()` against the same Firefox page. `--no-cli-mfa` falls back to the VNC-driven flow, where Log In + 2FA are driven by hand. `--check` validates the persisted profile (mostly diagnostic — Schwab invalidates the session on Firefox close). Driven by the wrapper's `download` subcommand. |
| [`download.py`](download.py) | `--mode statements`: walks the Statements & Tax Forms page per account, configures the chip filter to Statements / Tax Forms / Letters / Reports & Plans (Trade Confirms intentionally skipped), paginates the full result set, saves each PDF (plus XML / CSV for tax-form variants where Schwab offers them) under `<bronze-dir>/<UTC-ts>/statements/<suffix>/`. Writes `run.json` manifest incrementally with a `status` field (`in-progress` → `complete`/`dry-run`). `--mode transactions`: drives the Schwab "Export Transactions Data" modal to save CSV + JSON + XML of the full tx-history under `<bronze-dir>/<UTC-ts>/transactions/<suffix>/`; with `--debug`, also saves one landing HTML baseline under `<bronze-dir>/<UTC-ts>/screenshots/` (off by default; never read by load; reclaimed by `prune`). `--mode all` (the default) runs them in sequence. `--dry-run` walks without clicking PDF download buttons (the tx-history exports still fire; the dump is recorded `status=dry-run` so load skips it). By default, each transaction's "More" modal is also driven and the per-row detail (Settle Date / CUSIP / Principal / Commission / Industry Fee) stashed in a sidecar; `--no-more-detail` skips that pass — see DESIGN.md §7 for the cost trade-off. Read-only — see [CLAUDE.md](CLAUDE.md) §1. |
| [`pdf_parsers.py`](pdf_parsers.py) | Parses Schwab monthly brokerage statement PDFs across three layout eras: `parse_transactions` (the "Transaction Details" table → `TransactionRow` dicts with category, symbol/CUSIP, quantity, price, charges, amount, ST/LT realised gain/loss), `parse_positions` (the holdings block → position rows), and `parse_cash_summary` (the cash-flow summary). Statement-period header parsing supplies the year for MM/DD dates. Also `parse_distribution_pdf` for 3rd-Party-Distribution letters. Runnable standalone: `python3 pdf_parsers.py <pdf>...` emits JSON. Feeds `load.py` (closed accounts disappear from the Transaction History page, so PDF parsing is the only backfill path). |
| [`load.py`](load.py) | Parse bronze artefacts into a queryable SQLite silver database using schemas in `migrations/`. Applies pending migrations on startup; each dump loads atomically. Parses four transaction feeds: statement PDFs (`statement_pdf`), tx-history JSON (`tx_history_json`), 1099-Composite XML/CSV sale lots (`form_1099b`, XML preferred — see [`tax_form_parsers.py`](tax_form_parsers.py) + [DESIGN.md](DESIGN.md) §6a), and 3rd-Party-Distribution transfer letters (`third_party_distribution` — [DESIGN.md](DESIGN.md) §6b). Silver schema mirrors `schwab-api`'s conventions (snapshot_at, account_external_id, content-dedup payload columns) — see [DESIGN.md](DESIGN.md) for the gold-layer merge contract. |

### Browser choice — camoufox-patched Firefox

Schwab uses Akamai Bot Manager plus its own anti-bot rules. Stock
Playwright-driven browsers — Chromium and Firefox alike — get
flagged on credential submit even when Akamai's `_abck` cookie is
trusted; the rejection mode is "Invalid login ID or password"
with valid credentials, suggesting Schwab compares the
fingerprint observed at TLS / canvas / WebGL / audio against the
one trusted by the bot-manager. We tried headless / headed
Chromium, patchright (Chromium and Firefox flavours), upstream
Playwright Firefox, and even cookie import from a real macOS
Firefox session — Schwab rejected all of them.

What works: [camoufox](https://github.com/daijro/camoufox), a
stealth-patched Firefox fork that overrides every fingerprint
surface consistently for the `os="macos"` mode (so a Linux
container's browser presents as a macOS Firefox throughout the
stack, not just at the UA-string layer). Combined with the
2FA challenge that Schwab pushes on every fresh session, camoufox
gets through the credential-submit gate reliably.

The session is alive only while *that* Firefox is alive (Schwab
kills it on close), so login + scrape happen in one continuous
session. Each `download` invocation pays one fresh MFA challenge
— Schwab won't honour a persisted session across runs.

Observed 2FA challenge behaviour (2026-07): a wrong code leaves
the form up with Schwab's error shown. The flow still submits **at
most one code per run** — repeated challenge submissions are the
defect class that locked an account via schwab-api (2026-08; see
that collector's DESIGN.md §3.1) — so a rejection quotes Schwab's
on-page error verbatim and aborts with rc 8; a re-run mints a
fresh challenge. **Letting the form sit idle is fatal for that
login attempt** — Schwab locks it behind an identity-verification
banner and rejects every further code however fresh; that lock is
per-attempt, not per-account, and a re-run recovers. A gateway
terminal notice (`#/information/<code>` — account lockout among
them) aborts immediately in every wait loop with the page's own
message, rc 9; an account lockout must be cleared with Schwab
directly — nothing here retries into it.

The CLI is intentionally minimal:

* `download` — one-shot CLI-MFA login + scrape. Pre-fills from
  `SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD`, auto-submits, prompts
  on stdin for the 2FA code, runs the statements + tx-history
  download in the same Firefox session, exits. Defaults to a
  3-month range; the shared `--lookback` flag names the window's
  start. A preset (`1w`/`4w`/`3m`/`6m`/`1y`/`2y`/`5y`/`all`) maps to
  the narrowest Schwab preset that still covers it; an ISO date
  fills the custom `SpecifyDateRange` mode on both Statements and
  Transaction history (falling back to the covering preset if the
  fill fails). A window wider than `Last10Years` is capped at
  ~10 years, with a warning.
* `load` — parse the bronze tree into the silver SQLite DB.
* `vnc-login` — fallback to a VNC-driven login + scrape when the
  CLI-MFA selectors drift or a non-code challenge is required.

```sh
# Standard path: CLI-MFA login + scrape. stdin/stdout must be a
# TTY (the wrapper allocates one automatically when invoked from
# a terminal); the script prints
#   Schwab 2FA: enter your 2FA code, then press Enter.
#   > _
# at which point you type the code and press Enter.
./schwab-web download \
    --screenshot-dir /debug/login-$(date +%Y%m%dT%H%M%SZ) -v

# Full backfill (capped at Schwab's 10 years of statements, with a warning):
./schwab-web download --lookback all
./schwab-web download --lookback 2016-01-01  # explicit starting point

# Fallback path: VNC. Start the container with VNC enabled, then
# tunnel + open the display from your laptop. Use this if the
# CLI-MFA flow misses (e.g. Schwab restyles the gateway).
./schwab-web vnc-login \
    --screenshot-dir /debug/login-$(date +%Y%m%dT%H%M%SZ) -v
# Prints:
#   vnc-login: VNC ready on 127.0.0.1:5900
#   vnc-login: password (single-use):  <16 hex chars>
# (If 5900 is already taken on the host — another VNC server, a
# stray macOS Screen Sharing session — the wrapper walks +1 to
# the first free port and announces it before the banner above.)
# Then on your laptop, using the port the wrapper printed:
ssh -L 5900:127.0.0.1:5900 <mbp-host>      # tunnel
open vnc://localhost:5900                  # macOS Screen Sharing
# (use the single-use password printed above)

# Either entry point: once login completes the script takes over
# the same Firefox page, runs statements + transactions downloads,
# and exits. Bronze data lands on the host under
# $XDG_DATA_HOME/wealthdb/schwab-web/<UTC-ts>/.
```

## Container build

This is a Docker collector; the host wrapper and the standard
`~/.secrets → /secrets` / `$XDG_DATA_HOME/wealthdb/schwab-web → /data` mounts
follow the shared rules in
[collectors/README.md](../README.md#conventions-shared-across-collectors).
Tool-specific notes only:

- **Base image:** `mcr.microsoft.com/playwright/python:v1.59.0-noble`
  (Ubuntu Noble with all the OS-level deps Playwright wants
  pre-installed at `/ms-playwright/`, including the Firefox build
  this codebase uses). The Playwright Python package is pulled via
  `requirements.txt`, version-pinned to match the base-image tag —
  bump both in lockstep.
- **Extra `/debug` mount** (`~/.cache/wealthdb/debug/schwab-web` by default):
  opt-in login/landmark screenshots + Playwright traces. Point
  `--screenshot-dir` / `--trace` at `/debug/...` so those diagnostics
  stay OUTSIDE the bronze/silver tree. (Distinct from `--debug`,
  which saves a tx-history landing HTML baseline INSIDE the run dir
  under `<run>/screenshots/` — off by default, reclaimed by `prune`.)
- **Path overrides:** `SCHWAB_WEB_SECRETS_DIR`, `SCHWAB_WEB_DATA_DIR`,
  `SCHWAB_WEB_DEBUG_DIR`.

### Build

```sh
git clone <this repo>
cd collectors/schwab-web
./schwab-web build       # one-time, ~2-3 min on first build
```

### Run

```sh
./schwab-web download                                  # CLI-MFA login + scrape (default: 3 months)
./schwab-web download --lookback all                   # full backfill (capped at ~10y, warns)
./schwab-web download --lookback 2016-01-01            # explicit starting point
./schwab-web download --dry-run --screenshot-dir /debug/download
./schwab-web download --debug                          # + tx-history landing HTML baseline under <run>/screenshots/
./schwab-web vnc-login                                 # VNC fallback
./schwab-web load                                      # defaults under the /data mount
./schwab-web prune --dry-run                           # preview bronze reclaim (debug artefacts + non-complete dumps)
```

### Credentials

`login.py` reads two env vars inside the container:

- `SCHWAB_LOGIN_ID` — Schwab Login ID. Customer-identifying;
  treat as sensitive even though it is not strictly a secret.
- `SCHWAB_PASSWORD` — Schwab login password.

These deliberately do *not* share a prefix with `schwab-api`'s
`SCHWAB_CLIENT_ID` / `SCHWAB_CLIENT_SECRET` (OAuth credentials for
the Trader API), so the two credential sets never collide in a
single shell env.

Credentials go in `~/.secrets/schwab-web.env`; see
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules. The wrapper forwards both env vars
from the host into the container via `-e`. The login script also
reads `/secrets/schwab-web.env` directly (file value wins over an
already-set host env var — see the file-vs-host rationale in
`login.py`'s `load_env_file` docstring).

```sh
# ~/.secrets/schwab-web.env
SCHWAB_LOGIN_ID='your-login-id'
SCHWAB_PASSWORD='your-password-with-$pecial-chars'
```

### Driving over VNC from another machine

The toolkit is built to run on a headless remote Linux host (e.g.
an always-on home server or a small Mac on the LAN). From a
separate laptop, an SSH tunnel into the host plus any VNC client
(macOS Screen Sharing works out of the box) drives the
in-container Firefox during the login step.

Schwab does device fingerprinting on retail logins; if the IP
running the container is new, expect an extra device-trust prompt
on top of 2FA the first time. After that the persistent
profile dir under `/secrets/schwab-web-profile/` carries enough
state that subsequent vnc-login runs land on a trusted-device
challenge.

## Layout

```
<bronze-dir>/                              e.g. $XDG_DATA_HOME/wealthdb/schwab-web/
├── 20260520T120000Z/                      one bronze dump per run
│   ├── statements/
│   │   └── <suffix>/                      account suffix, e.g. "NNN"
│   │       ├── Brokerage-Statement_…PDF   monthly + quarterly statements
│   │       ├── 1099-Composite_….{PDF,XML,CSV}   tax forms
│   │       └── …                          letters, reports & plans
│   ├── transactions/
│   │   └── <suffix>/
│   │       ├── …_Transactions_…csv        Schwab "Export Transactions Data"
│   │       ├── …_Transactions_…json       CSV / JSON / XML of the
│   │       ├── …_Transactions_…xml        full filtered tx set
│   │       └── more-details.json          sidecar with per-row "More"-modal
│   │                                       data (written by default; absent
│   │                                       with --no-more-detail)
│   ├── screenshots/                       debug-only (download --debug):
│   │   └── tx-<suffix>-landing.html        tx-history landing HTML baseline.
│   │                                       Never read by load; `prune`
│   │                                       reclaims it. Absent without --debug.
│   └── run.json                           manifest written by download.walk();
│                                           carries a `status` field
│                                           (in-progress → complete / dry-run)
├── 20260521T120000Z/
│   └── …
└── schwab-web.db                          silver SQLite database (default)
```

Bronze and silver paths are independently configurable. Trade
Confirmations are deliberately skipped at the chip-filter step
(low signal, high volume — see [CLAUDE.md](CLAUDE.md) §1).

## Reclaiming disk (`prune`)

`prune` deletes two categories from the bronze tree, across every
timestamped run dir, and never touches a `load` input:

- **`<run>/screenshots/` from complete dumps** — the tx-history
  landing HTML baselines `download --debug` writes. `load` never
  reads them, so removing them leaves silver byte-identical.
- **`<run>/transactions/*/page-*.html` orphans from complete dumps** —
  older run dirs carry an ungated copy of that same landing-page HTML
  inside the `transactions/<suffix>/` load-input dir as `page-001.html`.
  `load` never reads them (the tx-history loader reads only
  `more-details.json` and the manifest's `.csv`/`.json`/`.xml`
  exports), so `prune` reclaims them with a file glob scoped to match
  only `page-*.html` — never a load-input sibling.
- **whole non-complete dumps** — a run whose `run.json` is missing
  (the walk crashed before its first manifest write) or whose
  `status` is anything other than `"complete"` (an `"in-progress"`
  marker from a crashed walk, or a `"dry-run"` shell whose
  tx-history exports still fired). `load` skips these too; after
  pruning one, the next `load --force` rebuild reflects the removal.

Completeness comes from the `run.json` `status` field
(`"in-progress"` at run-dir creation, atomically overwritten with
`"complete"` / `"dry-run"` at the end). A statusless manifest falls
back to `dry_run`: because `download` writes `run.json`
incrementally, presence alone is not proof of completion, so the
fallback keeps a real dump (`dry_run` false) and prunes a
`--dry-run` shell (`dry_run` true). An
unreadable or corrupt `run.json` is left untouched. An in-flight
guard (`--min-age-hours`, default 1, keyed on the newest write in
the dir) protects a long backfill still in progress.

```sh
./schwab-web prune --dry-run     # preview; deletes nothing
./schwab-web prune               # reclaim debug artefacts + non-complete dumps
```

`prune` runs host-side (a pure-stdlib file walk needs no
container), so it bypasses the single-writer safety guard and can
reclaim disk while a `download` is mid-flight — the in-flight dump
is protected by the age guard.

## Reclaiming disk (`collapse-statements`)

Schwab re-renders a statement PDF on **every** download, so the same
logical statement comes back with fresh bytes each run and the
`statements/` tree grows one full copy per run (see
[DESIGN.md §4.4](DESIGN.md)). The byte-identical `wealthdb-collect dedup`
sweep can't touch these — the bytes differ. `collapse-statements` reclaims
them by **parse-equivalence**: it parses each statement PDF exactly as `load`
does and, within one logical statement across runs, hardlinks every
copy whose parsed content is identical onto the oldest copy.

It is **silver-safe but lossy at the byte level** — the re-rendered
bytes of the newer copies are discarded (the oldest copy's bytes back
them all). This is safe because `load` never re-reads a statement PDF's
on-disk bytes against the manifest: it keys `documents` off the manifest
sha256, finds the PDF by filename, and re-parses whatever bytes are
there. `run.json` is never touched, and two copies collapse only when
they parse **identically**, so `load --force` reproduces byte-identical
silver. Copies of one logical statement that do NOT parse alike (a
genuine restatement, or parser nondeterminism) are reported as
**DIVERGENT** and left entirely alone.

Unlike `prune`, `collapse-statements` needs the image's PDF parser, so it
runs in-container like `load`. It is a deliberate manual one-off (not wired
into orchestration): run it once after a backlog of re-downloaded
statements has built up.

```sh
./schwab-web collapse-statements --dry-run   # evidence report; collapses nothing
./schwab-web collapse-statements             # collapse the parse-equivalent copies
```

`--dry-run` prints the per-group plan, any DIVERGENT groups, and the
reclaimable bytes; `--min-age-hours` (default 1) guards an in-flight
download; `--sample-text-diff K` additionally shows, for K groups, that
the raw text layers of equivalent copies differ only in render metadata.

## Relationship to schwab-api

Both toolkits land into Schwab-shaped silver databases that the
`wealthdb` Schwab adapter consumes. They do not share a silver
DB file: `schwab-api` writes to `schwab-api.db` (Trader API
JSON projected into a relational shape), `schwab-web` writes
to `schwab-web.db` (web-scraped documents + PDF-parsed
transactions). The wealthdb gold layer merges them at the
canonical-layer level — same as the UBS-PSN ↔ UBS-Web
cross-source merge already in place.

The schemas are aligned on column names and conventions
(`snapshot_at`, `account_external_id`, `payload` JSON, content
dedup) but operate on **different identifier spaces** — gold
needs an explicit bridge for account-id and transaction-id
alignment. The full merge contract lives in
[DESIGN.md](DESIGN.md); for the focused cross-repo summary
(asks for `schwab-api`, hints for the `wealthdb`
gold-layer maintainer) see [INTEROP.md](INTEROP.md).

## Session lifecycle

Schwab kills the session as soon as the Firefox process that
minted it exits. Closing Firefox and re-opening from the same
profile dir reliably gets `SessionTimeOut=y` regardless of how
fresh the cookies + `_abck` are on disk.

The consequence is structural: **login and scrape happen in one
continuous Firefox session.** `vnc-login` opens Firefox +
pre-fills the form, Log In + 2FA are completed by hand over
VNC, and the Python script takes over the same `page` to walk
Statements & Tax Forms and Transaction History.

The script exits as soon as the scrape completes — same shape
as the sibling toolkits. Each `download` invocation pays one
MFA challenge.

`--check` is mostly diagnostic — it'll report DEAD even on a
profile that just completed a successful login if Firefox was
closed in between.
