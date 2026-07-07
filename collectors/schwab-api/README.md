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
| [`prune.py`](prune.py) | implemented | Reclaims disk by deleting non-complete dumps (crashed / interrupted downloads) from the bronze tree. Host-side, like `load`. `--dry-run` previews the plan. |
| [`recompress.py`](recompress.py) | implemented | One-time backlog sweep: replaces the plain data JSON inside pre-compression complete dumps with sha256-verified `.json.zst` twins — the form `download` now writes. `run.json` is never compressed. Host-side, manual only, never scheduled. `--dry-run` previews the plan. |

See [DESIGN.md](DESIGN.md) for the Schwab-specific design rationale
(semi-relational silver schema, temporal model, why each script
exists).

## Layout

The toolkit assumes a directory layout like:

```
<bronze-dir>/                       e.g. $XDG_DATA_HOME/wealthdb/schwab-api/
├── 20260512T104753Z/               one bronze dump per run
│   ├── run.json                    status manifest (in-progress → complete) — NOT compressed
│   ├── account_numbers.json.zst
│   ├── user_preference.json.zst
│   ├── accounts_positions.json.zst
│   ├── transactions_NNN.json.zst
│   └── open_orders.json.zst
├── 20260513T104753Z/
│   └── ...
└── schwab.db                       silver SQLite database (default name)
```

The six data artefacts are zstd-compressed in place as they land
(`.json.zst`; plain `.json` in pre-compression dumps — both forms load,
and the loader decompresses in Python). `run.json` stays uncompressed:
it is the status manifest `prune` and `load` read directly and must stay
greppable. See [DESIGN.md](DESIGN.md) §3.2 for the compression +
convergence contract and the manual `recompress` backlog sweep.

Bronze and silver paths are independently configurable; the layout
above is the path of least resistance for personal use.

## Token lifecycle

Schwab issues two tokens with very different lifetimes:

- **Access token** — 30 minutes. Refreshed transparently by `schwab-py`
  whenever a request needs one. You never touch this.
- **Refresh token** — **7 days, hard cap.** Cannot be renewed
  programmatically; the OAuth authorization flow must be repeated
  in a browser. `login.py` exists solely to perform this re-auth.

So in a normal week the rhythm is: run `login.py` once, then
`download.py` whenever you want fresh data. Once a week, `download.py`
will start reporting `invalid_client`; that's the signal to re-run
`login.py`.

## login.py

### How it works

`login.py` performs the OAuth 2.0 authorization-code grant against
`api.schwabapi.com`, driven through a **headed Camoufox browser in a
container** — the same anti-bot-resistant browser the
[`schwab-web`](../schwab-web/) collector uses. It builds the authorize
URL, opens it in Camoufox on an Xvfb display, **pre-fills** the Schwab
login from `SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD` (in
`~/.secrets/schwab-web.env` — the same credentials `schwab-web` uses),
auto-submits, **prompts for the 2FA code on stdin**, and drives the
consent pages — including ticking **every** checkbox on the "Select your
Schwab accounts to link" page, so a newly opened account is linked
without anyone remembering to tick it. It captures the `?code=…` redirect
straight from the browser and exchanges it for the token bundle (chmod
`0600`). All browser activity is traced to the debug dir
(`~/.cache/schwab-api-debug`).

This is why schwab-api is a **hybrid** collector: `login` runs in a
Camoufox container while `download` and `load` run on the host venv
(plain schwab-py REST + SQLite). The OAuth app credentials
(`SCHWAB_CLIENT_ID` / `SCHWAB_CLIENT_SECRET`) live in
`~/.secrets/schwab-api.env`.

### Usage

`login` runs in the container — build it once (`./schwab-api build`, or
`make build-schwab-api`). It's automated end-to-end; just answer the 2FA
prompt on the terminal (stdin must be a TTY):

```sh
./schwab-api login
# pre-fills + submits the login, prompts for your 2FA code, ticks all
# accounts, clicks through consent, writes the token.
```

If Schwab changes the UI and the automated selectors drift, fall back to
**`vnc-login`**, which drives the same flow but lets you complete login /
2FA / consent yourself over a VNC client (it prints the tunnel + a
single-use password); the account checkboxes are still auto-ticked:

```sh
./schwab-api vnc-login
```

`--manual` is the no-browser paste-the-URL flow. Check the current token's
remaining refresh-window life (no browser):

```sh
./schwab-api login --check
```

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--token-path` | `/secrets/schwab-api-token.json` | Path to read/write the OAuth token JSON file (the wrapper maps it to your secrets dir). |
| `--client-id` | _(env `SCHWAB_CLIENT_ID`)_ | Schwab OAuth Client ID. Falls back to env var. |
| `--client-secret` | _(env `SCHWAB_CLIENT_SECRET`)_ | Schwab OAuth Client Secret. Falls back to env var. |
| `--callback-url` | `https://127.0.0.1:8182` | OAuth callback URL. Must exactly match the value registered in your Schwab app. |
| `--profile-dir` | `/secrets/schwab-api-oauth-profile` | Persistent Camoufox profile dir for the browser flow. |
| `--cli-mfa` / `--no-cli-mfa` | on | Automate login + stdin 2FA + consent (default; `login` uses it) vs. drive it yourself over VNC (`--no-cli-mfa`; the `vnc-login` subcommand uses it). |
| `--auth-timeout` | `600` | Seconds to wait for the consent redirect to the callback URL. |
| `--screenshot-dir` / `--trace` | — | Capture page HTML/screenshots, and (with `--trace`) a Playwright trace bundle, to the dir. The container passes `--screenshot-dir /debug --trace` by default. NEVER commit these. |
| `--explore` | off | Debug: dump each distinct page's DOM to `--screenshot-dir` (for pinning selectors). |
| `--manual` | off | No-browser paste-the-URL flow (schwab-py). |
| `--check` | off | Inspect the token file's age. No browser, no network. |
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

Build the `.venv` with `make build-schwab-api` (the host-venv pattern —
see [collectors/README.md](../README.md#build-scaffolding)). Credentials
go in `~/.secrets/schwab-api.env` (`SCHWAB_CLIENT_ID`,
`SCHWAB_CLIENT_SECRET`), which the wrapper sources automatically. Mint
the first token with `./schwab-api login` (see above).

### Configuration

`download.py` reads no config files — all inputs are CLI flags or env
vars. The secrets / data / silver locations follow the shared directory
contract (`--secrets-dir` / `--data-dir` / `--silver-db` and the
`${PREFIX}_*` / `WEALTHDB_*` env vars) — see
[collectors/README.md](../README.md#anatomy-of-a-collector).
Source-specific defaults:

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
./schwab-api download --dry-run
```

Real download (last 90 days, the default):

```sh
./schwab-api download
```

Wider backfill via the shared `--lookback` shortcut, or an explicit
date:

```sh
./schwab-api download --lookback 1y
./schwab-api download --since 2024-01-01
```

Files land in `./data/<UTC-timestamp>/`:

| File | Source endpoint |
| --- | --- |
| `run.json` | _(local)_ — per-run status manifest (`in-progress` at run-dir creation, atomically overwritten with `complete` at the end). Read by `prune` to tell a finished dump from a crashed one; never read by `load`. **Never compressed.** |
| `account_numbers.json.zst` | `/accounts/accountNumbers` |
| `user_preference.json.zst` | `/userPreference` |
| `accounts_positions.json.zst` | `/accounts?fields=positions` |
| `transactions_NNN.json.zst` | `/accounts/{hash}/transactions` (one file per account × window) |
| `open_orders.json.zst` | `/orders` (cross-account), filtered to non-terminal statuses |
| `instruments.json.zst` | `/marketdata/v1/instruments` (only when `--with-instruments` is passed) — basic metadata (symbol, cusip, description, exchange, type, assetType) for every symbol seen in positions and transactions |

Each data artefact is zstd-compressed in place the moment it lands
(`.json.zst`), best-effort — a compression failure leaves the plain
`.json` and the run still succeeds, since `load` resolves either form.
Pre-compression dumps carry the plain names. `run.json` is the one file
never compressed.

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
| `--dest` | `$XDG_DATA_HOME/wealthdb/schwab-api` | Local destination directory. |
| `--client-id` | _(env `SCHWAB_CLIENT_ID`)_ | Schwab OAuth Client ID. Falls back to env var. |
| `--client-secret` | _(env `SCHWAB_CLIENT_SECRET`)_ | Schwab OAuth Client Secret. Falls back to env var. |
| `--since` | _today − 90d_ | Earliest transaction date (YYYY-MM-DD). Schwab caps the API window at 1 year per request; the loader chunks longer ranges automatically. |
| `--until` | _today (UTC)_ | Latest transaction date (YYYY-MM-DD, inclusive). |
| `--lookback` | _unset_ | Named shortcut: `1w` / `4w` / `3m` / `6m` / `1y` / `2y` / `5y` / `all`. Sets `--since` to `until − X`; overridden by an explicit `--since`. |
| `--with-instruments` | off | After positions and transactions, look up metadata for every symbol seen and write a separate `instruments.json` artefact. Schwab omits `description` on equity positions/transactions; this fills the gap consistently across asset classes. Intended for reduced-schedule runs (instrument metadata changes rarely). |
| `--dry-run` | off | Skip data fetch; only validate auth and list accounts. Creates no bronze run dir. |
| `--debug` | off | Fleet-wide debug-artefact gate. This is a pure REST collector that writes no bronze-resident debug artefacts (browser-flow captures belong to `login.py` and land outside bronze), so the flag currently gates nothing — it exists for help-text parity across collectors. Use `--verbose` for DEBUG-level logging. |
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
- **Transient-fault retry, but no resume / scheduling.** A request that
  fails with a transport fault — a read/connect timeout or a dropped
  connection — is retried a few times with exponential backoff (Schwab's
  `/transactions` and `/instruments` endpoints are often slow, so the
  read timeout is also raised above schwab-py's flat 30s; tune with
  `--read-timeout`). HTTP *status* errors (rate limits, 5xx) are not
  retried — surface them and let the scheduler decide. There's no
  mid-dump resume; run from cron, launchd, or your scheduler of choice.

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
./schwab-api load                   # defaults under $XDG_DATA_HOME/wealthdb/schwab-api
```

The loader scans `<bronze-dir>` for subdirectories whose names match
the dump-timestamp format `YYYYMMDDTHHMMSSZ` and processes each one
not already recorded in `dump_runs`.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--silver-db` | `$XDG_DATA_HOME/wealthdb/schwab-api/schwab-api.db` | Path to the silver SQLite database. Created if missing. |
| `--bronze-dir` | `$XDG_DATA_HOME/wealthdb/schwab-api` | Directory containing bronze dump subdirectories. |
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

## prune.py

### How it works

`prune.py` reclaims disk from the bronze tree, running host-side like
`load` (a pure file walk — no container, no network). It is a thin
wrapper over the shared, unit-tested prune engine in
[`collectorkit.prune`](../../shared/collectorkit/collectorkit/prune.py).

```sh
./schwab-api prune --dry-run   # print the deletion plan, delete nothing
./schwab-api prune             # delete it
```

Unlike the browser-driven collectors, schwab-api has no bronze-resident
debug artefacts to sweep: a run dir is a flat set of JSON load inputs,
and the browser-flow page captures / traces belong to `login.py` and
land in a separate debug dir outside bronze. So `prune`'s only effect is
removing whole run dirs that are **not complete dumps** — a download that
crashed or was interrupted before it finished. Such a dir may still hold
a partial `account_numbers.json` (and some transactions), which `load`
would otherwise ingest as a truncated snapshot; after pruning one, the
next `load --force` rebuild reflects the removal.

Completeness is read from each dump's `run.json` status: `in-progress`
(a crashed walk) is non-complete, `complete` is kept. Dumps that predate
the manifest carry no `run.json`; for those the fallback signal is the
presence of `open_orders.json` — the last artefact a complete run writes
unconditionally. An unreadable or corrupt `run.json` is left alone
(never taken as proof a dump is partial). Load inputs of complete dumps,
and non-run entries at the bronze root (the silver `schwab-api.db`), are
never touched, so silver stays reproducible. An in-flight guard
(`--min-age-hours`, default 1, keyed on recent write activity) keeps
`prune` from removing a long transaction backfill that is still running.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--bronze-dir` | `$XDG_DATA_HOME/wealthdb/schwab-api` | Bronze tree root (the wrapper passes the resolved data dir). |
| `--dry-run` | off | Print the deletion plan; remove nothing. |
| `--min-age-hours` | `1` | Leave non-complete dumps touched within this window alone (protects an in-flight download). |

## recompress.py

### How it works

`download` zstd-compresses every data artefact as it lands
(`account_numbers.json.zst`, `transactions_NNN.json.zst`, …), and `load`
reads the `.json.zst` and plain `.json` forms alike (it decompresses in
Python). Run dirs written before compression existed can be converted
once with the `recompress` verb — a thin wrapper over the shared,
unit-tested [`collectorkit.recompress`](../../shared/collectorkit/collectorkit/recompress.py)
engine that replaces each plain data JSON inside a **complete** dump with
a compressed twin. The original is unlinked only after the twin has been
decompressed and sha256-verified against it, and an interrupted sweep is
safe to re-run. `run.json` is excluded from its patterns and never
touched.

Unlike `prune`, this rewrites load inputs, so it is strictly manual:
never schedule it, review the plan first, and verify afterwards with
`load --force` (silver must come out identical). It reuses prune's
completeness predicate, so the two verbs can never disagree about a dump,
and it runs host-side like `load` / `prune`.

```sh
./schwab-api recompress --dry-run   # print the sweep plan, rewrite nothing
./schwab-api recompress             # convert complete dumps, with byte accounting
./schwab-api load --force           # convergence check: silver must be unchanged
```

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--bronze-dir` | `$XDG_DATA_HOME/wealthdb/schwab-api` | Bronze tree root (the wrapper passes the resolved data dir). |
| `--dry-run` | off | Print the sweep plan; rewrite nothing. |
| `--min-age-hours` | `1` | Leave run dirs touched within this window alone (closes the race with a download that just finalised). |
