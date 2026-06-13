# ubs-psn

A toolkit for ingesting UBS Private Standard Network (PSN) banking data:
fetching the raw per-order-type zips over SFTP Pull and parsing them into
a queryable SQLite silver database.

Part of the **wealthdb** suite — see [the architecture overview](../../ARCHITECTURE.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions.

See [DESIGN.md](DESIGN.md) for the silver-schema contract and the
source-specific design notes.

## Tools

| Script | Purpose |
| --- | --- |
| [`download.py`](download.py) | Fetches all pending PSN data from UBS over SFTP Pull and stores the per-order-type zips locally, organised by UTC timestamp. |
| [`load.py`](load.py) | Parses bronze dumps into a queryable SQLite silver database. Applies pending migrations on startup; each dump loads atomically. Idempotent — already-loaded dumps are skipped. |

## download.py

### How it works

Per the UBS PSN SFTP Pull factsheet, each authorised order type is exposed
at `download/<TYPE>/<TYPE>.zip`. UBS materialises a given zip only when
new data is queued for that type, and removes it from the server after a
successful download. `download.py` therefore:

1. Connects to the UBS SFTP server (defaults: Switzerland endpoint).
2. Verifies the server's host-key SHA-256 fingerprint against
   `host_fingerprints.txt`.
3. Authenticates with an RSA key (UBS only supports RSA).
4. For every known order type, `stat()`s the remote path and downloads
   the zip if present.

A missing remote zip is normal (no data queued) and is skipped silently.
The script intentionally never lists the `download/` directory — the SFTP
account is restricted from `READDIR` on it anyway.

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

Files land in `./data/<UTC-timestamp>/<ORDERTYPE>.zip`. If the run
downloaded nothing, the timestamped directory is removed.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--host` | `sftp-keyport-ch.ubs.com` | UBS SFTP hostname or IP |
| `--port` | `26701` | UBS SFTP port |
| `--client-id` | _(required)_ | UBS customer / SFTP login ID |
| `--dest` | `~/wealthdb/ubs-psn` | Local destination directory |
| `--key` | `~/.secrets/ubs_psn_key` | Private RSA key path |
| `--ignore-fingerprint-mismatch` | off | Warn instead of abort on host-key mismatch |
| `--dry-run` | off | Skip downloads |
| `-v`, `--verbose` | off | DEBUG-level logging |

### Caveats

- **First successful download is one-shot.** Because UBS deletes the
  server-side zip on a successful download, you cannot re-download a
  given file via this script. Use `--dry-run` for connectivity tests.
- **Order types are the documented union.** A customer may not be
  provisioned for every order type listed; the script attempts each
  and silently skips those that yield `FileNotFoundError`.
- **No retry / resume / scheduling.** Run from cron, launchd, or your
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
| `ZAH.zip` → MT535 holdings | `holdings` |
| `ZM5.zip` → MT537 pending | `pending_securities` |
| `Z40.zip` → MT940 statement headers + `:61:` lines | `cash_balances` + `events` (`kind='cash_movement'`) |
| `ZAG.zip` → MT515 trade confirmations | `events` (`kind='trade_confirmation'`) |
| `ZAN.zip` → MT566 corporate action confirmations | `events` (`kind='corporate_action_confirmation'`) |

`ZAY.zip` (MT950 bank-to-bank statements) is intentionally **not**
loaded — for retail PSN it duplicates MT940. `ZMH` (MT536 statement of
transactions) and other MT types will be added when we have real
samples. Bronze still keeps every retrieved zip for auditability.

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
  `fx_rates`, `forward_contracts`, contract tables) are append-only
  per snapshot.
- **Slow-changing master data** (`account_holders`, `cash_accounts`,
  `safekeeping_accounts`, `portfolios`, `instruments`) dedups against
  the most recent row for each entity: insert only when the canonical-
  JSON payload differs, after stripping UBS header noise
  (`DWHMsgId` / `MsqSeqNo` / `CrtnDtTm`).
- **Events** (`events`) use either window-DELETE-then-INSERT
  (cash_movement, keyed on account + value-date range) or row-level
  `INSERT OR REPLACE` on `event_external_id` for events the upstream
  retracts only by sending a new CANC message (trade_confirmation,
  corporate_action_confirmation).

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
  bank accounts (~5% of cash accounts in observed data).
- `portfolios.base_currency` is the portfolio's UBS reporting
  currency (`PrtflCcyIsoCd`).

### Usage

```sh
./ubs-psn load                      # defaults under ~/wealthdb/ubs-psn
```

The loader scans `<bronze-dir>` for subdirectories whose names match
the dump-timestamp format `YYYYMMDDTHHMMSSZ` and processes each one
not already recorded in `dump_runs`.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--silver-db` | `~/wealthdb/ubs-psn/ubs-psn.db` | Path to the silver SQLite database. Created if missing. |
| `--bronze-dir` | `~/wealthdb/ubs-psn` | Directory containing bronze dump subdirectories. |
| `--relationship-id` | `SFTPCH01` | UBS Server ID for the banking relationship the bronze dumps belong to. Override to load a different relationship into the same DB. |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### Schema migrations

To change the silver schema, add a new
`migrations/NNNN_<slug>.sql` file. The number must be strictly greater
than any existing migration. Each file:

- Contains the DDL and any data backfill needed.
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
