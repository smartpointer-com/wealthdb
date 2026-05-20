# schwab-web-dump

A toolkit for ingesting Charles Schwab data the Trader API does
not cover: driving the Schwab client UI (`client.schwab.com`)
under Playwright to export historic account statements, position
snapshots, transaction history exports, and other archived
documents, then (in subsequent scripts) parsing the raw downloads
into a queryable SQLite silver database for downstream tools —
e.g. local LLM-based agents and the
[`wealthdb`](https://github.com/ptu-gh/wealthdb) gold layer — to
consume.

Sibling projects:
[schwab-api-dump](https://github.com/ptu-gh/schwab-api-dump),
[ubs-psn-dump](https://github.com/ptu-gh/ubs-psn-dump),
[ubs-web-dump](https://github.com/ptu-gh/ubs-web-dump),
[swissquote-dump](https://github.com/ptu-gh/swissquote-dump). The
[DESIGN.md](https://github.com/ptu-gh/schwab-api-dump/blob/main/DESIGN.md)
document in `schwab-api-dump` covers the shared three-layer (bronze/
silver/gold) model and the conventions reused here.

## Why this design

[`schwab-api-dump`](https://github.com/ptu-gh/schwab-api-dump) already
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
question — depends on the user's configured factor) is required
on every fresh login. Unattended cron is therefore impossible;
this toolkit is human-triggered (one code entry or biometric tap
per fresh session) but reuses the persisted session cookie across
runs until Schwab invalidates it.

## Tools

The architecture follows the `ubs-web-dump` / `swissquote-dump`
template; subcommand names and roles are the same:

| Script | Status | Purpose |
| --- | --- | --- |
| [`login.py`](login.py) | implemented | `--manual`: open Firefox at the Schwab homepage, pre-fill the login form from `SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD`, block on Firefox close — the operator drives Log In + VIP 2FA themselves via VNC. `--check`: validate the persisted profile against the Account Summary URL and log the cookie jar including `_abck` trust state. Automated credential submit is not supported — Schwab anti-bot rejects it. |
| [`download.py`](download.py) | implemented | `--mode statements`: walks the Statements & Tax Forms page per account, configures the chip filter to Statements / Tax Forms / Letters / Reports & Plans (Trade Confirms intentionally skipped), paginates the full result set, saves each PDF (plus XML / CSV for tax-form variants where Schwab offers them) under `<dest>/<UTC-ts>/statements/<suffix>/`. Writes `run.json` manifest incrementally. `--mode transactions`: drives the Schwab "Export Transactions Data" modal to save CSV + JSON + XML of the full tx-history under `<dest>/<UTC-ts>/transactions/<suffix>/`, plus one landing HTML capture for debug. `--mode both` runs them in sequence. `--dry-run` walks without clicking PDF download buttons (the tx-history exports still fire). `--with-more-detail`: also drive each transaction's "More" modal and stash the per-row detail (Settle Date / CUSIP / Principal / Commission / Industry Fee) in a sidecar — off by default, see DESIGN.md §4.4 for why. Read-only — see [CLAUDE.md](CLAUDE.md) §1. |
| [`pdf_parsers.py`](pdf_parsers.py) | implemented (transactions section) | Extracts the "Transaction Details" table from Schwab monthly brokerage statement PDFs. Statement-period header parsing gives us the year for the MM/DD dates. Output: a list of `TransactionRow` dicts with category (Sale/Purchase/Withdrawal/Deposit/Dividend/Interest), symbol/CUSIP, quantity, price, charges, amount, and realised gain/loss (with ST/LT term). Runnable standalone: `python3 pdf_parsers.py <pdf>...` emits JSON. Will be used by `load.py` for closed-account history backfill (closed accounts disappear from the Transaction History page; PDF parsing is the only path). |
| [`load.py`](load.py) | implemented | Parse bronze artefacts into a queryable SQLite silver database using schemas in `migrations/`. Applies pending migrations on startup; each dump loads atomically. Silver schema mirrors `schwab-api-dump`'s conventions (snapshot_at, account_external_id, content-dedup payload columns) — see [DESIGN.md](DESIGN.md) for the gold-layer merge contract. |

### Browser choice — camoufox-patched Firefox, driven manually via VNC

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
stack, not just at the UA-string layer). Combined with a manual
login over VNC (Schwab still pushes a 2FA challenge to the user's
device on every fresh session — automating the form submit
remains tripwired regardless of fingerprint hygiene), camoufox
gets through the credential-submit gate reliably.

The session is alive only while *that* Firefox is alive (Schwab
kills it on close), so login and scrape happen in one continuous
session. The `vnc-login` subcommand wires up the keep-alive loop
that lets multiple scrape iterations share a single login.

```sh
# Once: start the container with VNC enabled, in the background
./schwab-web-dump vnc-login \
    --profile-dir /secrets/schwab-web-profile \
    --screenshot-dir /debug/login-$(date +%Y%m%dT%H%M%SZ) -v

# It prints:
#   vnc-login: VNC ready on 127.0.0.1:5900
#   vnc-login: password (single-use):  <16 hex chars>

# From your laptop:
ssh -L 5900:127.0.0.1:5900 <mbp-host>      # tunnel
open vnc://localhost:5900                  # macOS Screen Sharing
# (use the single-use password printed above)

# In the VNC window: Firefox is at www.schwab.com with the login
# form pre-filled. Click Log In, enter your VIP code, land on
# Account Summary. The script polls the URL; once it sees
# client.schwab.com/app/..., it takes over the same Firefox
# session and runs the statements + transactions download.
# When done, Firefox closes itself, the container exits, and
# bronze data is on the host under ~/wealthdb/schwab-web/<TS>/.
```

## Container build

Playwright + Firefox + PDF tooling is heavy; running it directly
on the host pollutes the OS. This toolkit ships as a Docker image
and runs entirely inside the container.

Base image: `mcr.microsoft.com/playwright/python:v1.59.0-noble`
(Ubuntu Noble with all the OS-level deps Playwright wants
pre-installed at `/ms-playwright/`, including the Firefox build
this codebase uses). The Playwright Python package is pulled via
`requirements.txt`, version-pinned to match the base-image tag —
bump both in lockstep.

### Build

```sh
git clone <this repo>
cd schwab-web-dump
./schwab-web-dump build       # one-time, ~2-3 min on first build
```

### Run

The repo ships a thin `schwab-web-dump` shell wrapper around
`docker run` that mounts three host paths into the container:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/secrets` | `~/.secrets` | Firefox profile dir, `schwab-web.env` |
| `/data` | `~/wealthdb/schwab-web` | bronze artefacts + silver DB |
| `/debug` | `~/.cache/schwab-web-debug` | opt-in screenshots / Playwright traces |

Pass any debug-flag value as `/debug/...` so debug artefacts stay
out of the bronze/silver tree.

```sh
./schwab-web-dump vnc-login --profile-dir /secrets/schwab-web-profile
./schwab-web-dump login --check --profile-dir /secrets/schwab-web-profile -v
./schwab-web-dump download --dry-run --screenshot-dir /debug/download
./schwab-web-dump load --silver-db /data/schwab-web.db --bronze-dir /data
```

Inside the container, `login.py`, `download.py`, and `load.py`
live at `/app/`; `/secrets/` is the credential mount; `/data/` is
the bronze + silver mount; `/debug/` is the opt-in debug-artefact
mount.

Override any of the host paths via env vars:
`SCHWAB_WEB_SECRETS_DIR`, `SCHWAB_WEB_DATA_DIR`,
`SCHWAB_WEB_DEBUG_DIR`.

If you prefer to drive `docker run` directly, the equivalent of
`./schwab-web-dump <cmd>` is:

```sh
docker run --rm -it \
    -v ~/.secrets:/secrets \
    -v ~/wealthdb/schwab-web:/data \
    -v ~/.cache/schwab-web-debug:/debug \
    schwab-web-dump:latest \
    <cmd> <flags>
```

### Credentials

`login.py` reads two env vars inside the container:

- `SCHWAB_LOGIN_ID` — Schwab Login ID. Customer-identifying;
  treat as sensitive even though it is not strictly a secret.
- `SCHWAB_PASSWORD` — Schwab login password.

These deliberately do *not* share a prefix with `schwab-api-dump`'s
`SCHWAB_CLIENT_ID` / `SCHWAB_CLIENT_SECRET` (OAuth credentials for
the Trader API), so the two credential sets never collide in a
single shell env.

The wrapper forwards both env vars from the host into the
container via `-e`. The login script also reads
`/secrets/schwab-web.env` directly (file value wins over an
already-set host env var — see the file-vs-host rationale in
`login.py`'s `load_env_file` docstring).

```sh
# ~/.secrets/schwab-web.env (chmod 600, never committed)
# Use SINGLE quotes around values with $/!/backtick — double
# quotes let `source` do $-expansion and silently mangle them.
SCHWAB_LOGIN_ID='your-login-id'
SCHWAB_PASSWORD='your-password-with-$pecial-chars'
```

### Driving over VNC from another machine

The toolkit is built to run on a headless remote Linux host (e.g.
an always-on home server or a small Mac on the LAN). The operator
sits at a separate laptop, SSH-tunnels into the host, and uses any
VNC client (macOS Screen Sharing works out of the box) to drive
the in-container Firefox during the login step.

Schwab does device fingerprinting on retail logins; if the IP
running the container is new, expect an extra device-trust prompt
on top of VIP 2FA the first time. After that the persistent
profile dir under `/secrets/schwab-web-profile/` carries enough
state that subsequent vnc-login runs land on a trusted-device
challenge.

## Layout

```
<bronze-dir>/                              e.g. ~/wealthdb/schwab-web/
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
│   │       ├── page-001.html              one debug snapshot
│   │       └── more-details.json          optional sidecar with per-row
│   │                                       "More"-modal data (only when
│   │                                       --with-more-detail is set)
│   └── run.json                           manifest written by download.walk()
├── 20260521T120000Z/
│   └── …
└── schwab-web.db                          silver SQLite database (default)
```

Bronze and silver paths are independently configurable. Trade
Confirmations are deliberately skipped at the chip-filter step
(low signal, high volume — see [CLAUDE.md](CLAUDE.md) §1).

## Relationship to schwab-api-dump

Both toolkits land into Schwab-shaped silver databases that the
`wealthdb` Schwab adapter consumes. They do not share a silver
DB file: `schwab-api-dump` writes to `schwab-api.db` (Trader API
JSON projected into a relational shape), `schwab-web-dump` writes
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
(asks for `schwab-api-dump`, hints for the `wealthdb`
gold-layer maintainer) see [INTEROP.md](INTEROP.md).

## Session lifecycle

Schwab kills the session as soon as the Firefox process that
minted it exits. Closing Firefox and re-opening from the same
profile dir reliably gets `SessionTimeOut=y` regardless of how
fresh the cookies + `_abck` are on disk.

The consequence is structural: **login and scrape happen in one
continuous Firefox session.** `vnc-login` opens Firefox +
pre-fills the form, the operator completes Log In + VIP 2FA via
VNC, and the Python script takes over the same `page` to walk
Statements & Tax Forms and Transaction History.

After the first scrape iteration, the script enters a keep-alive
loop: it polls for an external trigger file (default
`/data/.rerun`), and on touch reloads `download.py` + the
landmark module and re-runs the scrape against the same live
Firefox. This lets the operator iterate scrape code without
re-doing the MFA dance — the trigger is wired to a
`./schwab-web-dump rerun` subcommand on the host side. Set
`--rerun-trigger ''` on the `login.py` invocation to disable the
loop (the script exits after one iteration, Firefox closes,
Schwab kills the session).

`--check` is mostly diagnostic — it'll report DEAD even on a
profile that just completed a successful login if Firefox was
closed in between.
