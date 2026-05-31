# schwab-api

Part of the **wealthdb** suite — see [the architecture overview](../../ARCHITECTURE.md) for the bronze → silver → gold model and [collectors/README.md](../README.md) for shared collector conventions.

A toolkit for ingesting Charles Schwab Trader API portfolio data:
fetching account metadata, positions, transactions, and open orders
over the read-only subset of the Schwab REST API, then parsing the
raw JSON into a queryable SQLite silver database.

## Tools

| Script | Status | Purpose |
| --- | --- | --- |
| [`login.py`](login.py) | implemented | Interactive OAuth login flow; mints the token file that `download.py` consumes. Required once initially and once per 7-day refresh window thereafter. |
| [`download.py`](download.py) | implemented | Fetches account hashes, user preferences, positions, transactions, and open orders over the Schwab REST API and stores the raw JSON locally, organised by UTC timestamp. Read-only. |
| [`load.py`](load.py) | implemented | Parses raw JSON dumps into a queryable SQLite silver database. Applies pending migrations on startup; each dump loads atomically. Idempotent — already-loaded dumps are skipped. |

See [DESIGN.md](DESIGN.md) for the Schwab-specific design rationale
(semi-relational silver schema, temporal model, why each script
exists).

## Layout

The toolkit assumes a directory layout like:

```
<bronze-dir>/                       e.g. ~/wealthdb/schwab-api/
├── 20260512T104753Z/               one bronze dump per run
│   ├── account_numbers.json
│   ├── user_preference.json
│   ├── accounts_positions.json
│   ├── transactions_NNN.json
│   └── open_orders.json
├── 20260513T104753Z/
│   └── ...
└── schwab.db                       silver SQLite database (default name)
```

Bronze and silver paths are independently configurable; the layout
above is the path of least resistance for personal use.

## Token lifecycle

Schwab issues two tokens with very different lifetimes:

- **Access token** — 30 minutes. Refreshed transparently by `schwab-py`
  whenever a request needs one. You never touch this.
- **Refresh token** — **7 days, hard cap.** Cannot be renewed
  programmatically; the user must repeat the OAuth authorization flow
  in a browser. `login.py` exists solely to perform this re-auth.

So in a normal week the rhythm is: run `login.py` once, then
`download.py` whenever you want fresh data. Once a week, `download.py`
will start reporting `invalid_client`; that's the signal to re-run
`login.py`.

## login.py

### How it works

`login.py` runs the OAuth 2.0 authorization-code flow against
`api.schwabapi.com`. In the default (automated) mode it spins up a
local HTTPS server on the callback URL, opens the system browser to
the Schwab consent page, captures the redirect, exchanges the code,
and writes the resulting token bundle to the path you specify. The
file is chmod'ed to `0600` after creation.

The browser will show a self-signed-certificate warning on the
callback step because `schwab-py` generates a fresh cert for the
local server. Accept it and continue — the cert only protects traffic
between Schwab's redirect and your localhost.

### Usage

Initial mint (and weekly re-mint):

```sh
.venv/bin/python login.py --token-path ~/.secrets/schwab-api-token.json
```

Headless / SSH environments — paste the redirect URL by hand:

```sh
.venv/bin/python login.py --token-path ~/.secrets/schwab-api-token.json --manual
```

Check whether the current token still has refresh-window life left
(no browser, no network):

```sh
.venv/bin/python login.py --token-path ~/.secrets/schwab-api-token.json --check
```

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--token-path` | _(required)_ | Path to read/write the OAuth token JSON file. |
| `--client-id` | _(env `SCHWAB_CLIENT_ID`)_ | Schwab OAuth Client ID. Falls back to env var. |
| `--client-secret` | _(env `SCHWAB_CLIENT_SECRET`)_ | Schwab OAuth Client Secret. Falls back to env var. |
| `--callback-url` | `https://127.0.0.1:8182` | OAuth callback URL. Must exactly match the value registered in your Schwab app. |
| `--manual` | off | Use the paste-the-URL flow instead of the local-HTTPS-server flow. |
| `--check` | off | Inspect the token file and print its age and estimated expiry. No browser, no network. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

## download.py

### How it works

Schwab issues an OAuth access token that is valid for 30 minutes and a
refresh token that is valid for 7 days. `download.py` loads a previously
minted token file and refreshes the access token transparently for the
duration of the run.

It then fetches:

1. The account-number → hash mapping (`/accounts/accountNumbers`).
2. The user preferences blob (`/userPreference`).
3. Accounts with positions (`/accounts?fields=positions`).
4. Transactions per account, chunked into ≤1-year windows.
5. Open orders across all accounts, filtered to non-terminal statuses.

Every response is written to disk verbatim as JSON. No parsing,
normalisation, or filtering happens at this stage; that is the silver
layer's job.

The CLI never imports or calls any Schwab write endpoints (order
placement, replacement, cancellation, or transfers). See
[CLAUDE.md](CLAUDE.md) §1.

### Prerequisites

- Python 3.11+
- A Schwab developer account with an approved app under the **Trader API
  – Individual** product. The app's Callback URL must be registered as
  `https://127.0.0.1:8182` (or whatever URL you intend to use for
  `login.py`). Setting the app's order rate limit to `0` is recommended
  as an additional read-only safeguard.
- The app's **Client ID** and **Client Secret** (as Schwab labels them
  in its developer portal), made available to the scripts as
  `SCHWAB_CLIENT_ID` and `SCHWAB_CLIENT_SECRET` environment variables.
- An OAuth token file at the path passed via `--token-path`. Run
  `login.py` to mint one. Refresh tokens expire 7 days after issue; you
  will need to re-run `login.py` weekly.

### Setup

```sh
git clone <this repo>
cd collectors/schwab-api
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

Credentials go in `~/.secrets/schwab-api.env` (`SCHWAB_CLIENT_ID`,
`SCHWAB_CLIENT_SECRET`); see
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

```sh
source ~/.secrets/schwab-api.env
.venv/bin/python login.py --token-path ~/.secrets/schwab-api-token.json
```

### Configuration

`download.py` reads no config files. All inputs are CLI flags or
environment variables. Defaults are conservative:

- Token file path: `~/.secrets/schwab-api-token.json` (override with
  `--token-path`).
- Output directory: `~/wealthdb/schwab-api` (override with `--dest`).
- Transaction window: last 90 days (`--lookback 1w|4w|3m|6m|1y|2y|5y|all`
  for a named shortcut, or `--since YYYY-MM-DD` for an explicit
  lower bound; Schwab caps each API request at 1 year, so the loader
  chunks longer windows automatically).
- Client ID / Client Secret: passed via `--client-id` / `--client-secret`,
  or read from `SCHWAB_CLIENT_ID` / `SCHWAB_CLIENT_SECRET` env vars as
  fallback.

### Usage

Dry run — refreshes tokens, lists linked accounts, exits without
fetching positions, transactions, or open orders:

```sh
.venv/bin/python download.py --dry-run
```

Real download (last 90 days, the default):

```sh
.venv/bin/python download.py
```

Wider backfill via the shared `--lookback` shortcut, or an explicit
date:

```sh
.venv/bin/python download.py --lookback 1y
.venv/bin/python download.py --since 2024-01-01
```

Files land in `./data/<UTC-timestamp>/`:

| File | Source endpoint |
| --- | --- |
| `account_numbers.json` | `/accounts/accountNumbers` |
| `user_preference.json` | `/userPreference` |
| `accounts_positions.json` | `/accounts?fields=positions` |
| `transactions_NNN.json` | `/accounts/{hash}/transactions` (one file per account × window) |
| `open_orders.json` | `/orders` (cross-account), filtered to non-terminal statuses |
| `instruments.json` | `/marketdata/v1/instruments` (only when `--with-instruments` is passed) — basic metadata (symbol, cusip, description, exchange, type, assetType) for every symbol seen in positions and transactions |

Transaction files are numbered rather than tagged with the account hash
so that `ls`ing a dump directory does not leak account identifiers. The
hash, window bounds, and per-transaction payload live inside each file.

`open_orders.json` only contains orders whose status is non-terminal
(WORKING, PENDING_*, AWAITING_*, NEW, ACCEPTED, QUEUED, etc.). Closed
orders — FILLED, CANCELED, REJECTED, EXPIRED, REPLACED — are dropped at
the dump layer; full order history is intentionally not captured.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--token-path` | `~/.secrets/schwab-api-token.json` | Path to the OAuth token JSON file. |
| `--dest` | `~/wealthdb/schwab-api` | Local destination directory. |
| `--client-id` | _(env `SCHWAB_CLIENT_ID`)_ | Schwab OAuth Client ID. Falls back to env var. |
| `--client-secret` | _(env `SCHWAB_CLIENT_SECRET`)_ | Schwab OAuth Client Secret. Falls back to env var. |
| `--since` | _today − 90d_ | Earliest transaction date (YYYY-MM-DD). Schwab caps the API window at 1 year per request; the loader chunks longer ranges automatically. |
| `--until` | _today (UTC)_ | Latest transaction date (YYYY-MM-DD, inclusive). |
| `--lookback` | _unset_ | Named shortcut: `1w` / `4w` / `3m` / `6m` / `1y` / `2y` / `5y` / `all`. Sets `--since` to `until − X`; overridden by an explicit `--since`. |
| `--with-instruments` | off | After positions and transactions, look up metadata for every symbol seen and write a separate `instruments.json` artefact. Schwab omits `description` on equity positions/transactions; this fills the gap consistently across asset classes. Intended for reduced-schedule runs (instrument metadata changes rarely). |
| `--dry-run` | off | Skip data fetch; only validate auth and list accounts. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### Caveats

- **7-day refresh ceiling.** Schwab refresh tokens cannot be programmatically
  extended past 7 days; you must re-run `login.py` at least once a week.
  If `download.py` reports `invalid_client` on token refresh, the
  refresh window has expired.
- **No read-only OAuth scope exists.** The token `download.py` loads
  could place orders if the wrong code called the wrong endpoint. This
  repo's contract — enforced by code review — is that it never imports
  or calls any write endpoint. See [CLAUDE.md](CLAUDE.md) §1.
- **All amounts USD.** The Schwab API does not return a currency field
  on positions or transactions; everything is implicitly USD.
- **No retry / resume / scheduling.** Run from cron, launchd, or your
  scheduler of choice.

## load.py

### How it works

Parses one or more bronze dump directories (as produced by
`download.py`) and inserts them into a SQLite silver database. The
schema is defined in [`migrations/0001_initial.sql`](migrations/0001_initial.sql);
the loader applies any pending migrations on startup before loading
data, so the silver database is always at the latest schema version.

Each dump is loaded atomically — a failure mid-load rolls back to the
prior state, and a re-run retries the whole dump. Already-loaded
dumps are detected via `dump_runs` and skipped, so the loader is safe
to point at a `--bronze-dir` containing a mix of new and already-
processed snapshots.

Reload semantics:

- **Snapshots** (`accounts`, `user_preference`, `account_balances`,
  `positions`, `open_orders`, `instruments`) are append-only. Each
  dump produces a new row per (snapshot, entity), with three exceptions:
  - `accounts` deduplicates against the most recent row for each
    account: insert only if the canonical-JSON payload differs.
  - `user_preference` does the same, after stripping per-request
    noise fields (e.g. `schwabClientCorrelId`, which Schwab
    regenerates on every API call). Bronze keeps the original.
  - `instruments` deduplicates per symbol, same direct payload
    comparison. Populated only when bronze contains an
    `instruments.json` (i.e. when `download.py --with-instruments`
    was used).
- **Events** (`transactions`) use window-DELETE-then-INSERT per
  `(account, time-window)`. The dump emits non-overlapping windows;
  the loader replaces exactly that range, which catches upstream
  removals/corrections that row-level upsert would miss.

See [DESIGN.md](DESIGN.md) §4 for the full rationale.

### Usage

```sh
.venv/bin/python load.py            # defaults under ~/wealthdb/schwab-api
```

The loader scans `<bronze-dir>` for subdirectories whose names match
the dump-timestamp format `YYYYMMDDTHHMMSSZ` and processes each one
not already recorded in `dump_runs`.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--silver-db` | `~/wealthdb/schwab-api/schwab-api.db` | Path to the silver SQLite database. Created if missing. |
| `--bronze-dir` | `~/wealthdb/schwab-api` | Directory containing bronze dump subdirectories. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### Schema migrations

To change the silver schema, add a new `migrations/NNNN_<slug>.sql`
file. The number must be strictly greater than any existing migration.
Each file:

- Contains the DDL and any data backfill needed.
- Ends with `INSERT INTO schema_meta (silver_schema_version, applied_at)
  VALUES (N, CAST(strftime('%s','now') AS INTEGER));` as the
  migration-complete marker the loader checks for.

The loader executes each new migration in numeric order and commits
between files. Silver databases must always be at the latest schema —
never write code that handles "if column X exists".
