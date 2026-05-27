# relevate-dump

Scrape Vested Benefits account positions, balances, transactions,
and documents from Relevate's customer portal
(`portal.pens-expert.ch`) and land them in a queryable SQLite
"silver" database. Read-only; CLI-only; REST-only; human-triggered;
bridges to [wealthdb](https://github.com/ptu/wealthdb) at the gold
layer as the future `relevate` adapter source.

Companion to [`swissquote-dump`](https://github.com/ptu/swissquote-dump),
[`ubs-web-dump`](https://github.com/ptu/ubs-web-dump),
[`fidelity-web-dump`](https://github.com/ptu/fidelity-web-dump),
and [`schwab-web-dump`](https://github.com/ptu/schwab-web-dump);
mirrors their bronze → silver → (wealthdb gold) layering and
their ground rules (read-only, never weaken auth, never leak PII).

## Status

- **Bronze (login + download) — working.** `login.py` replays
  Relevate's Airlock IAM auth flow (mTAN via SMS, no bearer
  token — cookie-only session); `download.py` walks
  `/middlelayer/v2/` and lands per-portfolio JSON + per-document
  PDFs into a versioned bronze tree.
- **Silver loader — working.** `load.py` walks the bronze tree,
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
  [CLAUDE.md §1](CLAUDE.md).
- **No 2FA automation.** Every fresh login pushes an mTAN to the
  operator's phone; the operator types it into stdin. No
  TOTP-secret storage, no SMS auto-grab, no email-forwarding
  rules.
- **No unattended scheduling.** Cron / launchd / Actions are
  out of scope — they can't survive the mTAN gate anyway, and
  they would invite session-cookie burn from parallel logins.
  See [CLAUDE.md §2](CLAUDE.md).
- **No mutation surface in the CLI.** No `--password` flag
  (would leak via `ps`). Credentials reach the toolkit via env
  vars sourced from `~/.secrets/relevate.env`.

## Prerequisites

- Docker (tested with Colima on macOS — note that if you
  populate `mounts:` in `colima.yaml`, you must list
  `/Users/<you>` explicitly or the default `$HOME` share goes
  away).
- `~/.secrets/` directory (`chmod 700`) with a `relevate.env`
  file (`chmod 600`).

## Quick start

```sh
# 1. Put credentials in ~/.secrets/relevate.env (chmod 600):
#       export RELEVATE_LOGIN='your-login-or-OASI-number'
#       export RELEVATE_PASSWORD='your-password'
#
# 2. Build the image.
./relevate-dump build

# 3. Mint a session. Prompts on stdin for the mTAN code sent to
#    your phone. Writes ~/.secrets/relevate-state.json (chmod 600).
./relevate-dump login

# 4. (Optional) verify the session is alive without burning a
#    fresh mTAN.
./relevate-dump login --check     # prints ALIVE / DEAD / MISSING

# 5. Dump bronze. ~10 sec for ~50 files (per-portfolio JSON +
#    per-document PDFs).
./relevate-dump download

# 6. Parse bronze into silver SQLite (~/wealthdb/relevate/relevate.db).
#    Idempotent: re-running skips dumps already loaded.
./relevate-dump load
```

Iteration-cheap reruns of `download` for one slice:

```sh
./relevate-dump download --dry-run                       # just enumerate; no fetches
./relevate-dump download --mode accounts                 # master listing + ancillaries
./relevate-dump download --mode portfolios --limit-portfolios 1
./relevate-dump download --mode documents --limit-documents 3
./relevate-dump download --skip-documents                # everything except PDFs
```

## Layout

```
relevate-dump/
├── relevate-dump        # host wrapper around docker run
├── Dockerfile           # python:3.12-slim + requests
├── entrypoint.sh        # login / download / load / sh dispatch
├── login.py             # Airlock auth: POST /b2c/access -> /password/check -> /mtan/otp/check
├── download.py          # GET /middlelayer/v2/{...} into bronze tree
├── load.py              # bronze -> silver SQLite, idempotent via dump_runs
├── migrations/          # numbered SQL migrations
│   └── 0001_initial.sql
├── requirements.txt     # requests only
├── README.md            # this file
├── DESIGN.md            # the design doc — read this
├── CLAUDE.md            # ground rules for agents
├── .gitignore
└── .dockerignore
```

On the host:

```
$HOME/.secrets/                       # chmod 700
├── relevate.env                      # chmod 600; export RELEVATE_LOGIN=...
└── relevate-state.json               # chmod 600; Airlock cookies after login

$HOME/wealthdb/relevate/              # bronze + (future) silver
├── <UTC-ts>/                         # one bronze dir per `download` run
│   ├── run.json                      # manifest
│   ├── accounts/                     # master listing + ancillaries
│   ├── portfolios/<slug>/            # per-portfolio JSON
│   └── documents/                    # PDF binaries + index.json
└── relevate.db                       # silver SQLite (when load.py lands)

$HOME/.cache/relevate-debug/          # opt-in scratch logs / traces
```

## Configuration

The wrapper sources `$HOME/.config/relevate-dump.cfg` if present
(plain bash, `key=value`). Override defaults:

```sh
RELEVATE_SECRETS_DIR=/path/to/secrets
RELEVATE_DATA_DIR=/path/to/wealthdb/relevate
RELEVATE_DEBUG_DIR=/path/to/cache/relevate-debug
RELEVATE_IMAGE=relevate-dump:latest
RELEVATE_CONTAINER=relevate-dump
```

`RELEVATE_LOGIN` and `RELEVATE_PASSWORD` live in
`$RELEVATE_SECRETS_DIR/relevate.env` (`chmod 600`). The file is
plain bash — the wrapper `source`s it before invoking docker, so
use `export KEY='value'` with single quotes to keep shell
metachars literal:

```sh
export RELEVATE_LOGIN='your-login-or-OASI-number'
export RELEVATE_PASSWORD='your-password-with-$pecial-chars'
```

## Subcommands

| Command    | Status |
|------------|--------|
| `build`    | Working |
| `login`    | Working (use `--check` to probe without burning an mTAN) |
| `download` | Working (use `--dry-run` to enumerate; `--mode` / `--limit-*` for iteration) |
| `load`     | Working (applies migrations, ingests not-yet-loaded bronze runs into SQLite silver) |
| `sh`       | Working (interactive shell in the container) |

`./relevate-dump help` prints the canonical list.

## Privacy

The repo is intended to be publishable. **Do not commit any
identifier-shaped value, response body, or screenshot that
contains real Relevate account data.** See
[CLAUDE.md §4](CLAUDE.md) for the full PII rules. Pre-commit:
grep the staged diff for known real values BEFORE the first
`git add`.
