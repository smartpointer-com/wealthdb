# relevate

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates the provider's web client and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates Relevate / Pensexpert's own web client**: it
authenticates with your credentials and your multi-factor confirmations
and replays the portal's private REST API. The session it holds is **fully
privileged** — the same login a human uses to manage the account — and
Relevate / Pensexpert offers no read-only sub-scope, so nothing but this
codebase's own discipline restricts the session to reading. If malicious
code were ever introduced into this repository, its dependency chain, or
the container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Relevate /
Pensexpert's terms of service; verifying that your use is permitted is
likewise your responsibility.

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
affiliated with, endorsed by, or sponsored by Relevate / Pensexpert or any
other financial institution; nothing in this repository is financial,
legal, or tax advice.

Scrape Vested Benefits account positions, balances, transactions,
and documents from Relevate's customer portal
(`portal.pens-expert.ch`) and land them in a queryable SQLite
"silver" database. Read-only; CLI-only; REST-only; human-triggered.

Part of the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions.

## How it works

- **Bronze (login + download).** `login.py` replays
  Relevate's Airlock IAM auth flow (mTAN via SMS, no bearer
  token — cookie-only session); `download.py` walks
  `/middlelayer/v2/` and lands per-portfolio JSON + per-document
  PDFs into a versioned bronze tree.
- **Silver loader.** `load.py` walks the bronze tree,
  applies SQL migrations, and ingests each not-yet-loaded run in
  one transaction. Idempotent via `dump_runs.snapshot_at`.
  Schema in [DESIGN.md §7](DESIGN.md).

## What it scrapes

- Per-portfolio: positions (target allocation), balances,
  performance time-series, fees, model-portfolio composition.
- Documents: quarterly reports, fee statements, credit notes,
  pension agreement, pension plan, investor profile, leaving
  statement (every PDF surfaced via `/middlelayer/v2/documents`).
- Ancillary metadata: contact messages / notifications,
  compliance products, SSO claims.

## What it does NOT do

- **No write actions.** No withdrawal requests, no beneficiary
  changes, no contact-detail edits. See
  [AGENTS.md §1](AGENTS.md).
- **No 2FA automation.** Every fresh login pushes an mTAN to the
  registered phone; the code is typed in on stdin. No
  TOTP-secret storage, no SMS auto-grab, no email-forwarding
  rules.
- **No unattended scheduling.** Cron / launchd / Actions are
  out of scope — they can't survive the mTAN gate anyway, and
  they would invite session-cookie burn from parallel logins.
  See [the repo-root AGENTS.md §2](../../AGENTS.md).
- **No mutation surface in the CLI.** No `--password` flag
  (would leak via `ps`). Credentials reach the toolkit via env
  vars sourced from `~/.secrets/relevate.env`.

## Prerequisites

- Docker (tested with Colima on macOS — note that if you
  populate `mounts:` in `colima.yaml`, you must list
  `/Users/<you>` explicitly or the default `$HOME` share goes
  away).
- Credentials in `~/.secrets/relevate.env` (`RELEVATE_LOGIN`,
  `RELEVATE_PASSWORD`); see
  [collectors/README.md](../README.md#conventions-shared-across-collectors)
  for the shared env-file rules.

## Quick start

```sh
# 1. Put credentials in ~/.secrets/relevate.env (chmod 600):
#       export RELEVATE_LOGIN='your-login-or-OASI-number'
#       export RELEVATE_PASSWORD='your-password'
#
# 2. Build the image.
./relevate build

# 3. Mint a session. Prompts on stdin for the mTAN code sent to
#    your phone. Writes ~/.secrets/relevate-state.json (chmod 600).
./relevate login

# 4. (Optional) verify the session is alive without burning a
#    fresh mTAN.
./relevate login --check     # prints ALIVE / DEAD / MISSING

# 5. Dump bronze. A few seconds; per-portfolio JSON +
#    per-document PDFs.
./relevate download

# 6. Parse bronze into silver SQLite ($XDG_DATA_HOME/wealthdb/relevate/relevate.db).
#    Idempotent: re-running skips dumps already loaded.
./relevate load
```

Iteration-cheap reruns of `download` for one slice:

```sh
./relevate download --dry-run                       # just enumerate; no fetches
./relevate download --mode accounts                 # master listing + ancillaries
./relevate download --mode portfolios --limit-portfolios 1
./relevate download --mode documents --limit-documents 3
./relevate download --no-documents                  # everything except PDFs
./relevate download --documents-force               # re-fetch every PDF (no hardlink reuse)
```

Date-window flags (the shared collector-fleet contract):

```sh
./relevate download                             # default: last 90 days
./relevate download --lookback 1y               # 1w/4w/3m/6m/1y/2y/5y/all
./relevate download --lookback 2020-01-01       # an ISO date works too; the window's
                                                 # start drives both the /deposits year
                                                 # iteration AND the docs filter
```

`--lookback` names the window's starting point; the window runs from
there to today and covers everything the source offers in it. The
`/deposits` endpoint takes year granularity only, so the window's
start year bounds the iteration.

Documents are filtered AT FETCH: `download.py` reads
`/middlelayer/v2/documents` (always full index), skips PDF binaries
whose `createDate` falls outside the window, and writes the full
index to bronze for traceability.

In-window PDFs are download-avoidant across runs via the shared
`collectorkit.docdedup` engine: an executed-once immutable doc `load`
never parses (fee statement, pension agreement/plan, investor profile,
account-opening doc) identical to a prior complete run is hardlinked
in instead of re-fetched; every parsed / tax-adjacent doc (quarterly
report, credit note, leaving statement) and any unrecognised kind is
always fetched and content-compared (a byte-identical copy is still
hardlinked to reclaim disk, a re-issue keeps its fresh bytes). The
mode is chosen off the same `fileName`-derived kind `load.py` parses
on, so a parsed doc is never mis-linked. `--documents-force` bypasses
the index for a clean-slate re-fetch.

## Layout

```
relevate/
├── relevate        # host wrapper around docker run
├── Dockerfile           # wealthdb/base-python + requirements.txt
├── entrypoint.sh        # login / download / load / prune / sh dispatch
├── prune.py             # reclaim non-complete dumps (thin collectorkit.prune wrapper)
├── login.py             # Airlock auth: POST /b2c/access -> /password/check -> /mtan/otp/check
├── download.py          # GET /middlelayer/v2/{...} into bronze tree
├── load.py              # bronze -> silver SQLite, idempotent via dump_runs
├── pdf_parsers.py       # quarterly-report + credit-note PDF text parsers
├── migrations/          # numbered SQL migrations
│   ├── 0001_initial.sql
│   └── 0002_historical_snapshots.sql
├── tests/               # pytest suite (fixture-driven, no live calls)
├── requirements.txt     # pytest + requests + pypdf
├── README.md            # this file
├── DESIGN.md            # the design doc — read this
├── AGENTS.md            # ground rules for agents
├── .gitignore
└── .dockerignore
```

On the host:

```
$HOME/.secrets/                       # chmod 700
├── relevate.env                      # chmod 600; export RELEVATE_LOGIN=...
└── relevate-state.json               # chmod 600; Airlock cookies after login

$XDG_DATA_HOME/wealthdb/relevate/              # bronze + silver
├── <UTC-ts>/                         # one bronze dir per `download` run
│   ├── run.json                      # manifest
│   ├── accounts/                     # master listing + ancillaries
│   ├── portfolios/<slug>/            # per-portfolio JSON
│   └── documents/                    # PDF binaries + index.json
└── relevate.db                       # silver SQLite (written by load)

$HOME/.cache/wealthdb/debug/relevate/          # opt-in scratch logs / traces
```

## Configuration

The wrapper sources `$HOME/.config/relevate.cfg` if present
(plain bash, `key=value`). Override defaults:

```sh
RELEVATE_SECRETS_DIR=/path/to/secrets
RELEVATE_DATA_DIR=/path/to/wealthdb/relevate
RELEVATE_DEBUG_DIR=/path/to/cache/relevate-debug
RELEVATE_IMAGE=wealthdb/relevate:latest
RELEVATE_CONTAINER=relevate
```

`RELEVATE_LOGIN` and `RELEVATE_PASSWORD` live in
`$RELEVATE_SECRETS_DIR/relevate.env`; see
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

## Subcommands

| Command    | Purpose |
|------------|---------|
| `build`    | Build the container image |
| `login`    | Mint a session (use `--check` to probe without burning an mTAN) |
| `download` | Dump bronze (use `--dry-run` to enumerate; `--mode` / `--limit-*` for iteration) |
| `load`     | Applies migrations, ingests not-yet-loaded bronze runs into SQLite silver; skips a dump the walk never finished |
| `prune`    | Reclaims non-complete dumps from the bronze tree; `--dry-run` to preview |
| `sh`       | Interactive shell in the container |

`./relevate help` prints the canonical list.

### Reclaiming disk

```sh
./relevate prune --dry-run   # print the deletion plan, delete nothing
./relevate prune             # delete it
```

`prune` removes whole non-complete run dirs across the bronze tree: a
walk that crashed before writing a terminal `run.json` status, and
legacy `--dry-run` shells (a `--dry-run` today writes nothing under
bronze). From a *complete* dump it reclaims one thing — the
`screenshots/` HTTP trace a `download --debug` left behind, which a
routine download never writes. relevate drives no browser, so there are
no DOM dumps or Playwright traces beside it. The dump's data — including
the document PDFs read cross-dump for historical-snapshot and
credit-note parsing — is never touched, so silver stays reproducible.
Deleting a non-complete dump surfaces on the next `load --force`
rebuild. An in-flight guard (`--min-age-hours`, default 1, keyed on
recent write activity) keeps it from removing a download that is still
running. A `download` is born with `run.json` `status: "in-progress"`
and stamps `"complete"` / `"dry-run"` / `"incomplete"` at the end;
`prune` keys on that field, with an unreadable or corrupt manifest left
untouched (UNKNOWN, never deleted).

## Privacy

The repo is intended to be publishable. **Do not commit any
identifier-shaped value, response body, or screenshot that
contains real Relevate account data.** See
[the repo-root AGENTS.md §4](../../AGENTS.md) for the full PII rules. Pre-commit:
grep the staged diff for known real values BEFORE the first
`git add`.
