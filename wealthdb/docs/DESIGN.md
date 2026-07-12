# wealthdb: Design

## 1. Audience and scope

This document describes the design of `wealthdb` — the **gold**
layer of the personal-portfolio data pipeline whose bronze and silver
layers are owned by per-bank dump repositories (`schwab-api`,
`ubs-psn`, `ubs-web`, `swissquote`, future siblings).

It is intended to be read alongside
[schwab-api/DESIGN.md](../../collectors/schwab-api/DESIGN.md),
which establishes the three-layer (bronze/silver/gold) model and the
per-broker silver-schema conventions reused here. This document covers
only what gold adds.

## 2. Where gold sits

```
┌────────────────────┐  ┌────────────────────┐  ┌────────────────────┐
│   ubs-psn     │  │   schwab-api  │  │   swissquote  │
│   bronze: zips     │  │   bronze: JSON     │  │   bronze: CSV+XLS  │
│   silver: SQLite   │  │   silver: SQLite   │  │   silver: SQLite   │
└──────────┬─────────┘  └─────────┬──────────┘  └─────────┬──────────┘
           │                      │                       │
           └──────────────────────┴───────────────────────┘
                                  │
                                  ▼
                ┌─────────────────────────────────────┐
                │   wealthdb — `wealthdb` CLI       │
                │   gold: DuckDB                      │
                │   - per-bank silver adapters        │
                │   - canonical cross-bank schema     │
                │   - portfolio query / analytics     │
                └─────────────────────────────────────┘
```

Per-bank silver databases are the **input contract**. Each silver
schema is owned by its respective dump repo; `wealthdb` reads
silver but never writes to it.

Gold is the **agent- and human-facing** layer: canonical, indexed,
strictly relational, and the surface that future analytics
(`positions`, `networth`, asset-class rollups) sit on top of.

## 3. Goals and non-goals

### Goals

- **Cross-bank canonical view.** One `positions` table that answers
  "what do I hold today, anywhere?". Asset class, currency, and bank
  dimensions are filter columns, not JSON probes.
- **Incremental merge from silver.** Each `wealthdb load` brings forward
  only what's new since the previous merge, so daily runs stay cheap.
- **Faithful provenance.** Every gold row carries the silver
  `silver_source_id` and a payload pointer back to the silver row(s)
  it derives from. Nothing is irreversibly transformed.
- **Snapshot-time semantics.** Positions queries are as-of a date.
  Each silver source contributes its latest snapshot ≤ that date,
  independently.
- **Multi-currency output at query time.** Position and net-worth
  reports can be rendered in any ISO currency; the choice is made
  per-invocation, not baked into the database. FX conversion uses
  either "current" rates (latest available) or "historic" rates
  (*default*, snapshot-time): the flat nearest rate at or before
  the line's own day, no interpolation. See §10.6.
- **Hands-off operation.** Non-interactive CLI. One process, one
  database, no daemons. Re-runs are idempotent.
- **Read-only sharing.** A single gold DB file can be served
  read-only over a network share (NFS, SMB, S3-FUSE) or shipped
  via `scp`. Multiple consumers can run `wealthdb holdings positions` /
  `wealthdb status` against it concurrently while exactly one
  upstream owner runs `wealthdb load`. See §4.10.
- **Dockerised everything.** Build, dev, and prod all run inside a
  single container image. The host OS stays clean.

### Non-goals (deliberately omitted)

- **Web UI / GUI.** CLI only.
- **Built-in market-data feeds (v1 only).** Real-time prices,
  ex-dividend dates, ratings, historic and implied volatility, etc.
  are out of scope for v1 — gold operates on whatever each silver
  carries. These feeds are planned future work and will arrive via
  their own ingest path independent of any bank silver. See §13.8.
- **P&L, tax-lot tracking, performance attribution.** Future work.
  The schema accommodates them (acquisition_date column, full
  transactions history) but no command computes them yet.
- **Cross-silver instrument deduplication.** Two silvers may hold the
  "same" equity under different `instrument_external_id`s; gold keeps
  them separate at the row level. `instruments.isin` is the join key
  for queries that want a unified view. A canonical-instrument
  mapping table can be added later if it earns its keep.
- **Bitemporal corrections.** Same trade-off as silver — monotemporal
  on source time. When silver retracts a past event, gold loses the
  prior version too.
- **Backwards compatibility in the schema.** Gold migrates forward
  only. If you need an old shape, restore the database from backup.
- **Multi-user / multi-tenant.** One gold DB per human.

## 4. The `wealthdb` CLI

`wealthdb` is the single executable. All operations are subcommands.
The only subcommand that prompts interactively is `wealthdb config`
(the first-time setup wizard); every other subcommand is
fully non-interactive and the gold layer never asks for secrets.

### 4.1 Subcommands

```
wealthdb config [-c <cfg>]            (setup) Interactive first-time wizard; writes the config file.
wealthdb init                         (RW)    Initialise an empty gold DB at the configured path.
wealthdb load    <id> | -a            (RW)    Merge new silver snapshots into gold.
wealthdb reset   <id> | -a            (RW)    Purge a silver source's data from gold.
wealthdb holdings <view> [flags]      (RO)    Point-in-time views: positions, accounts, portfolios, sources, global.
wealthdb transactions [flags]         (RO)    Print transactions over a date range.
wealthdb status  [<id>]               (RO)    Report gold state vs each silver source.
wealthdb snapshots <id> | -a          (RO)    List snapshots gold has loaded (one silver, or all).
wealthdb help [<subcommand>]
```

`-a` means "all configured silver sources". `<id>` is the user-defined
`silver_source_id` from the config file.

`(RW)` subcommands require a writeable gold database; `(RO)`
subcommands work on a read-only one. See §4.10 for how read-only
mode is detected, declared, and reported. `(setup)` denotes a
subcommand that writes the config file, not the gold DB; it
needs write permission on `--config`'s path but doesn't open the
gold DB at all.

### 4.2 Top-level flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-c`, `--config` | `$HOME/.config/wealthdb.cfg` | Path to config file. Tilde (`~`) and `$HOME` are expanded. |
| `-r`, `--read-only` | off | Force read-only access even when the gold DB is writeable. Useful for ad-hoc safety during exploration ("I'm running queries on prod and don't want to accidentally mutate anything"). Without this flag, mode is auto-detected from filesystem permissions (§4.10). |
| `-v`, `--verbose` | off | DEBUG-level logging. |

### 4.3 `wealthdb init`

Creates the gold DuckDB file at the path given by the `gold_db`
field in the config file, runs all migrations from `migrations/`,
and exits. Fails if the file already exists (use `wealthdb reset -a`
followed by `wealthdb init` to rebuild — or just `rm`).

### 4.4 `wealthdb load <id> | -a`

Asks the plugin for the silver's latest logical change number,
compares it against gold's stored `high_watermark`, and — if
silver has advanced — pulls the delta (snapshots + transactions)
in the bracketing time window and merges it into gold via a
windowed-delete-then-re-emit. Wrapped in a single transaction per
silver source; a failure rolls everything back.

The pattern is structurally **incremental view maintenance**:
gold is a materialized view of silver, refreshed by processing the
bounded delta since the last load. See §8 for the full algorithm.

### 4.5 `wealthdb reset <id> | -a`

Deletes all rows owned by the given `silver_source_id` from every
gold table, including `silver_sources` and `load_audit`. The
silver database itself is untouched.

Use case: a silver was rebuilt from bronze (re-parse, new migration,
data correction) and gold needs to be re-synced from scratch.

### 4.6 `wealthdb holdings positions`

Prints the consolidated portfolio as of a date.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-d`, `--as-of` | today (UTC) | Date in `YYYY-MM-DD` to query as-of. |
| `-f`, `--format` | `table` | One of `table`, `csv`, `csv_plain`, `json`. |
| `-x`, `--currency` | value of `default_currency` in the config file | ISO 4217 output currency for value columns (e.g. `USD`, `CHF`). The short form `-x` is mnemonic for "(currency) exchange"; `-c` is deliberately not used here so it stays reserved for the top-level `--config` flag (§4.2). |
| `--fx-mode` | `historic` | `historic` = convert using the flat nearest FX rate at or before each position's snapshot day (no interpolation; a day before the first known rate clamps to the earliest available rate); `current` = convert using the latest FX rate available, regardless of snapshot time. |
| `--include-cash` | on | Include cash balances as synthetic rows with `asset_class = 'cash'`. |

Formats:
- `table` — Postgres-style aligned ASCII (one column header line,
  one row per position).
- `csv` — RFC-4180 CSV with a header row.
- `csv_plain` — same, without the header row.
- `json` — array of objects, one per position.

Output columns always include `currency` (the position's natural
currency, e.g. USD for a US equity) and `value_<CURRENCY>` (the
converted value in the requested output currency). When the natural
currency equals the output currency, the conversion is the identity
and the value passes through unchanged.

Internally: for each silver source, find the latest `snapshot_at` ≤
`--as-of` by querying `MAX(snapshot_at)` on `positions` /
`cash_balances` filtered to that silver, then union those rows. FX
conversion is a join against `fx_rates` per §10.6.

### 4.7 `wealthdb status [<id>]`

Without `<id>`: one line per silver source from the config file,
each with `silver_source_id`, `kind`, `path`, gold-side oldest/
newest `snapshot_at`, position count at the latest snapshot,
transaction count, stored `high_watermark`, and silver-side
`LatestChangeNumber` (from `plugin.Status()`). A `*` flag marks
sources where silver has advanced past the watermark — load
candidates.

With `<id>`: detailed report — per-table counts in gold for that
silver source, full `plugin.Status()` (oldest/latest snapshot and
transaction timestamps in silver, latest change number), the
stored watermark, and the last few rows from `load_audit`.

### 4.8 `wealthdb snapshots <id> | -a`

Lists every distinct `snapshot_at` gold has data for — derived
from `SELECT DISTINCT snapshot_at FROM positions UNION ... FROM
cash_balances WHERE silver_source_id = ?`. Oldest first. Useful
for debugging "I expected to see a snapshot from yesterday and
don't".

With `<id>`: snapshots for the one named silver source. With
`-a`: snapshots for every configured silver, grouped by source.

### 4.9 `wealthdb config [-c <cfg-file-path>]`

Interactive first-time setup wizard. Writes the config file at
`-c` (default `$HOME/.config/wealthdb.cfg`) by walking
through:

1. **Gold DB path** — default `$XDG_DATA_HOME/wealthdb/wealthdb.db`. The wizard
   confirms the parent directory exists (offering to `mkdir -p`)
   but does **not** create the gold DB itself — that's `wealthdb
   init`'s job.
2. **Default output currency** — default `USD`. ISO 4217 only;
   validated against a built-in list.
3. **First silver source** — `id` (slug matching `^[A-Za-z0-9_-]+$`),
   `kind` (one of `schwab`, `ubs`, `swissquote`, `auto`), and
   `path` (default `$XDG_DATA_HOME/wealthdb/<id>/<id>.db`). The wizard opens
   the silver DB read-only, verifies it parses as SQLite and has
   a `dump_runs` table, and — for `kind != "auto"` — verifies the
   declared kind matches what auto-detection would have inferred
   (warns on mismatch but accepts).
4. **Add another silver source?** — repeats step 3 until
   declined.

On success the wizard writes the JSON config and prints a
suggested next-step message ("Run `wealthdb init`, then `wealthdb
load -a`."). On any failure (silver path invalid, ID collision,
parent dir uncreatable) the wizard rejects the input and re-asks
the offending question — it never writes a partial or invalid
config file.

The wizard is the **only** subcommand that prompts interactively;
every other subcommand consumes the file produced here.

#### Refusal to clobber

If the target config file already exists, `wealthdb config` exits
with a clear message (exit code 4, the same code as `init` uses
when the gold DB already exists, for consistency) and does not
overwrite. To re-run the wizard, delete or move the existing file
first; to add a silver to an existing config, edit the JSON
directly. (A future `wealthdb config add-silver` subcommand could
soften this — see §4.11.)

#### Non-interactive fallback

In a pipe / non-TTY environment, `wealthdb config` refuses to run
("wealthdb config requires an interactive terminal; run with `docker
run -it` or write the JSON directly per §5"). This is preferable
to silently consuming stdin and producing an empty config.

### 4.10 Read-only mode

`wealthdb` supports two access modes against the gold database:

- **Read-write** — `init`, `load`, `reset` plus all read commands.
- **Read-only** — only `positions`, `status`, `snapshots`, `help`.
  Suitable when the gold DB lives on a read-only share, has been
  `chmod`'d 0444 for safekeeping, or sits on a consumer
  host that should never write.

#### Detection

On startup, after parsing config, `wealthdb` decides the mode in this
order:

1. If `-r` / `--read-only` is passed, the mode is **read-only**
   unconditionally. It is the explicit "treat this run as
   read-only even though writes are possible" safety opt-in.
2. Else, `os.Stat` the gold DB file:
   - If it does not exist and the subcommand is `init`, proceed in
     read-write mode.
   - If it does not exist and the subcommand is anything else,
     fail with the "no gold DB; run `wealthdb init`" message in
     §4.10's error table below.
3. Else, check whether the **directory** containing the gold DB is
   writeable by the current user (DuckDB needs to create the WAL
   sidecar and lockfile alongside the DB). Use `unix.Access(dir,
   unix.W_OK)`.
4. Else, check whether the gold DB **file** itself is writeable
   (`unix.Access(path, unix.W_OK)`).
5. If both checks succeed, mode is **read-write**. Otherwise mode
   is **read-only**.

The detected mode is logged at INFO level on every invocation, so
operators always see in the log whether the run could have written.

#### Opening DuckDB

In read-only mode, `wealthdb` opens the gold DB with DuckDB's
`access_mode='read_only'` option. This is a hard guarantee at the
driver level — even a buggy subcommand cannot write. It also
allows multiple concurrent readers without contending on DuckDB's
single-writer lock.

In read-write mode, `wealthdb` opens with the default access mode.
DuckDB takes an exclusive lock; a second writer waiting on the
same file blocks. (Readers in the meantime hold no lock and are
unaffected.)

#### Subcommand gating

If the resolved mode is read-only and a (RW) subcommand was
invoked, `wealthdb` exits **before opening DuckDB** with a clear
error message — never with a generic "permission denied" stack
trace. The same error table applies to other early-failure modes
(missing DB, existing DB on `init`, etc.) so all operational
failures get a coherent, actionable message instead of an I/O
crash.

| Condition | Exit | Message |
| --- | --- | --- |
| RW subcommand on read-only-detected DB | 2 | `wealthdb: '<cmd>' requires write access to the gold database, but '<path>' is read-only (detected: <reason>). Use 'wealthdb --read-only positions' (or similar) for read operations.` |
| RW subcommand with `-r` / `--read-only` flag set | 2 | `wealthdb: '<cmd>' requires write access, but -r/--read-only was specified. Drop the flag or run a different subcommand.` |
| RO subcommand on non-existent DB | 3 | `wealthdb: gold database '<path>' does not exist. Run 'wealthdb init' first (requires write access).` |
| `init` on existing DB | 4 | `wealthdb: gold database '<path>' already exists. Use 'wealthdb reset -a' to clear data, or delete the file manually if you really want a fresh DB.` |
| DuckDB open fails mid-run | 5 | `wealthdb: failed to open gold database '<path>': <specific cause>. <hint based on cause>.` |
| Silver DB unreadable during load | 6 | `wealthdb: cannot read silver database '<path>' for source '<id>': <cause>. Check that the path in the config exists and is readable by this process.` |

`<reason>` in row 1 is one of: `parent directory not writeable`,
`file mode lacks owner write bit`, `mounted read-only`, etc.
`<hint>` in row 5 covers the common cases: `another wealthdb process
is writing` (lock conflict — try again or wait), `WAL file is
stale, may indicate a crashed prior writer`, `file is corrupt or
not a DuckDB file`.

Exit codes 2–6 are distinct so callers (shell scripts, CI) can
discriminate. Exit code 1 is reserved for unexpected runtime
errors (panics, etc.).

#### Write-suppression in shared helpers

Code paths that exist for both read and write subcommands (e.g.,
`silver_sources` upsert on load, but read for status) take an
explicit `mode` parameter from the top-level setup and refuse to
attempt writes when the mode is read-only. This is belt-and-braces
on top of DuckDB's `access_mode='read_only'` — the driver would
reject the write anyway, but failing earlier produces a better
error message.

#### Operational shape

Typical setup for a shared read-only gold DB:

```
host A (writer):      runs `wealthdb load -a` on a cron; writes /shared/wealthdb.db
host B,C,... (readers): mount /shared read-only; run `wealthdb holdings positions`,
                       `wealthdb status`, etc.
```

The reader hosts can ship the same image as the writer host with
no configuration difference — `--read-only` is auto-detected from
the mount mode. The writer host runs without the flag.

### 4.11 Future subcommands (sketch only)

These are reserved namespaces; their final shape will be designed
when implemented. The schema must not preclude them.

- `wealthdb networth [-d DATE] [--by bank,asset_class,currency]` —
  rolled-up balance sheet.
- `wealthdb filter` / dedicated subcommands per asset_class
  (`wealthdb equities`, `wealthdb bonds`, `wealthdb fx`) — convenience
  views over `wealthdb holdings positions`.
- `wealthdb pnl` — realised / unrealised P&L from `transactions` +
  current positions.

The point of mentioning them now is to ensure the schema carries
the columns these will need (`asset_class`, `vehicle`,
`acquisition_date`, `currency`, etc.) from day one.

## 5. Configuration

JSON file. Path defaults to `$HOME/.config/wealthdb.cfg` and is
overridable with `-c` / `--config`. The `.cfg` extension is a
convention; the content is JSON. Tilde (`~`) and `$HOME` are
expanded in both the `--config` argument and the path values
inside the file.

Conventional host layout, produced by `wealthdb config`'s defaults:

```
$HOME/.config/wealthdb.cfg             config file (this file)
$XDG_DATA_HOME/wealthdb/                        all wealthdb data
├── wealthdb.db                        gold DuckDB
├── ubs-psn/ubs-psn.db                UBS PSN silver SQLite + bronze dirs
├── ubs-web/ubs-web.db                UBS web silver SQLite + bronze dirs
├── schwab-api/schwab-api.db          Schwab API silver SQLite + bronze dirs
└── swissquote/swissquote.db         Swissquote silver SQLite + bronze dirs
```

Example config file:

```json
{
    "gold_db":          "$XDG_DATA_HOME/wealthdb/wealthdb.db",
    "default_currency": "USD",
    "silver_sources": [
        {
            "id":   "ubs",
            "kind": "ubs",
            "fx_priority": 0,
            "subsources": [
                {"kind": "ubs-web", "path": "$XDG_DATA_HOME/wealthdb/ubs-web/ubs-web.db"},
                {"kind": "ubs-psn", "path": "$XDG_DATA_HOME/wealthdb/ubs-psn/ubs-psn.db"}
            ],
            "relationships": [
                {"label": "Main", "web_id": "<web banking_relationship_id>", "psn_id": "SFTPCHxx"}
            ]
        },
        {
            "id":   "schwab-retail",
            "kind": "schwab",
            "path": "$XDG_DATA_HOME/wealthdb/schwab-api/schwab-api.db"
        },
        {
            "id":   "swissquote-1",
            "kind": "swissquote",
            "path": "$XDG_DATA_HOME/wealthdb/swissquote/swissquote.db"
        },
        {
            "id":   "fred",
            "kind": "fred",
            "path": "$XDG_DATA_HOME/wealthdb/fred/fred.db",
            "fx_priority": 1
        }
    ],
    "account_overrides": {
        "schwab-retail": {
            "<account-hash-1>": {"nickname": "Main brokerage", "category": "personal"},
            "<account-hash-2>": {"nickname": "Education account",      "category": "esa"}
        },
        "swissquote-1": {
            "1234567": {"nickname": "CHF trading", "category": "personal"}
        }
    },
    "instrument_overrides": {
        "schwab-retail": {
            "78463V107": {"asset_class": "metal", "vehicle": "etf"}
        }
    },
    "web": { "enabled": true, "port": 3000 }
}
```

### 5.1 Fields

| Field | Type | Meaning |
| --- | --- | --- |
| `gold_db` | string | Filesystem path to the DuckDB file. Created by `wealthdb init`. `~` and `$HOME` expanded. |
| `default_currency` | string | ISO 4217. Used as the default `--currency` for `wealthdb holdings positions` (and future net-worth commands) when `--currency` is omitted. Overridable per invocation. |
| `equity_transfers` | string | Optional. Filesystem path to a CSV ledger of equity transfers in/out of a tracked account that the collectors don't capture as valued flows. The loader injects each row as a canonical `transfer_in`/`transfer_out` transaction. `~` / `$HOME` / `${VAR}` expanded; a missing file is a no-op. See §13.10. |
| `web` | object | Optional. Enables the dockerized Metabase BI server driven by `wealthdb web` (host-side). See [web/README.md](../../web/README.md). |
| `web.enabled` | bool | `true` to allow `wealthdb web start`. Absent block or `false` = the server is not configured. |
| `web.port` | integer | Host loopback port Metabase is published on (127.0.0.1 + [::1] → container 3000). Default 3000. |
| `silver_sources[]` | array | Registered silver databases. |
| `silver_sources[].id` | string | User-defined unique identifier. Used in CLI args. Must match `^[A-Za-z0-9_-]+$`. |
| `silver_sources[].kind` | string | Picks the adapter (e.g. `schwab`, `ubs`, `swissquote`, `fred`, …), or `auto` to auto-detect (§5.2). The full set is the `silver_kind` whitelist enforced in gold (`internal/gold/migrations`) and mirrors the registered adapters under `internal/silver/`. |
| `silver_sources[].path` | string | Filesystem path to the silver SQLite. `~` and `$HOME` expanded. Relative paths resolved against the config file's directory. Used by single-file adapters (Schwab, Swissquote, single-source UBS, fred). Mutually exclusive with `subsources`. |
| `silver_sources[].fx_priority` | integer | Optional. FX-rate precedence when several sources publish the same `(base, quote)` pair: lower = higher priority, absent/null = lowest. Ties broken by the order sources appear in this array. Affects only FX resolution (§10.6 / §13.2) — no effect on positions or transactions. |
| `silver_sources[].subsources[]` | array | Optional. For adapters that merge several backing silvers under one logical source (UBS = `ubs-web` + `ubs-psn`). Each entry has its own `kind` and `path`. At least one entry required when present. |
| `silver_sources[].subsources[].kind` | string | Subsource discriminator. UBS recognises `ubs-web` and `ubs-psn`. |
| `silver_sources[].subsources[].path` | string | Filesystem path to that subsource's silver SQLite. Expanded like `path`. |
| `silver_sources[].relationships[]` | array | Optional. Pairs cross-subsource entity identities under a single label. UBS uses it to map web `banking_relationship_id` to PSN `SFTPCH0X`. |
| `silver_sources[].relationships[].label` | string | Required. Canonical user-readable name stamped on canonical records regardless of which subsource produced them. |
| `silver_sources[].relationships[].web_id` | string | Optional. Web silver's `banking_relationship_id` (opaque SPA token, or `account_number_prefix` fallback). |
| `silver_sources[].relationships[].psn_id` | string | Optional. PSN silver's `relationship_id` (SFTP server identifier like `SFTPCHxx`). At least one of `web_id` / `psn_id` must be set. |
| `silver_sources[].relationships[].psn_start_override` | string | Optional `YYYY-MM-DD`. Overrides the auto-detected web↔PSN transaction-splice cutover for this relationship. Defaults to `MIN(snapshot_at)` in PSN's data for the paired `psn_id`. |
| `account_overrides` | object | Optional. Nested map keyed by `silver_source_id` (outer) and `account_external_id` (inner) carrying user-supplied per-account `nickname`, `category`, `tax_wrapper`, and/or `management_style` strings. See §13.9; all four inner fields are optional but at least one must be set per entry. `tax_wrapper` and `management_style` values are validated against the canonical enums (`internal/canonical/enums.go`) at config-load time. The loader applies overrides AFTER the adapter stamps its own values, so config wins on overlap. |
| `instrument_overrides` | object | Optional. Nested map keyed by `silver_source_id` (outer) and `instrument_external_id` (inner) pinning a per-instrument taxonomy pair. Each entry sets both `asset_class` (the exposure) and `vehicle` (the wrapper); both are required and validated as an admitted taxonomy pair (§7.2, docs/TAXONOMY.md) at config-load time. For holdings the adapter's structured signals and name heuristics misclassify — e.g. an exchange-traded commodity trust whose security name doesn't give away what it holds (`metal × etf`). The loader applies overrides AFTER the adapter classifies, to both the instrument dimension and every position row referencing it, so config wins on overlap. See §13.9. |
| `inception_overrides` | object | Optional. Pins the returns-window START date per source / portfolio / account so an entity's track record begins at its first real capital rather than a tiny pre-history dust base. Three grain-keyed maps (`sources`, `portfolios`, `accounts`), values `YYYY-MM-DD` (UTC). Consumed by the returns engine at query time — it stamps no gold column. See §5.4. |

### 5.2 `kind: "auto"`

The loader opens the silver DB read-only, inspects `sqlite_master`
for tables unique to each known kind (e.g., `safekeeping_accounts`
implies UBS; `user_preference` implies Schwab; `currency_balances`
implies Swissquote), and selects the adapter accordingly. Falls
back to an explicit error if no adapter matches.

Most config files should pin `kind` explicitly. `auto` is a
convenience for scratch testing.

### 5.3 Config-file changes vs gold state

Adding a new silver source to the config and running `wealthdb load
<new-id>` works: the source is registered into `silver_sources`
on first load. Removing a silver source from the config does not
remove it from gold — run `wealthdb reset <id>` first.

### 5.4 Inception overrides (returns window)

`inception_overrides` pins where a returns track record
STARTS, per entity, so a portfolio funded on top of a tiny pre-history
dust base (an account-opening gift, a stub position) isn't measured from
that base — which would leave the money-multiple correct but blow the
*time-weighted* return up by orders of magnitude. It is general to every
source, not just crypto.

```json
"inception_overrides": {
    "sources":    { "cointracking": "2017-07-01" },
    "portfolios": { "cointracking": { "cu_000001": "2019-09-24" } },
    "accounts":   { "schwab-retail": { "<account-hash>": "2020-01-01" } }
}
```

- **Keys** are the stable external ids the rest of the config uses
  (`silver_source_id`, `portfolio_external_id`, `account_external_id`);
  copy them from the `entity_id` column of `wealthdb returns <grain>`.
- **Values** are `YYYY-MM-DD` (UTC midnight), validated at load.
- **Resolution** when an entity is computed is most-specific-first: the
  accounts grain tries account → its portfolio → source; the portfolios
  grain tries portfolio → source; the sources grain tries source. The
  **global** grain is never anchored (v1).
- **Semantics:** `winFrom = max(data-inception, configured-inception, --from)`
  — it can only move an anchor LATER, never earlier (an inception before an
  entity's data is a no-op). The opening base `V0` becomes the entity's
  carry-forward value on that date. Truncation is tagged
  `configured_inception` (replacing `since_data_inception`). Composes with
  the policy `Inception` mode (e.g. UBS `first-real-snapshot`) as `max`.
  MOIC is unaffected (time-insensitive); this re-anchors the TWR/MWR chain.
- Absent block ⇒ every entity keeps its data-derived inception, byte for
  byte. A typo'd portfolio/account id silently no-ops (it can't be checked
  against gold at load) — a known v1 gap; a source-id typo fails the load.

### 5.5 Returns exclusion (higher grains)

`returns_exclude` omits whole accounts or portfolios from the SOURCES and GLOBAL
return aggregates while still reporting them at their own grain — for holdings
tracked in a shared login that belong to another person.

```json
"returns_exclude": {
    "portfolios": { "cointracking": ["cu_000002"] },
    "accounts":   { "schwab-retail": ["<account-hash>"] }
}
```

- **Keys** are the same stable external ids as elsewhere
  (`portfolio_external_id`, `account_external_id`), from the `entity_id` column.
- **Semantics:** an excluded entity still shows at its OWN grain — the accounts
  grain shows every account, and an excluded PORTFOLIO still shows its own
  portfolios row (only an excluded ACCOUNT drops from its portfolio there). At
  the sources and global grains, an excluded account, and every account of an
  excluded portfolio, are omitted from the aggregate.
- **Returns only.** Holdings / net-worth views are unaffected; excluding
  someone's holdings from your net worth is a separate owner-dimension task.
- Absent ⇒ nothing excluded, byte-identical to before.

## 6. Plugin / adapter architecture

Each bank has a Go package that implements a common `Adapter`
interface. Adapters are statically compiled into the `wealthdb`
binary; no dynamic libraries, no out-of-process plugin servers.
Registration happens at package init; the main binary picks the
right one based on `silver_sources[].kind`.

### 6.1 Design contract

Two principles shape the API:

1. **Plugins never touch the gold database.** They read silver and
   yield canonical change records. The main app owns all writes to
   gold, all transaction boundaries, and the watermark bookkeeping.
   This keeps each plugin independent and stops bugs in one plugin
   from corrupting state owned by another.
2. **Plugins never see the gold schema.** They produce values in
   the canonical types defined below; gold's tables and SQL are
   invisible to them.

The contract is **change-windowed**, not snapshot-by-snapshot:
the main app asks the plugin "what changed since logical change
number N?", receives a time window, and pulls all canonical
records inside that window. This matches silver's window-DELETE-
then-INSERT model exactly — see §8.

### 6.2 Interface

```go
package silver

type Adapter interface {
    Kind() string                                       // "schwab", "ubs", "swissquote"
    Open(ctx context.Context, path string) (Connection, error)
}

type Connection interface {
    Close() error

    // Status is a read-only snapshot of where the silver DB stands.
    // Cheap to call (a few aggregate queries). The main app calls
    // this on every `wealthdb load` and `wealthdb status`.
    Status(ctx context.Context) (Status, error)

    // ChangeWindow returns the time window of changes in silver
    // since the given logical change number, plus the new change
    // number to advance the watermark to upon successful load.
    //
    // If HasChanges is false, the caller is done and skips the
    // Snapshots / Transactions calls; the new change number may
    // still differ from `sinceChangeNumber` (silver advanced but
    // produced no observable changes inside the window).
    ChangeWindow(ctx context.Context, sinceChangeNumber int64) (Window, error)

    // Snapshots iterates dimension upserts and snapshot-grain facts
    // (positions, cash balances, fx rates, account/instrument
    // updates) whose snapshot_at falls inside the given window.
    // Pull-model batched iterator: caller loops Next until done.
    Snapshots(ctx context.Context, window Window) (SnapshotStream, error)

    // Transactions iterates event-grain facts whose occurred_at
    // falls inside the given window. Pull-model batched iterator.
    Transactions(ctx context.Context, window Window) (TransactionStream, error)
}

type Status struct {
    OldestSnapshotAt     int64  // Unix seconds UTC; -1 if silver has no snapshots
    LatestSnapshotAt     int64  // -1 if no snapshots
    OldestTransactionAt  int64  // Unix seconds UTC; -1 if silver has no transactions
    LatestTransactionAt  int64  // -1 if no transactions
    LatestChangeNumber   int64  // monotone, opaque to gold; valid values start at 0; -1 = "silver is empty / nothing observable yet"
}

type Window struct {
    Start              int64    // earliest changed timestamp (inclusive, Unix seconds UTC)
    End                int64    // latest changed timestamp (inclusive)
    NewChangeNumber    int64    // value to write into silver_sources.high_watermark on success
    HasChanges         bool     // false ⇒ no data fetch needed, just advance the watermark
}

type SnapshotStream interface {
    // Next returns the next batch and whether more batches follow.
    // ok=false signals stream end; the returned batch is still
    // valid and must be applied. Each batch contains zero or more
    // records of each kind, multiplexed.
    Next(ctx context.Context) (batch SnapshotBatch, more bool, err error)
    Close() error
}

type SnapshotBatch struct {
    Accounts     []AccountChange
    Instruments  []InstrumentChange
    Positions    []PositionChange
    CashBalances []CashBalanceChange
    FxRates      []FxRateChange
}

type TransactionStream interface {
    Next(ctx context.Context) (batch TransactionBatch, more bool, err error)
    Close() error
}

type TransactionBatch struct {
    Transactions []TransactionChange
}

// The *Change types mirror the gold-table column shapes one-to-one.
// They live in the silver package so plugins import only the
// canonical types, never the gold-writer implementation.
type PositionChange struct {
    SnapshotAt            int64
    AccountExternalID     string
    PositionKey           string
    InstrumentExternalID  string  // "" if NULL in gold
    AssetClass            string  // exposure (§7.2, TAXONOMY.md)
    Vehicle               string  // wrapper (§7.2, TAXONOMY.md)
    Currency              string
    Quantity              *Decimal
    MarketValue           *Decimal
    BookValue             *Decimal
    AccruedInterest       *Decimal
    AcquisitionDate       *Date
    Payload               json.RawMessage
}
// ...AccountChange, InstrumentChange, CashBalanceChange,
// FxRateChange, TransactionChange similarly mirror their tables.
```

### 6.3 Registration

`silver.Register(name, factory)` is called from each backend
package's `init()`. Backends live under `internal/silver/schwab`,
`internal/silver/ubs`, `internal/silver/swissquote`.
`cmd/wealthdb/main.go` blank-imports each backend to trigger
registration:

```go
import (
    _ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/schwab"
    _ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/ubs"
    _ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/swissquote"
)
```

Adding a new bank means adding a package and one blank import —
both trivial.

### 6.4 The logical change number

`LatestChangeNumber` and the watermark stored in
`silver_sources.high_watermark` are **opaque int64s** from gold's
perspective. Gold compares them numerically (`>`, `==`) but never
interprets the value.

Sentinel: **`-1` means "no observable state yet"** — a fresh
silver with no data returns `LatestChangeNumber = -1`, and a
never-loaded silver's `high_watermark` is initialised to `-1`.
Valid plugin-emitted values start at 0.

Each plugin chooses an encoding that is **strictly monotone** — a
new load cycle must produce an equal-or-greater value than any
previous load cycle on the same silver DB. The natural choice for
all current plugins is `MAX(silver.dump_runs.snapshot_at)` (with
`-1` returned when the silver has no `dump_runs` rows); a plugin
is free to pick something else as long as monotonicity and the
`-1` sentinel hold.

What this guarantees:
- Replays are idempotent (`ChangeNumber == watermark` ⇒ no-op).
- A silver DB restored from backup or rebuilt is detected: if its
  new change number is **less than** the stored watermark, gold
  refuses to load and asks for `wealthdb reset <id>`.

What it does **not** guarantee:
- Same `LatestChangeNumber` with different silver content. If
  silver is amended in place without bumping the change number,
  gold sees a no-op. Plugins must avoid this.

### 6.5 Pull-model batched iterator

The plugin yields batches of canonical records via `Next` calls.
A batch is at the plugin's discretion (one snapshot's worth, a
fixed row count, whatever paginates cleanly against silver). Gold
applies one batch at a time inside the load transaction; no batch
is partially applied.

Push-model (visitor) would also work; pull-model wins because:
- The main app controls pacing (e.g., commit periodically on very
  large windows — future work).
- Streaming reads from silver SQLite map naturally to a
  resumable cursor.
- Cancellation via `context.Context` is one-line at the loop boundary.

### 6.6 Adapter responsibilities

Each adapter is the **only** place that understands its bank's silver
schema. It:

- Implements `Status`, `ChangeWindow`, `Snapshots`, `Transactions`.
- Knows which silver tables exist and what columns are promoted.
- Projects each silver row into one or more canonical `*Change`
  records.
- Picks the `(asset_class, vehicle)` taxonomy pair from per-bank type
  fields (Schwab `assetType`, UBS SDFI `Tp` codes, Swissquote XLS
  section headers).
- Handles per-bank identifier conventions (Schwab account hash, UBS
  relationship_id + IBAN/safekeeping code, Swissquote customer ID).
- Decides whether a "position" in silver belongs in gold's `positions`
  or `cash_balances`. (Schwab's CASH_EQUIVALENT positions, for
  example, route to `cash_balances`.)

The main binary deliberately knows nothing about these mappings.

### 6.7 Per-bank adapter design docs

Bank-specific mappings (which silver table feeds which gold table,
how the `(asset_class, vehicle)` pair is derived per source, how
`transactions.kind` maps from each bank's discriminator, identifier
conventions,
deferred silver tables, open questions) live in their own files
to keep this document focused on gold-side architecture:

- [adapters/schwab.md](adapters/schwab.md)
- [adapters/ubs.md](adapters/ubs.md)
- [adapters/swissquote.md](adapters/swissquote.md)

Each adapter doc is self-contained for the engineer writing or
maintaining that adapter. New bank adapters add a new file in
the same directory.

### 6.8 Cross-cutting adapter rules

A few rules apply to every adapter regardless of bank:

- **Unrecognised `kind` / `transaction_type` values fall through
  to gold `kind = 'other'`** with the raw source value preserved
  in `payload`, rather than failing the load. `wealthdb status -v`
  reports the count of `other` rows per silver source so taxonomy
  drift is visible.
- **Unrecognised `asset_class` source codes fall through to the
  `(other, other)` pair** with the raw code preserved in `payload`.
  Same rationale.
- **ETFs classify by underlying exposure, not by the wrapper.**
  Every adapter whose structured signal identifies an
  exchange-traded fund (UBS CFI group `CE`, Schwab
  `instrument.type = EXCHANGE_TRADED_FUND`, Swissquote's "ETFs"
  section, fidelity-web's silver `etf` class) sets `vehicle = etf`
  and refines the exposure from the security name via the shared
  `silver.RefineETFExposure`: crypto ETFs/ETPs → `crypto`,
  physical-metal ETFs/ETPs → `metal` (miners funds excluded —
  they hold stocks), bond / fixed-income ETFs → `fixed_income`,
  everything else → `public_equity`. Products whose names don't give
  away the exposure are pinned via the config's
  `instrument_overrides` (§5.1, §13.9).
- **Deferred silver tables** — silver tables not yet projected
  into gold by any adapter are catalogued per-bank in the adapter
  doc and globally in §13.7.

## 7. Gold schema

DuckDB. One file at the path given by the `gold_db` field in the
config file. The schema is defined in
`migrations/NNNN_<slug>.sql`, applied in numeric order on `wealthdb
init`. The same migration discipline as silver: never rewrite an
applied migration, always add a new file.

### 7.1 Conventions

- **Timestamps**: `BIGINT` Unix seconds, UTC. Matches silver. The
  one exception is `load_audit.loaded_at`, which uses Unix
  nanoseconds so a back-to-back `wealthdb load` pair can't
  collide on the PK.
- **Dates**: `DATE` for calendar-only fields like `acquisition_date`.
- **Money**: `DECIMAL(28, 4)` for amounts, `DECIMAL(28, 8)` for
  quantities (bonds in fractional units, FX in 4–6 decimals), and
  `DECIMAL(20, 10)` for FX rates. DuckDB's `DECIMAL` is exact; never
  use `DOUBLE` for money.
- **Currency codes**: `TEXT`, ISO 4217 three-letter codes (`CHF`,
  `USD`, `XAU`, ...). Stored in the original case (uppercase).
- **JSON**: DuckDB's native `JSON` type for `payload` columns.
- **Identifiers**: silver-source-scoped where they originate from a
  bank (`account_external_id`, `instrument_external_id`,
  `transaction_external_id`). Never globally unique on their own;
  always paired with `silver_source_id` in PKs.
- **Foreign keys**: intended relationships are documented in this
  section's prose but the DDL **does not declare them**. The
  reason is a DuckDB limitation worth understanding rather than
  glossing over:

    `wealthdb reset <id>` (§9) purges every row in gold that
    belongs to one silver source — parent rows in `accounts`,
    `instruments`, `silver_sources` and the child rows that
    reference them in `positions`, `cash_balances`, `transactions`,
    `load_audit`. For correctness the reset must be atomic, so
    all those DELETEs run inside one transaction. The natural
    order is children first (positions, cash_balances,
    transactions), then parents (accounts, instruments,
    silver_sources).

    DuckDB rejects this. Its FK enforcement runs at statement
    boundaries against a view of the referencing table that does
    not include the same-transaction child deletes — so the
    `DELETE FROM accounts` step fails with "key … is still
    referenced by a foreign key", even though every row that
    referenced it was deleted moments earlier in the same
    transaction.

    DuckDB exposes no escape hatch:
    - `PRAGMA foreign_keys = OFF` (SQLite) — unknown parameter.
    - `SET enable_foreign_keys = false` / `SET disable_constraint_checking = true` — unknown parameters.
    - `SET CONSTRAINTS ALL DEFERRED` (PostgreSQL) — parse error.
    - `DEFERRABLE INITIALLY DEFERRED` on the FK declaration —
      **silently accepted by the parser but has no effect**; the
      parent DELETE still fails immediately. Worse than
      rejecting outright.

    The choices left are (a) drop the FK declarations, (b) split
    reset across multiple transactions (loses atomicity), or
    (c) DROP / re-ADD the constraint around the reset (DDL
    inside the live data path). We picked (a): the loader is
    the sole writer to gold, so application logic is the
    authority on referential integrity. The intended FKs remain
    documented in this §7.2 schema sketch as the conceptual
    contract; the migration omits them.

    If DuckDB ships proper deferred-constraint support in a
    future version, this trade-off is easy to revisit — add FK
    declarations to a new migration, add `DEFERRABLE INITIALLY
    DEFERRED`, done.

### 7.2 Schema sketch

The SQL below is the design-time sketch. The actual `migrations/
0001_initial.sql` will track this but may differ in minor details.

```sql
-- ============================================================
-- META / BOOKKEEPING
-- ============================================================

-- Applied gold migrations. Each migration ends with an INSERT here.
CREATE TABLE schema_meta (
    gold_schema_version INTEGER PRIMARY KEY,
    applied_at          BIGINT  NOT NULL    -- Unix seconds UTC
);

-- Registered silver databases. Populated lazily by `wealthdb load` on
-- first contact with a silver source.
--
-- `high_watermark` is the plugin's logical change number as of the
-- last successful load. Opaque to gold (see §6.4); gold only
-- compares it to fresh values returned by Status() / ChangeWindow().
CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN ('schwab', 'ubs', 'swissquote')),
    silver_path         TEXT    NOT NULL,            -- as observed at last load
    high_watermark      BIGINT  NOT NULL,            -- plugin's logical change number after the last load
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

-- Audit log of `wealthdb load` invocations that observed changes. One
-- row per (silver source, completed load that processed a non-empty
-- window). Pure debug surface — `wealthdb status` reads from data
-- tables and `silver_sources`; this table is for "why did the last
-- load look weird?" forensics.
CREATE TABLE load_audit (
    silver_source_id        TEXT    NOT NULL,
    loaded_at               BIGINT  NOT NULL,        -- when this load committed
    change_number_before    BIGINT,                  -- NULL on first-ever load
    change_number_after     BIGINT  NOT NULL,
    window_start            BIGINT  NOT NULL,        -- earliest changed timestamp in this window
    window_end              BIGINT  NOT NULL,        -- latest changed timestamp in this window
    snapshots_loaded        INTEGER NOT NULL,        -- total snapshot-grain rows applied
    transactions_loaded     INTEGER NOT NULL,        -- total event-grain rows applied
    PRIMARY KEY (silver_source_id, loaded_at),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

-- ============================================================
-- DIMENSIONS — accounts and instruments
--
-- Slow-changing master data. One row per (silver_source, external_id),
-- updated in place when a newer snapshot reports different attributes.
-- The `payload` column carries the latest silver payload for forensics.
-- ============================================================

-- account_kind values:
--   'brokerage'     — Schwab trading account
--   'cash'          — IBAN-keyed cash account (UBS SDCA, future banks)
--   'safekeeping'   — UBS safekeeping (custody) sub-account
--   'custody'       — managed-custody account where a third-party
--                     manager directs the holdings on the client's behalf
--   'overlay'       — synthetic per-portfolio account that holds
--                     positions the bank attributes to the portfolio
--                     directly rather than to a sub-account (UBS
--                     forward contracts). One per portfolio,
--                     lazily emitted when needed.
--   'other'         — unclassifiable, fall back to payload
--
-- portfolio_external_id (nullable) names the parent portfolio in
-- the `portfolios` table. Schwab and Swissquote accounts leave it
-- NULL (no portfolio grouping); UBS cash/safekeeping/overlay
-- accounts populate it. See `wealthdb holdings portfolios` rollup semantics.
CREATE TABLE accounts (
    silver_source_id        TEXT    NOT NULL,
    account_external_id     TEXT    NOT NULL,
    account_kind            TEXT    NOT NULL,   -- technical container; see §13.9
    display_name            TEXT,               -- user-facing label, payload-derived
    base_currency           TEXT,               -- ISO 4217 if known
    relationship_id         TEXT,               -- UBS relationship dimension; NULL otherwise
    nickname                TEXT,               -- user-set label; Schwab silver supplies, config override fills others
    account_category        TEXT,               -- free-text bank descriptor; see §13.9
    portfolio_external_id   TEXT,               -- parent portfolio (NULL when ungrouped)
    tax_wrapper             TEXT,               -- tax / regulatory registration; see §13.9
    management_style        TEXT,               -- self_directed / advisory / discretionary / automated; see §13.9
    first_seen_at           BIGINT  NOT NULL,   -- earliest snapshot_at observed
    last_seen_at            BIGINT  NOT NULL,   -- latest snapshot_at observed
    payload                 JSON,
    PRIMARY KEY (silver_source_id, account_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

-- portfolios is its own entity (added in migration 0004). A
-- portfolio is the wealth-management wrapper (UBS-specific today)
-- that GROUPS one or more accounts under a single mandate; it
-- does not hold positions or cash directly — its component
-- accounts do. Portfolio-level totals come from rolling up
-- component-account values; see `wealthdb holdings portfolios`.
CREATE TABLE portfolios (
    silver_source_id        TEXT    NOT NULL,
    portfolio_external_id   TEXT    NOT NULL,
    display_name            TEXT,
    base_currency           TEXT,               -- portfolio reporting currency (UBS PrtflCcyIsoCd)
    relationship_id         TEXT,               -- UBS relationship dimension; NULL otherwise
    nickname                TEXT,               -- user-set label (config-side override only today)
    first_seen_at           BIGINT  NOT NULL,
    last_seen_at            BIGINT  NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, portfolio_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

-- asset_class (exposure) + vehicle (wrapper) are the two-dimensional
-- instrument taxonomy — docs/TAXONOMY.md is the authority.
--   asset_class — one of 13 exposures: public_equity, private_equity,
--     fixed_income, private_debt, real_estate, infrastructure, metal,
--     crypto, cash, foreign_exchange, hedge_fund, multi_asset, other.
--   vehicle     — one of 18 wrappers: stock, etf, fund, spv, bond,
--     convertible_note, loan, option, future, forward, time_deposit,
--     demand_deposit, physical, structured_product, right, mortgage,
--     escrow, other.
-- The writer accepts only admitted (exposure, vehicle) pairs
-- (canonical.ValidTaxonomyPair); an unrecognised source code falls
-- through to (other, other) with the raw code preserved in `payload`.
-- The vehicle column was added by gold migration 0028; the writer
-- always populates it though the column itself is nullable.
--
-- `instrument_external_id` is whatever the silver layer uses as its
-- per-source instrument key:
--   Schwab     — CUSIP if present, else symbol (mirrors silver)
--   UBS        — ISIN (mirrors silver)
--   Swissquote — symbol + '@' + currency (silver's PK suffix)
CREATE TABLE instruments (
    silver_source_id        TEXT    NOT NULL,
    instrument_external_id  TEXT    NOT NULL,
    asset_class             TEXT    NOT NULL,       -- exposure (see above)
    vehicle                 TEXT,                   -- wrapper; writer-required
    isin                    TEXT,               -- ISO 6166; NULL where source doesn't supply
    cusip                   TEXT,
    symbol                  TEXT,
    name                    TEXT,
    currency                TEXT,               -- ISO 4217 primary trading currency
    first_seen_at           BIGINT  NOT NULL,
    last_seen_at            BIGINT  NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, instrument_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

CREATE INDEX ix_instruments_isin   ON instruments(isin);
CREATE INDEX ix_instruments_symbol ON instruments(symbol);

-- ============================================================
-- FACTS — positions and cash balances (snapshot grain)
--
-- One row per (silver_source, snapshot_at, account, position_key).
-- `position_key` is stable within a silver source's view of an
-- account at a snapshot. The adapter picks it.
-- ============================================================

CREATE TABLE positions (
    silver_source_id        TEXT    NOT NULL,
    snapshot_at             BIGINT  NOT NULL,
    account_external_id     TEXT    NOT NULL,
    position_key            TEXT    NOT NULL,        -- adapter-chosen, see comment above
    instrument_external_id  TEXT,                    -- NULL for one-off contracts not in `instruments`
    asset_class             TEXT    NOT NULL,        -- exposure, mirrored from instruments for index-friendliness
    vehicle                 TEXT,                    -- wrapper, mirrored from instruments; writer-required
    currency                TEXT    NOT NULL,        -- position's natural currency (ISO 4217)
    quantity                DECIMAL(28, 8),          -- units / nominal / face value
    market_value            DECIMAL(28, 4),          -- in `currency`
    book_value              DECIMAL(28, 4),          -- in `currency`; NULL when source doesn't provide
    accrued_interest        DECIMAL(28, 4),          -- bonds; NULL otherwise
    acquisition_date        DATE,                    -- holding-period analysis; NULL when unknown
    payload                 JSON,                    -- raw silver row(s) that produced this fact
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, position_key),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

-- Per-account, per-currency cash balance at a snapshot. Separate from
-- `positions` because cash has no instrument, the quantity *is* the
-- amount, and net-worth queries want cash and securities to roll up
-- differently. balance_kind discriminates UBS's opening/closing/
-- available, Schwab's initial/current/projected/aggregated, and
-- Swissquote's per-currency totals (single 'closing').
CREATE TABLE cash_balances (
    silver_source_id        TEXT    NOT NULL,
    snapshot_at             BIGINT  NOT NULL,
    account_external_id     TEXT    NOT NULL,
    currency                TEXT    NOT NULL,        -- ISO 4217
    balance_kind            TEXT    NOT NULL,        -- 'opening','closing','available','current','projected','aggregated'
    amount                  DECIMAL(28, 4) NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, currency, balance_kind),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

-- ============================================================
-- FACTS — FX rates (snapshot grain)
--
-- Multiple silvers may publish overlapping rates (UBS TDFXR,
-- Swissquote List-of-Assets row's `rate_to_chf`, the `fred` reference
-- feed). We keep all rows and let the `fx_daily` view (§10.6) pick per
-- (pair, UTC day): it prefers the rate from the highest-priority source
-- covering that day, falling back to the next. Priority is the per-source
-- `silver_sources[].fx_priority` config field (lower = higher priority,
-- absent = lowest, ties by config order), stamped into the
-- `silver_sources.fx_priority` gold column on load. See §10.6 / §13.2.
-- ============================================================

CREATE TABLE fx_rates (
    silver_source_id        TEXT    NOT NULL,
    snapshot_at             BIGINT  NOT NULL,
    base_currency           TEXT    NOT NULL,        -- ISO 4217
    quote_currency          TEXT    NOT NULL,        -- ISO 4217
    mid_rate                DECIMAL(20, 10) NOT NULL,
    bid_rate                DECIMAL(20, 10),
    ask_rate                DECIMAL(20, 10),
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, base_currency, quote_currency)
);

-- Lookup index for currency conversion at query time. The `fx_norm` /
-- `fx_daily` views in §10.6 scan fx_rates by (base, quote) pair ordered
-- by snapshot_at to pick the per-day winner and the most recent rate at
-- or before a target day; this index makes that an O(log N) range scan
-- instead of a full table scan.
CREATE INDEX ix_fx_rates_pair_time
    ON fx_rates(base_currency, quote_currency, snapshot_at);

-- ============================================================
-- FACTS — transactions (event grain)
--
-- All cash- and security-affecting events: trades, dividends,
-- coupons, fees, transfers, FX settlements, corporate actions, ...
--
-- `kind` is the canonical taxonomy (see comment); adapters translate
-- bank-specific types into one of these values. Unmappable types
-- fall back to 'other' and stash the bank's own type string in
-- `payload`.
--
-- Load semantics: windowed delete-then-insert keyed on `occurred_at`,
-- mirroring silver's own event-table model. The plugin yields all
-- silver events whose timestamp falls in the change window; gold
-- wipes the same window first and re-inserts. See §8.
-- ============================================================

-- kind values (canonical taxonomy):
--   'buy'                 — securities purchase
--   'sell'                — securities sale
--   'dividend'            — equity/ETF cash distribution
--   'coupon'              — bond coupon
--   'capital_gain'        — fund capital-gain distribution
--   'interest'            — cash account interest
--   'fee'                 — custody/trade/tax-statement fee
--   'tax'                 — withholding tax, stamp duty
--   'deposit'             — incoming wire/cash
--   'withdrawal'          — outgoing wire/cash
--   'fx_spot'             — FX conversion settlement
--   'fx_forward'          — FX-forward settlement
--   'corporate_action'    — stock split, name change, merger, ...
--   'transfer_in'         — securities transfer in (DRS, ACATS)
--   'transfer_out'        — securities transfer out
--   'journal'             — internal account-to-account journal
--   'other'               — unmapped; bank's own type lives in payload
CREATE TABLE transactions (
    silver_source_id        TEXT    NOT NULL,
    transaction_external_id TEXT    NOT NULL,
    occurred_at             BIGINT  NOT NULL,        -- Unix seconds UTC
    account_external_id     TEXT    NOT NULL,
    instrument_external_id  TEXT,                    -- NULL for cash-only events
    kind                    TEXT    NOT NULL,
    currency                TEXT    NOT NULL,
    gross_amount            DECIMAL(28, 4),
    net_amount              DECIMAL(28, 4),
    quantity                DECIMAL(28, 8),          -- for trades; NULL otherwise
    price                   DECIMAL(28, 8),          -- per-unit, for trades
    payload                 JSON,
    PRIMARY KEY (silver_source_id, transaction_external_id),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

CREATE INDEX ix_transactions_account_time
    ON transactions(silver_source_id, account_external_id, occurred_at);
CREATE INDEX ix_transactions_kind_time
    ON transactions(kind, occurred_at);
```

### 7.3 What's deliberately omitted

- **Views.** The first cut keeps gold as tables only. Future
  convenience views (`v_latest_positions`, `v_networth`) can land
  alongside future subcommands; designing them before the queries
  exist is premature.
- **Materialised positions-as-of cache.** The as-of query in §10 runs
  fast on the index; no cache needed at personal scale.
- **A surrogate `account_id` / `instrument_id` integer key.** Tuple
  PKs `(silver_source_id, account_external_id)` are noisier in
  joins but eliminate a whole class of "how do I generate a stable
  surrogate" bugs. DuckDB has no SERIAL/SEQUENCE that fits well.
- **Audit columns on dimensions** beyond `first_seen_at` /
  `last_seen_at`. `payload` carries the rest if you need it.
- **A separate `holdings_history` table.** Positions itself is the
  history — one row per (snapshot, account, position_key).

## 8. Load semantics

`wealthdb load` is structurally an **incremental view maintenance**
(IVM) operation in the data-streaming sense: gold is a
materialized view of silver, and the load procedure brings that
view up to date by processing exactly the bounded delta produced
since the last refresh. The same shape — watermark, delta window,
delete-in-window then re-emit — is what streaming engines do for
continuous queries. We just trigger it manually.

### 8.1 The full sequence for `wealthdb load <id>`

```
1. open silver via the plugin
2. status     := plugin.Status()
3. watermark  := silver_sources.high_watermark    (-1 if first load)

4. if status.LatestChangeNumber == watermark:
       no-op, exit (silver has not advanced)
   if status.LatestChangeNumber < watermark:
       error: silver appears to have gone backwards (rebuilt? restored?).
       Report that `wealthdb reset <id>` is needed, then retry.

5. window := plugin.ChangeWindow(sinceChangeNumber=watermark)

6. BEGIN TRANSACTION (in gold)

7. if window.HasChanges:
       DELETE FROM positions      WHERE silver_source_id = ?
                              AND snapshot_at BETWEEN window.Start AND window.End;
       DELETE FROM cash_balances  WHERE silver_source_id = ?
                              AND snapshot_at BETWEEN window.Start AND window.End;
       DELETE FROM fx_rates       WHERE silver_source_id = ?
                              AND snapshot_at BETWEEN window.Start AND window.End;
       DELETE FROM transactions   WHERE silver_source_id = ?
                              AND occurred_at BETWEEN window.Start AND window.End;
       -- accounts and instruments are NOT deleted; they are upsert
       -- targets whose payload/last_seen_at refresh on every load.

       stream := plugin.Snapshots(window)
       loop:
           batch, more, err := stream.Next()
           apply upserts:  batch.Accounts, batch.Instruments
           insert facts:   batch.Positions, batch.CashBalances, batch.FxRates
           if not more: break

       stream := plugin.Transactions(window)
       loop:
           batch, more, err := stream.Next()
           insert facts:   batch.Transactions
           if not more: break

       INSERT INTO load_audit (silver_source_id, loaded_at,
                               change_number_before, change_number_after,
                               window_start, window_end,
                               snapshots_loaded, transactions_loaded)
            VALUES (...);

8. UPDATE silver_sources
        SET high_watermark = window.NewChangeNumber,
            last_loaded_at = now,
            silver_path    = (config path),
            silver_kind    = (config kind)
    WHERE silver_source_id = ?;
   INSERT lazily on first load.

9. COMMIT.
```

One transaction wraps the gold-side work. A crash anywhere rolls
everything back; the next `wealthdb load` retries from the same
watermark. The plugin's `Open` / `Status` / `ChangeWindow` /
`Snapshots` / `Transactions` calls are read-only against silver
and don't need transactional scope.

`wealthdb load -a` runs the same sequence per silver source, each
in its own transaction. A failure in one source does not roll
back already-completed sources.

### 8.2 Why a windowed delete (not a full-replace)

The previous draft used full-replace for `transactions` — delete
all events for the silver source on every load, re-insert all of
them. That was correct but wasteful at scale.

The windowed-delete approach (above) is just as correct and bounded
by the size of the delta:

- **For snapshots**: every snapshot whose `snapshot_at` falls in
  the window is wiped and re-emitted. Re-emitted rows have the same
  natural PK and therefore the same identity; previously-loaded
  snapshots outside the window stay untouched.
- **For transactions**: every event whose `occurred_at` falls in
  the window is wiped and re-emitted. This handles silver's
  amend-in-place behaviour cleanly: if silver retracted an event
  inside the window, that event simply doesn't reappear, and the
  retraction propagates to gold.

The plugin guarantees that any silver record changed since the
previous watermark has its `snapshot_at` or `occurred_at` inside
the returned window. That's the load contract.

### 8.3 The adapter contract for `Snapshots` and `Transactions`

When `Snapshots(window)` is called, the plugin must emit, across
all batches:
- One `PositionChange` per silver position row whose `snapshot_at`
  is in `[window.Start, window.End]`.
- One `CashBalanceChange` per silver cash-balance row similarly.
- One `FxRateChange` per silver FX-rate row similarly.
- `AccountChange` / `InstrumentChange` updates for every account
  and instrument referenced by the above. Re-emitting an unchanged
  dimension is fine (the gold-side upsert is idempotent); skipping
  a referenced dimension is **not** fine (the FK would dangle).

When `Transactions(window)` is called, the plugin emits one
`TransactionChange` per silver event row whose `occurred_at` is in
the window.

The plugin does **not** consult gold for "what's already there" —
the windowed delete in step 7 has wiped the slot first. The plugin
is therefore stateless w.r.t. previous loads.

### 8.4 Dimension upsert semantics

`accounts` and `instruments` are not windowed. Their gold-side
upsert is:

```sql
INSERT INTO accounts (...) VALUES (...)
ON CONFLICT (silver_source_id, account_external_id) DO UPDATE
   SET display_name    = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                              THEN EXCLUDED.display_name ELSE accounts.display_name END,
       base_currency   = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                              THEN EXCLUDED.base_currency ELSE accounts.base_currency END,
       /* ...same CASE pattern for the other attribute columns and payload... */
       first_seen_at   = LEAST   (accounts.first_seen_at, EXCLUDED.first_seen_at),
       last_seen_at    = GREATEST(accounts.last_seen_at,  EXCLUDED.last_seen_at);
```

The per-column `CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at`
guard means re-emitting an *older* observation doesn't overwrite
newer-observed attributes. The seen-at range still expands in both
directions regardless — re-emitting an older snapshot pulls
`first_seen_at` backward but never disturbs the attributes that a
newer observation set.

(An earlier sketch used a single `WHERE EXCLUDED.last_seen_at >=
accounts.last_seen_at` at the end of the SET clause; that gated
the whole UPDATE including `first_seen_at` expansion, defeating
the union semantics. Per-column CASE keeps the two concerns
independent.)

### 8.5 Silver-went-backwards detection

If the plugin's `LatestChangeNumber` is **strictly less than** the
stored `high_watermark`, gold refuses to load. Possible causes:

- Silver was restored from an older backup.
- Silver was rebuilt from bronze with a different parser order
  that produced a lower MAX(snapshot_at) (unlikely — snapshots
  are timestamp-keyed — but possible if the change-number encoding
  ever changes).

The escape hatch is `wealthdb reset <id>` followed by `wealthdb load
<id>`. Auto-recovery would risk silently dropping data and is
out of scope for v1.

## 9. Reset semantics

`wealthdb reset <id>` runs:

```sql
BEGIN TRANSACTION;
DELETE FROM transactions   WHERE silver_source_id = ?;
DELETE FROM fx_rates       WHERE silver_source_id = ?;
DELETE FROM cash_balances  WHERE silver_source_id = ?;
DELETE FROM positions      WHERE silver_source_id = ?;
DELETE FROM instruments    WHERE silver_source_id = ?;
DELETE FROM accounts       WHERE silver_source_id = ?;
DELETE FROM load_audit     WHERE silver_source_id = ?;
DELETE FROM silver_sources WHERE silver_source_id = ?;
COMMIT;
```

The order matters because of foreign keys. The silver database
itself is untouched. The watermark is removed with the
`silver_sources` row, so a follow-up `wealthdb load <id>` starts
fresh (treating `high_watermark` as -1).

`wealthdb reset -a` runs the same per silver source. There is no
"truncate everything" path — to fully rebuild, `rm` the gold DB and
re-run `wealthdb init`.

## 10. Query patterns

> **Note on SQL throughout this document.** The SQL fragments
> here and in §8 (load semantics) are **functional
> specifications**, not implementation requirements. They state
> the observable result a reader should expect; the actual Go
> code may use prepared statements, batched/multi-row inserts,
> different join orderings, CTE inlining or de-inlining,
> DuckDB-specific extensions (`ASOF JOIN`, window functions,
> views, table macros), or query rewrites for performance — as
> long as the result is equivalent.
> Pseudo-SQL and pseudo-code blocks in §8 are likewise
> illustrative of the load orchestration, not a literal program
> the implementation must mirror.

### 10.1 Positions as of a date

The canonical "what do I hold on date X" query:

```sql
WITH latest_per_source AS (
    SELECT silver_source_id, MAX(snapshot_at) AS snapshot_at
      FROM positions
     WHERE snapshot_at <= :as_of_epoch
     GROUP BY silver_source_id
)
SELECT p.*
  FROM positions p
  JOIN latest_per_source l
    ON p.silver_source_id = l.silver_source_id
   AND p.snapshot_at      = l.snapshot_at;
```

The compound PK `(silver_source_id, snapshot_at, account_external_id,
position_key)` makes both the `MAX` and the join cheap.

### 10.2 Positions including cash

Same query, UNION ALL with cash rows projected into the positions
shape:

```sql
SELECT ..., 'cash' AS asset_class, NULL AS instrument_external_id,
       currency, amount AS quantity, amount AS market_value, ...
  FROM cash_balances
 WHERE balance_kind = 'closing'   -- or whatever the canonical pick is
```

`balance_kind` precedence per source is encoded in the adapter,
not in queries. (Schwab → `current`; UBS → `closing`; Swissquote →
its sole per-currency total.)

### 10.3 Filter by asset class and vehicle

Exposure and wrapper are separate columns, so a query can filter on
either dimension. Fixed-income exposure held as direct bonds or bond
funds (excluding, say, bond ETFs):

```sql
SELECT * FROM positions
 WHERE silver_source_id = ?
   AND snapshot_at = (SELECT MAX(snapshot_at) FROM positions WHERE silver_source_id = ?)
   AND asset_class = 'fixed_income'
   AND vehicle IN ('bond', 'fund');
```

### 10.4 Holding-period filter (future, sketch)

```sql
SELECT * FROM positions
 WHERE asset_class = 'public_equity'
   AND acquisition_date <= :now - INTERVAL 1 YEAR;
```

`acquisition_date` will be NULL initially; a future migration
populates it from `transactions` history.

### 10.5 Net worth by bank

This is implemented as the `report_accounts` table macro (migration
0021), which rolls each account's latest-snapshot positions and cash up
to a single converted value per account, grouped under its silver
source. The conversion to `:output_currency` is the §10.6 `fx_daily`
lookup applied per line; the macro takes `:output_currency` and an
as-of as parameters and is invoked from both the CLI
(`SELECT * FROM report_accounts(?, ?)`) and the Metabase models.

`report_sources` (migration 0025) takes this one step coarser: it
buckets the same lines by `silver_source_id` instead of account, so
`wealthdb holdings sources` is one row per silver source — the grain between
`wealthdb holdings accounts` / `portfolios` and the whole-portfolio `wealthdb
global`. Base currency, tax wrapper, and management style are rolled up
agree-or-NULL across the source's non-overlay accounts (like
`report_portfolios`). Because all four reports aggregate the same
converted lines, `sum(sources.total_<ccy>) == sum(accounts.total_<ccy>)
== sum(portfolios.total_<ccy>) == global.total_<ccy>` by construction.

See §10.6 for the conversion itself. FX-source precedence among silvers
is a separate design point (§13.2).

### 10.6 Currency conversion with historic FX rates

Currency conversion lives **entirely in SQL** — two DuckDB views plus
the report macros, defined in migrations `0020_fx_views.sql` and
`0021_report_macros.sql`. There is no Go-side resolver: every
value-producing query reaches the same rates through these views, so
the CLI and the Metabase models convert identically by construction.

**`fx_norm` — both directions, with priority.** Each stored `fx_rates`
row `(base_currency, quote_currency, mid_rate)` means "1 `quote` =
`mid_rate` `base`". `fx_norm` emits every row in **both** directions: a
direct `quote → base` rate of `mid_rate`, and a reciprocal `base →
quote` rate of `1 / mid_rate`. Each emitted edge carries its source's
`silver_sources.fx_priority` (NULL coalesced to a max sentinel so
priority-less sources sort last).

**`fx_daily` — one winner per pair per UTC day.** `fx_norm`'s edges are
bucketed to the UTC day (`snapshot_at // 86400`). Within each
`(from_ccy, to_ccy, day)`, a `ROW_NUMBER()` ordered by `fx_priority`
ASC, then `snapshot_at` DESC picks a single winning rate. So a day two
sources both cover goes to the higher-priority source; a day only one
covers is filled by that one. (A reference source like `fred` thus
keeps the deep historic tail an account source lacks, without
overriding that account source on the days they overlap.)

**Conversion inside the report macros.** The macros convert a line by
an **ASOF LEFT JOIN** to `fx_daily` that takes the most recent rate **at
or before** the line's own day — flat, with no interpolation between
rates. A line whose day predates the pair's FX history **clamps to the
earliest available rate** (migration 0023 adds a day-0 floor row per
pair to `fx_daily`), so deep-history holdings still convert. Only a pair
with no rate at all yields `NULL` → an **empty value cell**. The lookup
is a `COALESCE` that tries, in order, the first leg that resolves
winning:

1. **identity** — when `from == to`, the amount passes through unchanged;
2. **direct** — the `from → to` rate from `fx_daily`;
3. **triangulate through CHF** — `from → CHF → to`;
4. **triangulate through USD** — `from → USD → to`.

CHF and USD are the pivots because the feeds publish mostly CHF→X
(UBS, Swissquote) and X→USD (FRED, crypto) pairs, so almost every
needed cross resolves through one of them. Direct and reciprocal pairs
are already both present in `fx_norm`, so leg 2 covers either
direction without a separate fallback.

**`current` vs `historic`.** Both modes use the same ASOF machinery;
they differ only in the as-of passed to the macro:

- **`historic`** (default) passes the real as-of date, so each line
  lands on the newest rate at or before its own day.
- **`current`** passes an as-of of `MAX(BIGINT)`, so the ASOF join
  always lands on each source's latest snapshot.

There is **no linear interpolation** in either mode. A line predating
its pair's FX history clamps to the earliest available rate (above);
only a pair with no rates at all leaves the value cell empty.

**Cross-silver precedence.** Precedence is **data, not runtime state**.
Migration `0019` added a `silver_sources.fx_priority` INTEGER column;
on every `load` / `reload`, `gold.SetFxPriorities(ctx, db,
cfg.FxSourceOrder())` stamps each source's rank into it (rank 0 =
highest priority; sources not listed = NULL = lowest). The config field
is still `silver_sources[].fx_priority` (lower = higher priority,
absent = lowest, ties broken by config declaration order), flattened by
`config.FxSourceOrder()`. `fx_norm` / `fx_daily` read the stamped
column. See §13.2.

### 10.7 History reports (time series)

For charting value over time, each snapshot report has a
`report_*_history(p_ccy)` variant (migration 0022, plus `report_sources_history`
in 0025): `report_global_history`, `report_sources_history`,
`report_accounts_history`, `report_portfolios_history`, `report_positions_history`.
Each emits one row per entity per UTC day, from the first snapshot to today,
with the value **carried forward** — on a day with no new snapshot the most
recent snapshot's value is repeated. Positions and cash are carried
independently (a source's two series can diverge), and the active snapshot is
chosen by `snapshot_at` so multiple same-day dumps resolve to the latest. Each
line is valued **once** at its own snapshot's FX day, then expanded onto a daily
spine via an at-or-before ASOF join — so `history@today` equals the matching
`report_*(MAX)` "latest" report and per-day compute stays cheap. Empty
entity-days are omitted (they would be 0). `report_global_history` is
`Σ report_accounts_history`. The Metabase models wrap the **multi-currency**
variants of these (§10.8), not the single-currency macros directly.

### 10.8 Multi-currency reports (Metabase)

The Metabase models need a value column **per reporting currency** (USD, CHF,
EUR) so a user picks the currency by picking a column — Metabase native
*models* don't expose template-tag parameters to questions built on them, so a
"target currency" widget wouldn't reach the charts. Calling
`report_x(MAX, 'USD' | 'CHF' | 'EUR')` three times and joining would re-run the
whole pipeline per currency: only the out-currency conversion varies; the scan,
cash dedup, and **base-currency** conversion are currency-agnostic. Migration
`0024_multi_currency_reports.sql` factors that shared work out so it runs once.

- **Shared line bases.** `account_lines_base(p_asof)`,
  `portfolio_lines_base(p_asof)`, and `hist_acct_lines_base()` do the scan +
  cash dedup + base-currency conversion **once**, emitting one row per line with
  its raw `(ccy, amt, snap)` for the callers to convert.
  `portfolio_acct_map()` (the orphan→`''` routing of §2/migration 0023) and
  `portfolio_buckets()` (the bucket list + rolled-up taxonomy) are the single
  source of truth for the portfolio rollup, shared by every portfolio macro.
  `source_buckets()` (migration 0025) is the per-silver-source analogue of
  `portfolio_buckets()` — the rolled-up taxonomy + base currency, shared by
  every source macro; `source_lines_base(p_asof)` is the latest-snapshot line
  base behind `report_sources` / `report_sources_multi` (the history variants
  build their own all-snapshot lines, like the portfolio history macros).
- **Single-currency macros refactored.** `report_accounts` /
  `report_portfolios` and their `_history` variants are rebuilt on these bases;
  their output columns/values are **unchanged** (the Go scan and gold tests are
  unaffected — verified by a byte-for-byte before/after diff).
- **`report_x_multi` macros.** `report_{global,sources,accounts,portfolios,positions}_multi(p_asof)`,
  `report_transactions_multi(p_from, p_to)`, and the five `report_*_history_multi()`
  emit one value set per currency — `{positions_value,cash_balance,total_value}_{usd,chf,eur}`
  plus the base trio. Each `_<ccy>` column equals `report_x(MAX, '<ccy>')` for
  that currency by construction (same base, identical 5-leg COALESCE). They are
  Metabase-only, so they emit **DECIMAL** money directly (no CLI VARCHAR-trim
  round-trip); the `web/provision.py` wrapper then only casts epoch columns to
  TIMESTAMP. Per-currency conversion shares pivot legs (`ccy→{CHF,USD,EUR}` plus
  the `CHF→{USD,EUR}` / `USD→{CHF,EUR}` crosses) rather than N independent
  blocks. The reporting set is USD/CHF/EUR, fixed in the macros; change it in a
  new migration.
- **Account display defaults.** `report_accounts_multi` and
  `report_accounts_history_multi` apply the conventional
  `tax_wrapper='taxable_personal'` / `management_style='self_directed'` defaults,
  so the account-level Metabase reports are **never NULL** — matching the
  `wealthdb holdings accounts` render (§4). The single macros and the raw `accounts`
  table keep NULL, preserving gold's "unknown vs. explicitly default"
  distinction (the defaults are a display concern). Portfolio rollups keep their
  strict NULL-on-mixed semantics (faithful to `wealthdb holdings portfolios`, which shows
  blank for ambiguous buckets).

### 10.9 Returns — TWR / MWR (`wealthdb returns`)

`wealthdb returns <view>` (`<view>` = `accounts` | `portfolios` | `sources` |
`global`) computes **time-weighted** (TWR) and **money-weighted** (MWR / XIRR)
returns over a window. Unlike the holdings views it is **not** a Metabase model:
the XIRR root-find and geometric chaining are iterative, not expressible as a
DuckDB macro. It is a CLI-only, hybrid computation:

- **SQL assembles**, reusing existing macros — no new migration. The per-account
  carry-forward value series comes from `report_accounts_history(p_ccy)` (§10.7;
  its ASOF-inner join already omits pre-inception days, so a boundary before an
  account's first snapshot reads as NULL — "not yet alive", not 0). External
  flows come from `report_transactions(from,to,p_ccy)` (`net_amount` converted at
  `occurred_at`). The source's adapter kind (`silver_sources.silver_kind`) and a
  `DISTINCT snapshot-day per account` query complete the inputs.
- **Go computes** (`internal/returns`, pure + unit-tested): Modified-Dietz per
  sub-period, geometric chaining, XIRR (Newton + bisection), the per-adapter flow
  policy, and the synthetic onboarding/closure mechanics. The gold orchestration
  is `internal/gold/returns.go` (`RunReturns`), aggregating the per-account spine
  to every grain.

Method and conventions (see `docs/RETURNS-NOTES.md` for the full rationale and
the verified per-adapter flow table):

- **TWR** = chained period-Modified-Dietz; `--period {monthly|quarterly|annual|
  total}` controls the per-bucket rows, but the since-inception cumulative figure
  is chained over the entity's **actual valuation (snapshot) days**, independent of
  `--period`. Snapshot-aligned sub-periods keep each flow in the same bucket as the
  value change it causes; a fixed calendar grid splits a flow from a later value
  realisation when a period boundary lands in a snapshot gap, manufacturing a
  sub-(−100%) bucket that collapses the geometric chain (see RETURNS-NOTES
  §"snapshot-aligned headline"). For dense (daily-snapshot) sources this is just
  daily bucketing.
- **MWR** = XIRR over the window's external flows + opening/terminal values; n/a
  (with a reason) for no-flow / no-sign-change / non-unique / NAV-only entities.
- **Each source declares a pluggable `ReturnsPolicy`**, co-located in
  `internal/silver/<source>/policy.go` and registered from that package's `init()`,
  resolved by adapter kind via a registry (`internal/returns/returnspolicy.go`).
  The engine takes **zero source names or branches** — it reads knobs off the
  resolved policy (onboarding grain, conduit kinds, external-only, inception
  anchor) and the policy is source-scoped even at the global grain. **Flow
  classification** is one member of that policy (banks/pension = flow-complete;
  crypto = fiat flows only; manual/carta/equityzen = NAV-only). `value_outccy`
  already carries the canonical sign, so Dietz `F_i = +value_outccy` and XIRR
  `cf = -value_outccy` with no per-kind exception.
- **Historic-FX only** (no `--fx-mode`): FX movement is part of the return. **Net
  of fees and taxes paid** (after-tax) — costs stay inside the value series.
- **Mortgage / net-negative entities** are excluded from coarse rollups and shown
  as a separate `nonpositive_base` liability line.
- **Account-grain is exact**; coarse grains are best-effort (heuristic transfer
  netting, synthetic onboarding for staggered inception). **Returns are NOT
  additive across grains** — `global == Σ accounts` is a *value* identity, not a
  return identity.
- **Staggered inception (corrected semantics).** When a constituent joins a
  coarse aggregate *mid-window* (debut `d > winFrom`), its arrival is booked once
  as a synthetic onboarding inflow of its **full** first-snapshot value at `d`,
  and **all of its own external flows dated `≤ d` are subsumed** (dropped from the
  aggregate flow series) — they happened while the aggregate value series did not
  yet reflect the account, so counting them too would double-count the capital and
  drive the chained TWR below −100%. A constituent already alive at `winFrom`
  keeps every in-window flow and gets no onboarding. The symmetric **closure**
  case subsumes a closing constituent's drains across its zeroing gap into the
  synthetic closure outflow. The netting interaction is handled by running
  transfer/journal netting over the *full* candidate set (pre-debut legs included)
  **before** subsumption, so genuine internal pairs annihilate and only a
  constituent's own surviving pre-debut/closure capital is subsumed — never
  orphaning a phantom outflow whose sibling sits on an already-alive account
  (whose value series does reflect it). Onboarding still legitimately recognizes
  *untracked pre-existing* capital (a late account with no funding transactions at
  all, e.g. a custody account whose backfill carries no transactions), which is NOT a double-count.
  See `docs/RETURNS-NOTES.md` §M6.
- **Conduit relationships (per-source policy knobs).** A source whose policy opts
  in (UBS today) is treated specially by the source-blind engine purely via its
  `ReturnsPolicy`: **conduit-kind** accounts (UBS cash) feed the value spine but
  emit no per-account onboarding; **per-entity-once** onboarding books the group
  step-up net of same-day negative sibling funding drops (floored at 0), so an
  internal cash→securities move inside a relationship onboards nothing — the
  capital was already counted at inception; **external-only** drops internal churn
  (UBS pre-tags external vs. internal in silver, so the engine hook is inert for
  it); **inception = first-real-snapshot** anchors the window past sparse cash-only
  pre-history (kills the tiny-base artifact). All four are source-scoped, so
  every other source stays byte-identical even in the merged global entity. See
  `docs/RETURNS-NOTES.md` §M7.

The honesty surface is the **`quality` column**: every n/a carries a reason, and
every approximation is tagged (`since_data_inception`, `partial_window`,
`staggered_inception`, `empty_bucket`/`carried_forward`, `boundary_same_snapshot`,
`dropped_while_nonzero`, `dietz_degenerate`, `nonpositive_base`, `mwr_no_flows`,
`mwr_no_sign_change`, `mwr_nonunique`, `mwr_no_converge`, `mwr_incomplete_flows`,
`unmatched_transfers=N`, `journal_present`, `nav_only`, `nav_only_capital_call_risk`,
`crypto_unclassified_transfers`, `unknown_adapter_policy`, `fx_clamped_flow`,
`pre_fx_history`, `after_tax`). Cross-grain note: `global == Σ accounts` is a
**value** identity (verified by reconciliation test), but **returns are not
additive across grains**.

## 11. Repository layout

```
wealthdb/
├── DESIGN.md                       — this document (gold-layer architecture)
├── README.md                       — user-facing usage
├── CLAUDE.md                       — agent ground rules
├── Dockerfile                      — single-stage; Go toolchain + binary in one image
├── docker-entrypoint.sh            — sets up paths, then exec wealthdb
├── wealthdb                          — thin host-side wrapper around `docker run` (production binary)
├── wealthdb-test                     — thin host-side wrapper around `docker run go test ...` (§12.5)
├── config.example.json             — annotated example config
├── go.mod
├── go.sum
├── docs/
│   └── adapters/                   — per-bank adapter design docs (one file per bank)
│       ├── schwab.md
│       ├── ubs.md
│       └── swissquote.md
├── cmd/
│   └── wealthdb/
│       └── main.go                 — CLI entry point, flag parsing, subcommand dispatch
├── internal/
│   ├── config/                     — JSON config parsing & validation
│   ├── gold/                       — DuckDB schema, transaction wrappers, queries
│   │   ├── schema.go               — embed migrations/, run them on init
│   │   ├── load.go                 — the per-source load orchestration in §8
│   │   ├── reset.go
│   │   ├── positions.go            — as-of query, formatting
│   │   └── status.go
│   ├── returns/                    — source-agnostic returns math + pluggable policy
│   │   ├── returnspolicy.go        — ReturnsPolicy superset + kind-keyed registry
│   │   ├── policy.go               — FlowPolicy, Regime, FlowPolicyFor
│   │   ├── dietz.go / xirr.go / chain.go / synthetic.go
│   ├── silver/                     — adapter interface and registry
│   │   ├── adapter.go              — interfaces from §6
│   │   ├── registry.go             — silver.Register / silver.Get
│   │   ├── schwab/                 — Schwab adapter (impl + co-located policy.go)
│   │   ├── ubs/                    — UBS adapter (impl + co-located policy.go, conduit knobs)
│   │   └── swissquote/             — Swissquote adapter (impl + co-located policy.go)
│   └── output/                     — table / csv / csv_plain / json formatters
└── migrations/
    └── 0001_initial.sql            — the schema in §7.2
```

The `internal/` prefix prevents downstream packages from depending
on the internals — `wealthdb` is an application, not a library.

## 12. Container / build / run

Single-stage Dockerfile, single image tag (`wealthdb:latest`),
used unchanged for development, testing, and production. See §12.4
for why we don't separate stages.

### 12.1 Image

Base: `golang:1.24-bookworm` (glibc, full Go toolchain, gcc/g++
for CGO). The Dockerfile:

1. Copies `go.mod` / `go.sum` and runs `go mod download` in its own
   layer for cache friendliness.
2. Copies the source.
3. Builds the binary at `/usr/local/bin/wealthdb`.

Entry point is the production binary. The `./wealthdb-test`
wrapper overrides with `--entrypoint go` to run tests in the same
image. See §12.5.

DuckDB's Go driver (`github.com/duckdb/duckdb-go/v2`) requires CGO
and glibc — the `bookworm` base satisfies both. Alpine / musl was
rejected for this reason.

### 12.2 Volume mounts

The host wrapper mounts host paths to the **same paths** inside the
container — so `$XDG_DATA_HOME/wealthdb/wealthdb.db` resolves identically on
both sides and no path translation is needed. Two specific bind
mounts:

```
$HOME/.config/wealthdb.cfg → $HOME/.config/wealthdb.cfg     config file
$XDG_DATA_HOME/wealthdb/            → $XDG_DATA_HOME/wealthdb/                gold + silver data tree
```

The container also sets `HOME` to match the host's so that `~`
and `$HOME` references in the config file resolve to the same
absolute paths inside and outside.

The host-side `wealthdb` shell wrapper does the mounting. Two shapes:

**Writer host** (runs `wealthdb config`, `wealthdb init`, `wealthdb load`,
`wealthdb reset`):

```sh
mkdir -p "$HOME/.config" "$XDG_DATA_HOME/wealthdb"
[ -f "$HOME/.config/wealthdb.cfg" ] || : > "$HOME/.config/wealthdb.cfg"
docker run --rm -it \
    -e HOME="$HOME" \
    -u "$(id -u):$(id -g)" \
    -v "$HOME/.config/wealthdb.cfg:$HOME/.config/wealthdb.cfg" \
    -v "$XDG_DATA_HOME/wealthdb:$XDG_DATA_HOME/wealthdb" \
    wealthdb:latest \
    "$@"
```

(`-it` is needed for `wealthdb config`; harmless for the others.)

**Reader host** (only `wealthdb holdings positions`, `wealthdb status`, ...,
against a read-only mount of someone else's gold tree):

```sh
docker run --rm \
    -e HOME="$HOME" \
    -u "$(id -u):$(id -g)" \
    -v "$HOME/.config/wealthdb.cfg:$HOME/.config/wealthdb.cfg:ro" \
    -v "/mnt/shared/wealthdb:$XDG_DATA_HOME/wealthdb:ro" \
    wealthdb:latest \
    "$@"
```

The `:ro` flag on the `$XDG_DATA_HOME/wealthdb` mount makes the gold DB file
unwriteable inside the container; `wealthdb`'s mode detection (§4.10)
picks this up and refuses (RW) subcommands with a clear message.

### 12.3 Build and first-run commands

```sh
./wealthdb build               # docker build, ~2 min
./wealthdb config              # one-time interactive setup wizard (§4.9)
./wealthdb init                # creates the gold DB at the configured gold_db path
./wealthdb load -a             # merges every configured silver into gold
./wealthdb holdings positions -f csv    # query
```

The wrapper passes through arguments verbatim to the in-container
`wealthdb` binary.

### 12.4 Why single-stage

Multi-stage Dockerfiles normally buy three things: a smaller
runtime image (build tools discarded), layer caching, and
build/runtime separation. None apply here:

1. **No image-size win.** The dev/test/prod image needs the Go
   toolchain, source, CGO build deps, *and* the built binary —
   nothing to discard.
2. **Caching matched by layer ordering.** Copying `go.mod` /
   `go.sum` and running `go mod download` *before* copying the
   source gives the same dep-layer cache benefit with one stage.
3. **No separation of concerns to make.** Build and runtime are
   the same environment, by design.

Multi-stage becomes worth it if a deploy target wants a tiny
production-only image, or if a code-generation step needs tools
absent from runtime. Retrofitting is mechanical. Not pre-paid.

### 12.5 Running tests

Tests run **inside the container**, so the host stays clean of Go
toolchain, DuckDB headers, SQLite headers, and the rest. A
companion wrapper `./wealthdb-test` (sibling to `./wealthdb`) routes
`go test` invocations into the container while passing arguments
through verbatim. This means individual tests are invoked from
the host with the same syntax a Go developer expects:

```sh
./wealthdb-test ./...                                 # full suite
./wealthdb-test ./internal/gold/...                   # one package tree
./wealthdb-test ./internal/gold/ -run TestPositionsAsOf
./wealthdb-test ./internal/gold/ -run TestFx -v -count=1
./wealthdb-test ./... -race
```

Argv after `wealthdb-test` is handed to `go test` as-is. Verbose
output, `-run`, `-race`, `-count`, `-bench`, etc. all work
unchanged. Exit code is `go test`'s exit code.

#### Wrapper shape

```sh
#!/bin/sh
# wealthdb-test — run `go test` inside the wealthdb container
set -eu
REPO="$(cd "$(dirname "$0")" && pwd)"

# Persist Go's caches across invocations so incremental builds
# stay fast — bind-mount the host's cache dirs (or dedicated
# named-volumes; the host paths are simpler to inspect).
mkdir -p "$HOME/.cache/wealthdb-test/go-build" "$HOME/.cache/wealthdb-test/go-mod"

docker run --rm \
    -u "$(id -u):$(id -g)" \
    -e HOME="$HOME" \
    -e GOCACHE="$HOME/.cache/wealthdb-test/go-build" \
    -e GOMODCACHE="$HOME/.cache/wealthdb-test/go-mod" \
    -v "$REPO:$REPO" \
    -v "$HOME/.cache/wealthdb-test:$HOME/.cache/wealthdb-test" \
    -w "$REPO" \
    --entrypoint go \
    wealthdb:latest \
    test "$@"
```

#### One image, not two

`./wealthdb-test` uses the same `wealthdb:latest` image as the
production wrapper, just with `--entrypoint go`. The single image
carries the full Go toolchain so `go test` works against the
mounted source tree; no separate builder image is needed.

#### Cache locations

Go's build cache (`GOCACHE`) and module cache (`GOMODCACHE`) live
on the host under `$HOME/.cache/wealthdb-test/` so that incremental
re-runs are fast across container restarts. The cache dir is
isolated from any host-side Go toolchain — running tests via the
wrapper never pollutes a host `$GOPATH`.

#### No host Go required

A user with only Docker installed can run the entire test suite
end-to-end. This is the same contract as `./wealthdb` itself.

## 13. Open questions / future work

These are deliberately left out of v1 but worth recording.

### 13.1 Cross-silver instrument canonicalisation

If both UBS and Schwab hold AAPL, gold today has two `instruments`
rows. Queries that want "all AAPL holdings" join on `isin`. This
works but is awkward. A future `instruments_canonical` table with
a mapping from `(silver_source_id, instrument_external_id)` to a
canonical instrument ID would tidy this up. Hold until the awkwardness
actually bites.

### 13.2 FX-rate precedence among silvers — implemented

**Resolved.** This was once an open question (§10.6 originally took
whichever row sorted first). Of the options floated, the implemented
rule is the **configured per-source** one, refined with a day-bucket
tiebreak so a reference source never overrides an account source on the
days they overlap. Precedence is now **data, not runtime state**: it
rides on a stamped gold column that the `fx_daily` view reads.

- `fx_daily` (§10.6) buckets each `(from, to)` pair to the **UTC day**,
  so the snapshot nearest the target day always wins — a reference
  source (e.g. `fred`, reaching back to 1971) keeps the deep historic
  tail a daily account source lacks, without overriding that account
  source on days it covers.
- **Within a day**, a source-priority tiebreak chooses the winner: the
  per-source `silver_sources[].fx_priority` config field — lower = higher
  priority, absent/null = lowest, ties broken by config declaration
  order. `config.FxSourceOrder()` flattens this to an ordered list of
  `silver_source_id`s; on every `load` / `reload`,
  `gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder())` stamps each
  source's rank into the `silver_sources.fx_priority` gold column (added
  by migration `0019`; rank 0 = highest, unlisted = NULL = lowest), and
  the `fx_norm` / `fx_daily` views read that column directly. (The old
  runtime hook `gold.SetFxSourceOrder` and `internal/gold/fx.go` are
  gone.)
- **Exact timestamp** (`snapshot_at` DESC) breaks any remaining tie.

Still out of scope today: non-snapshotted FX sources (e.g. ECB
reference rates, or a manually maintained `fx_overrides` table for
currencies no silver covers — `fred` now covers the major ones).

### 13.3 acquisition_date backfill

For holding-period queries to work, `acquisition_date` must be
populated. Strategy: a `wealthdb backfill acquisitions` subcommand
that walks `transactions` (filtered to `kind IN ('buy',
'transfer_in')`) and writes the earliest matching date into each
position row. FIFO vs LIFO is a deeper design point.

### 13.4 Tax-lot tracking

Once `acquisition_date` is in place, the natural next step is
per-lot positions (one row per buy, decremented by sells). That's
a different grain than today's `positions` table; it would live
in a separate `position_lots` table rather than reshape `positions`.

### 13.5 Adapter for `auto` kind

The detection rules in §5.2 work but are fragile to silver-schema
changes. Long-term: silver should publish its own `kind` in
`schema_meta`. Open a coordinated change with the dump repos
before relying on `auto` in production configs.

### 13.6 Concurrent loads

Gold is single-writer today. If two `wealthdb load` invocations
race, DuckDB will serialise them (file lock); the second will see
the first's updated `high_watermark` after that transaction
commits and will simply observe a smaller (or empty) change
window. Concurrent readers (in read-only mode, see §4.10) take no
lock and are unaffected. No explicit coordination needed at this
stage.

### 13.7 Deferred silver tables

Per the per-bank adapter docs (§6.7), several silver tables exist
but no v1 adapter projects them into gold. Cross-bank summary of
what each would need:

- **`schwab.user_preference`** — Schwab UI/streamer config. No
  portfolio relevance. Likely permanent omit.
- **`schwab.open_orders`** — operational, not portfolio state. A
  hypothetical `wealthdb orders` subcommand could surface these
  through a dedicated `open_orders` gold table; deferred until
  there's a clear use case.
- **`ubs.account_holders`** — client/legal-owner metadata. Could
  populate a `clients` dimension joining to `accounts` via
  `relationship_id`. Useful for households with multiple legal
  entities; defer until a use case lands.
- **`ubs.pending_securities`** — MT537 pending-settlement
  snapshots. The majority of rows are "no activity" markers
  (`ACTI//N`); the rest are transient (positions show up in
  `holdings` once settled). Worth surfacing only if a use case
  cares about T+2 visibility.
- **`ubs.portfolio_performance`** — TDPOPF monthly performance
  rollups. Natural input for a future `wealthdb performance`
  subcommand; would land in its own `portfolio_performance` gold
  table to avoid muddling the snapshot/event model.
- **`ubs.cash_account_pricing`** — TDCAPI interest-rate config.
  Useful for projecting interest income; would land in a
  dimension-style `cash_account_pricing` gold table joined to
  `accounts`.
- **`swissquote.documents`** — PDF document index. Bronze is the
  source of truth for binaries; gold doesn't store them. Would
  only be projected if `wealthdb` ever needed to enumerate document
  metadata.

Adding any of these is a localised change: one new gold table
(or column), one adapter `Snapshots` / `Transactions` extension,
no impact on the load contract or other adapters.

### 13.8 Market data

A planned future addition: pricing and instrument metadata that
no bank silver carries — real-time and historic prices, ex-
dividend dates and amounts, agency ratings, historic and implied
volatility, option chains, fundamental metrics, etc. These are
properties of an instrument (the asset itself) rather than of a
holding (who owns how much at which bank), so they don't fit any
existing silver.

Sketch of where this lands when designed:

- **Source.** TBD — candidates include a paid market-data vendor
  API (Polygon, Tiingo, EOD historical data), a free-tier feed
  with caveats (Alpha Vantage), or local scraping of public IR
  pages. The chosen source dictates the bronze format.
- **Layer.** Likely its own silver-equivalent (a separate
  bronze→silver pipeline, possibly as a new sibling collector like
  `marketdata`), feeding into gold via the same plugin
  contract as the bank silvers — `Adapter`, `Status`, `ChangeWindow`,
  `Snapshots`, `Transactions`. Reusing the plugin pattern means
  gold doesn't have to special-case market data.
- **Gold tables.** New tables joined to `instruments` via
  `(silver_source_id, instrument_external_id)` or via `isin`:
  - `instrument_prices`  — snapshot grain `(instrument, timestamp, price, currency)`
  - `instrument_events`  — event grain `(instrument, kind, occurred_at, ...)`
                            for ex-div, splits, ratings changes, etc.
  - `instrument_vol`     — snapshot grain `(instrument, timestamp, hist_vol, impl_vol_atm, ...)`
- **Impact on existing schema.** Additive only. Today's
  `positions.market_value` (sourced from the bank's view) coexists
  with future `instrument_prices`-derived mark-to-market — queries
  pick whichever they prefer, with the bank's number remaining the
  authoritative "what the bank says you have" figure.

This is a sketch only; the source and ingest design will be
fleshed out when the feature is scheduled.

### 13.9 Account taxonomy (kind / tax_wrapper / management_style) and nickname

Users typically hold several distinct kinds of accounts at the
same bank — personal brokerage, managed wealth account, UTMA /
ESA / IRA / 529 / Säule 3a wrappers for tax purposes, separate
cash accounts. Filtering positions and net-worth roll-ups by
these dimensions is more useful than slicing by raw account ID.
Independently, a user-friendly `nickname` lets the CLI render
something more recognisable than the bank's identifier.

The `accounts` table carries three orthogonal classifier columns
plus the free-text descriptors (migrations 0002 promoted
`account_category`/`nickname`; migration 0008 added the two
structured-enum columns):

- **`account_kind`** — the technical container the bank exposes:
  `brokerage`, `cash`, `safekeeping`, `custody`, `overlay`,
  `crypto_exchange`, `crypto_self_custody`, `other`. Required;
  every adapter stamps this.
- **`tax_wrapper`** — the tax / regulatory registration.
  Nullable; defaults to `taxable_personal` at render time.
  Values cover US (`traditional_ira`, `roth_ira`, `sep_ira`,
  `simple_ira`, `401k`, `403b`, `457b`, `529`, `coverdell_esa`,
  `hsa`, `daf`, `custodial_utma`, `custodial_ugma`,
  `trust_grantor`, `trust_non_grantor`, `trust_charitable`) and
  Switzerland (`pillar_2`, `vested_benefits` /
  Freizügigkeitskonto, `pillar_3a`), plus generic
  `taxable_personal`, `taxable_joint`, `foundation`, `other`.
- **`management_style`** — who places trades. Nullable;
  defaults to `self_directed` at render time. Values:
  `self_directed`, `advisory`, `discretionary`, `automated`.
- **`account_category`** — free-text bank-supplied descriptor,
  unchanged from migration 0002. Demoted from "the primary
  classifier" to "supplementary metadata"; useful for verbatim
  strings the structured enums don't capture (UBS's
  "Custody / Cash-Custody", etc.).

Adapter-supplied values today:

- **UBS** populates `account_category` from
  `cash_accounts.AcctTpDesc` for cash legs, and from
  `safekeeping_accounts.AcctTpDesc` + `AcctSubTypeDesc`
  (concatenated as `"Type / Sub"` when the sub is present) for
  safekeeping legs. `tax_wrapper` is derived from `AcctTpCd`
  via an explicit known-code switch (every observed PSN code
  maps to `taxable_personal`); unknown codes log a one-shot
  WARN and leave the column nil. UBS PSN doesn't expose a
  per-account nickname, so `nickname` stays NULL unless the
  config-side override fills it in. `management_style` is not
  surfaced by silver and stays nil.
- **Schwab** populates `nickname` from the v3-promoted
  `silver.accounts.nickname` column (which mirrors the user-set
  label from `/userPreference`). `account_category` and
  `tax_wrapper` stay NULL — Schwab's `securitiesAccount.type` is
  CASH/MARGIN (margin enablement), not a wealth-management
  category, and the schwab-api endpoints surveyed expose no
  structured wrapper field. Config-side override fills both.
- **Swissquote** populates `account_category` and `tax_wrapper`
  from `silver.accounts.account_product` (the per-account
  product label scraped from the eBanking account-overview
  page): `Trading`/`Savings` → `taxable_personal`,
  `Säule 3a` → `pillar_3a`, `Freizügigkeit` → `vested_benefits`.
  `nickname` stays NULL; Swissquote doesn't expose one.
- **Fidelity** populates `tax_wrapper` and `management_style`
  via a join through `silver.portfolios.kind`: `529` →
  `tax_wrapper=529`; `trust_managed` → `tax_wrapper=
  trust_non_grantor` + `management_style=discretionary`. Per-
  account registration labels (Roth IRA / Coverdell ESA / etc.)
  aren't currently surfaced by silver; the path to add them is
  documented in fidelity-web's DESIGN.md §11.6.

Adapters use SQLite PRAGMA-based feature detection where the
silver schema has evolved (e.g. Swissquote's pre-v5 silvers used
`account_type` instead of `account_product`); older silvers
still load with the corresponding columns left NULL.

Config-side override (extends §5): users can specify
`account_overrides` as a nested map keyed by
`(silver_source_id, account_external_id)` with optional
`nickname`, `category`, `tax_wrapper`, and/or `management_style`
fields. `tax_wrapper` and `management_style` values are
validated against the canonical enums at config-load time. The
loader applies overrides *after* the adapter has stamped its
own values, so the override takes precedence on overlap. This
is the path for sharpening accounts the adapter can't classify
on its own (e.g. a Schwab IRA whose wrapper isn't reachable
from any silver-side field).

The instrument dimension has the same escape hatch:
`instrument_overrides`, keyed by `(silver_source_id,
instrument_external_id)`, pins a per-instrument `(asset_class,
vehicle)` pair (validated as an admitted taxonomy pair). The loader
patches both the `instruments` row and every `positions` row
referencing the instrument — the fact rows carry their own
`asset_class`/`vehicle` copy, so the two must move together. Use it
where neither the source's structured signal nor the ETF name
refinement (§6.8) gets the pair right — the canonical example is an
exchange-traded commodity trust whose security name never mentions
the metal or the ETF-ness (`metal × etf`).

Selectable columns: `wealthdb holdings accounts -C
silver_source,account,account_kind,tax_wrapper,management_style,...`.
The three structured classifiers are part of the default
column set; the descriptor columns (`account_category`,
`account_nickname`) are registered but opt-in.

Still on the roadmap:
- `--tax-wrapper personal,roth_ira` filter for the readout
  subcommands.
- Long-form `wealthdb status` flag to show wrapper / style
  alongside account IDs.

### 13.10 Equity-transfer ledger

Securities transferred *into* a tracked account (e.g. shares
transferred in from another custodian at appreciated value)
are a capital inflow at their market value on the transfer date —
but the collectors often don't capture them as valued transactions
(the position just appears in a later snapshot, or the transfer is
booked as a $0-cash share journal). Left uncorrected, that value
reads as in-account performance, inflating returns — most visibly
the money-weighted figure, which over-weights an early arrival on a
small base. The discriminator is **cost basis ≪ market value**: an
appreciated transfer-in carries a basis far below its value (the
gain happened elsewhere), whereas an ordinary holding has basis ≈
value.

The optional `equity_transfers` CSV ledger (config §5) records each
known transfer; the loader turns every row into a canonical
`transfer_in` / `transfer_out` transaction at load time, so the
returns engine books it as an ordinary flow with no special-casing
and it shows in the transactions report. Columns:
`silver_source_id, account, occurred_at (YYYY-MM-DD), direction
(in|out), quantity, cost_basis, value, currency, instrument, note`.
`value` is the market value at transfer — the capital flow;
`cost_basis` (the pre-transfer basis) rides along in the payload for
reference and is not used by the returns math. `account` accepts
either the gold `account_external_id` or an account nickname
(resolved at load). A row whose value isn't yet known is left at 0
— a placeholder that books nothing until filled in.

Mechanics (`internal/loader/transfers.go`): ledger transactions get
a deterministic `xfer:`-prefixed synthetic id keyed on the row's
content (not its amounts), so editing a value updates the same row;
each load deletes the source's prior `xfer:` rows and re-inserts the
current set, so a `reload` picks up edits. A transfer dated at or
before an account's first snapshot is *subsumed by the
staggered-inception onboarding flow* (§10.9) — it is not
double-counted — while a mid-life transfer is booked in full. The
ledger is source-agnostic; any source's transfers are just rows
with that `silver_source_id`.
