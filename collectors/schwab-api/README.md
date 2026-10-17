# schwab-api

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector uses a write-capable brokerage API and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector holds OAuth credentials to the official Charles Schwab
Trader API — a **write-capable API**: the granted token family is not
limited to reading and also reaches order-placement and other
account-changing endpoints, so nothing but this codebase's own discipline
restricts it to the read-only subset. If malicious code were ever
introduced into this repository, its dependency chain, or the environment
it runs in, it could trade or otherwise act on your accounts with your
full authority and cause **irreversible financial damage, up to the total
loss of the assets reachable from those credentials**.

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

schwab-api reads portfolio data from the Charles Schwab Trader API. It
fetches account metadata, balances, positions, transactions and open
orders over the read-only subset of the REST API. It then parses the raw
JSON into a SQLite silver database.

It is a **hybrid** collector. `login` runs in a Camoufox container,
because Schwab's refresh token needs an interactive browser grant every
7 days. `download`, `load`, `prune` and `recompress` run on the host
venv.

## Verbs

| Verb | What it does |
| --- | --- |
| `login` | Runs the OAuth grant in a headed browser and writes the token file. Needed once, then once per 7-day refresh window. |
| `vnc-login` | The same grant, driven by hand over VNC. A fallback for when Schwab's pages change. |
| `download` | Fetches account hashes, preferences, positions, transactions, open orders and instrument metadata. Writes the raw JSON to a new bronze dump. Read-only. |
| `load` | Parses new bronze dumps into the silver database. Each dump loads atomically. Already-loaded dumps are skipped. |
| `prune` | Reclaims disk: `download --debug` traces, unfinished dumps, and old login captures. |
| `recompress` | A manual sweep that compresses plain-JSON dumps to the `.json.zst` form `download` writes. |

`./schwab-api help` summarises the verbs. `./schwab-api <verb> --help`
lists every flag of a verb. [DESIGN.md](DESIGN.md) holds the design
rationale: the silver schema, the temporal model, and the login flow's
failure discipline.

## Setup

1. Register an app under Schwab's **Trader API – Individual** product.
   Register its callback URL as `https://127.0.0.1:8182`, or pass the
   registered value with `login --callback-url`. Setting the app's order
   rate limit to `0` adds a read-only safeguard.
2. Put the app's **Client ID** and **Client Secret** in
   `~/.secrets/schwab-api.env` as `SCHWAB_CLIENT_ID` and
   `SCHWAB_CLIENT_SECRET`. The wrapper sources the file. `--client-id`
   and `--client-secret` override them.
3. `login` signs in with the Schwab web login. It reads
   `SCHWAB_LOGIN_ID` and `SCHWAB_PASSWORD` from
   `~/.secrets/schwab-web.env`, the file the
   [`schwab-web`](../schwab-web/) collector uses.
4. Build both halves with `make build-schwab-api`. That builds the host
   venv and the login image.
5. Mint the first token with `./schwab-api login`.

## Token lifecycle

Schwab issues two tokens:

- The **access token** lasts 30 minutes. `schwab-py` refreshes it
  whenever a request needs one.
- The **refresh token** lasts **7 days, hard cap**. Only a new browser
  grant renews it. `login` performs that grant.

When the refresh token has expired, `download` stops. It quotes Schwab's
error and asks for a new `login`. An `invalid_client` error is
different: it points at the client ID or secret, not the token.

## login

`login` performs the OAuth 2.0 authorization-code grant against
`api.schwabapi.com`. It opens the authorize page in headed Camoufox on an
Xvfb display, the same browser schwab-web uses. Then it:

1. fills in and submits the Schwab login;
2. prompts for the 2FA code on stdin (stdin must be a TTY);
3. drives the consent pages and ticks **every** checkbox on the account
   selection page, so a newly opened account is linked too;
4. captures the `?code=…` redirect and exchanges it for the token
   bundle, written with mode `0600`.

The flow submits the credentials once and at most one 2FA code per run.
It clicks only on pages it has classified as consent pages. Schwab's
terminal notice pages (`#/information/<code>`, an account lockout among
them) end the run with the page's own message. Such a notice exits with
code 7; any other terminal flow failure exits with code 8. Re-running is
the recovery path. Schwab itself unlocks a locked account.
[DESIGN.md](DESIGN.md) §3.1 explains these rules.

```sh
./schwab-api login          # automated: answer the 2FA prompt on the terminal
./schwab-api vnc-login      # drive login, 2FA and consent by hand over VNC
./schwab-api login --check  # show the token's age, then prove it works
```

`vnc-login` prints the VNC tunnel and a one-time password. It still
ticks the account checkboxes. The wait for the consent redirect defaults
to five minutes (`--mfa-timeout`).

`login --check` shows the token's age and estimated expiry. It then
proves the token works the way `download` uses it: it refreshes the
access token and lists the account hashes. It prints `ALIVE`, or
Schwab's error and a non-zero exit. It opens no browser and fires no
2FA.

`login --manual` is a no-browser fallback: it prints the authorize URL
and reads the redirected URL back.

Browser captures are opt-in. Pass `--screenshot-dir /debug` through the
wrapper. They land in the debug dir
(`~/.cache/wealthdb/debug/schwab-api`, or `$SCHWAB_API_DEBUG_DIR`),
outside bronze. `prune` reclaims them. `--trace` adds a Playwright trace,
but the pinned Playwright tracer crashes the pinned Camoufox build, so it
ends the login.

## download

`download` loads the token file and refreshes the access token as
needed. It then fetches, in order:

1. the account-number to hash mapping (`/accounts/accountNumbers`);
2. the user preferences (`/userPreference`);
3. accounts with positions and balances (`/accounts?fields=positions`);
4. transactions per account, in windows of at most one year;
5. open orders across all accounts;
6. instrument metadata for every symbol seen in positions and
   transactions (`/marketdata/v1/instruments`).

Every response is written verbatim. Nothing is parsed or normalised at
this stage. The code calls no write endpoint; [AGENTS.md](AGENTS.md) §1
lists the allowed surface.

```sh
./schwab-api download --dry-run            # refresh the token, list account hashes, write nothing
./schwab-api download                      # transactions of the last 90 days
./schwab-api download --lookback 1y        # a wider window, by preset
./schwab-api download --lookback 2024-01-01  # or from a date
```

`--lookback` follows the fleet's window contract
([collectors/README.md](../README.md#collector-cli-conventions)). It
narrows only the transactions. Positions, balances and orders are always
the current state. A window longer than a year is split into one-year
requests, Schwab's per-request cap.

A dry run uses the stored token. It signs in to nothing, fires no 2FA,
and creates no run dir.

### Bronze layout

Each run writes one dump under the data dir:

```
$XDG_DATA_HOME/wealthdb/schwab-api/
├── 20260101T120000Z/               one bronze dump per run
│   ├── run.json                    status manifest, never compressed
│   ├── account_numbers.json.zst    /accounts/accountNumbers
│   ├── user_preference.json.zst    /userPreference
│   ├── accounts_positions.json.zst /accounts?fields=positions
│   ├── transactions_NNN.json.zst   /accounts/{hash}/transactions, one per account and window
│   ├── open_orders.json.zst        /orders, non-terminal statuses only
│   └── instruments.json.zst        /marketdata/v1/instruments
└── schwab-api.db                   silver SQLite database
```

- `run.json` follows the fleet's status lifecycle
  ([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
  `load` skips a dump that is not complete. [prune](#prune) states the
  rule.
- Each data file is zstd-compressed as it lands. If compression fails,
  the plain `.json` stays and the run still succeeds. `load` reads
  either form. Older dumps may hold plain `.json`.
- Transaction files are numbered, not named after the account hash. A
  directory listing shows no account identifier.
- Open orders are filtered to non-terminal statuses (working, pending,
  queued and so on). Filled, cancelled, rejected, expired and replaced
  orders are dropped. Order history is not captured.
- `--no-instruments` skips the instrument lookup. Schwab omits
  `description` on equity positions and transactions, and this lookup
  fills it in.
- `--debug` writes an HTTP trace to `<run>/screenshots/http-trace.jsonl`.
  It holds one metadata line per request, retries included, and no
  bodies or credentials. `load` ignores it.

### Caveats

- **No read-only OAuth scope exists.** The token could place orders.
  Only this code's discipline keeps it to reads.
- **All amounts are USD.** The API returns no currency field on
  positions or transactions.
- **Transient faults are retried.** A read or connect timeout, or a
  dropped connection, is retried a few times with exponential backoff.
  The read timeout is 60 s, above schwab-py's 30 s, because
  `/transactions` and `/instruments` are often slow. `--read-timeout`
  changes it. HTTP status errors (rate limits, 5xx) are not retried.
- **No resume.** A failed run leaves an unfinished dump. The next run
  starts a fresh one.

## load

`load` reads every dump under the data dir that `dump_runs` does not
record yet. It applies pending migrations first, so silver is always at
the latest schema. Each dump loads in one transaction. A failure rolls
the whole dump back, and the next run retries it.

```sh
./schwab-api load           # load new dumps
./schwab-api load --force   # delete silver and rebuild it from all bronze
```

### Silver tables

Every table except `dump_runs` keeps the source record as canonical JSON
in `payload`. `account_external_id` is always the Schwab account hash.

| Table | Rows | Key | Promoted columns |
| --- | --- | --- | --- |
| `accounts` | one per account, when its metadata changes | `snapshot_at`, `account_external_id` | `account_number` (plaintext), `account_type` (`CASH` / `MARGIN`), `preference_type`, `nickname` |
| `user_preference` | one per dump, when the preferences change | `snapshot_at` | — |
| `account_balances` | one per account and balance block per dump | `snapshot_at`, `account_external_id`, `balance_kind` (`initial` / `current` / `projected` / `aggregated`) | — |
| `positions` | one per holding per dump | `snapshot_at`, `account_external_id`, `instrument_key` (CUSIP, else symbol) | `average_cost`, `unrealized_gain_loss` |
| `open_orders` | one per open order per dump | `snapshot_at`, `account_external_id`, `order_external_id` | `status` |
| `transactions` | one per Schwab activity | `activity_id` | `timestamp`, `account_external_id`, `kind` |
| `instruments` | one per symbol, when its record changes | `snapshot_at`, `symbol` | — |
| `dump_runs` | one per loaded dump | `snapshot_at` | `silver_schema_version`, `run_dir` |

Reload rules:

- **Snapshot tables** append rows per dump. `accounts`,
  `user_preference` and `instruments` insert a row only when the payload
  differs from the latest row for that entity. `user_preference` first
  drops `schwabClientCorrelId`, which Schwab regenerates on every call.
- **`transactions`** is replaced per account and window. The loader
  deletes the window's rows, then inserts what the dump returned. This
  catches transactions Schwab later removed.
- `instruments` also holds descriptions that the loader builds for bonds
  and options that `/instruments` does not cover.

[DESIGN.md](DESIGN.md) §4 gives the rationale, the cost-basis column
semantics and the migration rules. How gold reads these tables is in the
[Schwab adapter doc](../../wealthdb/docs/adapters/schwab.md).

## prune

`prune` follows the fleet's safety envelope
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
For schwab-api it removes:

- `screenshots/` from complete dumps, the HTTP trace `download --debug`
  writes;
- whole dumps that are not complete, such as a crashed download;
- login captures in the debug dir that have been idle for
  `--min-age-hours` (default 1).

A dump without a `run.json` status counts as complete when it holds
`open_orders.json` or `open_orders.json.zst`, the last file a finished
run always writes. `load` applies the same rule, so pruning a dump never
changes silver.

```sh
./schwab-api prune --dry-run   # print the deletion plan
./schwab-api prune             # delete it
```

## recompress

`recompress` compresses the plain data JSON in complete dumps to
`.json.zst`. It keeps each original until the compressed twin has been
decompressed and checked against its sha256. `run.json` is never
touched. An interrupted sweep is safe to re-run. It uses the same
completeness test as `prune`.

It rewrites load inputs, so it is manual only and never scheduled.
Review the plan first, then check that a rebuild gives the same silver:

```sh
./schwab-api recompress --dry-run   # print the sweep plan
./schwab-api recompress             # compress complete dumps
./schwab-api load --force           # silver must come out unchanged
```
