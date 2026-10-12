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
| [`prune.py`](prune.py) | Reclaims bronze disk by removing zip-less shells (crashed runs, pulls that found nothing queued) and legacy debug captures. A run dir holding any zip is never a deletion candidate. See [Reclaiming disk](#reclaiming-disk). |

Each script lists its flags with `--help`, for example
`./ubs-psn download --help`.

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
   `<TYPE>.zip`, plus any unexpectedly-named undotted file, with a
   warning, under its server filename. Bronze keeps every zip, because a
   queued file left behind risks being lost. Dot-prefixed archive copies
   are never fetched by a normal pull.
6. Refuses an unexpected name that would escape the run dir or overwrite
   a file the run holds (`run.json`, `listing.json`, any order type's
   queue file, a file already fetched). It logs a warning and leaves the
   file on the server.
7. Validates each fetched file as a zip. The check reads the content,
   never the size: the served stream can undercut the listed size and
   still be a complete zip. A corrupt fetch aborts the run loudly with
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
automatically by the wrapper (`--client-id` overrides it). The wrapper
passes `<secrets-dir>/ubs_psn_key` as the private key.

### Configuration

`host_fingerprints.txt` lists the SHA-256 fingerprints (OpenSSH base64,
no `SHA256:` prefix or padding required) the SFTP server is allowed to
present. The values shipped come from the published UBS PSN SFTP Pull
factsheet (Switzerland). UBS may rotate these without re-issuing the
factsheet; if the fingerprint changes, confirm the new value out-of-band
with UBS before adding it to this file.

### Usage

Probe the key. This opens one authenticated SSH session and closes it.
It lists nothing and consumes no file:

```sh
./ubs-psn login --check
```

Dry run. This connects, verifies the host key, authenticates, and exits.
It creates no run dir, lists nothing and fetches nothing:

```sh
./ubs-psn download --dry-run
```

Real download:

```sh
./ubs-psn download
```

(All three read `UBS_PSN_CLIENT_ID` from `~/.secrets/ubs-psn.env`;
append `--client-id CHxxxxxx` to override.)

Archive recovery — fetch the dated archive copies instead of the queue
files, e.g. after a missed or corrupted delivery:

```sh
./ubs-psn download --recover                    # the whole ~2-month archive
./ubs-psn download --recover --lookback 4w      # only the last four weeks
```

On a normal pull `--lookback` cannot narrow anything, so it is logged
and ignored. `--debug` is accepted and has no effect: every run already
records `listing.json`.

### Output

Files land in `<data-dir>/<UTC-timestamp>/<ORDERTYPE>.zip`. The data dir
defaults to `$XDG_DATA_HOME/wealthdb/ubs-psn`. A recovery run lands
`<ORDERTYPE>_<YYYYMMDD>.zip`, one per archive copy.

Each run dir also holds two records:

- `listing.json` is the full pre-pull listing of every
  `download/<ORDERTYPE>/` dir, with the accepted host-key fingerprint and
  a capture stamp. It is written before the first fetch on every run.
- `run.json` follows the fleet status lifecycle
  ([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
  It also records `mode`, which is `"download"` or `"recover"`. A run
  that downloaded nothing ends with `{"status": "empty"}`. Its shell
  stays so the listing that explains it can be read, and `prune`
  reclaims it once quiescent.

The bronze tree is drawn in [DESIGN.md §7](DESIGN.md#7-bronze-layout-and-pruning).

### Caveats

- **Fetching a queue file is one-shot; the archive is the safety net.**
  UBS deletes the server-side queue zip on a successful download, so
  that exact fetch cannot be repeated. A dated archive copy of the
  batch stays on the server for roughly two months, and `--recover` can
  re-fetch it any number of times. Beyond the archive window the data
  is irreplaceable. Use `login --check` or `--dry-run` for connectivity
  tests.
- **Order types are the documented union.** A customer may not be
  provisioned for every order type listed; the script lists each and
  records the absent dirs in `listing.json` as `"absent"`.
- **No retry, resume or scheduler of its own.**

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
processed snapshots. The loader reads every zip a run dir holds,
whatever its `run.json` status.

Each dump loads:

| Source | Target |
| --- | --- |
| `ZMD.zip` → `SDCL` / `SDCA` / `SDSA` / `SDPO` / `SDFI` XML | `account_holders` / `cash_accounts` / `safekeeping_accounts` / `portfolios` / `instruments` |
| `ZME.zip` → `TDFXR` / `TDFWD` XML | `fx_rates` / `forward_contracts` |
| `ZME.zip` → `TDOPT` / `TDMM` / `TDOTC` XML | `option_contracts` / `money_market_contracts` / `otc_contracts` |
| `ZME.zip` → `TDCAPI` cash-account pricing / interest bookings | `cash_account_pricing` |
| `ZME.zip` → `TDPOPF` monthly portfolio performance | `portfolio_performance` |
| `ZAH.zip` → MT535 holdings | `holdings` |
| `ZM5.zip` → MT537 pending | `pending_securities` |
| `Z40.zip` → MT940 statement headers + `:61:` lines | `cash_balances` + `events` (`kind='cash_movement'`) |
| `ZAG.zip` → MT515 trade confirmations | `events` (`kind='trade_confirmation'`) |
| `ZAN.zip` → MT566 corporate action confirmations | `events` (`kind='corporate_action_confirmation'`) |

`ZAY.zip` (MT950 bank-to-bank statements) is intentionally **not**
loaded — for retail PSN it duplicates MT940. `ZMH` (MT536 statement of
transactions) and the other MT types are not loaded either. Bronze
still keeps every retrieved zip for auditability.

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
  rows carrying that mark. So a re-delivery converges, and an entry the
  bank has amended away disappears, whatever the entries were
  value-dated to. A delete keyed on a value-date range would miss
  entries the bank value-dated outside the statement that carried them.
  Two entries the bank booked under one `:61:` bank reference — its own
  charge alongside the transfer that incurred it — are told apart by
  their position in the statement, the second taking a `#1` suffix on
  the shared id.

Identifier canonicalisation (migration 0002):

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

The cost-basis columns on `holdings` and the charge keys of a trade
confirmation are described in [DESIGN.md §8](DESIGN.md#8-cost-basis).

### Usage

```sh
./ubs-psn load                      # defaults under $XDG_DATA_HOME/wealthdb/ubs-psn
./ubs-psn load --force              # delete the silver DB, rebuild from all bronze
```

The loader scans `<bronze-dir>` for subdirectories whose names match
the dump-timestamp format `YYYYMMDDTHHMMSSZ` and processes each one
not already recorded in `dump_runs`. `--relationship-id` (default
`SFTPCH01`) names the UBS Server ID the dumps belong to.

### Schema migrations

Migrations follow the fleet convention
([collectors/README.md](../README.md#loadpy--silver)). A backfill that
needs the loader's own SWIFT parse runs as a pass in `load.py` instead
of in SQL (see [DESIGN.md](DESIGN.md) §8). Silver databases are always
at the latest schema, so code never handles "if column X exists".

### Caveats

- **A new MT/XML loader needs a rebuild to backfill.** `dump_runs`
  records "this dump was loaded", so a plain re-run skips
  already-processed dumps. `load --force` rebuilds silver from all
  bronze.
- **Some "static" UBS XML feeds (SDCA, SDSA) carry daily-varying
  fields** (book balance, accrued interest, market value). Content
  dedup correctly captures these as new rows on each batch.

## Reclaiming disk

```sh
./ubs-psn prune --dry-run   # print the deletion plan, delete nothing
./ubs-psn prune             # delete it
```

`prune` uses the fleet prune engine
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze))
but is deliberately narrow here. A run dir holding any zip is never a
deletion candidate, whatever its `run.json` says, because a fetched
queue zip cannot be fetched again. From such a dump it reclaims only a
legacy `screenshots/` capture. Otherwise it removes only a zip-less
shell once quiescent: a crashed run, or a `status: "empty"` pull.
[DESIGN.md §7](DESIGN.md#7-bronze-layout-and-pruning) gives the
classification.
