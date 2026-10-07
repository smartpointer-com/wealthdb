# ubs-psn

A toolkit for ingesting UBS Private Standard Network (PSN) banking data:
fetching the raw per-order-type zips over SFTP Pull and parsing them into
a queryable SQLite silver database.

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions.

See [DESIGN.md](DESIGN.md) for the silver-schema contract and the
source-specific design notes.

## Tools

| Script | Purpose |
| --- | --- |
| [`download.py`](download.py) | Fetches all pending PSN data from UBS over SFTP Pull and stores the per-order-type zips locally, organised by UTC timestamp. `--recover` replays the server's ~2-month dated archive instead. |
| [`load.py`](load.py) | Parses bronze dumps into a queryable SQLite silver database. Applies pending migrations on startup; each dump loads atomically. Idempotent — already-loaded dumps are skipped. |
| [`prune.py`](prune.py) | Reclaims bronze disk by removing zip-less shells (crashed runs, pulls that found nothing queued) and legacy debug captures. Safety-scoped: a run dir holding any zip is never a deletion candidate, because a fetched queue zip cannot be fetched again. See [Reclaiming disk](#reclaiming-disk). |

## download.py

### How it works

Per the UBS PSN SFTP Pull factsheet, each authorised order type is exposed
at `download/<TYPE>/<TYPE>.zip`. UBS materialises a given queue zip only
when new data is queued for that type, and removes it from the server
after a successful download. Next to the queue file the server retains
dot-prefixed dated archive copies (`.<TYPE>_<YYYYMMDD>.zip`) reaching back
roughly two months; fetching one of those does *not* consume it.
`download.py` therefore:

1. Connects to the UBS SFTP server (defaults: Switzerland endpoint).
2. Verifies the server's host-key SHA-256 fingerprint against
   `host_fingerprints.txt`.
3. Authenticates with an RSA key (UBS only supports RSA).
4. Lists every `download/<TYPE>/` dir once and records the full listing
   (filenames, sizes, the accepted host-key fingerprint) in the run
   dir's `listing.json`, before anything is fetched.
5. Fetches every undotted file the listing named: the expected
   `<TYPE>.zip`, plus — with a warning — any unexpectedly-named
   undotted file, under its server filename (bronze keeps every zip;
   leaving a queued file behind risks losing it). Dot-prefixed archive
   copies are never fetched by a normal pull.
6. Validates each fetched file as a zip (content check, never a size
   comparison — the served stream can undercut the listed size and
   still be a complete zip). A corrupt fetch aborts the run loudly with
   the run dir kept in place.

An absent `download/<TYPE>/` dir is normal (order type not provisioned),
as is a dir with nothing queued; both are recorded in `listing.json` and
skipped.

### Prerequisites

- Python 3.11+
- An active UBS PSN agreement with the SFTP Pull channel selected.
- An RSA SSH key pair (>= 2048 bits) whose public key has been emailed
  to UBS at `sh-psn@ubs.com` and activated on UBS's side.

Generate a key pair with:

```sh
ssh-keygen -t rsa -b 4096 -f ~/.secrets/ubs_psn_key -N ""
```

then email `~/.secrets/ubs_psn_key.pub` to UBS as instructed in the PSN
SFTP factsheet.

### Setup

Build the `.venv` with `make build-ubs-psn` (the host-venv pattern — see
[collectors/README.md](../README.md#build-scaffolding)). The SFTP login
id is read from `UBS_PSN_CLIENT_ID` in `~/.secrets/ubs-psn.env`, sourced
automatically by the wrapper (`--client-id` overrides it).

### Configuration

`host_fingerprints.txt` lists the SHA-256 fingerprints (OpenSSH base64,
no `SHA256:` prefix or padding required) the SFTP server is allowed to
present. The values shipped come from the published UBS PSN SFTP Pull
factsheet (Switzerland). UBS may rotate these without re-issuing the
factsheet; if the fingerprint changes, confirm the new value out-of-band
with UBS before adding it to this file.

### Usage

Dry run — connects, verifies host key, authenticates, exits without
touching files:

```sh
./ubs-psn download --dry-run
```

Real download:

```sh
./ubs-psn download
```

(Both read `UBS_PSN_CLIENT_ID` from `~/.secrets/ubs-psn.env`; append
`--client-id CHxxxxxx` to override.)

Archive recovery — fetch the dated archive copies instead of the queue
files, e.g. after a missed or corrupted delivery:

```sh
./ubs-psn download --recover                    # the whole ~2-month archive
./ubs-psn download --recover --lookback 4w      # only the last four weeks
```

Files land in `./data/<UTC-timestamp>/<ORDERTYPE>.zip` (a recovery run
lands `<ORDERTYPE>_<YYYYMMDD>.zip`, one per archive copy). A `run.json`
carrying `{"status": "in-progress", "mode": …}` is written when the run
dir is created and atomically overwritten with `{"status": "complete",
…}` once the pull finishes; `mode` is `"download"` or `"recover"`. A
`listing.json` — the full pre-pull listing of every
`download/<ORDERTYPE>/` dir, with the accepted host-key fingerprint and
a capture stamp — is recorded before the first fetch on every run. A
run that downloaded nothing keeps its shell with `{"status": "empty"}`
so the listing that explains it stays inspectable; `prune` reclaims
such shells once quiescent.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--host` | `sftp-keyport-ch.ubs.com` | UBS SFTP hostname or IP |
| `--port` | `26701` | UBS SFTP port |
| `--client-id` | _(required)_ | UBS customer / SFTP login ID |
| `--env-file` | — | KEY=VALUE credentials env file, sourced before the client id is resolved (also honours `UBS_PSN_ENV_FILE`; the wrapper sources `~/.secrets/ubs-psn.env` already) |
| `--bronze-dir` | `$XDG_DATA_HOME/wealthdb/ubs-psn` | Bronze tree root |
| `--key` | `~/.secrets/ubs_psn_key` | Private RSA key path |
| `--ignore-fingerprint-mismatch` | off | Warn instead of abort on host-key mismatch |
| `--check` | off | Probe the credential and exit: connect, authenticate, disconnect. Lists nothing, consumes no files. Backs `login --check`. Mutually exclusive with `--recover`. |
| `--recover` | off | Fetch the dot-prefixed dated archive copies (`.<ORDERTYPE>_<YYYYMMDD>.zip`) instead of the queue files. The copies survive fetching, so recovery is freely re-runnable; each lands undotted as `<ORDERTYPE>_<YYYYMMDD>.zip` in a normal run dir, and the queue files are never touched. `--lookback` bounds the replay (default: the whole ~2-month archive). |
| `--lookback` | _(none)_ | Under `--recover`: only archive copies dated on/after the resolved start (a preset like `4w`, or an ISO date) are fetched. On a normal pull it cannot narrow anything (the pull takes whatever is queued) and is logged and otherwise ignored. |
| `--dry-run` | off | Skip downloads (mints no run dir, lists nothing) |
| `--debug` | off | Accepted for fleet uniformity. Every run already records its diagnostics — the full pre-pull listing lands in `listing.json` unconditionally — so there is nothing extra to capture; the flag is logged and otherwise ignored. Wire-level tracing is `-v`/`--verbose` (stderr, not bronze). |
| `-v`, `--verbose` | off | DEBUG-level logging |

### Caveats

- **Fetching a queue file is one-shot; the archive is the safety net.**
  UBS deletes the server-side queue zip on a successful download, so
  that exact fetch cannot be repeated — but a dated archive copy of the
  batch stays on the server for roughly two months and `--recover` can
  re-fetch it any number of times. Beyond the archive window the data
  is irreplaceable. Use `--dry-run` for connectivity tests.
- **Order types are the documented union.** A customer may not be
  provisioned for every order type listed; the script lists each and
  records the absent dirs in `listing.json` as `"absent"`.
- **No retry / resume / scheduling.** Run from cron, launchd, or a
  scheduler of choice.

## load.py

### How it works

Parses bronze dump directories produced by `download.py` and inserts
their content into a SQLite silver database. The schema is defined in
the `migrations/` directory; the loader applies any pending migrations
on startup before loading data, so the silver database is always at
the latest schema version.

Each dump is loaded atomically — a failure mid-load rolls back to the
prior state, and a re-run retries the whole dump. Already-loaded dumps
are detected via the `dump_runs` table and skipped, so the loader is
safe to point at a `--bronze-dir` containing a mix of new and already-
processed snapshots.

Currently ingested per dump:

| Source | Target |
| --- | --- |
| `ZMD.zip` → `SDCL` / `SDCA` / `SDSA` / `SDPO` / `SDFI` XML | `account_holders` / `cash_accounts` / `safekeeping_accounts` / `portfolios` / `instruments` |
| `ZME.zip` → `TDFXR` / `TDFWD` XML (and the contract stubs) | `fx_rates` / `forward_contracts` (and the contract tables) |
| `ZME.zip` → `TDCAPI` cash-account pricing / interest bookings | `cash_account_pricing` |
| `ZME.zip` → `TDPOPF` monthly portfolio performance | `portfolio_performance` |
| `ZAH.zip` → MT535 holdings | `holdings` |
| `ZM5.zip` → MT537 pending | `pending_securities` |
| `Z40.zip` → MT940 statement headers + `:61:` lines | `cash_balances` + `events` (`kind='cash_movement'`) |
| `ZAG.zip` → MT515 trade confirmations | `events` (`kind='trade_confirmation'`) |
| `ZAN.zip` → MT566 corporate action confirmations | `events` (`kind='corporate_action_confirmation'`) |

`ZAY.zip` (MT950 bank-to-bank statements) is intentionally **not**
loaded — for retail PSN it duplicates MT940. `ZMH` (MT536 statement of
transactions) and other MT types are added when real samples exist
to develop against. Bronze still keeps every retrieved zip for
auditability.

A dated zip (`<ORDERTYPE>_<YYYYMMDD>.zip`, landed by `download
--recover`) routes to the same per-order-type handling as
`<ORDERTYPE>.zip` — it is the same batch content under an archive name.
Re-delivered content converges rather than duplicating: every silver
table either upserts on its natural key or dedups on payload (see
below), and the per-file `snapshot_at` comes from the filename prefix
*inside* the zip, which the archive copy shares with the original
delivery.

Reload semantics:

- **`snapshot_at` is the per-file as-of date**, parsed from the
  `YYYY-MM-DD_` prefix that every PSN file inside a dump zip carries.
  A single bronze dump can therefore land data at multiple `snapshot_at`
  values — important for catch-up dumps where `download.py` was skipped
  for a day and UBS bundles two nights' worth of master data into the
  next zip. `dump_runs.snapshot_at` is separately the dump-retrieval
  timestamp (the bronze-directory name), kept as an audit trail of
  when each dump was processed.
- **Snapshots** (`holdings`, `cash_balances`, `pending_securities`,
  `fx_rates`, `forward_contracts`, contract tables) upsert on their
  natural key (`INSERT OR REPLACE` on the snapshot-scoped primary
  key), so the same batch content arriving twice — a queue-file pull
  plus a recovered archive copy — lands identical rows once.
- **Slow-changing master data** (`account_holders`, `cash_accounts`,
  `safekeeping_accounts`, `portfolios`, `instruments`) dedups against
  each entity's most recent row at or before the file's as-of date:
  insert only when the canonical-JSON payload differs, after stripping
  UBS header noise (`DWHMsgId` / `MsqSeqNo` / `CrtnDtTm`). Bounding the
  compare at the as-of date keeps a recovered archive copy of a batch
  whose content has since changed from re-inserting old state as a new
  change-point; a redelivery carrying a different payload for an
  already-loaded as-of date replaces that date's row.
- **Events** (`events`) use either statement-DELETE-then-INSERT
  (cash_movement) or row-level `INSERT OR REPLACE` on
  `event_external_id` for events the upstream retracts only by sending
  a new CANC message (trade_confirmation, corporate_action_confirmation).
  A cash movement records the statement that booked it (payload
  `statement`: the MT940 `:60F:`/`:62F:` period and the `:28C:`
  statement number), and re-loading a statement deletes exactly the
  rows carrying that mark — so a re-delivery converges and an entry the
  bank has amended away disappears, whatever the entries were
  value-dated to. Keying the delete on a value-date range instead lost
  entries the bank value-dated outside the statement that carried them
  (migration 0004). Two entries the bank booked under one `:61:` bank
  reference — its own charge alongside the transfer that incurred it —
  are told apart by their position in the statement, the second taking
  a `#1` suffix on the shared id.

Identifier canonicalisation (since migration 0002):

- `account_external_id` on the **cash** side is always the IBAN. MT940
  `:25:` arrives in UBS's internal padded-account-number form and is
  translated via `cash_accounts.payload.AcctId` inside the same dump
  transaction. Cash-side `events.account_external_id` rows for
  `cash_movement` use the IBAN too.
- `account_external_id` on the **safekeeping** side is the MT535
  `:97A::SAFE//` / UBS-internal `AcctId` form. `holdings`,
  `pending_securities`, and `events` of kinds `trade_confirmation` /
  `corporate_action_confirmation` use that form.
- `cash_accounts.portfolio_external_id` and
  `safekeeping_accounts.portfolio_external_id` are nullable promoted
  columns — UBS legitimately omits portfolio linkage on standalone
  bank accounts (e.g. plain current/savings accounts not enrolled in
  a wealth-management portfolio).
- `portfolios.base_currency` is the portfolio's UBS reporting
  currency (`PrtflCcyIsoCd`).

### Usage

```sh
./ubs-psn load                      # defaults under $XDG_DATA_HOME/wealthdb/ubs-psn
```

The loader scans `<bronze-dir>` for subdirectories whose names match
the dump-timestamp format `YYYYMMDDTHHMMSSZ` and processes each one
not already recorded in `dump_runs`.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--silver-db` | `$XDG_DATA_HOME/wealthdb/ubs-psn/ubs-psn.db` | Path to the silver SQLite database. Created if missing. |
| `--bronze-dir` | `$XDG_DATA_HOME/wealthdb/ubs-psn` | Directory containing bronze dump subdirectories. |
| `--relationship-id` | `SFTPCH01` | UBS Server ID for the banking relationship the bronze dumps belong to. Override to load a different relationship into the same DB. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### Schema migrations

To change the silver schema, add a new
`migrations/NNNN_<slug>.sql` file. The number must be strictly greater
than any existing migration. Each file:

- Contains the DDL and any data backfill SQL can express. A backfill
  that needs the loader's own SWIFT parse runs as a pass in `load.py`
  instead (see [DESIGN.md](DESIGN.md) §8).
- Ends with `INSERT INTO schema_meta (silver_schema_version, applied_at) VALUES (N, CAST(strftime('%s','now') AS INTEGER));` as the migration-complete marker the loader checks for.

The loader executes each new migration in numeric order and commits
between files. Silver databases must always be at the latest schema —
never write code that handles "if column X exists".

### Caveats

- **Adding a new MT/XML loader requires re-running against existing
  bronze dumps to backfill the new event kind / table.** Since
  `dump_runs` records "this dump was loaded", a plain re-run will skip
  already-processed dumps. To backfill, delete the silver DB and let
  the loader rebuild it from bronze.
- **Some "static" UBS XML feeds (SDCA, SDSA) carry daily-varying
  fields** (book balance, accrued interest, market value). Content
  dedup correctly captures these as new rows on each batch.

## Reclaiming disk

```sh
./ubs-psn prune --dry-run   # print the deletion plan, delete nothing
./ubs-psn prune             # delete it
```

`prune` shares the fleet-wide bronze-prune engine but is deliberately
narrow here. A fetched queue zip cannot be fetched again — UBS deletes
it server-side the moment it is downloaded, and its dated archive copy
ages out after roughly two months — and `load` ingests whatever zips
are present regardless of whether the dump finished, so a run dir
holding *any* zip is classified complete and never a deletion
candidate, even if a crash left `run.json` at `"in-progress"` or wrote
no manifest at all. The only thing it reclaims from a finished dump is
a legacy `screenshots/` capture (the pre-`listing.json` debug listing)
— never a zip, and never `listing.json`, which is provenance the run
records unconditionally. Beyond that it can remove only a zip-less
shell: a run dir minted before the first file arrived and then
abandoned, or the `status: "empty"` dump any pull leaves when nothing
was queued (kept so its `listing.json` stays inspectable until
reclaimed here; `--dry-run` creates no run dir at all). Runs host-side
like `load`; an unreadable or corrupt `run.json` is left untouched, and
an in-flight guard (`--min-age-hours`, default 1, keyed on recent write
activity) keeps it from removing a download that is still running.
