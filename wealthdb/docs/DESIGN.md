# wealthdb: Design

## 1. Audience and scope

This document describes the design of `wealthdb` — the **gold**
layer of the personal-portfolio data pipeline whose bronze and silver
layers are owned by the per-source collectors under `collectors/`
(see [the repo-root DESIGN.md](../../DESIGN.md) for the full fan-in).

The repo-root DESIGN.md sets out the three-layer (bronze/silver/gold)
model, and [schwab-api/DESIGN.md](../../collectors/schwab-api/DESIGN.md)
§4 the silver-schema conventions the collectors share. This document
covers only what gold adds.

## 2. Where gold sits

```
┌────────────────────┐  ┌────────────────────┐  ┌────────────────────┐
│   ubs-psn          │  │   schwab-api       │  │   ... one box per  │
│   bronze: zips     │  │   bronze: JSON     │  │   collector — see  │
│   silver: SQLite   │  │   silver: SQLite   │  │   collectors/      │
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
schema is owned by its collector; `wealthdb` reads silver but never
writes to it.

Gold is the **agent- and human-facing** layer: canonical, indexed,
strictly relational, and the surface the reports (holdings, returns,
gains, exposure, spending, income, cash flow) read.

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
  Each account contributes its own active snapshot at that instant:
  its latest snapshot ≤ that date, until that snapshot ends. The
  point-in-time reports and the daily history resolve it alike, per
  (source, account) (§10.7).
- **Multi-currency output at query time.** Position and net-worth
  reports can be rendered in any ISO currency; the choice is made
  per-invocation, not baked into the database. FX conversion uses
  historic rates (snapshot-time): the flat nearest rate at or before
  the line's own day, no interpolation. See §10.6.
- **Hands-off operation.** Non-interactive CLI. One process, one
  database, no daemons. Re-runs are idempotent.
- **Read-only sharing.** A single gold DB file can be served
  read-only over a network share (NFS, SMB, S3-FUSE) or shipped
  via `scp`. Multiple consumers can run `wealthdb holdings positions` /
  `wealthdb status` against it concurrently, in read-only mode and
  outside the window in which the single writer's `wealthdb load`
  holds the file. See §4.10.
- **Dockerised everything.** Build, dev, and prod all run inside a
  single container image. The host OS stays clean.

### Non-goals (deliberately omitted)

- **A UI of its own.** The engine is a CLI and an MCP server. The
  optional dashboards are Metabase over a snapshot of gold
  ([web/](../../web/)).
- **Built-in market-data feeds.** Real-time prices, ex-dividend
  dates, ratings and volatility are out of scope. Gold reads what each
  silver carries. Reference data on funds and securities arrives
  through the `refdata` source, and public rate series through `fred`
  and `curves` (§7.6).
- **Performance attribution.** No command computes it. Gold carries
  the cost basis, open lots and realized lots each source states
  (§7.4), the lot engine fills what a source leaves out (§7.5), and
  `wealthdb gains` reads both ([GAINS.md](GAINS.md)).
- **Cross-silver instrument deduplication.** Two silvers may hold the
  "same" equity under different `instrument_external_id`s; gold keeps
  them separate at the row level. `instruments.isin` is the join key
  for queries that want a unified view. The exposure report's security
  keys (§10.14) are the one cross-source mapping.
- **Bitemporal corrections.** Same trade-off as silver — monotemporal
  on source time. When silver retracts a past event, gold loses the
  prior version too.
- **Backwards compatibility in the schema.** Gold migrates forward
  only. An old shape is recovered by restoring the database from backup.
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
wealthdb reload  <id>... | -a         (RW)    Reset then load; -a builds a fresh compact gold and swaps it in (--in-place: on the live file).
wealthdb compact [--dry-run]          (RW)    Rewrite the gold DB into a fresh file to reclaim dead space.
wealthdb lots rebuild [--force]       (RW)    Replay the trades into lots: the cost basis no source states (docs/LOTS.md).
wealthdb holdings <view> [flags]      (RO)    Point-in-time views: positions, accounts, portfolios, sources, global.
wealthdb returns <view> [flags]       (RO)    TWR & MWR/XIRR returns: accounts, portfolios, sources, global; and stated, a side view of a document's stated periods.
wealthdb gains   <view> [flags]       (RO)    Realized and unrealized gains: summary, sources, portfolios, accounts, positions, realized, lots, coverage, check.
wealthdb exposure <view> [flags]      (RO)    What is really owned, every fund looked through: classes, sectors, sizes, countries, currencies, credit, maturities, coupons, cycles, situs, securities, issuers, funds, coverage.
wealthdb yield   <view> [flags]       (RO)    What the holdings earn and how they move with rates: global, sources, portfolios, accounts, positions, securities, funds, cash, curve, coverage.
wealthdb transactions [FROM [TO]] [flags] (RO) Print transactions over a date range.
wealthdb spending <view> [flags]      (RO)    Spending reports: summary, categories, transactions.
wealthdb income   <view> [flags]      (RO)    Income received and expected: summary, types, transactions, projection, calendar, sources, portfolios, accounts, positions.
wealthdb cashflow <view> [flags]      (RO)    Cash flow statement: summary, flows, sankey, transactions, coverage; projection for the months ahead.
wealthdb status  [<id>] [-v]          (RO)    Report gold state vs each silver source.
wealthdb snapshots <id> | -a [--latest] (RO)  List snapshots gold has loaded (one silver, or all).
wealthdb resolve-symbols              (RW)    Back-fill missing instrument ticker symbols via the configured LLM; skips hinted rows and cash-class instruments.
wealthdb resolutions [-s <id>]        (RO)    Dump the symbol_resolutions table (LLM + manual-override tickers), optionally for one source.
wealthdb categorize [spending|income] (RW)    Categorise the unplaced merchants and payers via the configured LLM.
wealthdb categorizations [spending|income] (RO) Dump the model-derived verdict stores; --forget SIG removes one (RW).
wealthdb mcp-serve --stdio | --http ADDR (RO) Serve the read-only reports to AI agents over MCP (mcp/DESIGN.md).
wealthdb version                      (RO)    Print the version: a release tag, or `<last release> nightly <commit>`.
wealthdb help [<subcommand>]
```

`-a` means "all configured silver sources". `<id>` is the
`silver_source_id` the config file defines.

Every subcommand accepts its flags before or after its positionals:
`transactions 2025 -f csv` reads like `transactions -f csv 2025`.
`--` ends the flags, and a lone `-` (an open window end) is a
positional. `wealthdb help <subcommand>` prints a subcommand's full
flag list.

`(RW)` subcommands require a writeable gold database; `(RO)`
subcommands work on a read-only one. See §4.10 for how read-only
mode is detected, declared, and reported. `(setup)` denotes a
subcommand that writes the config file, not the gold DB; it
needs write permission on `--config`'s path but doesn't open the
gold DB at all.

### 4.2 Top-level flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-c`, `--config` | `${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg` | Path to config file. Tilde (`~`) and `$HOME` are expanded. |
| `-r`, `--read-only` | off | Force read-only access even when the gold DB is writeable, as a safety net for exploration. Without this flag, mode is auto-detected from filesystem permissions (§4.10). |

These come before the subcommand. A subcommand's own `-r` (on
`transactions` and `gains realized`) means "newest first".

### 4.3 `wealthdb init`

Creates the gold DuckDB file at the path given by the `gold_db`
field in the config file, runs all migrations from `migrations/`,
and exits. Fails with exit code 4 if the file already exists. To
start over, delete the file, or rebuild it from silver with
`wealthdb reload -a` (§9).

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
source-owned gold table, including `silver_sources` and `load_audit`;
the spending overlay's merchant store and account-scope stamp are not
source data (§9). The silver database itself is untouched.

Use case: a silver was rebuilt from bronze (re-parse, new migration,
data correction) and gold needs to be re-synced from scratch.

### 4.6 `wealthdb holdings positions`

Prints the consolidated portfolio as of a date.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-d`, `--as-of` | today (UTC) | Date in `YYYY-MM-DD` to query as-of. |
| `-f`, `--format` | `table` | One of `table`, `csv`, `csv_plain`, `json`. |
| `-C`, `--columns` | `default` | Comma-separated column names, `default`, `all`, or a `+ADD,-REMOVE` delta against the default set. |
| `-x`, `--currency` | value of `default_currency` in the config file | ISO 4217 output currency for value columns (e.g. `USD`, `CHF`). The short form `-x` is mnemonic for "(currency) exchange"; `-c` stays reserved for the top-level `--config` flag (§4.2). |
| `--with-cash` | off | Also emit one synthetic row per account+currency with non-zero cash (`asset_class = 'cash'`). |
| `--missing-basis` | `lots.missing_basis`, else `ignore` | `ignore` \| `zero`: how a missing cost basis counts ([GAINS.md §8](GAINS.md)). Under `zero` the cost basis is the part the lots know. |
| `-p`, `--privacy` | off | Redact identifying and monetary columns (see below). |

**Privacy classes.** `-p` is per-column, and every read-only view
that offers the flag draws from the same five classes (everything
else is left alone):

| Class | Renders as | Carries |
| --- | --- | --- |
| account-id | `****1234` — length preserved, a short tail and an IBAN country code left legible so rows stay distinguishable | account / portfolio / relationship / transaction ids. Applied only to identifier-shaped values: alphanumeric with a digit. A purely-alphabetic label (a bank-assigned `Education` / `Authorized`) and any value with a space or a paren are taxonomy or display text and pass through. |
| free-text | `***` — the whole cell, in every format | values that can carry a person's name, never look like an identifier, and were not typed by the household: statement narratives and their folds (`counterparty`, `description`, `merchant_signature` and the `merchant` built from it in §4.12). Also the instrument id of a holding no public key names, unless a person typed it. Shape-blind by design — the narratives worth hiding are multi-word, so a shape test would pass exactly them. |
| pseudonym | `manual-1` — the same value reads the same in every row and column of a run, in every format | the names a person typed (below). Shape-blind like free-text. |
| quantity | `***` (table) / empty (csv) / key dropped (json) | share counts. |
| money | `*****.**` (table) / empty (csv) / key dropped (json) | every monetary amount. |

**Typed names.** Every name a person typed is hidden. It prints as a
pseudonym, or as a redacted id where an account column shows the
account's identifier-shaped id in its place (below). A pseudonym is a
prefix and a number. The prefix is the kind of the source the value
comes from, such as `manual` or `cointracking`, and `declared` for a
declared account (§5.1). A category prints as `category-1` and a
nickname as `nickname-1`. The numbers count values in the order they
first appear in one CLI run or one MCP call, and each run counts
afresh. So a source's value reads the same in every row and column of a
run, and rows stay apart. The same text of two sources reads as two
pseudonyms; a category or a nickname reads as one. Public names stay as
they are: a bank's product label, a security's name. A merchant or
payer taken off a statement keeps `***`. The `silver_source` id names a
config entry and stays legible.

These names are typed:
- an account's name, when its source is a `cointracking` or a `manual`
  silver, when the account is a declared one, when its bank names an
  account by its roster's nickname (`chase`, `firstcitizens`,
  `raiffeisen_at`), or when the name equals the account's nickname;
- the name and id of a `cointracking` or `manual` portfolio;
- an `angellist` account's id, its holder's portal slug, whatever its shape;
- a nickname;
- a manual holding's name, position key and instrument id, and an
  issuer that takes the holding's name;
- a transaction or lot id of a `cointracking`, `manual` or declared
  source, such as a manual exit's `exit:<position key>`;
- an account category that `account_overrides` sets in the config.

How they print:
- An account column shows a typed account's id instead of its name. An
  identifier-shaped id redacts as an account id. Any other id of a
  typed source can hold typed words, such as a tracker's
  `<user>:<wallet>`, so it prints as a pseudonym.
- A typed name with no id beside it (cashflow coverage) prints as a
  pseudonym.
- A manual holding's name prints as the pseudonym of its key, so the
  holding reads as one pseudonym. A cash flow line carries no key, so
  its name is a pseudonym of its own text.
- Exposure and yield key a holding with no public key
  `holding:<source>:<id>`. Such a key of a manual holding prints as the
  holding's pseudonym. Any other masks whole.
- The `(no portfolio)` sentinel is a marker, not a name, and stays.
- `-p` cannot tell a display name built from the holder's words from
  the bank's, so it prints display names as they are. An adapter that
  cannot tell them apart makes the name the nickname, which `-p` masks.
  So a Plaid account's own name is its nickname, and its display name
  is its institution and mask.

Ratios and dates stay legible throughout — a return or a spending
share is not an amount. Independently of the per-column pass, every
cell also runs through a content scrub that masks structured bank
identifiers wherever they appear, including inside columns that are
not redacted at all (a mortgage account number surfacing as an
instrument key).

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

The cost basis columns are opt-in through `-C`: `cost_basis`,
`unrealized_gain`, `unrealized_pct`, `basis_stamp`,
`quantity_without_basis`, `acquisition_date`, `accrued_interest`,
`clean_value`, and the converted `cost_basis_outccy` and
`unrealized_gain_outccy` (headers `cost_basis_<CCY>` and
`unrealized_gain_<CCY>`). docs/GAINS.md defines them.

Internally: for each account, take its active snapshot at `--as-of`
(§10.1): `pos_active_at` on `positions`, `cash_active_at` on
`cash_balances`, then union those rows. FX conversion is a join
against `fx_rates` per §10.6.

### 4.7 `wealthdb status [<id>]`

Without `<id>`: one line per silver source from the config file,
each with `silver_source_id`, `kind`, `path`, gold-side oldest/
newest `snapshot_at`, position count at the latest snapshot,
transaction count, the date its holdings were last observed (§6.4),
stored `high_watermark`, and silver-side
`LatestChangeNumber` (from `plugin.Status()`). A `*` flag marks
sources where silver has advanced past the watermark — load
candidates.

With `<id>`: detailed report — per-table counts in gold for that
silver source, full `plugin.Status()` (oldest/latest snapshot and
transaction timestamps in silver, latest change number, when the
holdings and the ledger were last observed), the stored watermark,
and the last few rows from `load_audit`.

### 4.8 `wealthdb snapshots <id> | -a`

Lists every distinct `snapshot_at` gold has data for — derived
from `SELECT DISTINCT snapshot_at FROM positions UNION ... FROM
cash_balances WHERE silver_source_id = ?`. Oldest first. It
answers why an expected snapshot is missing.

With `<id>`: snapshots for the one named silver source. With
`-a`: snapshots for every silver source gold has loaded, grouped by
source.
`--latest` prints only the newest snapshot per source.

### 4.9 `wealthdb config [-c <cfg-file-path>]`

Interactive first-time setup wizard. Writes the config file at
`-c` (default `${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg`) by walking
through:

1. **Gold DB path** — default `$XDG_DATA_HOME/wealthdb/wealthdb.db`
   (`~/.local/share/wealthdb/wealthdb.db` without the variable). The wizard
   confirms the parent directory exists but does **not** create
   the gold DB itself — that's `wealthdb init`'s job.
2. **Default output currency** — default `USD`. ISO 4217 only;
   validated for ISO 4217 shape (three uppercase ASCII letters).
3. **First silver source** — `id` (slug matching `^[A-Za-z0-9_-]+$`),
   `kind` (any registered adapter kind), and `path` (no
   default). The wizard opens the silver DB read-only and verifies it
   parses as SQLite and has a `dump_runs` table (a load-only silver's
   `load_runs` also passes). It does not check the kind against the
   file. A DuckDB silver (cointracking) fails the SQLite probe and is
   added to the JSON by hand.
4. **Add another silver source?** — repeats step 3 until
   declined.

On success the wizard writes the JSON config and prints the next
steps: `wealthdb init`, then `wealthdb load -a`. An invalid answer
(a missing parent directory, a bad silver path, an id collision) is
rejected and the question asked again. The wizard never writes a
partial or invalid config file.

The wizard is the **only** subcommand that prompts interactively;
every other subcommand consumes the file produced here.

#### Refusal to clobber

If the target config file already exists and is not empty,
`wealthdb config` exits with code 4 (the code `init` uses for an
existing gold DB) and does not overwrite it. An empty file counts as
absent: the host wrapper creates one so it can bind-mount the path
before any config exists (§12.2). To re-run the wizard, delete or
move the existing file, or pass another `-c` path. To add a silver to
an existing config, edit the JSON directly.

#### Non-interactive fallback

When stdin is not a terminal, `wealthdb config` refuses to run and
exits 2. The message says to re-run with `docker run -it` or to write
the JSON by hand per §5. Reading a pipe would silently produce an
empty config.

### 4.10 Read-only mode

`wealthdb` supports two access modes against the gold database:

- **Read-write** — every subcommand §4.1 marks `(RW)`, plus all the
  read commands.
- **Read-only** — the ones §4.1 marks `(RO)`. Two are conditional and
  the table says so: `categorizations` dumps read-only but writes
  under `--forget`, and `categorize` writes unless `--dry-run`, which
  plans against the enrichment as of the last load and writes nothing.

  Neither list is repeated here. §4.1 marks every subcommand and is
  the one place the two sets are written down; a copy would drift the
  first time a subcommand was added.
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

The detected mode drives subcommand gating.

#### Opening DuckDB

In read-only mode, `wealthdb` opens the gold DB with DuckDB's
`access_mode='read_only'` option. This is a hard guarantee at the
driver level — even a buggy subcommand cannot write. Read-only
opens are also the only concurrent ones: several of them attach
the same file at once. They are not what a read subcommand gets
by default, though — `openGoldForRead` picks read-write whenever
the filesystem permits, so that concurrency needs `-r` /
`--read-only` or a path this process cannot write.

In read-write mode, `wealthdb` opens with the default access mode.
DuckDB takes an exclusive lock on the file for as long as the
handle is open, and refuses to attach a file that any other handle
— read-write or read-only — already holds. A single live reader is
therefore enough to fail a read-write open.

An open that meets another process's handle gets DuckDB's
conflicting-lock IO error from `db.Ping()`; DuckDB itself neither
blocks nor queues. Every command's open retries that error on a short
ladder (0, 1, 3 and 5 seconds), then fails and the dispatcher exits 5
(`ExitOpenFailed`). The wait is for a reader: one holds its handle for
one report, seconds at most. A writer outlasts the ladder. One read-write handle OR
several read-only handles, never both — which is why `wealthdb web`
serves Metabase a snapshot copy instead of the live file
(web/DESIGN.md §2), while the MCP server opens live gold read-only
for each call and holds nothing between calls (mcp/DESIGN.md §2).

#### The gold write mutex

DuckDB's lock covers only the window a handle is open, and two
write commands hold no handle across their work: `compact` and
`reload -a` read the outgoing file, build a replacement, and
rename it over the live path. A verdict or a load written into the
outgoing file inside that window lands in the inode the rename
unlinks, and both commands report success.

So every write command (`load`, `reset`, `reload`, `compact`,
`lots`, `categorize`, `categorizations --forget`, `resolve-symbols`,
`web-materialize`) takes an
advisory `flock` for the whole command on a sidecar file,
`<gold_db>.wealthdb.lock`. The sidecar is created on first use and
left in place afterwards: it is an expected artefact beside the
gold DB, it carries no state, and a backup or a copy can ignore
it. The lock is advisory rather than an `O_EXCL` lockfile so that
the kernel releases it when a process dies; a lockfile would
survive a `SIGKILL` and block every later write with no recovery
path. It is taken non-blocking: a write command that finds it held
exits immediately with `ExitOpenFailed` (5) naming the sidecar,
rather than queueing behind a model run that may take an hour.

Read-only opens never take the mutex. Concurrent readers are what
the read-only share contract exists for, and a reader cannot lose
a write.

`categorize` and `resolve-symbols` hold the mutex end to end but
release the DuckDB handle across their model calls, so readers are
not shut out for the length of a run. Both send their candidates
in batches and store each batch's answers as it completes. Re-taking
the handle to store them can lose the race against a reader
that got in meanwhile, so that store is retried on a short bounded
backoff, and a final failure prints the unstored answers in the
`--dry-run` plan format rather than discarding what was paid for.

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
| RW subcommand on read-only-detected DB | 2 | `wealthdb: '<cmd>' requires write access to the gold database, but '<path>' is read-only (detected: <reason>).` |
| RW subcommand with `-r` / `--read-only` flag set | 2 | `wealthdb: '<cmd>' requires write access, but -r/--read-only was specified. Drop the flag or run a different subcommand.` |
| Usage error: an unknown subcommand, a bad flag or argument | 2 | the `flag` package's own message; `wealthdb: unknown subcommand "<cmd>"` with the usage; or the subcommand's own `wealthdb: <cmd>: <reason>` |
| RO subcommand on non-existent DB | 3 | `wealthdb: gold database '<path>' does not exist. Run 'wealthdb init' first (requires write access).` |
| `init` on existing DB | 4 | `wealthdb: gold database '<path>' already exists. Use 'wealthdb reset -a' to clear data, or delete the file manually if you really want a fresh DB.` |
| DuckDB open fails mid-run | 5 | the driver's own error, passed through whole and prefixed by the stage that hit it: `wealthdb: ping duckdb "<path>": <driver error>` (or `open duckdb` / `migrate gold`) |
| Write subcommand while another holds the write mutex | 5 | `wealthdb: '<cmd>' cannot write '<path>': another wealthdb write command (load / reset / reload / compact / lots / categorize / categorizations --forget / resolve-symbols / web-materialize) is running and holds '<path>.wealthdb.lock'. Wait for it to finish and re-run.` |
| Silver DB unreadable during load | 6 (reserved) | not emitted: a silver open failure returns a plain error and exits 1 |

`<reason>` in row 1 is one of: `parent directory not writeable`,
`file mode lacks owner write bit`, `mounted read-only`, etc. The
DuckDB-open row synthesises nothing — no cause of its own and no hint:
the driver's message is the whole diagnosis, and the case worth
branching on is the lock conflict
(`IO Error: Could not set lock on file "<path>": Conflicting lock
is held in …`), which means another `wealthdb` process holds the
file and the run is a retry once that one finishes (§13.6).
Corruption and a stale WAL surface the same way, each as its own
driver error.

Codes 3–6 are distinct, so callers (shell scripts, CI) can
discriminate. Code 2 covers both a usage error and a write refused on
a read-only gold. Code 6 is allocated but unused. Nothing returns it,
and a silver open failure exits 1 with the plain error
(`errs.ExitSilverIO` carries the same note). Exit code 1 is for
unexpected runtime errors, such as panics, and for the failures no row
above claims.

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

### 4.11 `wealthdb categorize [spending|income]` / `wealthdb categorizations [spending|income]`

Both take an optional family positional. Without one, both families
run in order (spending, then income), as two plans and two summaries
against one model. A name runs that family alone. `--forget SIG` with
no family removes the signature from both verdict stores, because one
counterparty can sit in each with a different verdict.

`categorize` is the model tier of both families. The deterministic
tiers run on every `load`; what they leave behind is a set of
counterparty signatures whose category follows from nothing but the
counterparty's name, and this is what asks a model about them. Work is
priced PER SIGNATURE and the verdicts land in a global store —
`spend_merchant_categories` for spending, `income_payer_categories` for
income — so a merchant met by several cards, or a payer paying into
several accounts, is asked about once and answered once.

A normal run re-asserts every deterministic verdict first (the pass
`load` runs), then asks about what is left. `--dry-run` opens gold
read-only and so cannot run that pass; its plan is computed against the
enrichment as of the last load and says so. A dry run still asks the
model, batch by batch, and writes nothing.

[SPENDING.md §5 and §6](SPENDING.md) own the rest: the fence and the
context level that decide what leaves the machine, batches (`--batch`,
default 40), anchors (`--max-anchors`), retries (`--max-attempts`),
`--all` and `--refine`, and the per-run counters and canaries.
[INCOME.md §7](INCOME.md) gives the one income difference: an absent
`income.categorization` block inherits `spending.categorization` whole.

`categorizations` dumps the family's store — the `resolutions`
counterpart for the enrichment overlays. It takes no source filter:
each table is keyed by signature alone, with no silver source, because
a merchant is the same merchant whichever card met it and a payer the
same payer whichever account it paid.

`categorizations --forget SIGNATURE` (repeatable) is the store's one
undo: the model is sometimes wrong at counterparty scope, and nothing else
can remove a stored verdict short of editing gold by hand. It deletes
the rows with exactly those signatures in one transaction, prints each
removal (signature, name, category) and each signature it did not
find, and exits non-zero only on an error — a miss is a report, not a
failure. It follows the write gate of `categorize` and
`resolve-symbols`: write access is required, the read-only refusal
names `--dry-run`, and the dry run opens gold read-only and prints what
would be removed. The next `categorize` re-asks a forgotten merchant,
because the backlog excludes stored signatures. It also interacts with
the signature-version re-key (docs/SPENDING.md §4): the enrichment
pass carries a verdict onto a new key only when every row that carried
the old key moved to the same new one, and leaves a verdict whose rows
split behind, reported on that family's summary; forgetting an
artefact's key *before* a bump is the clean way to stop it carrying
anywhere.

### 4.12 `wealthdb spending <view>`

The read surface over the spending population (docs/SPENDING.md, and
§10.10 for the macros underneath). The grain is the positional
`<view>` — `summary`, `categories`, or `transactions` — exactly as the
returns family puts its grain in a positional and everything else in a
flag.

| Flag | Default | Meaning |
| --- | --- | --- |
| `[FROM [TO]]` | trailing twelve months | Positional window, same grammar as `transactions` (`YYYY` / `YYYY-MM` / `YYYY-MM-DD`, an explicit pair, `-` for an open bound). |
| `--period` | `monthly` | `daily` \| `weekly` \| `monthly` \| `quarterly` \| `annual` \| `total`. Maps onto the `date_trunc` part `report_spending_*` buckets by; `total` collapses the window into one bucket. |
| `--level` | `primary` | `primary` \| `detailed` — the category vocabulary `categories` groups by. The other views ignore it. |
| `-f`, `--format` | `table` | `table` \| `csv` \| `csv_plain` \| `json`. |
| `-C`, `--columns` | `default` | Per-view column set: names, `default`, `all`, or a `+ADD,-REMOVE` delta. |
| `-x`, `--currency` | `default_currency` | ISO 4217 output currency; historic FX at each line's `occurred_at`. |
| `-p`, `--privacy` | off | Redact account ids, narratives and amounts. |

The window defaults to the trailing twelve months rather than inception,
and there are no row-filter flags; docs/SPENDING.md §7 argues both.

**Privacy classes** (§4.6): `merchant`, `counterparty`, `description`
and `merchant_signature` take the free-text class. The account's id and
display name take the account-id class. A typed name, such as the
account nickname, prints as a pseudonym. Amounts take the money class.
Spending category, provenance and `share_pct` stay legible.
[SPENDING.md §7](SPENDING.md), *What `-p` redacts*, gives the reasons.

Exit codes follow §4.10's table: `2` for a usage error (unknown view,
invalid `--period` / `--level` / `-x`, unknown column), `3` when the
gold DB does not exist. `spending` is `(RO)` and never needs write
access.

### 4.13 `wealthdb income <view>`

The read surface over the income population (docs/INCOME.md, and §10.11
for the macros underneath) — §4.12 read in the other direction. The
views of what was received take §4.12's grain, flags and window
default, with three differences, each a decision recorded in
docs/INCOME.md §10. The forward views add what the holdings are
expected to pay over a window that may reach into the future
(docs/INCOME.md §12, and §10.15 for the code).

| Flag | Default | Meaning |
| --- | --- | --- |
| `<view>` | required | `summary` \| `types` \| `transactions`, what was received; `projection` \| `calendar` \| `sources` \| `portfolios` \| `accounts` \| `positions`, the forward views. No `payers` view: payers rank on the dashboard only, as merchants do. |
| `[FROM [TO]]` | trailing twelve months; on the forward views, `yield.horizon_months` after the current month | As §4.12. A positional year is the idiom for the calendar-year report: `wealthdb income types 2025 --period annual`. The forward views' window: docs/INCOME.md §12. |
| `--period` | `monthly` | As §4.12. Of the forward views only `projection` takes it: `monthly` \| `quarterly` \| `annual`. |
| `--level` | **`detailed`** | The one default that differs from spending's. The income taxonomy has ONE vendored primary, so the primary level folds every vendored type and extension into `INCOME` beside the deltas. Refused on the forward views. |
| `--rates` | `constant` | `constant` \| `forward`: how the floating rates move. Forward views only. |
| `--maturing` | `cash` | `cash` \| `idle` \| `roll`: what a maturing principal earns. Forward views only. |
| `--shift` | `0` | A parallel shift of the floating rates, in whole basis points. Forward views only. |
| `-C/--columns` | `default` | `withheld` is available on `summary` and off by default: tax deducted at source, shown beside the income it was taken from and never subtracted from `net_income`. On the forward views `withheld` is the withholding table's model, a default column of `projection`. |
| `-f`, `-x`, `-p` | as §4.12 | `-p` masks `payer`, `payer_signature`, `counterparty` and `description` whole; types, provenance and `share` stay legible. On the forward views it stars amounts, quantities, prices and account ids. A name a person typed prints as a pseudonym (§4.6). |

The views of what was received refuse `--rates`, `--maturing` and
`--shift`. The MCP `income` tool takes the same window and flags
(docs/INCOME.md §12).

`income` and `reversals` are positive magnitudes and `net_income` is
the difference, mirroring `spend` / `refunds` / `net_spend`; the
`transactions` view keeps canonical signs.

`wealthdb transactions` carries `payer`, `income_primary` and
`income_detailed` behind `-C`, beside the spending trio. A row can hold
both — a deposit the matcher paired is `internal_transfer` in each
overlay — and that view is the one surface showing a transaction from
both sides at once.

### 4.14 `wealthdb cashflow <view>`

The household's cash flow statement over both populations plus the
movements neither family books (docs/CASHFLOW.md for the design, and
the `report_cashflow_*` macros for the surface underneath). Same window
default, same flags, same privacy classes; six views instead of
three, one flag of its own, and three refusals on the statement's
views — the third being `coverage -x`, each account being reported in
its own currency. The sixth view, `projection`, projects the months
ahead (docs/CASHFLOW.md §12): it takes the income projection's window
and assumptions instead.

| Flag | Default | Meaning |
| --- | --- | --- |
| `<view>` | required | `summary` \| `flows` \| `sankey` \| `transactions` \| `coverage` \| `projection`. |
| `[FROM [TO]]` | trailing twelve months | As §4.12. A household reads this per year: `wealthdb cashflow sankey 2025`. On `projection`: the horizon after the current month; whole months from next month on, anything else refused. |
| `--period` | `monthly` | As §4.12, and **refused** on `sankey`: a period on an edge list would mean one list per bucket, which is a loop's job. `transactions` ignores it, as the families' do. On `projection`: `monthly` \| `quarterly` \| `annual`; `total` is **refused**. |
| `--level` | `group` | `section` \| `class` \| `group` — the grain a node is netted at. `section` is **refused** on `sankey`, which would then have no inner column. **Refused** on `projection`. |
| `--investing` | `whole` | `whole` \| `class` — net the investing section as one `Investments` node, or per asset class. Honoured by `flows` and `sankey`; the summary is always whole. **Refused** on `projection`. |
| `--rates`, `--maturing`, `--shift` | `constant`, `cash`, 0 | `projection` only, as on `wealthdb income`'s forward views (§4.13); **refused** on the other views. |
| `-C/--columns` | `default` | `summary` carries `yield`, `taxes`, `fees`, `giving`, `savings_rate` and the reconciliation memo (`cash_measured`, `fx_effect`, `cash_flow_measured`, `unexplained`) off by default; `flows` and `sankey` carry the dotted node keys; `projection` carries the operating-out classes `consumption`, `fees`, `taxes`, `giving` and `uncategorized`. |
| `-f`, `-x`, `-p` | as §4.12 | `-p` masks account ids, amounts and the `name` column; sections, classes, groups and shares stay legible, which is what makes the diagram's privacy twin normalisation alone. A name a person typed prints as a pseudonym (§4.6). |

The MCP `cashflow` tool takes the same views and flags. On
`projection` its period defaults to `monthly` and refuses `total`. Its
window there is the projection's (docs/CASHFLOW.md §12).

Positive is cash arriving in the pool and negative is cash leaving it.
The four sections sum to `net_cash_flow`, and a bucket's `flows` rows —
the Cash row included — sum to zero; both identities are structural.

`wealthdb transactions` carries `cashflow_section`, `cashflow_class`
and `cashflow_group` behind `-C`, beside the other two trios.

### 4.15 `wealthdb gains <view>`

What was gained or lost on what is held, realized and unrealized, over
a window (docs/GAINS.md for the figures, §10.13 for the macros). The
families' idiom: a positional view, a positional window with the
trailing twelve months as its default, `--period` on the aggregate
views, and the shared `-f`, `-C`, `-x`, `-p`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `<view>` | required | `summary` \| `sources` \| `portfolios` \| `accounts` (the holdings grains, which reconcile), `positions`, `realized`, `lots`, `coverage`, `check` (the lot engine against the stated lots, docs/LOTS.md §9). |
| `[FROM [TO]]` | trailing twelve months | As §4.12. A positional year reads a calendar year: `wealthdb gains realized 2025`. `lots` reads the window's end. |
| `--period` | `monthly` | As §4.12, on the four aggregate views; the others read the whole window. |
| `--documents` | `primary` | `primary` \| `all`, on `realized` only: `all` lists every document's copy of a sale. Refused on the other views. |
| `-r`, `--reverse` | off | `realized` only: newest first. Refused on the other views. |
| `--missing-basis` | `lots.missing_basis`, else `ignore` | `ignore` \| `zero`: how a missing cost basis counts ([GAINS.md §8](GAINS.md)). Refused on `coverage` and `check`, which count a missing cost rather than read it. |
| `-f`, `-C`, `-x`, `-p` | as §4.12 | `-p` masks account ids, quantities and amounts; percentages, basis stamps, dates and public security names stay legible. A name a person typed prints as a pseudonym (§4.6). |

Every aggregate and positions row carries a `quality` column naming
each way its figures can be incomplete, and `coverage` gives a verdict
per account.

### 4.16 `wealthdb lots rebuild [--force]`

Runs the lot engine's pass (docs/LOTS.md): it replays every source
whose lot policy is not off and rewrites the ledger, the rebuilt cost
basis on positions and the engine's realized lots in one transaction.
`load` and `reload` run the same pass after the enrichment pass, and it
replays whenever the gold rows it reads, the config or the engine
changed; this verb applies such a change without a load. The pass
replays nothing when no input changed since the last one; `--force`
replays anyway. It prints one line per source: mode,
methods, lots, realized lots, positions filled, seeds, implied
disposals, blips and anchors.

### 4.17 `wealthdb exposure <view>`

What is really owned, as of a day: every fund looked through to what
it holds, added to what is held directly (docs/EXPOSURE.md for the
figures, §10.14 for the macros). `situs` gives where each holding
legally sits, each fund whole (EXPOSURE.md §13). The holdings idiom: a
positional view, `-d` for the day, and the shared `-f`, `-C`, `-x`,
`-p`. `--period` turns a view into a series over a window.

| Flag | Default | Meaning |
| --- | --- | --- |
| `<view>` | required | `classes` \| `sectors` \| `sizes` \| `countries` \| `currencies` \| `credit` \| `maturities` \| `coupons` \| `cycles` \| `situs` \| `securities` \| `issuers` \| `funds` \| `coverage`. |
| `-d`, `--as-of` | today | The day. |
| `--country` | `risk` | `risk` \| `domicile`: the country a security counts in, on `countries` and `securities`. Refused on the other views. |
| `--period` | none | `monthly` \| `quarterly` \| `annual`: the view at the end of every period of the window `[FROM [TO]]`, a `period` column first (EXPOSURE.md §7). Refused on `securities`, `issuers` and `funds`, and with `-d`. |
| `-f`, `-C`, `-x`, `-p` | as §4.12 | `-C` names a share column without its `_pct` suffix. `-p` hides every amount and every instrument id (EXPOSURE.md §7). A name a person typed prints as a pseudonym (§4.6). On `coverage`, `-f ndjson` prints the declarations ledger's skeleton records still to fill in (EXPOSURE.md §9); it takes no `-C`, `-p` or `--period`. |

Every view sums to `holdings global`, and every row carries a
`quality` column naming what its figure rests on.

### 4.18 `wealthdb yield <view>`

What the holdings earn, and how their value moves with interest rates,
as of a day (docs/YIELD.md for the figures, §10.15 for the code). The
exposure idiom: a positional view, `-d` for the day, `--period` for a
series, and the shared `-f`, `-C`, `-x`, `-p`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `<view>` | required | `global` \| `sources` \| `portfolios` \| `accounts` (the aggregate views, which reconcile), `positions`, `securities`, `funds`, `cash`, `curve`, `coverage`. |
| `-d`, `--as-of` | today | The day. |
| `--converted` | off | The aggregate views at one row per entity, every currency together (YIELD.md §5). Refused on the other views. |
| `--period` | none | `monthly` \| `quarterly` \| `annual`: the aggregate views and `curve` at the end of every period of the window `[FROM [TO]]`, a `period` column first. Refused on the other views, and with `-d`. |
| `-f`, `-C`, `-x`, `-p` | as §4.12 | `-C` names a rate column without its `_pct` suffix. `-p` hides every amount, a price, every account id, and a security or fund key that is a holding's own (YIELD.md §2). A name a person typed prints as a pseudonym (§4.6). On `coverage`, `-f ndjson` prints the ledger's yield records still to fill in (YIELD.md §10); it takes no `-C`, `-p` or `--period`. |

An aggregate row is one entity in one currency, and the total row
weighs the currencies by value. Every row but `curve`'s and
`coverage`'s carries a `quality` column naming what its figures rest
on.

## 5. Configuration

JSON file. Path defaults to `wealthdb.cfg` under the XDG config dir
(`$XDG_CONFIG_HOME`, falling back to `~/.config`) and is
overridable with `-c` / `--config`. The `.cfg` extension is a
convention; the content is JSON, and an unknown field fails the
load. Every path value in the file is expanded the same way:

1. a leading `~` or `$HOME` becomes the home directory;
2. `$VAR` and `${VAR}` become the variable's value, and an unset
   variable fails the load rather than expanding to nothing;
3. a path still relative resolves against the config file's
   directory.

Conventional host layout, produced by `wealthdb config`'s defaults.
The host wrapper mounts the data root at `$WEALTHDB_DATA_ROOT` when
that is set (§12.2), so a config can write
`${WEALTHDB_DATA_ROOT}/...` paths:

```
$XDG_CONFIG_HOME/wealthdb.cfg          config file (this file)
$XDG_DATA_HOME/wealthdb/               all wealthdb data
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
            "<account-hash-2>": {"nickname": "ESA One",        "category": "esa"}
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
| `gold_db` | string | Filesystem path to the DuckDB file. Created by `wealthdb init`. Expanded as above. |
| `default_currency` | string | ISO 4217. The output currency of every command that values money, and of the MCP server, when `-x`/`--currency` is not given. When it is a reporting currency (USD, CHF, EUR or GBP, §10.8), the web dashboards open in it too. |
| `equity_transfers` | string | Optional. Filesystem path to a CSV ledger of equity transfers in/out of a tracked account that the collectors don't capture as valued flows. The loader injects each row as a canonical `transfer_in`/`transfer_out` transaction. Expanded as above; a missing file is a no-op. See §13.9. |
| `web` | object | Optional. Enables the dockerized Metabase BI server driven by `wealthdb web` (host-side). See [web/README.md](../../web/README.md). |
| `web.enabled` | bool | `true` to allow `wealthdb web start`. Absent block or `false` = the server is not configured. |
| `web.port` | integer | Host loopback port Metabase is published on (127.0.0.1 + [::1] → container 3000). Default 3000. |
| `mcp` | object | Optional. Enables the MCP server driven by `wealthdb mcp` (host-side). See [mcp/README.md](../../mcp/README.md). |
| `mcp.enabled` | bool | `true` to allow `wealthdb mcp start` and `wealthdb mcp stdio`. Absent block or `false` = the server is not configured. |
| `mcp.port` | integer | Host loopback port the server is published on (127.0.0.1 + [::1], same port in the container). Default 3300; must differ from the web port. |
| `mcp.auth` | string | `token` (default): every HTTP request carries the bearer token. `none`: no token, honoured only beside `mcp.insecure`. |
| `mcp.insecure` | bool | Acknowledges `mcp.auth: none`. Validation refuses `none` without it. |
| `silver_sources[]` | array | Registered silver databases. |
| `silver_sources[].id` | string | User-defined unique identifier. Used in CLI args. Must match `^[A-Za-z0-9_-]+$`. |
| `silver_sources[].kind` | string | Picks the adapter (e.g. `schwab`, `ubs`, `swissquote`, `fred`, …). It must name a registered kind (§5.2). The full set is the `silver_kind` whitelist enforced in gold (`internal/gold/migrations`) and mirrors the registered adapters under `internal/silver/`. |
| `silver_sources[].path` | string | Filesystem path to the silver database, expanded as above. Used by single-file adapters (Schwab, Swissquote, single-source UBS, fred). Mutually exclusive with `subsources`. |
| `silver_sources[].fx_priority` | integer | Optional. FX-rate precedence when several sources publish the same `(base, quote)` pair: lower = higher priority, absent/null = lowest. Ties broken by the order sources appear in this array. Affects only FX resolution (§10.6 / §13.2) — no effect on positions or transactions. |
| `silver_sources[].taxable_wrapper` | string | Optional. The taxable wrapper this source's taxable accounts actually sit in, validated against the canonical `tax_wrapper` enum at config-load time. A bank feed says what a product IS and never who holds it, so an adapter emits the generic `taxable_personal` for any taxable account; this states the deployment's answer once per source instead of once per account, so an account opened later is right on the load that first sees it. It rewrites `taxable_personal` and nothing else — a retirement, trust or custodial wrapper the adapter had positive evidence for is kept, and an account with no wrapper at all is left alone. A per-account `account_overrides.tax_wrapper` still wins. See §13.8. |
| `silver_sources[].subsources[]` | array | Optional. For adapters that merge several backing silvers under one logical source (UBS = `ubs-web` + `ubs-psn`). Each entry has its own `kind` and `path`. At least one entry required when present. |
| `silver_sources[].subsources[].kind` | string | Subsource discriminator. UBS recognises `ubs-web` and `ubs-psn`. |
| `silver_sources[].subsources[].path` | string | Filesystem path to that subsource's silver SQLite. Expanded like `path`. |
| `silver_sources[].relationships[]` | array | Optional. Pairs cross-subsource entity identities under a single label. UBS uses it to map web `banking_relationship_id` to PSN `SFTPCH0X`. |
| `silver_sources[].relationships[].label` | string | Required. Canonical user-readable name stamped on canonical records regardless of which subsource produced them. |
| `silver_sources[].relationships[].web_id` | string | Optional. Web silver's `banking_relationship_id` (opaque SPA token, or `account_number_prefix` fallback). |
| `silver_sources[].relationships[].psn_id` | string | Optional. PSN silver's `relationship_id` (SFTP server identifier like `SFTPCHxx`). At least one of `web_id` / `psn_id` must be set. |
| `silver_sources[].relationships[].psn_start_override` | string | Optional `YYYY-MM-DD`. Overrides the auto-detected web↔PSN transaction-splice cutover for this relationship. Defaults to `MIN(snapshot_at)` in PSN's data for the paired `psn_id`. |
| `silver_sources[].schwab` | object | Optional, on a `schwab` source only. The guard on the api feed's overnight marks (docs/adapters/schwab.md §4.1): `mark_spike_pct`, the move in percent past which a position's mark may be a spike (default 10), and `mark_revert_pct`, how close in percent one of the next two marks must come back to the previous mark for the move to read as a spike (default 2). Both above 0, `mark_revert_pct` below `mark_spike_pct`; validated at config-load time. |
| `account_overrides` | object | Optional. Nested map keyed by `silver_source_id` (outer) and `account_external_id` (inner) carrying per-account `nickname`, `category`, `account_kind`, `tax_wrapper`, and/or `management_style` strings, or `exclude: true` to drop the account and every fact keyed to it (a sweep over gold after the load's streams drain, §13.8). All inner fields are optional but at least one must be set per entry, and `exclude` may not be combined with a column override. `account_kind`, `tax_wrapper` and `management_style` values are validated against the canonical enums (`internal/canonical/enums.go`) at config-load time. `account_kind` restates what an account is where its source types it wrong. It takes any kind but three. `overlay` is gold's synthetic kind. `card` and `mortgage` are refused because an adapter signs a liability's balance by its own kind. For the same reasons an account its adapter stamped `card`, `mortgage` or `overlay` keeps that kind: the loader ignores the override and logs a line. `tax_wrapper_from` (YYYY-MM-DD, only beside `tax_wrapper`) dates the wrapper: from that day the account has it, and before it the wrapper it has without this override, which the load records in `account_wrapper_history`. The day is today or earlier. The cash flow boundary reads the wrapper as of each row's day ([CASHFLOW.md](CASHFLOW.md) §3). The loader applies overrides AFTER the adapter stamps its own values, so config wins on overlap. |
| `declared_accounts` | object | Optional. Map keyed by an id the holder chooses (`[A-Za-z0-9_-]+`) of accounts the household OWNS at institutions the product does not collect: `account_kind` and `tax_wrapper` required and validated against the canonical enums, `nickname` and `currency` optional. The load writes each one as an account row under the reserved source `declared` (no `silver_sources[].id` may take that name), after the per-source loads and before the enrichment pass, and prints how many it stamped; a declaration removed from the config leaves on the next load. A declared account carries no balance and no facts: it exists so a rule's `far` can name it as the other side of an own-account move, which the cash flow statement then places by the declared wrapper exactly as it places a collected far account — a household wrapper makes the move pool-internal, a vehicle wrapper draws it as that vehicle's crossing (docs/CASHFLOW.md §4). Nothing the product measures can falsify a declaration, which is why the wrapper is required and the load prints how many declarations sit inside the pool. Deployment-specific: lives in the config, never in the repository. |
| `exposure` | object | Optional. What the exposure report needs that no source knows: `proxies` (a fund whose composition stands in for another, with `always` and `leverage`), `reference_funds`, `size_funds`, `declarations` (the path to the declarations ledger, NDJSON; expanded as above), `custody` and `max_gap_days` (default 400). Validated at config-load time. docs/EXPOSURE.md §9 defines each key, its bounds and the ledger's records; §7.6 says how a load stores them. Deployment-specific: lives in the config, never in the repository. |
| `yield` | object | Optional. The yield inputs that no source knows: `curves` (a currency's public yield curve, one series of a configured source per tenor, `"3M": "fred:DGS3MO"`), `withholding` (percent withheld at source by situs country and tax wrapper, `default` for any wrapper, and `accounts` for an account whose rate is not its wrapper's), `rating_funds` (a rating-sliced fund's bucket or range, `BBB` or `AAA-A`), `conventions` (coupon frequency and day count per market: a currency, or a currency and a bond sector or `bill`; built-in defaults), `horizon_months` (1 to 36, default 12), `cycles` (overrides of the built-in map from the eleven sectors to cyclical, defensive and sensitive) and `settlement_cycles` (a market's settlement cycles, each in business days from a day on; a market named replaces its built-in cycles). Validated at config-load time; §7.6 says how a load stores it. Deployment-specific: lives in the config, never in the repository. |
| `holding_aliases` | array | Optional. Entries `{ "source", "account"?, "key", "as", "note"? }` naming a holding a source keys two ways, so the gains, exposure and yield reports read both keys as one: a statement's overview line (`key`) until a daily feed lists the same holding by ISIN (`as`), say. `account` scopes the alias to one account; absent, it covers every account of the source. The load writes the block into gold's `holding_aliases` table whole, and `gains_windows` maps every position, lot and event key through it before a bucket's start meets its end (docs/GAINS.md §5). Validated for a configured source, two different keys, one alias per key, and no alias of an alias. Deployment-specific: lives in the config, never in the repository. |
| `portfolio_overrides` | object | Optional. Portfolio-grain counterpart of `account_overrides`. Nested map keyed by `silver_source_id` (outer) and `portfolio_external_id` (inner); the override applies to every account whose `portfolio_external_id` matches — e.g. a whole crypto portfolio inside an IRA / trust / Stiftung wrapper. Accepts `tax_wrapper` (unlike `account_overrides`, which also takes nickname / category / account_kind / management_style) or `exclude: true`, which drops the portfolio and every account inside it with their facts; one of the two must be set and they may not be combined. A per-account `tax_wrapper` override still wins over a portfolio one. See §13.8. |
| `instrument_overrides` | object | Optional. Nested map keyed by `silver_source_id` (outer) and `instrument_external_id` (inner) pinning a per-instrument taxonomy pair. Each entry sets both `asset_class` (the exposure) and `vehicle` (the wrapper); both are required and validated as an admitted taxonomy pair (§7.2, docs/TAXONOMY.md) at config-load time. For holdings the adapter's structured signals and name heuristics misclassify — e.g. an exchange-traded commodity trust whose security name doesn't give away what it holds (`metal × etf`). The loader applies overrides AFTER the adapter classifies, to both the instrument dimension and every position row referencing it, so config wins on overlap. The class pass leaves a pinned instrument alone. See §13.8. |
| `transaction_instruments` | object | Optional. Nested map keyed by `silver_source_id` (outer) and the **lookup token** an adapter failed on (inner), naming the `instrument_external_id` that token means. Closes a row (a securities trade, or an income or withholding row of a security) whose feed states its instrument in a way nothing else in the product can resolve — a Swiss valor for a line the instrument dimension has no valor for, a fund renamed since the trade, a ticker of neither shape, a statement name that fits two holdings. The token is whatever the row's `instrument_hint` holds (§10.8); read the open set with `wealthdb transactions -C +instrument_hint`. Validated for a declared source and non-empty halves at config-load time; the instrument id itself cannot be checked without gold, as with the override families above. It states the IDENTITY only — what the instrument IS stays `instrument_overrides`' question. See §13.8. |
| `supersession` | object | Optional. Ends a source's account at a date because something else carries it from there. An account can outlive its source — a deposit account whose bank is acquired keeps running under the acquirer's collector, a holding moves custodian — and gold keys an account on `(silver_source_id, account_external_id)`, so the two are two accounts and BOTH count. One grain-keyed map (`accounts`), keyed by `silver_source_id` to `account_external_id` to `YYYY-MM-DD` (UTC), validated for a declared source and a parseable date at config-load time. The loader drops that account's positions, cash balances and transactions dated on or AFTER the date, and writes one zero row AT it for whatever the account still held — dropping alone is not enough, because gold carries a key forward until something supersedes it and a source that simply stops reporting supersedes nothing (§10.7). Cutting by date rather than dropping the account keeps the earlier half of a statement or dump that straddles the handover. The drop is of what the adapter read: an `equity_transfers` (§13.9) row dated on or after the date is refused by name and fails the load instead. This bounds a series at the END; `inception_overrides` below bounds the start. An entry matching nothing is a silent no-op. See §13.8. |
| `inception_overrides` | object | Optional. Pins the returns-window START date per source / portfolio / account so an entity's track record begins at its first real capital rather than a tiny pre-history dust base. Three grain-keyed maps (`sources`, `portfolios`, `accounts`), values `YYYY-MM-DD` (UTC). Consumed by the returns engine at query time — it stamps no gold column. See §5.4. |
| `returns_exclude` | object | Optional. Omits whole accounts or portfolios from HIGHER-grain return aggregates (`sources`, `global`) while still reporting them at their own grain — e.g. keep holdings tracked in a shared login that belong to another person out of the source/global returns. Two grain-keyed maps (`portfolios`, `accounts`), each keyed by `silver_source_id` to a list of external ids; a listed source id must name a declared silver source. Returns only — holdings / net-worth are unaffected. See §5.5. |
| `returns_policy_overrides` | object | Optional. Per-source adjustments to the registered ReturnsPolicy, keyed by `silver_source_id` (not adapter kind). `flow_regime` replaces the source's flow classification with a named regime's canonical kind sets (`"flow_complete"` \| `"crypto_partial"` \| `"nav_only"`); `accounts_grain` sets the per-account display mode (`"normal"` \| `"blanked"` \| `"hidden"`). Unset fields keep the registered policy's values. See §5.6. |
| `returns_hide` | object | Optional. Suppresses accounts' or portfolios' OWN return rows at every grain while their values and flows keep contributing to every aggregate — the display mirror of `returns_exclude`. Same grain-keyed shape (`portfolios`, `accounts` per `silver_source_id`). See §5.7. |
| `lots` | object | Optional. The lot engine (docs/LOTS.md §7): `method` (`fifo` default, `lifo`, `hifo`, `lofo`, `average`), the global method; `missing_basis` (`ignore` default, `zero`), the readers' default reading of a missing cost basis; `sources` keyed by `silver_source_id` with `method`, `mode` (`fill` \| `shadow` \| `stated` \| `off`) and `grain` (`account` \| `portfolio`) overriding the source kind's policy; `portfolios` and `accounts` keyed by `silver_source_id` to an external id to `{"method": …}`; `basis`, a list of `{ "source", "account"?, "instrument", "per_unit", "acquired"?, "note"? }`, the cost basis of an instrument's lots the engine opens without one (a seed, a receipt nothing pairs, a transfer in with no ledger cost), at quantity × `per_unit` with `acquired` as the acquisition date. The method in force for a key is its account's, else its portfolio's, else its source's, else the kind's own, else the global one. Every source id must name a configured source; a bad value fails the load. Absent ⇒ fifo and each kind's registered policy. |
| `returns_transfer_matching` | object | Optional, off by default. Enables the cross-source transfer matcher: an external leg whose counterparty leg exists in ANOTHER source (opposite sign, same native currency, equal amount within `tolerance_pct`, within `window_days`) nets out of every return aggregate containing BOTH legs, while finer grains keep counting each leg. Fields: `enabled` (bool), `window_days` (0–30, default 5), `tolerance_pct` (0–5, default 0.5). See §5.8. |
| `cashflow` | object | Optional. Groups the cash flow statement's knobs: the pool's exclusions, the wrapper boundaries, and the cash flow projection's look-back. It is deliberately small: cashflow adds no tier and buys nothing from a model, so the rules, pins and transfer overrides that decide what a row IS are the two families' — a verdict written there is what cashflow reads. See docs/CASHFLOW.md §7. |
| `cashflow.accounts` | object | Optional. The cash POOL, and the only account gate cashflow applies. **Exclusion only** — `{"exclude": {"<source>": ["<account-id>"]}}` — because every account is pooled by default, so an `include` could fence nothing and the loader refuses the unknown field rather than ignoring it. It inherits neither family's scope on purpose: an account a family drops is not thereby outside the household's cash. |
| `cashflow.wrappers` | object | Optional. Moves a tax wrapper across the household boundary, `{"<wrapper>": "<destination>"}`, where the destination is `household` (no crossing at all), `retirement`, `education`, `health`, `trusts` or `giving`. Per WRAPPER, not per account — an account a source mis-labelled is fixed with the per-account `tax_wrapper` override, so every consumer agrees whose money it is. An unknown wrapper or destination fails the load naming the entry. Re-stamped by every enrichment pass. |
| `cashflow.projection` | object | Optional. The cash flow projection's look-back: `lookback_months`, 1 to 36, default 12. See docs/CASHFLOW.md §7 and §12. |
| `income` | object | Optional. Groups the income feature's per-deployment knobs — `accounts`, `rules[]`, `pins`, `categorization` — in `spending`'s shapes. Two blocks spending has are deliberately absent: there is one internal-transfer matcher and one transfer-override ledger, and both families read them (docs/INCOME.md §1). Absent ⇒ every account counts, no rules and no pins apply, and the model tier inherits `spending.categorization`. |
| `income.accounts` | object | Optional. The income account scope, in `spending.accounts`' shape and stamped into gold's `income_account_scope`. Its own table on purpose: an account excluded from spending because its outflows double-count something is not thereby an account whose inflows are not income. |
| `income.rules[]` | array | Optional, default empty. As `spending.rules[]`, with the value field named **`type`** and validated against the INCOME vocabulary — a spending value here fails the load naming `income.rules[i].type`. So does a spelling the vocabulary retired, and the error names the value to write instead (docs/INCOME.md §2). Its optional `asset_class` is admitted only where `type` is `capital_return`, and its optional `far` only where `type` is `internal_transfer`; the far account of an inbound leg is written onto the spending overlay, the one overlay with far columns (docs/CASHFLOW.md §4). |
| `income.pins` | string | Optional. Path to the income pins ledger: the spending ledger's format with an `income_detailed` column. See §13.10. |
| `income.categorization` | object | Optional. As `spending.categorization`, `fence_person_names` included. **Absent ⇒ inherits `spending.categorization` whole** — one household, one local model, one answer to what may leave the machine. Whole-block rather than per-field: a half-inherited endpoint is a configuration nobody wrote down, and a half-inherited fence would be one that quietly turned itself off. `context` additionally accepts `payer`, the income spelling of the narrowest level. |
| `spending` | object | Optional. Groups the spending feature's per-deployment knobs. Absent ⇒ every account counts, the internal-transfer matcher runs on its defaults, no rules and no pins apply, and `wealthdb categorize` refuses for want of a model. See docs/SPENDING.md. |
| `spending.accounts` | object | Optional. Account-scope overrides, keyed by `silver_source_id` in the `returns_exclude` shape, with `include` / `exclude` lists of account ids. An account may not appear in both. Stamped into gold's `spend_account_scope` by every enrichment pass, so removing an entry removes its effect. An entry naming an account gold does not hold scopes nothing; the pass counts such entries and the load summary reports how many. Which way round the overrides bite is the scope rule — see docs/SPENDING.md §1. |
| `spending.internal_transfer_matching` | object | Optional. Knobs for the matcher that pairs the two legs of an own-account move so neither counts as spending: `window_days` (0–30, default 5) and `tolerance_pct` (0–5, default 0.5). Deliberately the same defaults as `returns_transfer_matching` — one matching core, one banding. Both bound the matcher's banded passes only: a pair the override ledger states, or one a source asserts — by stamping a reference on both legs, or by describing one leg's currency and figure on the other — is not a guess and is not banded. `names` (list of `{source, account?, match}`, each `match` a regular expression) says which narratives name which account as the other side of a movement; a leg that names one pairs with nothing else, and named pairs are claimed before the amount pass. See docs/SPENDING.md §3. |
| `spending.rules[]` | array | Optional, default empty. The deployment's own entries in the rule tier, each `{ "match": <regex>, "category": <spend_detailed>, "scope"?, "asset_class"?, "far"? }`. `match` is compiled case-insensitively and tested against `counterparty`, the full `description` and `provider_category`; among config rules the first written wins. `category` may be any valid `spend_detailed` value, vendored or delta. `scope` narrows a rule to a `source`, `portfolio` or `account` and an inclusive `from`/`to` date range. `asset_class` is admitted only beside `investment` and `far` (a `declared_accounts` id) only beside `internal_transfer` ([CASHFLOW.md §4 and §7](CASHFLOW.md)). An invalid pattern, one matching the empty string, or an unknown value fails the load naming `spending.rules[i]`. Deployment-specific: lives in the config, never in the repository. [SPENDING.md §3](SPENDING.md), *Config-supplied rules*, owns the semantics. |
| `spending.pins` | string | Optional. Filesystem path to a CSV ledger of per-transaction category pins — the top of the precedence lattice, for the row nothing else can classify. Columns `silver_source_id, account, occurred_at (YYYY-MM-DD), amount, currency, spend_detailed, asset_class, note` and no others — an unknown header fails the parse, and `asset_class` is optional; `account` is a gold `account_external_id` or a nickname, resolved as `equity_transfers` resolves it; `spend_detailed` may be any valid value, vendored or delta; `note` is free text kept for the ledger's own readability and is not carried into gold. `~` / `$HOME` / `${VAR}` expanded, a relative path resolved against the config file's directory; a missing file is a no-op. Re-stamped by every enrichment pass, so removing a row removes its effect. See §13.10. |
| `spending.transfer_overrides` | string | Optional. Path to a CSV ledger of manual match and unmatch decisions for the internal-transfer matcher, for a pair the data cannot settle, and of `reimburse` lines that link a repayment to its outflow. Expanded as above; a missing file is a no-op. Both families read it. See [SPENDING.md §3](SPENDING.md), *The override ledger* and *Reimbursements*. |
| `spending.categorization` | object | Optional. Configures `wealthdb categorize`: the model endpoint (`model`), how much of a transaction reaches it (`context`), and the per-merchant narrative cap (`descriptor_samples`). Absent ⇒ the subcommand refuses; the deterministic tiers are unaffected and keep running on load. |
| `spending.categorization.model` | object | Optional. LLM endpoint asked for a category per merchant signature. Same shape and same API support as `symbol_resolution.model` (`baseUrl`, `api`, `apiKey`, `name`). |
| `spending.categorization.context` | string | Optional. `"merchant"` (default) sends merchant signatures only; `"descriptor"` adds the raw statement narratives; `"transaction"` adds date, amount, account kind and nearby-transaction signatures. The default is the most private level by decision, not by accident. Independent of the fence, which gates candidacy at every level. |
| `spending.categorization.descriptor_samples` | integer | Optional, 0–20, default 3. Caps the raw narratives sent per merchant at the two context levels that send any. Range-checked even at the `merchant` level, where nothing reads it. |
| `spending.categorization.fence_person_names` | boolean | Optional, **default `true`**. Keeps a signature that is a bare person's name — two or more all-letter tokens with no organisation marker — off a non-card account out of the model tier, at every context level. Turn it off only when the model endpoint is on this machine: a person's name is the one signature shape that is PII by itself, and every other arm of the fence reads a marker beside the name rather than the name. Card rows are exempt by construction (most card merchants are two plain words). `income.categorization` inherits it with the rest of the block. See docs/SPENDING.md §5. |
| `symbol_resolution` | object | Optional. Groups the knobs for `wealthdb resolve-symbols`: the LLM endpoint (`model`) and the ticker-mapping override list (`overrides`). Both inner fields optional; the subcommand fails if `model` is unset and `--overrides-only` wasn't passed. |
| `symbol_resolution.model` | object | Optional. LLM endpoint used to back-fill missing instrument tickers (`baseUrl`, `api`, `apiKey`, `name`). Only the OpenAI-compatible Chat Completions API (`api: "openai-completions"`) is supported today. |
| `symbol_resolution.overrides[]` | array | Optional. User-authored ticker-mapping overrides applied at the start of every run under `model_name='manual-override'`. Each entry keys on `silver_source_id` + `lookup_kind` (`instrument_external_id` or `name`) + `lookup_value`; set `symbol` to correct a ticker, or `delete: true` to suppress a row where no real ticker exists. |

### 5.2 Adapter kinds

A silver source's `kind` names the adapter that reads it. The loader
never opens a silver to guess one. Validation refuses a kind no adapter
registers, and its message lists the registered kinds. The setup wizard
lists the same kinds in its prompt.

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
    "sources":    { "cointracking": "2020-01-01" },
    "portfolios": { "cointracking": { "cu_000001": "2021-07-01" } },
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
  **global** grain is never anchored.
- **Semantics:** `winFrom = max(data-inception, configured-inception, FROM)`,
  where `FROM` is the window's start. It can only move an anchor
  LATER, never earlier (an inception before an entity's data is a
  no-op). The opening base `V0` becomes the entity's
  carry-forward value on that date. Truncation is tagged
  `configured_inception` in place of `since_data_inception`. Composes with
  the policy `Inception` mode (e.g. UBS `first-real-snapshot`) as `max`.
  MOIC is unaffected (time-insensitive); this re-anchors the TWR/MWR chain.
  Ledger flows dated before the resolved inception fall outside every
  window; the `flows_before_inception` quality flag surfaces that exclusion
  (and, conversely, marks the entities where this override is the
  documented remedy — a transaction backfill reaching further back than
  the value spine supports).
- Absent block ⇒ every entity keeps its data-derived inception. A
  misspelled portfolio or account id is a silent no-op, because the load
  cannot check it against gold. A misspelled source id fails the load.

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
  grain shows every non-hidden account (§5.7), and an excluded PORTFOLIO still
  shows its own portfolios row (only an excluded ACCOUNT drops from its
  portfolio there). At
  the sources and global grains, an excluded account, and every account of an
  excluded portfolio, are omitted from the aggregate.
- **Returns only.** Holdings and net-worth views are unaffected. To drop
  an account from gold entirely, use `account_overrides.exclude` (§13.8).
- Absent ⇒ nothing excluded.

### 5.6 Returns policy overrides (per source)

Every source's returns behaviour is driven by a `ReturnsPolicy` registered in
code beside its adapter (docs/RETURNS-NOTES.md "Pluggable per-source
policy") — a default chosen for the data the collector normally delivers.
`returns_policy_overrides` adjusts that policy per deployment, for setups
whose data completeness differs from the default's assumption (e.g. a carta
silver holding positions but no transaction history).

```json
"returns_policy_overrides": {
    "carta":  { "flow_regime": "nav_only" },
    "mycoin": { "accounts_grain": "normal" }
}
```

- **Keys** are `silver_sources[].id` values (validated at load), NOT adapter
  kinds — two sources sharing an adapter override independently.
- **`flow_regime`** (`"flow_complete"` | `"crypto_partial"` | `"nav_only"`)
  REPLACES the source's whole flow classification with the named regime's
  canonical kind sets: flow_complete → the bank external + netting sets,
  crypto_partial → fiat deposit/withdrawal external with no netting set,
  nav_only → no counted flows. Wholesale replacement is deliberate: swapping
  only the regime name would leave the old kind sets attached — a hybrid no
  regime defines. A regime override also marks the policy known, so
  `unknown_adapter_policy` clears.
- **`accounts_grain`** (`"normal"` | `"blanked"` | `"hidden"`) replaces the
  per-account display mode: full rows, rows with TWR/MWR blanked to n/a +
  `accounts_grain_meaningless`, or no rows at all (plumbing — values and
  flows still enter every aggregate; the deposit-bank conduit sources
  register `hidden` as their default). Overriding to `"normal"` is how a
  deployment re-surfaces a hidden source's rows — all but its cash
  accounts, which stay hidden whatever this says (§5.7).
- **Partial semantics:** unset fields keep the registered policy's values;
  code-side knobs without a config field (onboarding scope, conduit kinds,
  inception mode, …) are never touched.
- Applied identically by `wealthdb returns` and the materialized
  `report_returns` behind the web dashboard. Absent ⇒ registered policies
  apply unchanged.

### 5.7 Returns hiding (display only)

`returns_hide` suppresses accounts' or portfolios' OWN rows at every returns
grain while their values and flows keep contributing to every aggregate that
contains them — the display mirror of `returns_exclude` (§5.5), which removes
an entity from the coarse-grain math instead.

```json
"returns_hide": {
    "accounts":   { "ubs-main": ["<iban-account-id>"] },
    "portfolios": { "cointracking": ["cu_000002"] }
}
```

- **Keys** are the same stable external ids as elsewhere, per
  `silver_sources[].id` (validated at load).
- **Semantics:** a hidden account emits no accounts-grain row; a hidden
  portfolio emits no portfolios-grain row and hides its member accounts'
  rows too. An entity consisting ONLY of hidden constituents (a source whose
  every account is hidden, a no-portfolio bucket of hidden accounts) emits
  no row either. The global grain always shows — hidden plumbing still
  aggregates there, values and flows included.
- **Always hidden:** every account of kind `cash`, whatever its source and
  its policy. A cash account is plumbing — money passes through it between
  other holdings — so a return of its own is noise: n/a on a drained base,
  or a chained −100% on one drained and refilled. By the rule above, a
  portfolio or source made only of cash accounts emits no row either.
- **Policy composition:** per-source policies already hide whole conduit
  sources (`AccountsGrainHidden` — the deposit-bank collectors register it
  by default); this block covers deployment-specific ids on top. An id in
  both `returns_hide` and `returns_exclude` is excluded from the math AND
  shows nowhere.
- **Returns only.** Holdings / net-worth views are unaffected.
- Absent ⇒ nothing hidden beyond the cash accounts and policy.

### 5.8 Cross-source transfer matching (opt-in)

Per-source flow classification can never pair the two legs of a
cross-custodian move: the sending source books a real withdrawal/journal, the
receiving one a real deposit, and each is correct at its own boundary — but an
aggregate containing BOTH accounts double-represents the move (the
`unmatched_transfers=N` count includes exactly these legs).
`returns_transfer_matching` closes that gap:

```json
"returns_transfer_matching": {
    "enabled": true,
    "window_days": 5,
    "tolerance_pct": 0.5
}
```

- **Matching** links opposite-sign external legs across DIFFERENT sources —
  same native currency, equal amount within `tolerance_pct` of the larger leg
  (absolute floor 0.01, so `0` means exact-to-a-cent; the default 0.5% covers
  wire fees deducted in transit), within `window_days` — 1:1 greedy,
  deterministic, ranked by (amount gap, day distance) so an exact-amount
  partner beats a nearer-day coincidence. Native amounts (not output-currency
  conversions) drive the match, so the three materialized currency partitions
  derive identical pairs whenever their attached-flow universes coincide (a
  leg whose FX is unresolved in a partition is a candidate only where it
  attached and can shift greedy pairings there). Same-source pairs are out of scope: within a source
  the silver classifier and the transferLike netter own internality.
  Equity-transfer ledger legs (`xfer:` ids) are exempt — an intentionally
  recorded pair the transferLike netter already handles.
- **Netting is per entity**: a linked pair nets only in aggregates where BOTH
  legs are live members of the window — both accounts in the entity (post
  `returns_exclude`), both days inside it, neither leg in a subsumption
  region (a pre-debut or closure-drain leg is represented by its synthetic
  onboarding/closure amount, so its live partner keeps counting). The
  sources/portfolios grains therefore keep counting each leg as the real
  boundary flow it is for them; typically only global nets.
- **Quality:** entities that netted pairs tag `cross_source_netted=N`;
  their `unmatched_transfers` count shrinks accordingly. Cross-source
  netting obeys `--netting off` like the heuristic netter — the diagnostic
  view shows every raw leg.
- **Off by default.** Absent block or `enabled: false` ⇒ the matcher never
  runs; output is byte-identical to the per-source heuristics alone. A
  matched-nothing run is also byte-identical.
- **Limitations:** same-native-currency pairs only (a CHF→USD
  cross-source wire never matches); one-to-one only (a split wire — one
  withdrawal, two deposits — pairs at most one deposit).

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

`internal/silver/adapter.go` defines the contract. An `Adapter` opens
a `Connection`, and the connection answers four calls.

```go
package silver

type Adapter interface {
    Kind() string                                       // "schwab", "ubs", "swissquote"
    Open(ctx context.Context, spec OpenSpec) (Connection, error)
}

type Connection interface {
    Close() error
    Status(ctx context.Context) (canonical.Status, error)
    ChangeWindow(ctx context.Context, sinceChangeNumber int64) (canonical.Window, error)
    Snapshots(ctx context.Context, window canonical.Window) (SnapshotStream, error)
    Transactions(ctx context.Context, window canonical.Window) (TransactionStream, error)
}

type SnapshotStream interface {
    Next(ctx context.Context) (batch canonical.SnapshotBatch, more bool, err error)
    Close() error
}

type TransactionStream interface {
    Next(ctx context.Context) (batch canonical.TransactionBatch, more bool, err error)
    Close() error
}
```

- **`Open`.** A single-file adapter reads `spec.Path`. A merged
  adapter (UBS = ubs-web + ubs-psn) reads `spec.Subsources` and aligns
  identities through `spec.Relationships`.
- **`Status`.** A cheap read of where silver stands. Every
  `wealthdb load` and `wealthdb status` calls it. Its timestamps are
  Unix seconds UTC, and -1 means nothing is observable.
  `LatestChangeNumber` is monotone and opaque to gold. Its valid values
  start at 0, and -1 means silver is empty. The two `Observed*Through`
  watermarks are §6.4's.
- **`ChangeWindow`.** The window of changes since the given change
  number, and the number to write into `silver_sources.high_watermark`
  once the load succeeds. With `HasChanges` false, the caller skips
  `Snapshots` and `Transactions`. The new number may still differ from
  the old one: silver advanced, but nothing observable changed inside
  the window. A reference source may bound each kind of reference
  record on a span of its own (`Window.Spans`). The stream and the
  load's windowed delete then read each kind over its span, and leave a
  kind without changes alone.
- **`Snapshots`.** The dimension upserts and snapshot-grain facts whose
  `snapshot_at` falls inside the window. A `SnapshotBatch` multiplexes
  the dimensions (portfolios, accounts, instruments), the facts
  (positions, open lots, cash balances, FX rates) and the reference
  records (§7.6).
- **`Transactions`.** The event-grain facts whose `occurred_at` falls
  inside the window.
- **The streams.** Both are pull-model batched iterators (§6.5). The
  batch that comes with `more=false` is still valid and is applied.
- **Realized lots.** A connection whose silver states realized lots
  also implements `silver.RealizedLotReader` (§8.3).

The records live in `internal/canonical`, so an adapter imports only
the canonical types, never the gold writer. Each `*Change` type mirrors
one gold table's columns. The field lists are not restated here, since
they would drift. They are in `canonical/types.go` (the dimensions,
positions, cash, FX, transactions, `Status`, `Window` and the batches),
`canonical/basis.go` (the lots) and `canonical/refdata.go` (the
reference records).

### 6.3 Registration

`silver.Register(&Adapter{})` is called from each backend
package's `init()` (the name comes from the adapter's `Kind()`).
Backends live under `internal/silver/<source>`. The registered set is
not restated here — it would drift the moment one is added. Two places
carry it, and both are checked by the build: the blank-import block in
`cmd/wealthdb/main.go` below, and the `silver_kind` CHECK on
`silver_sources` (§7.2), which a load validates every source against.

```go
import (
    _ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/schwab"
    _ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/ubs"
    _ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/swissquote"
    // ...one blank import per backend package listed above.
)
```

Adding a new source means adding a package and one blank import —
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
a dump-driven plugin is `MAX(silver.dump_runs.snapshot_at)` (with
`-1` returned when the silver has no `dump_runs` rows); a plugin
is free to pick something else as long as monotonicity and the
`-1` sentinel hold. The synthetic kind reads
`MAX(dump_runs.change_number)`, a number its writer stamps on each run,
and its window spans only the runs past the watermark
([adapters/synthetic.md](adapters/synthetic.md) §8).

What this guarantees:
- Replays are idempotent (`ChangeNumber == watermark` ⇒ no-op).
- A silver DB restored from backup or rebuilt is detected: if its
  new change number is **less than** the stored watermark, gold
  refuses to load and asks for `wealthdb reset <id>`.

What it does **not** guarantee:
- Same `LatestChangeNumber` with different silver content. If
  silver is amended in place without bumping the change number,
  gold sees a no-op. Plugins must avoid this.

#### Observed through

Every fact in gold is dated by when it is true: a snapshot's as-of,
a transaction's date. The two observed watermarks of `Status` say
when the source was last looked at. A portal that marks its holdings
quarterly and was read this morning is observed through this
morning, while its latest snapshot is the quarter's end. A feed that
stopped running is observed through its last run, however current
its carried values look.

- `ObservedSnapshotsThrough` is the latest run that read the
  holdings, or the latest statement of a statement corpus.
- `ObservedTransactionsThrough` is the end of the first gapless
  stretch the runs' fetch windows cover, from the earliest window.
  Windows that meet within a day join (`silver.CoveredThrough`).
- A silver that records no fetch windows counts each run as reading
  the ledger from the previous run or from the start of the history,
  so both watermarks are its latest run (`silver.ObserveByRuns`).
  That is every kind but these:

| kind | holdings | ledger |
|---|---|---|
| fidelity (also svb) | runs that read the positions page; the latest statement of a corpus | the activity windows of the runs, and the span of the statements |
| ubs | the earlier of the report feed and the web feed: each states a part of the holdings | the same; the web feed's windows |
| schwab | the later of the api and web feeds: each reads every account | the same |
| carta, viac | runs that are not dry runs | the same |
| manual | the latest load of the book | the same |
| synthetic | the end of the latest run's window | the run windows |

The loader stamps both into `silver_sources` on every load, including
one that finds no new data (§8.1): that a run found nothing new is
itself the observation a freshness reader needs. The view
`source_observed` reads them, NULL where a silver does not say;
`wealthdb status`, the `holdings sources` column `observed_date`, the
MCP `status` tool and the Data Freshness dashboard count staleness
from it.

**The roster a read states.** A portal that marks its holdings on
event dates writes nothing to its silver when it drops a holding whose
marks are unchanged. Without more, the holding would persist in gold
until the next data date. What such a read does state is a roster:
the holdings the portal still lists. The adapters for these portals
(angellist, carta, equityzen) read the roster each dump listed
(`silver.Removals`) and add the time of the first complete dump that
omitted a holding to their snapshot times. At that time they emit the
roster without it, every other position at the mark it had: the
snapshot is dated at the read, each position's `valued_at` at its
mark. Gold ends the holding at the read and invents no mark for it.
`positions.valued_at` is NULL where the mark is the snapshot's own,
as it is for every source that reads live values. It is also set on a
schwab api position whose mark the guard reads as a spike: it names
the snapshot of the previous mark the position takes
([adapters/schwab.md](adapters/schwab.md) §4.1).
`holdings
positions` and `holdings sources` show it as `valued_date`, and the
Data Freshness dashboard counts a source's quiet days from it.

Absence is evidence only inside what a run read. Some collectors do
not cover every account or asset class on every run: ubs-psn carries
the securities and ubs-web the cash and the mortgages; a fidelity-web
run has a mode; a statement backfill covers one account. Gold already
holds that line: migration 0051 resolves the active snapshot per
account per day, the returns spine ends an account only when a later
run re-covered everything it was last seen with and left it out, and
the supported way to end a holding is a closure row
(`silver.ClosureMarkerBatch`). So an adapter reads a holding as
removed only from a dump it can show read the whole portfolio, never
from absence alone. Each adapter checks that against what the dump
itself declares: angellist counts the positions a dump listed against
the investments its portfolio summary declares; equityzen counts the
deals against its manifest's `investments`, on a manifest whose
status is complete and whose scope has the offerings; carta reads the
roster from its manifest's entity list, never from a dry run or an
empty list. A dump that captured no summary or no manifest, or whose
listing fell short of the declared count, says nothing about the
roster: a download that broke half-way, or a loader that could not
read it, must not end a holding. A collector with partial runs must
record, per dump, the coverage the run declares beside the roster
before it may feed `silver.Removals`.

### 6.5 Pull-model batched iterator

The plugin yields batches of canonical records via `Next` calls.
A batch is at the plugin's discretion (one snapshot's worth, a
fixed row count, whatever paginates cleanly against silver). Gold
applies one batch at a time inside the load transaction; no batch
is partially applied.

Push-model (visitor) would also work; pull-model wins because:
- The main app controls pacing (e.g., commit periodically on very
  large windows).
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
  fields (Schwab `assetType`, UBS CFI codes and asset-class codes,
  Swissquote XLS section headers).
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
conventions, deferred silver tables, open questions) live in their
own files under [adapters/](adapters/), one per adapter that needed
one; the directory is the list. This keeps the present document
focused on gold-side architecture. (Adapters without a dedicated doc
are described inline where they diverge from the gold-side contract
above.)

Each adapter doc is self-contained for the engineer writing or
maintaining that adapter. New bank adapters add a new file in
the same directory.

### 6.8 Cross-cutting adapter rules

A few rules apply to every adapter regardless of bank:

- **Unrecognised `kind` / `transaction_type` values fall through
  to gold `kind = 'other'`** with the raw source value preserved
  in `payload`, rather than failing the load. `wealthdb status -v`
  reports the count of `other` rows per silver source so taxonomy
  drift is visible. Two adapters depart. **amex** kinds a row with no
  usable direction by the sign of its amount (`purchase` /
  `card_payment`) and keeps the raw value in `payload.source_kind`: on
  a card ledger `other` reaches neither the spending base nor the
  internal-transfer matcher, so the row would vanish from both rather
  than surface as backlog ([adapters/amex.md](adapters/amex.md) §5).
  **plaid** kinds an investment row of an unknown subtype by Plaid's
  type where the type alone fixes it (`buy`, `sell`, `fee`), and keeps
  the raw pair in `payload.source_kind`
  ([adapters/plaid.md](adapters/plaid.md) §7).
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
  `instrument_overrides` (§5.1, §13.8).
- **Silver tables gold does not project** — each adapter doc lists
  its own, and §13.7 lists them across banks.

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
- **Market value and accrued income**: a position's `market_value`
  is its full value, accrued income included: interest accrued since
  the last coupon, and a dividend declared but not yet paid.
  `accrued_interest` is how much of `market_value` that income is, and
  NULL where the source does not state it, never 0 for unknown. The
  clean value is `market_value − accrued_interest`. An adapter whose
  source prints the two apart adds them, and keeps the printed value in
  the payload. A source that states only a clean value (the live
  feeds) has a clean `market_value` and a NULL `accrued_interest`.
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
    inside the live data path). Gold takes (a): the loader is
    the sole writer to gold, so application logic is the
    authority on referential integrity. The intended FKs remain
    documented in this §7.2 schema sketch as the conceptual
    contract; the migration omits them.

    If DuckDB ships proper deferred-constraint support in a
    future version, this trade-off is easy to revisit — add FK
    declarations to a new migration, add `DEFERRABLE INITIALLY
    DEFERRED`, done.

### 7.2 Schema sketch

The SQL below is the design-time sketch of the **load-path** tables —
the dimensions and facts `wealthdb load` writes and `wealthdb reset`
clears, together with the bookkeeping and index DDL around them
(`schema_meta` is written by the migrations themselves; an index is
neither written nor cleared). The applied migrations under
`internal/gold/migrations/` are authoritative for the current column
set; the sketch tracks them but may lag in detail.

Auxiliary tables that later migrations add are not sketched here; each
is documented where it is used:

- `symbol_resolutions` (migration `0006`) — §4.1, `resolutions`.
- `binary_versions` (migration `0012`).
- `report_returns` (migration `0026`) — §10.9.
- `report_gains` (migration `0117`) — §10.13.
- The exposure tables: the reference and config tables (migrations
  `0125` and `0128`) — §7.6; the derived tables (migrations `0126` and
  `0129`) and `report_exposure` (migrations `0127` and `0130`); migration
  `0131` touches both; the tables `report_exposure_debt`,
  `report_exposure_issuer_ranks` (migration `0138`) and
  `report_exposure_security_slices` (migration `0165`) — §10.14.
- The reference tables `fund_risk`, `fund_characteristics`,
  `security_period_facts` and `rate_observations`, and the `yield_*`
  config tables (migration `0134`) — §7.6.
- The reference table `dividend_calendar` and the config table
  `yield_settlement_cycles` (migration `0147`) — §7.6.
- The derived table `dividend_gaps` (migration `0148`) — §7.6.
- The reference table `stated_periods` (migration `0159`) — §7.6.
- The reference table `exposure_reports` (migration `0164`) — §7.6.
- The serving table `report_cashflow_plan` (migration `0149`) —
  §10.12.
- The serving tables `serving_*` (migration `0132`) — web/DESIGN.md §8.
- `materialize_inputs` (migration `0155`), the inputs the last
  `web-materialize` built from — web/DESIGN.md §8.
- The spending overlay: `spend_categories` (migration `0040`;
  docs/SPENDING.md §2), and `spend_txn_enrichment`,
  `spend_merchant_categories` and `spend_account_scope` (migration
  `0041`; keys and lifecycle in docs/SPENDING.md §8, macros in §10.10).
- The income overlay: `income_txn_enrichment`,
  `income_payer_categories` and `income_account_scope` (migration
  `0070`; the same three shapes with `payer_*` for `merchant_*`, macros
  in §10.11, docs/INCOME.md). The taxonomy is not a fourth table —
  `spend_categories` gained a `family` column in migration `0069` and
  carries both vocabularies.

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
--
-- Each new adapter widens the silver_kind CHECK in its own migration
-- (DuckDB cannot alter a constraint in place, so the migration renames
-- and recreates the table, carrying every column below).
CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex', 'synthetic', 'plaid', 'refdata',
        'curves'
    )),
    silver_path         TEXT    NOT NULL,            -- as observed at last load
    high_watermark      BIGINT  NOT NULL,            -- plugin's logical change number after the last load
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL,
    fx_priority         INTEGER,                     -- config's FX precedence, stamped on load; §10.6 / §13.2
    observed_snapshots_through    BIGINT DEFAULT -1, -- Status watermarks, stamped on every load; §6.4
    observed_transactions_through BIGINT DEFAULT -1
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

-- account_kind is the technical container; §13.8 lists its values.
--
-- portfolio_external_id (nullable) names the parent portfolio in
-- the `portfolios` table. Schwab and Swissquote accounts leave it
-- NULL (no portfolio grouping); UBS cash/safekeeping/overlay
-- accounts populate it. See `wealthdb holdings portfolios` rollup semantics.
CREATE TABLE accounts (
    silver_source_id        TEXT    NOT NULL,
    account_external_id     TEXT    NOT NULL,
    account_kind            TEXT    NOT NULL,   -- technical container; see §13.8
    display_name            TEXT,               -- user-facing label, payload-derived
    base_currency           TEXT,               -- ISO 4217 if known
    relationship_id         TEXT,               -- UBS relationship dimension; NULL otherwise
    nickname                TEXT,               -- user-set label; Schwab silver supplies, config override fills others
    account_category        TEXT,               -- free-text bank descriptor; see §13.8
    portfolio_external_id   TEXT,               -- parent portfolio (NULL when ungrouped)
    tax_wrapper             TEXT,               -- tax / regulatory registration; see §13.8
    management_style        TEXT,               -- self_directed / advisory / discretionary / automated; see §13.8
    first_seen_at           BIGINT  NOT NULL,   -- earliest snapshot_at observed
    last_seen_at            BIGINT  NOT NULL,   -- latest snapshot_at observed
    payload                 JSON,
    PRIMARY KEY (silver_source_id, account_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

-- portfolios is its own entity. A
-- portfolio is the wealth-management wrapper (UBS, cointracking, fidelity)
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
    nickname                TEXT,               -- portfolio label, where the source states one
    first_seen_at           BIGINT  NOT NULL,
    last_seen_at            BIGINT  NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, portfolio_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

-- asset_class (exposure) + vehicle (wrapper) are the two-dimensional
-- instrument taxonomy; docs/TAXONOMY.md lists the values and the
-- admitted pairs. The writer accepts only admitted pairs
-- (canonical.ValidTaxonomyPair); an unrecognised source code falls
-- through to (other, other) with the raw code preserved in `payload`.
-- The writer always populates vehicle, though the column is nullable.
--
-- `instrument_external_id` is whatever the silver layer uses as its
-- per-source instrument key:
--   Schwab     — CUSIP if present, else symbol (mirrors silver)
--   UBS        — ISIN (mirrors silver)
--   Swissquote — ISIN where known, else symbol + '@' + currency
--                (adapters/swissquote.md §8)
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
    valued_at               BIGINT,                  -- the instant the mark is from; NULL = snapshot_at (§6.4)
    account_external_id     TEXT    NOT NULL,
    position_key            TEXT    NOT NULL,        -- adapter-chosen, see comment above
    instrument_external_id  TEXT,                    -- NULL for one-off contracts not in `instruments`
    asset_class             TEXT    NOT NULL,        -- exposure, mirrored from instruments for index-friendliness
    vehicle                 TEXT,                    -- wrapper, mirrored from instruments; writer-required
    currency                TEXT    NOT NULL,        -- position's natural currency (ISO 4217)
    quantity                DECIMAL(28, 8),          -- units / nominal / face value
    market_value            DECIMAL(28, 4),          -- in `currency`, accrued income included (§7.1)
    book_value              DECIMAL(28, 4),          -- cost basis in `currency`; NULL unless its source states it or the lot engine rebuilds it whole
    basis_origin            TEXT,                    -- the stamp on book_value (§7.4); NULL with it
    basis_method            TEXT,
    basis_fees              TEXT,
    accrued_interest        DECIMAL(28, 4),          -- the accrued part of market_value; NULL when not stated
    acquisition_date        DATE,                    -- earliest acquisition; NULL when unknown
    book_value_known        DECIMAL(28, 4),          -- lot engine: the cost of the lots that have one
    quantity_without_basis  DECIMAL(28, 8),          -- lot engine: the quantity in lots that have none
    open_lots               INTEGER,                 -- lot engine: the lot count
    payload                 JSON,                    -- raw silver row(s) that produced this fact
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, position_key),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

-- A position's open lots, beside the position row with the same
-- source, snapshot, account and position key (§7.4).
CREATE TABLE position_lots (
    silver_source_id        TEXT    NOT NULL,
    snapshot_at             BIGINT  NOT NULL,
    account_external_id     TEXT    NOT NULL,
    position_key            TEXT    NOT NULL,
    lot_key                 TEXT    NOT NULL,        -- unique within the position
    instrument_external_id  TEXT,
    currency                TEXT    NOT NULL,
    quantity                DECIMAL(28, 8),          -- signed like positions.quantity
    book_value              DECIMAL(28, 4),
    market_value            DECIMAL(28, 4),
    acquisition_date        DATE,
    term                    TEXT,                    -- 'short' | 'long', as stated
    covered                 BOOLEAN,
    basis_origin            TEXT,                    -- NULL with book_value
    source_document         TEXT,                    -- sha256 of the source document
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, position_key, lot_key)
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
-- feed). Gold keeps all rows and lets the `fx_daily` view (§10.6) pick per
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
--   'staking'             — crypto staking reward
--   'contribution'        — fund/LP capital contribution
--   'distribution'        — fund/LP distribution
--   'fee'                 — custody/trade/tax-statement fee
--   'tax'                 — withholding tax, stamp duty
--   'deposit'             — incoming wire/cash
--   'withdrawal'          — outgoing wire/cash
--   'purchase'            — card spend
--   'refund'              — card merchant credit back
--   'card_payment'        — payment down of a card balance
--   'reward'              — card rewards credit
--   'fx'                  — FX conversion settlement
--   'fx_forward'          — FX-forward settlement
--   'fx_swap'             — FX-swap settlement
--   'corporate_action'    — stock split, name change, merger, ...
--   'transfer_in'         — securities transfer in (DRS, ACATS)
--   'transfer_out'        — securities transfer out
--   'journal'             — internal account-to-account journal
--   'other'               — unmapped; bank's own type lives in payload
--
-- `counterparty` and `provider_category` are nullable; a source with
-- no such field leaves them unset. `counterparty` is not merely informational: it is the input to the
-- merchant signature that groups spend, so how an adapter formats it
-- is a stated contract (drift re-keys merchants). `provider_category`
-- is the provider's own filing of the row — a card issuer's spend
-- category, a bank's booking type — stored verbatim; the spending
-- provider tier translates it per silver kind, and may place a delta
-- where the filing names the movement (SPENDING.md §3).
-- `description` may end with a memo — the payer's own message, which
-- an adapter emits apart as `TransactionChange.Memo` and the writer
-- stores behind `canonical.DescriptionMemoSeparator`, folding any
-- separator the narrative itself carried — which neither the merchant
-- signature nor the built-in rules read (SPENDING.md §4).
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
    description             TEXT,                    -- free-text row label, may end with a memo; see above
    counterparty            TEXT,                    -- merchant/payee; see above
    provider_category       TEXT,                    -- provider's own filing: spend category or booking type
    check_number            TEXT,                    -- cheque number on an outflow (§10.8)
    asset_class             TEXT,                    -- what a trade traded, where the instrument cannot say (§10.8)
    vehicle                 TEXT,
    instrument_hint         TEXT,                    -- the token an instrument lookup failed on (§10.8)
    payload                 JSON,
    PRIMARY KEY (silver_source_id, transaction_external_id),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

CREATE INDEX ix_transactions_account_time
    ON transactions(silver_source_id, account_external_id, occurred_at);
CREATE INDEX ix_transactions_kind_time
    ON transactions(kind, occurred_at);

-- Realized lots, or sales where a document prints no lots: one row per
-- lot per document (§7.4). Replaced whole per source on a load.
CREATE TABLE realized_lots (
    silver_source_id         TEXT    NOT NULL,
    realized_lot_external_id TEXT    NOT NULL,
    account_external_id      TEXT    NOT NULL,
    instrument_external_id   TEXT,
    instrument_hint          TEXT,                   -- the token the lookup failed on
    description              TEXT,                   -- security as printed
    document_kind            TEXT    NOT NULL,       -- form_1099b, year_end_summary, …, engine
    tax_year                 INTEGER NOT NULL,
    acquisition_date         DATE,
    acquired_various         BOOLEAN NOT NULL,
    disposal_date            DATE,
    settlement_date          DATE,
    currency                 TEXT    NOT NULL,
    quantity                 DECIMAL(28, 8),         -- magnitude
    proceeds                 DECIMAL(28, 4),         -- magnitude
    book_value               DECIMAL(28, 4),         -- magnitude
    realized_gain_loss       DECIMAL(28, 4),         -- signed; NULL when not printed
    wash_sale_disallowed     DECIMAL(28, 4),
    accrued_market_discount  DECIMAL(28, 4),
    term                     TEXT,
    covered                  BOOLEAN,
    form_8949_box            TEXT,
    basis_origin             TEXT,                   -- the stamp on book_value; NULL with it
    basis_method             TEXT,
    basis_fees               TEXT,
    is_primary               BOOLEAN NOT NULL,       -- counts each sale once per account and tax year
    source_document          TEXT,
    payload                  JSON,
    PRIMARY KEY (silver_source_id, realized_lot_external_id)
);

-- The lot engine's ledger (lots, lot_disposals, lot_realized,
-- lot_findings, lot_anchors) is the pass's output, rewritten whole by
-- every pass; lot_runs keeps one row per pass and source. lot_realized
-- holds the engine's realized lots in realized_lots' shape, and the
-- view realized_lots_all reads both: docs/LOTS.md §5.
```

### 7.3 What's deliberately omitted

- **Views ahead of a reader.** A view exists because a query needed
  it. The serving views for the optional Metabase server are named
  `web_*` (§10.10), and nothing in the engine depends on one.
- **Materialised positions-as-of cache.** The as-of query in §10 runs
  fast on the index; no cache needed at personal scale. The serving
  tables hold report output for the Metabase dashboards alone, which
  fire many queries at once (web/DESIGN.md §7). No engine command
  reads them.
- **A surrogate `account_id` / `instrument_id` integer key.** Tuple
  PKs `(silver_source_id, account_external_id)` are noisier in
  joins but eliminate a whole class of "how do I generate a stable
  surrogate" bugs. DuckDB has no SERIAL/SEQUENCE that fits well.
- **Audit columns on dimensions** beyond `first_seen_at` /
  `last_seen_at`. `payload` carries the rest.
- **A separate `holdings_history` table.** Positions itself is the
  history — one row per (snapshot, account, position_key).

### 7.4 Cost basis

Gold carries the cost basis each source states. Where a source states
none, the lot engine rebuilds one from its trades, stamped `rebuilt`
(docs/LOTS.md, §7.5).

Cost basis means different things in different sources. One source
sums tax lots, another keeps a weighted average. Fees are in the
figure or not. A sum across sources is only meaningful when each
figure says what it is, so every `book_value` carries a stamp. The
gold writer refuses a book value without one, and a stamp without a
book value.

| column | value | meaning |
|---|---|---|
| `basis_origin` | `stated` | one figure the source states, taken as is |
| | `derived` | arithmetic over stated figures: quantity × average cost, market value − unrealized gain, a sum of lots, a figure converted at the source's own rate |
| | `rebuilt` | replayed from trades by the lot engine |
| | `seeded` | an opening basis entered by hand |
| `basis_method` | `lots` | the sum of the source's tax lots, whatever method relieves them |
| | `average` | a weighted average cost |
| | `paid_in` | capital paid in, gross of capital paid back (private markets) |
| | `acquisition_value` | the value on the acquisition date |
| | `unknown` | the source does not say |
| | `fifo`, `lifo`, `hifo`, `lofo` | the lot engine's relief method, on a `rebuilt` basis (a rebuilt average says `average`) |
| `basis_fees` | `included`, `excluded` | whether purchase fees are in the figure |
| | `none` | the source charges no purchase fee |
| | `unknown` | the source does not say |

Rules every adapter follows:

- **The position's currency.** `book_value` is in the row's
  `currency`. An adapter converts only at a rate the source itself
  states. Otherwise the book value is NULL and the source's figure
  stays in the payload.
- **NULL means not stated.** Never 0 for unknown.
- **Private markets.** The book value is the capital paid in, gross of
  capital paid back. An in-kind distribution moves basis out with the
  asset; cash paid back does not.
- **`acquisition_date`** is the earliest acquisition the source
  states. A position with lots takes its earliest lot's date.

Each source maps its basis this way; its adapter doc or package
comment has the detail:

| source | `book_value` | stamp (origin · method · fees) | open lots | realized lots |
|---|---|---|---|---|
| schwab | API: market value − open P/L; statements: the printed cost | derived or stated · lots · included | statement lots | 1099-B, year-end summary, gain/loss report |
| fidelity | the printed cost | stated · lots · included | the positions page's lots | 1099-B, closed positions, statement sales |
| ubs | MT535 book cost, or the statement's cost value; a fund without one: capital called | stated or derived · average or paid_in · excluded | – | statement sales, export sales |
| swissquote | quantity × average cost | derived · average · excluded | – | – |
| viac | quantity × average acquisition price | derived · average · none | – | – |
| plaid | the institution's cost | stated · unknown · unknown | tax lots that add up to the holding | – |
| carta | cap table: an exercised certificate at its value at exercise, every other line at the cash paid; funds: capital paid in, less in-kind distributions | derived or stated · acquisition_value, lots or paid_in · none or included | share certificates, at the cash paid | the lot engine's row per sale, against the stated basis (docs/LOTS.md §5.3) |
| angellist | capital paid in, less in-kind distributions | stated or derived · paid_in · included | – | the engine's row per SPV payout, against the stated basis |
| equityzen | SPVs: shares held at the price paid, plus the execution fee pro rata; funds: capital paid in plus the fee | derived · average or paid_in · included, or unknown without a stated fee | – | the engine's row per sale, against the stated basis |
| manual | the paid-in series, else the value at acquisition | stated · paid_in or acquisition_value · unknown | – | the engine's row per exit (`exits.csv`), against the stated basis |
| synthetic | a holding: the sum of its lots, else its average cost; private markets: capital paid in; a home or mortgage: the value at acquisition | stated · lots, average, paid_in or acquisition_value · none, or for crypto included in lots and excluded from an average | each holding's lots, at cost | one row per lot a sale relieves, on a 1099-B or a statement |

cointracking, svb and relevate state no basis. The lot engine fills a
missing basis on the kinds docs/LOTS.md §1 lists, and realizes the
sales of the kinds that state a paid-in basis against it; relevate's
stays blank.

**Open lots** (`position_lots`) sit beside their position row: same
source, snapshot, account and position key. A lot's `basis_origin`
is set exactly when its `book_value` is. Its `term` and `covered` are
the source's own statement, not a computation.

**Realized lots** (`realized_lots`) hold one row per lot per document.
The same sale appears in several documents: a 1099-B, its correction,
a year-end summary, a statement. Gold keeps every copy, as silver
does, and marks one set `is_primary`. Within one source, account and
tax year, the primary rows count each sale once: the adapter ranks
its own document kinds and marks the best kind present
(`silver.MarkPrimary`). A tax year's stated result is therefore
`SUM(realized_gain_loss) WHERE is_primary`. Where a document prints
no gain, the column is NULL; docs/GAINS.md §2 derives one. The lot
engine writes its own rows to `lot_realized`, `document_kind =
engine`, one per sale and lot relieved; the readers read both tables
through `realized_lots_all`. An engine row is primary only in a tax
year no stated primary lot documents on the account or its portfolio
(docs/LOTS.md §5.3).

The readers of all three, `holdings positions`' cost basis columns and
`wealthdb gains`, are defined in docs/GAINS.md.

### 7.5 Lot engine

The lot engine rebuilds what a source does not state from its trades:
the open lots, a cost basis per position row and a realized lot per
sale, stamped `rebuilt` (docs/LOTS.md). It is a pass over gold after
the per-source loads (§8.1), shared by every source. A source kind
registers a `lots.Policy` beside its adapter, as it registers a
`ReturnsPolicy`, and no adapter computes a lot.

The engine is pure (`internal/lots`). The feed, the writer and the pass
are gold's (`lots_feed.go`, `lots_writer.go`, `lots_pass.go`). Stated
figures always win. The engine fills the gaps and keeps its own figure
beside a stated one for `gains check`.

### 7.6 Reference data

A reference source writes dated facts about funds, securities and
public rates, not about accounts. Each reference table is keyed by
`silver_source_id` and windowed by `as_of`, the day a row describes, as
a snapshot table is windowed by `snapshot_at`. Every composition ever
loaded is kept. The `refdata` kind writes the fund and security tables
([adapters/refdata.md](adapters/refdata.md)), the `fred` and `curves`
kinds the rates, and the `synthetic` kind can write all of them. A
rate's payload states its series' basis. `curves` names a spot rate and
its compounding. A series that names no basis, or a Treasury one
(`constant_maturity`, `discount`), is read as a Treasury par yield.
[YIELD.md](YIELD.md) §8 says how the yield report reads each basis, and
which curves it refuses.

Migration 0125 adds `fund_compositions`, `fund_lines`,
`fund_attributes`, `fund_counterparties`, `security_identities` and
`security_facts`. Migration 0134 adds columns to `fund_lines`, such
as a debt line's durations and yields. It also adds four tables:

- `fund_risk`: a filing's rate sensitivities, per fund, day, currency and
  tenor, as filed. The view `fund_risk_signed` reads each filing in one
  sign, positive where the fund loses value as rates rise (YIELD.md §6);
- `fund_characteristics`: the figures a provider states for a fund as a
  whole, such as its duration, its yields and its credit-quality
  breakdown;
- `security_period_facts`: a company's reported figures, each for its
  period and dated by the period's last day;
- `rate_observations`: one day's value of a public rate series, in
  percent.

Migration 0147 adds `dividend_calendar`: one distribution of a fund or
a security as a provider states it, with its ex, record and pay days.
Its `as_of` is the day the source stated it. A load replaces gold's
rows of each ref and provider whose rows changed, whole, so gold holds
what silver holds (adapters/refdata.md §4). The yield report reads a
dividend's record day from it (YIELD.md §4).

Migration 0148 adds `dividend_gaps`: the gap each issuer keeps from a
distribution's record day to its pay day, learned from the calendar,
and the gap of each pool of funds. The yield pass rebuilds it whole at
every load (`writeDividendGaps`), and the yield report reads a record
day from it where the calendar dates no distribution of a payment
(YIELD.md §4).

Migration 0159 adds `stated_periods`: an account's summary of one
period as a document states it. A row holds the values at the period's
two ends, the net additions, the gain, the income, the fees and the
accruals, each NULL where the document states none. It is keyed by the
account, the period's last day (`as_of`) and the stating document. An
account source writes it, not a reference source: the fidelity kind
reads it from a silver's `stated_periods` table, which the svb
collector fills from the month-end workbook. No report counts a row as
a position or a flow. `wealthdb returns stated` sets each period
against the returns engine's monthly figures for the account.

Migration 0164 adds `exposure_reports`: a holding's split on one day as
a source reads it from a document, such as a private-markets fund's
quarterly report (adapters/ubs.md §13). It is keyed by the holding and
the day (`as_of`). Each dimension's split is JSON, in percent. An
account source writes it, and `gold.SyncExposure` copies each row into
the declarations as a parsed record (below).

Migration 0167 gives `fund_compositions` an `origin` and a
`valid_until`, and `fund_lines` a `region`. An account source may write
a fund's composition too, one it parses from the fund's own documents
(`parsed`). It holds until the fund's next document
([EXPOSURE.md](EXPOSURE.md) §4). A reference provider's composition is
`provider`.

The refdata collector learns what to fetch from the hidden read-only
subcommand `refdata-wanted`, whose JSON contract adapters/refdata.md §5
defines.

Migration 0125 also adds the tables of the `exposure` config block
(§5.1); migration 0128 adds those of its declarations ledger. Every load
replaces them whole after the per-source loads, beside the holding
aliases (`gold.SyncExposure`, §8.1), so an entry taken out of the
config or the ledger leaves on the next load:

- `exposure_proxies`: a fund whose composition stands in for another;
- `exposure_reference_funds`: funds fetched for their lines alone;
- `exposure_size_funds`: broad funds whose weights size stocks, each
  with the share of its markets it covers;
- `exposure_declarations`: the declarations ledger's records, a
  record's situs among them (migration 0129), and one parsed record per
  row of `exposure_reports`. `origin` says which: `ledger` or `parsed`
  (migration 0164). A parsed record's `line_no` is negative;
- `exposure_declaration_leaves`: each record's parts, the cross product
  of the dimensions it declares, each a share of the holding;
- `exposure_custody`: who keeps each source's assets;
- `exposure_settings`: one row of settings.

The exposure report reads the six reference tables of migration 0125
and the `exposure_*` tables ([EXPOSURE.md](EXPOSURE.md), §10.14).

Migration 0134 adds the tables of the `yield` config block (§5.1). A
load replaces them whole the same way (`gold.SyncYield`):

- `yield_curves`: each currency's curve, one series per tenor;
- `yield_withholding`: percent withheld at source, by situs country and
  tax wrapper;
- `yield_withholding_accounts`: the same, by situs country and account,
  for the accounts whose rate is not their wrapper's (migration 0168);
- `yield_rating_funds`: a rating-sliced fund's bucket or range;
- `yield_conventions`: coupon frequency and day count per market, the
  built-in defaults with the config's entries;
- `yield_cycles`: each sector's cycle, the built-in map with the
  config's overrides;
- `yield_settlement_cycles`: each market's settlement cycles, the
  built-in defaults with the config's markets (migration 0147, which
  seeds the defaults).

`horizon_months` has no table: the forward views read it from the
config (§10.15).

Migration 0135 seeds the conventions and the cycles with the built-in
defaults, so a gold migrated but not yet loaded reads them. It adds the
declarations ledger's yield records, replaced whole with the ledger
(`gold.SyncExposure`): `yield_declared_rates`, `_ratings`, `_dividends`
and `_terms` ([EXPOSURE.md](EXPOSURE.md) §9). It also adds two tables the
yield pass rewrites at the end of every load (§8.1):

- `instrument_terms`: the terms the sources state for each bond-like
  instrument (a bond, a mortgage, a loan or a convertible note). It
  holds the coupon, maturity, frequency, coupon kind, day count, call or
  make-whole call, rating and default grade. Each field says where it
  comes from: the source's record, the description, OpenFIGI's ticker or
  the market's convention.
- `account_rates`: each interest rate an account earns or pays, per
  currency, side and tier, with the day it holds from, a term deposit's
  end, and where it comes from: a source's record, read per snapshot
  where the source states it per snapshot, or a ledger record. A source
  states a rate under the payload keys of YIELD.md §3.

The macro `instrument_terms_at(asof)` lays the ledger's terms and rating
records in force on a day over `instrument_terms`, field by field. It
also gives an instrument that only a record describes, such as a fund's
factsheet figures (its duration and yield).

## 8. Load semantics

`wealthdb load` is structurally an **incremental view maintenance**
(IVM) operation in the data-streaming sense: gold is a
materialized view of silver, and the load procedure brings that
view up to date by processing exactly the bounded delta produced
since the last refresh. The same shape — watermark, delta window,
delete-in-window then re-emit — is what streaming engines do for
continuous queries, here triggered by a load rather than a stream.

### 8.1 The full sequence for `wealthdb load <id>`

```
1. open silver via the plugin
2. status     := plugin.Status()
3. watermark  := silver_sources.high_watermark    (-1 if first load)
   stamp silver_sources.observed_* := status.Observed*  (§6.4; also on a no-op)

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
       DELETE FROM position_lots  WHERE silver_source_id = ?
                              AND snapshot_at BETWEEN window.Start AND window.End;
       DELETE FROM cash_balances  WHERE silver_source_id = ?
                              AND snapshot_at BETWEEN window.Start AND window.End;
       DELETE FROM fx_rates       WHERE silver_source_id = ?
                              AND snapshot_at BETWEEN window.Start AND window.End;
       DELETE FROM transactions   WHERE silver_source_id = ?
                              AND occurred_at BETWEEN window.Start AND window.End;
       -- and each reference table (§7.6) by as_of, the same way.
       -- accounts and instruments are NOT deleted; they are upsert
       -- targets whose payload/last_seen_at refresh on every load.

       stream := plugin.Snapshots(window)
       loop:
           batch, more, err := stream.Next()
           fold dimensions: batch.Portfolios, batch.Accounts,
                            batch.Instruments (one record per entity — §8.4)
           insert facts:    batch.Positions, batch.PositionLots,
                            batch.CashBalances, batch.FxRates,
                            the reference records (§7.6)
           if not more: break
       upsert the folded dimension records

       stream := plugin.Transactions(window)
       loop:
           batch, more, err := stream.Next()
           insert facts:   batch.Transactions
           if not more: break

       DELETE FROM realized_lots  WHERE silver_source_id = ?;
       insert plugin.RealizedLots()          -- when the plugin offers them

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

Seven whole-file steps then run **after** the per-source loop, outside
its transactions, because none is a per-source fact:

1. The config-driven FX source precedence is stamped into
   `silver_sources` (§13.2).
2. The config's declared accounts are stamped into `accounts`
   (`declared_accounts`, §5.1), its `holding_aliases` into the
   table of that name, its `exposure` block into the `exposure_*`
   tables and its `yield` block into the `yield_*` tables (§7.6).
3. The class pass (`gold.RefineClasses`) reads OpenFIGI's security
   types over the classes of the instruments whose adapter takes them
   (§13.8). Every later pass reads the classes.
4. The deterministic enrichment pass re-asserts every spend and income
   verdict in gold (§10.10, docs/SPENDING.md §3). An own-account move's
   two legs routinely arrive from two different sources, so neither is
   recognisable until both have landed.
5. The lot pass replays the trades into lots (docs/LOTS.md §2). A move
   links two sources the same way.
6. The exposure pass rebuilds the exposure report's derived tables
   (§10.14). An alias ties a line one source loaded to an instrument
   another one did.
7. The yield pass rebuilds `instrument_terms`, `account_rates` and
   `dividend_gaps` (§7.6). It reads the exposure pass's instrument
   keys.

A failed FX stamp is a warning: it degrades a conversion. The other
six fail as errors. A failed pass does not stop the next one; the
load reports each. A failed class pass leaves the classes as the
adapters read them. A failed declaration stamp or enrichment pass
leaves own-account moves counted as spending, which is a wrong answer
rather than a degraded one. A failed lot pass leaves the positions the
load rewrote without a rebuilt basis, and nothing would say why. A
failed exposure pass leaves the exposure report reading the derived
tables of an earlier load, and a failed yield pass the yield report.
`reload` runs all seven the same way, on both its in-place and fresh-file
paths.

### 8.2 Why a windowed delete (not a full-replace)

A full replace (delete every event of the source, re-insert all of
them) would be correct but costs the whole history on every load.
The windowed delete is just as correct and bounded by the size of
the delta:

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
- One `PositionLotChange` per open lot of each such position, where
  the source states lots.
- One `CashBalanceChange` per silver cash-balance row similarly.
- One `FxRateChange` per silver FX-rate row similarly.
- `AccountChange` / `InstrumentChange` updates for every account
  and instrument referenced by the above. Re-emitting an unchanged
  dimension is fine (the gold-side upsert is idempotent); skipping
  a referenced dimension is **not** fine (the FK would dangle).

When `Transactions(window)` is called, the plugin emits one
`TransactionChange` per silver event row whose `occurred_at` is in
the window.

A plugin whose silver states realized lots also implements
`silver.RealizedLotReader`. `RealizedLots()` is not windowed: it
returns every realized lot the source states, and the load replaces
the source's `realized_lots` rows with them. A sale's tax document
arrives months after the sale and restates it, so no window derived
from dumps or event dates is sure to cover it.

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
                              THEN COALESCE(EXCLUDED.display_name, accounts.display_name)
                              ELSE COALESCE(accounts.display_name, EXCLUDED.display_name) END,
       base_currency   = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                              THEN COALESCE(EXCLUDED.base_currency, accounts.base_currency)
                              ELSE COALESCE(accounts.base_currency, EXCLUDED.base_currency) END,
       /* ...same pattern for the other nullable attribute columns and
          payload; non-nullable columns (account_kind, asset_class)
          keep a bare newer-wins CASE... */
       first_seen_at   = LEAST   (accounts.first_seen_at, EXCLUDED.first_seen_at),
       last_seen_at    = GREATEST(accounts.last_seen_at,  EXCLUDED.last_seen_at);
```

Per column: **the newest non-NULL observation wins, and an older
observation still fills a column no newer record has carried** —
recency arbitrates conflicts, absence never wins. NULL means "this
record doesn't carry the field", not "set it to NULL", in both
directions: a newer record's NULL doesn't erase an older value, and
an older record's value isn't discarded just because a newer,
field-less record exists. A merged source needs this: one feed may
carry an attribute (a statement-derived `tax_wrapper`, say) that a
newer feed's rows lack. The seen-at range expands in both directions
regardless. Re-emitting an older snapshot pulls `first_seen_at`
backward. The guard is per column, not one `WHERE` on the whole
UPDATE, so the seen-at expansion never depends on recency.

The loader folds each load's dimension emissions down to one record
per entity before the upsert runs (`gold.ChangeAccumulator`).
Adapters re-emit accounts / instruments alongside every snapshot
they walk, so emissions far outnumber entities, and executing the
guard once per emission made dimension upserts dominate load time.
The fold applies records in
arrival order with the guard semantics above. Because both fold and
SQL are symmetric-coalescing per column, the folded single upsert
stores exactly what record-by-record upserts would. Emission order
does not affect which attributes survive.

### 8.5 Silver-went-backwards detection

If the plugin's `LatestChangeNumber` is **strictly less than** the
stored `high_watermark`, gold refuses to load. Possible causes:

- Silver was restored from an older backup.
- Silver was rebuilt from bronze with a different parser order
  that produced a lower MAX(snapshot_at) (unlikely — snapshots
  are timestamp-keyed — but possible if the change-number encoding
  ever changes).

The escape hatch is `wealthdb reset <id>` followed by `wealthdb load
<id>`. There is no auto-recovery: it would risk silently dropping
data.

## 9. Reset semantics

`wealthdb reset <id>` runs:

```sql
BEGIN TRANSACTION;
DELETE FROM symbol_resolutions    WHERE silver_source_id = ?;
DELETE FROM spend_txn_enrichment  WHERE silver_source_id = ?;
DELETE FROM income_txn_enrichment WHERE silver_source_id = ?;
DELETE FROM lots                  WHERE silver_source_id = ?;  -- and lot_disposals, lot_realized,
                                                               -- lot_findings, lot_anchors
DELETE FROM fund_compositions     WHERE silver_source_id = ?;  -- and the other reference
                                                               -- tables (§7.6)
DELETE FROM transactions          WHERE silver_source_id = ?;
DELETE FROM realized_lots         WHERE silver_source_id = ?;
DELETE FROM fx_rates              WHERE silver_source_id = ?;
DELETE FROM cash_balances         WHERE silver_source_id = ?;
DELETE FROM position_lots         WHERE silver_source_id = ?;
DELETE FROM positions             WHERE silver_source_id = ?;
DELETE FROM instruments           WHERE silver_source_id = ?;
DELETE FROM accounts              WHERE silver_source_id = ?;
DELETE FROM portfolios            WHERE silver_source_id = ?;
DELETE FROM load_audit            WHERE silver_source_id = ?;
DELETE FROM silver_sources        WHERE silver_source_id = ?;
COMMIT;
```

The order is facts, then dimensions, then registration: the order the
documented foreign keys would need (§7.1). The silver database
itself is untouched. The watermark is removed with the
`silver_sources` row, so a follow-up `wealthdb load <id>` starts
fresh (treating `high_watermark` as -1).

`symbol_resolutions`, the two enrichment tables and the lot ledger go
with the rest because all are per-source derived data. The next
resolve, enrichment or lot pass regenerates them. Six tables deliberately
survive, two per family: the verdict stores `spend_merchant_categories`
and `income_payer_categories` are global knowledge keyed by signature,
with no source column and verdicts that were paid for, and the scope
stamps `spend_account_scope`, `income_account_scope`,
`cashflow_account_scope` and `cashflow_wrapper_sides` are configuration
stamped into gold and re-stamped whole by every enrichment pass — the
`fx_priority` precedent rather than source data. Lifecycle table:
docs/SPENDING.md §8. The invariant is pinned by
`TestResetClearsEnrichmentKeepsMerchantStore` and
`TestResetClearsBothOverlays`.

`wealthdb reset -a` runs the same per silver source. A full rebuild is
`wealthdb reload -a`: every source-derived table is rebuilt into a fresh
compact file, which is swapped over the live path. The three paid
stores are carried across (`carryVerdictStore`, once per entry in
`paidStores`): the merchant store, the payer store and
`symbol_resolutions`. A carried taxonomy store has any retired spelling
in its category column respelled to its successor
(`canonical.RetiredDetailed`), so a carry from a file older than
migration 0111 brings none into the rebuilt file. `report_returns` does
not survive the swap, nor do `report_gains`, `report_exposure` and the
serving tables. `web-materialize` writes them (§10.9, §10.13, §10.14,
web/DESIGN.md §8), so the rebuilt file has none until a materialization
follows.
`reload -a --in-place` keeps the live file instead. It forgoes the
compaction and leaves these tables alone, but the per-source reset
still clears `symbol_resolutions` there. Removing the gold DB and
re-running `wealthdb init` is the manual fallback.

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
SELECT p.*
  FROM positions p
  JOIN pos_active_at([:as_of_epoch]) a
    ON p.silver_source_id    = a.src
   AND p.account_external_id = a.acct
   AND p.snapshot_at         = a.snap;
```

`pos_active_at` gives each account its active snapshot at the instant
(migration 0140, §10.7). `positions_at` is this query with the account
and instrument labels. `cash_active_at` is `pos_active_at`'s twin for
cash, and `cash_chosen` picks one balance per account and currency
from it. Every point-in-time report picks through `pos_active_at` or
`cash_active_at`. The compound PK `(silver_source_id, snapshot_at,
account_external_id, position_key)` makes the join cheap.

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
SELECT p.*
  FROM positions p
  JOIN pos_active_at([:as_of_epoch]) a
    ON p.silver_source_id = a.src AND p.account_external_id = a.acct
   AND p.snapshot_at = a.snap
 WHERE p.asset_class = 'fixed_income'
   AND p.vehicle IN ('bond', 'fund');
```

### 10.4 Holding-period filter (future, sketch)

```sql
SELECT * FROM positions
 WHERE asset_class = 'public_equity'
   AND acquisition_date <= :now - INTERVAL 1 YEAR;
```

`acquisition_date` is set only where silver states it: the
private-market sources, manual, the synthetic demo, and any position
with stated lots, which takes its earliest lot's date (§7.4).
Elsewhere it is NULL, so this filter sees those positions only
(§13.3). `position_lots` answers the same question per lot, and
`wealthdb gains lots` lists it.

### 10.5 Net worth by bank

This is implemented as the `report_accounts` table macro (migration
0021), which rolls each account's positions and cash at its active
snapshot (§10.1) up to a single converted value per account, grouped
under its silver source. The conversion to `:output_currency` is the
§10.6 `fx_daily` lookup applied per line; the macro takes
`:output_currency` and an as-of as parameters and is invoked from the CLI
(`SELECT * FROM report_accounts(?, ?)`). `web-materialize` reads its
multi-currency twin, `report_accounts_multi` (§10.8).

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

**Which day's rate.** A line converts at the rate of its own
snapshot's day. The as-of passed to a macro picks the snapshot, not
the rate: the CLI passes the `-d` date (default today), and
`web-materialize` passes `MAX(BIGINT)`, so each account's latest active
snapshot.
There is no linear interpolation between rates.

**Cross-silver precedence** is data, not runtime state: a stamped
`silver_sources.fx_priority` column that `fx_norm` reads. §13.2
describes it.

### 10.7 History reports (time series)

For charting value over time, five snapshot reports have a daily series in
every reporting currency (§10.8): `report_global_history_multi()`,
`report_sources_history_multi()`, `report_accounts_history_multi()`,
`report_portfolios_history_multi()` and `report_positions_history_multi()`.
The accounts series also has a single-currency form,
`report_accounts_history(p_ccy)`, which the returns engine reads (§10.9).
The other four have none: migration 0124 drops them, as nothing read them.
Each emits one row per entity per UTC day, from the first snapshot to today,
with the value **carried forward** — on a day with no new snapshot the most
recent snapshot's value is repeated. Positions and cash are carried
independently (a source's two series can diverge), and the active snapshot is
chosen by `snapshot_at` so multiple same-day dumps resolve to the latest. Each
line is valued **once** at its own snapshot's FX day, then expanded onto a daily
spine via an at-or-before ASOF join — so per-day compute stays cheap. A day an
entity has no line on at all is omitted (a day it is observed at zero on is
not: that emits an explicit 0 row). `report_global_history_multi` is
`Σ report_accounts_history_multi`. `report_cash_history_multi()` splits
the accounts series' cash by currency: one row per account, currency and
day, summing to the account's cash (migration 0171). The serving tables
hold the multi-currency series for the Metabase models (§10.8).

The active snapshot is resolved **per key per day** — migration `0051`,
`hist_active_pos()` / `hist_active_cash()`. Each key is ASOF-joined onto the day
spine on its own snapshot series, so it contributes its own most recent
observation at or before the day and the day's total is the sum across keys. The
key is `(source, account)` for positions and `(source, account, currency)` for
cash, where every balance row is its own observation. One active snapshot per
*source* would be right only for a source that writes every account in one run.
Where a deposit run, a card run and a statement backfill each land under their
own `snapshot_at`, it would report whichever run was last and drop every account
the others carried. Five rules complete the picture:

- **Zero is an observation.** The cash line bases keep `amount = 0` rows, so an account paid down to zero carries the
  zero, not the balance before it, and a zero line converts to 0 in every
  reporting currency instead of following the FX chain — no rate exists for a
  currency nobody quotes, and a NULL there would blank the whole entity-day's
  sum rather than just that line. Positions need no filter of their own: the
  carry unit is the account's whole snapshot, so a holding absent from that
  account's next snapshot is sold rather than carried, and an explicit $0
  position row (`silver.ClosureMarkerBatch`) reads as zero. The per-position
  series (`report_positions_history_multi`) keeps the plain FX chain, as its
  point-in-time twin does — a display row has no sum for a NULL to poison.
  The point-in-time reads take a zero cash row as an observation too.
  `cash_chosen` leaves zero balances out of its output, after the pick.
- **A later run that re-covers the key's company ends it.** Absence is evidence
  of closure exactly when the run that produced it was in a position to report
  the key: a key leaves the series at the first later snapshot of its source
  that covers every OTHER key observed alongside it in its own last snapshot
  *and still reported at that snapshot*. Company that left in the same run is
  not company the source could re-cover, so it is not required — otherwise two
  accounts closing in one nightly dump would each hold the other's ending open.
  A nightly full dump therefore ends a closed account on the next dump, whether
  one account closed or several; a card-only or
  deposit-only run covers none of another run's keys and ends nothing. A key
  observed alone has no company to re-cover.
- **The key's own cadence holds a recent observation open** (migration `0144`):
  - The re-cover ends a key's last observation only once its source's newest
    snapshot in that table lies more than one and a half cadences past it.
  - A key's cadence is the median gap between its own observation days. A key
    observed on one day only takes a month.
  - A positions key needs no grace where its re-cover run states the account's
    cash. That run reported the account, and its holdings were gone, as after
    a sale of every one.
  - The clock is the key's own table. A run that writes only cash moves no grace
    of a positions key.

  The cadence alone ends nothing. Some sources report a few accounts only in
  bursts, beside a nightly run of others. A cadence rule alone would drop them
  between bursts. The re-cover keeps them while each burst is a run of its own:
  no nightly run covers their company.
- **The source's clock bounds the rest.** A key no re-cover has ended stops
  contributing once its source has produced a snapshot more than
  `hist_carry_days()` (365) days after that key's last observation. The evidence
  is always the source's own activity, never the calendar — a source that stops
  running supersedes nothing, so its accounts keep their last values to the end
  of the spine. The price is that a key a partial-run source stops reporting
  without a zeroing row lingers for up to a year; an explicit zero row ends it
  immediately, which is what the adapters emit on closure.
- **Both endings apply to a key's LAST observation only**, so neither can open a
  hole in the middle of a series: a card reporting per statement cycle, a
  holding marked less often than yearly and a deposit re-dumped nightly sit in
  one source without expiring each other for as long as their runs carry their
  own `snapshot_at`. A slow key whose last observation *did* share a run with a
  faster sibling is ended by that sibling's next run, which covers the whole
  peer set. That ending waits for the slow key's grace (above).

The point-in-time reports take each key's active snapshot by the same rule, at
any instant (migration `0140`):

- a key's candidate is its newest observation at or before the instant;
- a candidate that is not the key's last observation stands;
- the key's last observation stands while the instant is before its ending
  snapshot, and while the source's clock at the instant is at most
  `hist_carry_days()` days past the observation's day. The ending snapshot is the
  re-cover, once the grace or the account's own cash lets it end.

Each observation so holds over a span (`pos_key_spans()` /
`cash_key_spans()`): from its own instant to the key's next observation. The
last one holds to the earlier of its two endings, or for ever.
`pos_active_at(instants)` / `cash_active_at(instants)` give the observation each
key holds at each instant. The re-cover ending, with its cadence, has one
definition (`pos_key_ends()` / `cash_key_ends()`), which `hist_active_pos()` /
`hist_active_cash()` and the cash flow's mortgage split read too. At a day's last
second the rule is the history's day rule.

What follows for readers of these macros:

- **A point-in-time read at a day's last second equals the history's day**, in
  value, for every source whose lines all have a rate to the currency. A zero
  line no rate converts reads 0 in the history; the point-in-time line bases
  leave it NULL, or drop it for cash. They reconcile on values, not on rows: the
  point-in-time reports (§10.1) carry a row for every account in the dimension,
  0 when its active snapshot gave it no lines, while the history emits rows only
  for entity-days that have lines.
- **The newest read.** An account reported only in some of a source's runs
  stands at the runs that report its companions for one and a half of its own
  cadences:
  - A quarterly account of a monthly statement so holds its last quarter end
    until its next one.
  - A new quarterly account, seen at one quarter end only, takes a month as its
    cadence. It drops out from its second month end until its second quarter
    end.
  - An account that closed without a zeroing row holds as long, then reads as
    closed from the run that left it out.
  - So does an account that sold every holding, where its runs state no cash
    of it.

  Gold cannot tell these shapes apart sooner without a sign from the source of
  which accounts a run could report.
- **A past read can change.** A read of a past instant changes when a later run
  passes a key's grace: the key then reads as closed from its re-cover run on.
  It changes back once the key is observed again.
- **`snapshot_at` is the account's own active snapshot**, in
  `report_positions_history_multi` and in every point-in-time row, so it varies
  per account within a source. A source's `snapshot_at` is the newest of its
  accounts'.
- **Cost:** the day spine is crossed with keys rather than with sources, so its
  cardinality grows by the accounts-per-source factor. A point-in-time read
  crosses its instants with the keys. Each read computes every key's span and
  ending over the whole of `positions` or `cash_balances`. One macro defines
  each key's observations (`pos_key_obs()` / `cash_key_obs()`, migration
  `0145`). A materializer of the web refresh reads many instants of one gold,
  so it computes the endings and the spans once. It pins them in its
  transaction, where temporary macros of the same names read them
  (`gold.beginPinned`). Every other read computes them, so no table of spans
  has to follow a load.

### 10.8 Multi-currency reports (Metabase)

The Metabase models need a value column **per reporting currency** (USD, CHF,
EUR, GBP) so the currency is picked by picking a column — Metabase native
*models* don't expose template-tag parameters to questions built on them, so a
"target currency" widget wouldn't reach the charts. Calling
`report_x(MAX, '<ccy>')` once per currency and joining would re-run the
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
  every source macro; `source_lines_base(p_asof)` is the point-in-time line
  base behind `report_sources` / `report_sources_multi` (the history variants
  build their own all-snapshot lines, like the portfolio history macros).
- **Single-currency macros on the same bases.** `report_accounts`,
  `report_portfolios` and `report_accounts_history` read these bases too.
- **`report_x_multi` macros.** `report_{global,sources,accounts,portfolios,positions}_multi(p_asof)`,
  `report_transactions_multi(p_from, p_to)`, and the five `report_*_history_multi()`
  emit one value set per currency — `{positions_value,cash_balance,total_value}_{usd,chf,eur,gbp}`
  plus the base trio. Where a single-currency form exists, each `_<ccy>`
  column equals it for that currency by construction (same base, same
  conversion order; see the helper below). They are Metabase-only, so they
  emit **DECIMAL** money directly (no CLI VARCHAR-trim round-trip); the
  `web/provision.py` wrapper then only casts epoch columns to TIMESTAMP.
- **Serving tables.** The dashboards do not call these macros.
  `web-materialize` copies each one's rows into a `serving_*` table
  (migration 0132, [web/DESIGN.md](../../web/DESIGN.md) §8). The `web_*`
  views and the Metabase models read those.
- **One conversion helper** (migration 0114). Every `_multi` macro converts
  through the same three objects:
  - `fx_reporting_legs`: each currency's rate to each reporting currency;
  - `fx_reporting_bridges`: the CHF and USD rates to each reporting currency;
  - `fx_reporting_value(amt, ccy, target, legs, bridges)`: one value.

  A macro joins each view once and calls the scalar macro per currency. The
  order is the single-currency macros' own: identity, direct, through CHF,
  through USD. Each view lies on a grid of every day one of its rates is quoted,
  so one ASOF lookup finds the rate the separate lookups did, and the products
  keep their order, so the values are the same to the bit. The reporting set
  lives in the two views, the scalar macro and each macro's column list.
  Adding a currency is a new migration that re-issues them, plus the returns
  materializer's list and `web/provision.py`'s `REPORTING_CURRENCIES`.
  `web/test_provision.py` checks that the three agree.
- **`account_kind` on the transaction macros.** `report_transactions` and
  `report_transactions_multi` carry the owning account's `account_kind`
  (migration 0039), so a consumer can fence a kind out: a card's finance
  charges and annual fees are `interest` / `fee` transactions that would
  otherwise read as investment income and portfolio costs. NULL when the
  transaction's account is absent from `accounts` (a LEFT JOIN), so a fence
  must keep NULLs.
- **Merchant and spend category on the transaction macros.** The same two
  macros also carry `merchant_name`, `spend_primary` and `spend_detailed`,
  resolved by `spend_txn_categories()`: the overlay's precedence lattice,
  one definition that `spending_lines_base` and the transaction reports share
  (§10.10, [SPENDING.md §3](SPENDING.md)). NULL for
  every row the enrichment pass does not reach. Unlike the spending reports,
  these keep a row the matcher called an own-account move and show what it was
  categorised as: `wealthdb transactions` is the whole ledger. `merchant_name`
  resolves in three steps ([SPENDING.md §7](SPENDING.md)): a delta row shows its
  issuer label or nothing, otherwise the store's name for the signature,
  otherwise the row's own `merchant_signature` verbatim.
- **Lockstep on the transaction macros.** `gold.TransactionsBetween` runs
  `SELECT *` with a positional scan, so every column added to these macros must
  land in `gold.TransactionRow` and the scan list in the same change. The
  returns flow loaders are safe by construction: they project named columns.
- **`transactions.check_number`** (migration 0075) is the number written on a
  paper cheque drawn on the holder's own account, kept so a row can be matched
  against the holder's own paper records. An adapter sets it **only on an
  outflow**: the field names an outgoing payment, and the sign test is what
  keeps it meaning that, because a bank may put its own instrument reference in
  the same silver column on an incoming credit and a deposit export may head
  that column "Check or Slip #". Deliberately not a transaction kind — a cheque
  is an instrument, not a distinct economic event — and deliberately a column
  rather than `payload`, which holds what could *not* be canonicalised.
- **`transactions.asset_class` / `.vehicle`** (migration 0097) are what a
  securities trade says it TRADED, in the same 2-D taxonomy `positions` and
  `instruments` carry (docs/TAXONOMY.md). An adapter sets them **only for what
  the instrument cannot answer**: both NULL on every row that is not a
  securities trade and on every trade whose instrument is known, one half
  alone where that is all the trade adds (an option resolves to its underlying
  and states `option` in the vehicle, leaving the exposure to it), and both
  where the feed named the kind of thing but no instrument. Restating a known
  instrument's pair here is the one thing they are not for: the copy would
  freeze at load time and mask the dimension, which `cashflow_txn_nodes` reads
  in preference (it coalesces the trade's pair over the instrument's).
  The writer gates each half against its own vocabulary and the two together
  as an admitted pair, so a wrapper value cannot reach the exposure column.
  Denormalised by decision: a transaction points at no position and no lot.
- **`transactions.instrument_hint`** (migration 0098) is the token an adapter
  resolved a row's instrument from when that resolution found nothing — a
  Swiss valor, a fund name, a ticker, whatever the feed states. The svb
  statement archives resolve in the collector instead, the one place the
  statements themselves can settle a row's link (`collectors/svb/DESIGN.md`),
  so there the collector states the token in the row's payload and the
  fidelity adapter carries it. NULL where no lookup was needed or it
  succeeded, so `instrument_external_id IS NULL AND instrument_hint IS NOT
  NULL` is exactly the set a `transaction_instruments` entry can still close,
  and `wealthdb transactions -C +instrument_hint` is how that set is read. `wealthdb resolve-symbols` does not put such a row to its
  model: the token is config's to close. See §13.8.
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
DuckDB macro. The computation lives in Go — the CLI computes on demand, and
`web-materialize` snapshots the same engine's output into `report_returns` for
the dashboards — as a hybrid:

- **SQL assembles** the inputs from existing macros. The per-account
  carry-forward value series comes from `report_accounts_history(p_ccy)`
  (§10.7). Its ASOF inner join omits pre-inception days, so a boundary before
  an account's first snapshot reads as NULL ("not yet alive"), not 0. External
  flows come from `report_transactions(from, to, p_ccy)` (`net_amount`
  converted at `occurred_at`). The adapter kind (`silver_sources.silver_kind`)
  and the distinct snapshot days per account complete the inputs.
- **Go computes** (`internal/returns`, pure and unit-tested): Modified Dietz
  per sub-period, geometric chaining, XIRR (Newton plus bisection), the
  per-source flow policy, and the synthetic onboarding and closure flows. The
  gold orchestration is `internal/gold/returns.go` (`RunReturns`), which
  aggregates the per-account spine to every grain.

Each source registers a `ReturnsPolicy` beside its adapter
(`internal/silver/<source>/policy.go`), resolved by adapter kind through
`internal/returns/returnspolicy.go`. The engine names no source: it reads
the policy's knobs. §5.6 lists what config may override.

The flags are `--method twr|mwr|both`, `--period monthly|quarterly|annual|total`
(default quarterly), `--annualize`, `--netting on|off` and
`--inception full|strict`; `wealthdb help returns` describes them. Every n/a
carries a reason and every approximation a tag in the `quality` column.

`returns stated` is a side view beside the four grains, not a fifth
grain. It sets each period a document states for an account
(`stated_periods`, §7.6) beside the engine's monthly figures for that
account. It shows the stated net additions against the engine's net
flow, and the stated ending value against its end value. The stated
figures create no flow. `wealthdb returns stated -h` lists its
columns.

[RETURNS-NOTES.md](RETURNS-NOTES.md) owns the method: the snapshot-aligned
headline, flow classification per source, card invisibility, staggered
inception and closure, netting, the policy knobs and every quality flag.
Three facts matter to the rest of gold:

- **Historic FX.** Values and flows convert at their own day's rate
  (§10.6), so currency movement is part of the return.
- **Credit cards are returns-invisible** at every grain, keyed on
  `account_kind = 'card'` in the engine. With cards loaded, the returns global
  therefore differs from `report_global` by the card balances.
- **Returns are not additive across grains.** `global == Σ accounts + hidden
  plumbing` holds for values, not for returns (§5.7).

### 10.10 Spending reports

The spending family ([SPENDING.md](SPENDING.md)) charts the population
`spending_lines_base(from, to)` defines: the scoped accounts and
spend-side kinds, with the category resolved, less own-account moves,
capital deployed, debt repayments and crossings to untracked plans.
[SPENDING.md §1 and §2](SPENDING.md) list the excluded values and say
why `card_spend` and `gift` stay in.
Three reports sit over it, with VARCHAR money in one currency for the
CLI. The transaction-grain report also has the `_multi` sibling §10.8
describes, with DECIMAL money in every reporting currency. The web
reads that one (`web_spending`, below) and buckets the lines itself:

| macro | grain |
|---|---|
| `report_spending_summary(f, t, ccy, period)` | one row per period bucket |
| `report_spending_categories(f, t, ccy, period, level)` | one row per (bucket, category), plus its share |
| `report_spending_transactions(f, t, ccy)` | one row per spending line |

`wealthdb spending <view>` (§4.12) is one view per macro and adds
nothing to them: the `--period` name maps to the `date_trunc` part,
`--level` passes through, and the Go side is a positional scan plus a
column registry.

- **Sign-split magnitudes.** `net_amount` is canonically signed, so a plain SUM
  is neither a period's spending nor its returns. `spend` and `refunds` are both
  **positive** magnitudes and `net_spend = spend − refunds`.
- **Period bucketing.** `spend_period_bucket(period, at)` emits the bucket's
  UTC-midnight epoch second via `date_trunc` (`day` | `week` | `month` |
  `quarter` | `year`), or NULL for the single `total` bucket. `total` is not a
  `date_trunc` part, and DuckDB binds the call even on a CASE branch it
  discards, so the substitution happens **inside** the call. The timestamp
  comes from `epoch_ms`, not `to_timestamp`: the latter is TIMESTAMPTZ and
  would truncate in the session's zone rather than UTC.
- **Shares over magnitudes.** A category's share is `|net_spend|` over the
  bucket's `Σ|net_spend|`, so a net-positive category cannot shrink the
  denominator and push the others past 100%; a bucket's shares sum to 1.
- **`(uncategorized)` is a label.** The backlog is materialised **before** the
  GROUP BY at both levels, so it groups like any other category instead of
  rendering as a blank row. At transaction grain the category stays NULL, and
  `web_spending` labels it at the view.
- **`merchant_name`** resolves as [SPENDING.md §7](SPENDING.md) describes:
  nothing on a delta line except a card bill's issuer, else the store's
  name, else the line's own `merchant_signature`. Every consumer inherits
  the column by name: the spending reports,
  `report_spending_transactions_multi`, `report_transactions`,
  `web_spending` and both CLI views.
- **Reconciliation.** Σ categories == the summary bucket, for every period and
  level. The identity is structural (one base, one sign split), so it cannot
  catch a wrong population: an over-eager internal-transfer match removes a row
  from both sides at once. `spend_enrichment_population` is the layer
  **before** the exclusion, so a removed row is still there with the tier that
  removed it ([SPENDING.md §6](SPENDING.md), the matched-pair canary).

Two serving views reduce timestamps with `epoch_ms`, so Metabase syncs them
like tables:

- **`web_spending`**: transaction grain over
  `serving_spending_transactions`, the materialized rows of
  `report_spending_transactions_multi`, with the account's identity for the
  picker and both category levels for the breakdowns. NULL renders as
  `(uncategorized)` here.
- **`web_card_balances_history`**: a card account's owed balance per UTC day,
  carried forward, in the reporting currencies
  (`serving_card_balances_history`, the rows of
  `report_card_balances_history_multi()`). One row per (source, account,
  **currency**), with the native balance beside the converted values. It
  resolves a day per key as §10.7 does, but bounds nothing at the end: a card
  its source stops reporting keeps its last owed balance to the end of this
  view's spine, where the account-history macros end it. Zero balances are
  kept: a paid-off card is at zero, and dropping the row would leave the
  series owing money forever.

Both views label an account through the shared `account_label` macro:
`<name> (<source> <kind>)`, where the name falls back to the account's id
when an adapter leaves it unset. A name alone says neither which
institution holds the account nor whether it is a deposit account or a
card, and names collide across sources. The name comes first, so a chart
that truncates a long label clips the annotation, not the identity. The
bare name and the id stay projected beside it.

### 10.11 Income reports

The spending macros of §10.10, read in the other direction over the
income population ([INCOME.md](INCOME.md)):

- `income_scoped_accounts()`, `income_enrichment_population(f, t)` and
  `income_lines_base(f, t)`;
- `income_txn_categories()`, the resolution, which names the payer as the
  instrument where a line carries one;
- the FX pair `income_lines_outccy` / `income_lines_multi`;
- `report_income_summary`, `report_income_types` and
  `report_income_transactions`, with a `_multi` form of the last;
- `web_income`, `web_spending`'s shape over the income lines. `(uncategorized)`
  labels the type columns but not the payer, which a delta line never has.

[INCOME.md](INCOME.md) owns how these differ from the spending set: the
kind floor read over the verdict store, the `distribution` floor, and the
`withheld` memo on the summary.

`report_transactions` carries `payer_name`, `income_primary` and
`income_detailed` after the spending trio. It is read by a positional
scan (`gold.TransactionRow`), so the macro, the row struct and the scan
list move together in one change.

### 10.12 Cash flow statement

The third reading of the same engine. Its statement is mostly SQL
([CASHFLOW.md](CASHFLOW.md) is the spec):

- `cashflow_wrapper_sides` holds each tax wrapper's side of the household
  boundary, and `cashflow_account_scope` the pool's exclusions. Every
  enrichment pass re-stamps both from config.
- `cashflow_pool_accounts()` is the account gate: every account that
  `cashflow.accounts` does not exclude and whose wrapper sits on the
  household side.
- `cashflow_txn_nodes(f, t)` maps every transaction to a
  `section.class.group` node and a disposition that says whether it is a
  line at all. `cashflow_lines_base(f, t)` is that, restricted to the pool
  and to the lines. It reads a mortgage as repaid where its last
  observation ends, by the shared ending of §10.7, which holdings read
  too ([CASHFLOW.md §5](CASHFLOW.md)).
- `report_cashflow_summary`, `_flows`, `_sankey` and `_transactions` sit
  over it, with the FX pair `cashflow_lines_outccy` /
  `cashflow_nodes_outccy`; `web_cashflow` serves the dashboard.
- `report_cashflow_reconciliation` and `report_cashflow_coverage` are
  diagnostics. The first compares the observed balances of
  `cashflow_balance_pool()` (the pooled accounts that carry both
  transactions and balances) with the flow over the dates those balances
  cover. The second does that per account and period, in the account's own
  currency, never converted.
- The cash flow projection (`cashflow projection`, CASHFLOW.md §12) is Go.
  `gold.CashflowPlanLookback` reads the look-back's parts over
  `cashflow_lines_outccy`. `gold.PlanMonths` splits today's book's
  `ProjectionRows` by the pool's accounts (`gold.CashflowPool`).
  `gold.CashflowPlan` sets the look-back against them.
  `gold.PlanStanding` gives the mortgages the book holds and their
  maturities. `cashflowPlan` in
  `cmd/wealthdb/cmd_cashflow.go` joins them for the CLI and the MCP
  `cashflow` tool. A web refresh stores the look-back's parts in
  `report_cashflow_plan` (migrations 0149 and 0169), the other income by
  class and account; `web_cashflow_plan` serves it, and
  `web_cashflow_pool` the pool's accounts.

The node assignment reads each family's resolution macro, never its
`_lines_base`, because several verdicts it needs are exactly the ones a
base excludes. It is SQL because the dashboard needs the same assignment
the CLI uses after its pickers apply. [CASHFLOW.md §4](CASHFLOW.md) owns
the rules.

### 10.13 Gains reports

The gains macros read the cost basis columns and lot tables of §7.4 and
§7.5. [GAINS.md](GAINS.md) defines every figure they return, and
[LOTS.md §5](LOTS.md) the lot engine's ledger.

Shared pieces:

- `fx_rates_to(ccy)` and `fx_amount`: one conversion path, in the order
  every single-currency report applies. `fx_rates_to` builds its grid over
  the currencies gold's figures are in, not every coin a crypto source
  prices.
- `positions_at(instants)`: the point-in-time rule of §10.1 at several
  instants at once, and `gains_position_lines` over it.
- `realized_lots_in`: the one place the realized-gain formula lives.
- `gains_events`: sells, in-kind moves and corporate actions. A sale the lot
  engine realized against a stated basis counts as documented, through the
  transaction each `lot_realized` row names.
- `gains_windows`: one row per bucket, account and instrument, which every
  gains report sums, so the grains and the positions view reconcile by
  construction. It maps every key through `holding_aliases` (the config
  block, written at every load) and `instrument_aliases()` (a source's
  CUSIP key to the ticker it also keys the security by) before a bucket's
  start meets its end.
- `basis_applies`, `stated_basis`, `basis_missing`, `gain_needs_cost`,
  `documented_years`, `documented_txns`, `lot_obs_at` and
  `lot_key_accounts`: the predicates and keys the readers and the lot pass
  share.
- `lots_at(instants)`: the engine's lots open at each instant.
- `lot_sources()`: each source's row of the last lot pass (mode, grain,
  methods, counts), which `gains_windows` turns into quality flags.

The readers are `report_gains_buckets(from, to, ccy, period, grain)`,
`report_gains_positions`, `report_gains_realized`, `report_lots`,
`report_gains_coverage` and `report_gains_check`. `report_positions` and
`report_cash` carry the cost basis columns after their others.
`report_positions` and every gains reader but coverage and check take
`p_missing := 'ignore'` and read a missing cost under it.

The `_all` forms (`gains_windows_all`, `gains_position_lines_all`,
`realized_lots_in_all`, `fx_rates_into`) take lists of output currencies
and readings of a missing basis, and each row names its own. Buckets,
events, findings and what a holding is at an instant depend on neither,
so one evaluation finds them once. The single-currency macros are these
over one-element lists.

The Metabase Gains dashboard reads `report_gains`, a derived table like
`report_returns` (§10.9). `web-materialize` rewrites it from one call of
`gains_windows_all` at every web refresh: `gains_windows(0, today, CCY,
'month')` for every reporting currency and reading, less the figures in a
holding's own currency. It carries `sold_gain_x` and `held_change_x`, the
split of a bucket's gain by what prices did ([GAINS.md §2](GAINS.md)). The
`web_gains` view labels its accounts, joins in their tax wrapper,
exposes `missing_basis` for the dashboard's picker, and reads a sale no
snapshot classes as the asset class `unknown` (migration 0170).

### 10.14 Exposure reports

The exposure macros look every holding through to what it holds.
[EXPOSURE.md](EXPOSURE.md) defines every figure they return.

Migrations 0126, 0128, 0129, 0131, 0137 and 0140 to 0142 define the
derived tables and the macros. Migrations 0127, 0130, 0133 and 0138
define the dashboard's tables and views (below). A macro's last
definition is the one in force.
The exposure pass (`gold.RebuildExposure`) runs after the lot pass at
the end of every load and reload, and rewrites the derived tables:

- `exposure_labels`, `exposure_countries`, `exposure_sic`: the label
  maps and vocabularies of `internal/canonical`;
- `fund_line_labels`: every label the fund lines carry, per provider,
  with the value it maps to;
- `instrument_keys`, `security_aliases`: each instrument's security
  key, and the key each alias reads as;
- `securities`: one row per security, its best-known attributes and
  where each came from, its market cap and its size among them
  (EXPOSURE.md §3);
- `security_underlyings`: the share each depositary receipt, or each
  line a report names by name alone, stands for, and how it was found;
- `exposure_situs`: each holding's legal situs and its basis, from each
  day its reading changes (EXPOSURE.md §13; `exposure_situs.go`).

The macros:

- `exposure_key(key)`, `exposure_fund_keys()` and `exposure_proxy_refs()`:
  a key read through the aliases, every key known as a fund, and the
  config's proxies under their keys;
- `exposure_holdings(asof, ccy)`: the positions and cash at an instant,
  each account at its active snapshot (§10.7), each line valued as
  `report_accounts` values it, an account it leaves out of the total
  left out whole, with its key read through the aliases;
- `exposure_comps()`, `exposure_attributes_at(asof)` and
  `exposure_funds_at(asof)`: the composition that describes each fund at
  the instant (the temporal join, proxies, leverage);
- `fund_filings_at(asof)` and `fund_filing_kinds(asof)`: each fund's
  filing at the instant, and the coupon kind, default flag and arrears
  flag it states of each line's security (migration 0141);
- `exposure_declarations_at(asof)`: the declaration that describes each
  holding's exposure at the instant (migration 0128), a record that
  states only a situs aside (migration 0129). On an equal date the
  ledger's record wins over a parsed one (migration 0164);
- `exposure_situs_at(asof)`: each holding's situs at the instant
  (migration 0129);
- `exposure_leaves(asof, ccy, country)`: the look-through, one row per
  leaf, with its class, sector, country, currency, a stock's size, flags,
  `how`, and its holding's situs and basis. It expands only the funds a
  holding reaches within three levels, and splits a declared holding
  into its parts. A fund's line carries what it states of its debt and
  its issuer, the LEI from `fund_lines.lei`, which the load reads off
  the line's payload (migration 0137). A line that states no coupon
  kind takes its fund's filing's (migration 0141);
- `exposure_yield_leaves(asof, ccy)`: the leaves with each one's
  maturity and its bucket, coupon kind, rating, cycle and issuer, a
  holding's from its terms on the day; `exposure_credit_leaves` and
  `exposure_maturity_leaves` split a leaf no rating or maturity
  describes by its fund's page (`exposure_page_breakdown`). Their
  helpers read a label, a band, a maturity and a name, and give a
  leaf's bucket: `exposure_label_words`, `exposure_rating_label` and
  `_grade`, `exposure_maturity_label`, `_words`, `_band`, `_years`,
  `_upper` and `_bucket`, `exposure_issuer_text` and `_name`,
  `exposure_credit_bucket`, `exposure_credit_unknown` and
  `exposure_maturity_split` (migration 0137);
- `exposure_quality(flags, value)` and `exposure_situs_quality(basis,
  value)`: the aggregates behind the `quality` columns;
- `exposure_divergence(asof)`: how far each fund read from a report lies
  from its proxy, by sector and by country;
- `report_exposure_classes`, `_sectors`, `_sizes`, `_countries`,
  `_currencies`, `_credit`, `_maturities`, `_coupons`, `_cycles`,
  `_issuers`, `_situs`, `_securities`, `_funds` and `_coverage`: the
  views.

Values leave the leaves as DOUBLE: the look-through splits a line by
weight. Each view's total equals `holdings global`; the printed rows,
rounded to the cent, agree within a cent per row.

`wealthdb exposure --period` runs a view once per period of the window
(EXPOSURE.md §7). Its instants come from `gold.PeriodEnds`, as the
dashboard's below do.

The Metabase Exposure dashboard reads `report_exposure` (migration
0127), a derived table like `report_gains`. `web-materialize`
(`gold.MaterializeExposure`) rewrites it at every web refresh. It holds
the leaves at the last second of every month from the first holding,
and at the end of today:

- grouped by the dimensions the dashboard charts: class, sector,
  country, region, currency, size, held directly or through funds,
  `how`, the situs with its basis, and the holding's source and its
  account's tax wrapper, which the pickers narrow to (migration 0130);
- in every reporting currency. The tree is walked once per instant, in
  US dollars. A leaf is a fixed share of the holding it descends from.
  So its value in another currency is its dollar value times the
  holding's value in that currency, over the holding's dollar value. A
  holding no rate converts to US dollars is left out in every currency.

The walk with each leaf's debt and issuer dimensions
(`exposure_yield_leaves`) costs several times the plain one, so two more
tables hold it at today and the last second of each of the 12 months
before, the yield tables' instants (migration 0138). Each statement
walks the instant for itself, so its memory is freed before the next:

- `report_exposure_debt`: the buckets of the dimensions `credit`,
  `maturity`, `coupon` and `cycle`, by source and tax wrapper, a leaf
  split by its fund's page as the views split it. Unlike the `credit`
  view, it keeps no split by held directly or through funds;
- `report_exposure_issuer_ranks`: the 100 largest issuers per currency
  and the `(none)` row, the running share over every issuer.

`report_exposure_security_slices` (migration 0165) holds the rows of
`report_exposure_securities` at the same instants, from the plain walk.
They are cut by source, tax wrapper and asset class, so the dashboard's
pickers reach them. A slice keeps the weight of the leaf each label
comes from, so the slices of a selection add up to the view's rows.

`web_exposure`, `web_exposure_debt`, `web_exposure_issuers` and
`web_exposure_securities` are the dashboard's reading of them
([web/DESIGN.md](../../web/DESIGN.md) §8).

### 10.15 Yield reports

The yield report computes in Go, over what gold reads at one instant.
[YIELD.md](YIELD.md) defines every figure it returns.

Migration 0135 defines `instrument_terms`, `account_rates` and the
ledger's yield tables (§7.6). Migration 0136 defines the holdings
macro. The yield pass (`gold.RebuildYield`) runs after the exposure
pass at the end of every load and reload. It reads the exposure pass's
instrument keys and securities, and rewrites `instrument_terms`,
`account_rates` and `dividend_gaps` (§7.6) in one transaction.

The pieces:

- `yield_holdings(asof, ccy)`: `exposure_holdings`, each line with its
  own amount, quantity, accrued interest and acquisition day;
- `instrument_terms_at(asof)`: the terms with the ledger's records laid
  over (§7.6);
- `gold.YieldInputs(asof, ccy)`: everything the report reads at one
  instant, as `yield.Inputs`: the holdings, their accounts, terms and
  rates, the curves, each fund's figures and lines, the income and
  dividends received, the stocks' reported figures, and each holding's
  amount on every day of the year before. An observation's amounts
  hold over its span (§10.7), so one read of the year's observations
  gives every day. A security's holdings pool at every source;
- the alias fold, `yield.Inputs.Fold`. An instrument id reads as the
  first of these that applies:
  1. the account's holding alias;
  2. the source-wide holding alias;
  3. for a CUSIP id the source also keys by ticker, the ticker
     (`instrument_aliases()`).

  SQL folds the same way (`holdingAliasJoins`). A payment, a holding, a
  position and a trade meet under the folded id;
- the record day, `readYieldUnits`: what each account held of each
  instrument paid, at its snapshots, moved by its trades since
  (YIELD.md §4 says which account a cash account's trade moves).
  `yield.RecordOf` gives a payment's record day: the one the dividend
  calendar states (`dividend_calendar`, read by `readCalendar`), else
  the one its ex day derives by the market's settlement cycle
  (`yield_settlement_cycles`), else the one its issuer's learned gap
  gives (`dividend_gaps`, read by `readDividendGaps`; `yield.LearnGap`),
  else the market's lag. It also gives the day the quantity paid on
  reads: the ex day where the calendar or a learned gap gives the days,
  else the lag's record day. `yield.RecordLag` gives each market's lag
  before the pay day, its record window and its settlement calendar. A
  dividend's rate reads what its account held as that day began
  (YIELD.md §4);
- `internal/fixedincome`: the bond and rate arithmetic, as pure
  functions;
- `internal/yield`: `yield.Compute` measures every holding and projects
  its income into a `Book`. The views are the book's methods:
  `Aggregate`, `Funds`, `Cash`, `Curve`, `Securities` and `Coverage`.
  Every grain sums the same measures per holding, so the grains
  reconcile by construction;
- `cmd_yield.go` and `cmd_yield_views.go`: the views' columns and the
  usage text. `securities` joins exposure's securities to the book
  (`gold.YieldSecurities`).

`wealthdb yield --period` computes one book per period of the window,
at the instants of `gold.PeriodEnds`, as exposure's series do.

The forward views of `wealthdb income` (INCOME.md §12) read today's
book:

- `Book.Projection`, which `yield.Compute` builds: the projection as
  parts per holding, month and maturing rule. An accrual covers its
  days, and a payment counts on its day;
- `Book.Amounts`: a window's received and expected amounts under the
  assumptions (the rate path, the maturing rule, the shift);
  `ProjectionRows`, `IncomeRows` and `Calendar` turn them into the
  views' rows, so the views reconcile by construction;
- `gold.IncomeReceived`: the received half, each income line named by
  the holding it belongs to;
- `Config.YieldHorizon`: the default window, `yield.horizon_months`;
- `cmd/wealthdb/cmd_income_projection.go`: the forward views' window,
  flags and columns, shared by the CLI and the MCP `income` tool.

The Metabase Yield dashboard, the Income dashboard's cards of the
holdings' income and the Cash Flow dashboard's projection read tables
`web-materialize` writes (`gold.MaterializeYield`, migration 0138),
each from the books `yield.Compute` gives:

- `report_yield_positions` and `report_yield_buckets`: every holding's
  figures and its DV01 per tenor, in every reporting currency, today
  and at the last second of each of the 12 months before. A holding
  carries what its realized yield divides, its income received and its
  mean daily value, so a card pools a selection's income over its mean
  value as a row does (`yield_realized_pct`). The table holds no days,
  so the CLI's thin rule for a row does not apply to a card. A bucket
  carries its tenor's order and what holds it (`yield.HeldIn`), as the
  `curve` view splits it;
- `report_yield_parts`, `report_yield_rates` and `report_yield_events`:
  today's projection as its parts, in each holding's currency with its
  rate to each reporting currency, the rate table, and the principals
  maturities return. A holding's parts come from the first reporting
  currency's book that holds it, so a holding no rate converts to US
  dollars keeps its projection in the other currencies. A card sums a
  part's income (`yield.Part.Income`) at query time, so a rate path, a
  maturing rule, a shift and a horizon cost no storage;
- `report_yield_received`: the income lines the income projection
  counts as received (its six types), to the instant, per reporting
  currency;
- `report_cashflow_plan`: the cash flow projection's look-back parts per
  reporting currency and month ahead (§10.12).

The `web_yield*` views are the dashboards' reading of them
([web/DESIGN.md](../../web/DESIGN.md) §8). A refresh computes 52 books
(13 instants in 4 currencies).

## 11. Repository layout

```
wealthdb/
├── README.md                       — what the engine does, with examples
├── Dockerfile                      — single-stage; Go toolchain + binary in one image (ENTRYPOINT is the binary)
├── wealthdb                        — host-side wrapper around `docker run` (production binary)
├── wealthdb-go                     — host-side wrapper around `docker run go ...` (§12.5)
├── wealthdb-test                   — thin alias: `wealthdb-go test ...` (§12.5)
├── go.mod / go.sum
├── docs/
│   ├── DESIGN.md · RETURNS-NOTES.md · GAINS.md · EXPOSURE.md · LOTS.md · SPENDING.md · INCOME.md · CASHFLOW.md · TAXONOMY.md
│   └── adapters/                   — per-source adapter design (amex, carta, chase, cointracking, plaid, refdata, schwab, swissquote, synthetic, ubs)
├── cmd/
│   └── wealthdb/                   — CLI entry point + one cmd_<subcommand>.go per subcommand
├── internal/
│   ├── canonical/                  — change types + enums (asset_class, vehicle, …); zero deps
│   ├── silver/                     — adapter interface + registry, one package per source:
│   │   │                             amex angellist carta chase cointracking curves
│   │   │                             equityzen fidelity firstcitizens fred manual plaid
│   │   │                             raiffeisen_at refdata relevate schwab swissquote
│   │   │                             synthetic ubs viac
│   │   └── <source>/               — impl (snapshots/transactions/classmap) + co-located policy.go, and lotpolicy.go where the lot engine replays the source
│   ├── gold/                       — DuckDB schema, writer, queries, report macros
│   │   └── migrations/             — 0001…NNNN SQL, //go:embed-ed by schema.go
│   ├── returns/                    — source-agnostic TWR/MWR math + pluggable per-source policy
│   ├── lots/                       — the lot engine, pure (docs/LOTS.md) + pluggable per-source policy
│   ├── fixedincome/                — bond and rate arithmetic, pure (a currency's zero curve and forward rates)
│   ├── spending/                   — the deterministic spend-enrichment pass (§10.10, docs/SPENDING.md)
│   ├── loader/                     — the §8 silver→gold load orchestration (the only silver↔gold bridge)
│   └── config/ · ledgercsv/ · pathmode/ · wizard/ · output/ · errs/ · version/
└── (Dockerfile, wrappers per above)
```

**Dependency direction** is strictly one-way — anything lower may
import anything higher, never the reverse:

```
canonical                    (zero deps)
   ↑
lots, returns, fixedincome   import canonical
   ↑
silver (interface)           imports canonical
   ↑
silver/<source> adapters     import silver + canonical + returns + lots (policies)
   ↑
gold                         imports canonical + returns + lots — NOT silver
   ↑
loader                       imports gold + silver (the bridge)
   ↑
cmd/wealthdb                 imports loader + gold + config + wizard + output
```

**`gold` never imports `silver`.** Only gold's tests import the
adapters, to register their policies. The writer takes canonical
`*Change` values, not adapter handles, and `loader` is the sole place silver
and gold meet. Migrations live under `internal/gold/migrations/`
because Go's `//go:embed` can't traverse up the tree. The
`internal/` prefix keeps every package unimportable downstream —
wealthdb is an application, not a library.

## 12. Container / build / run

Single-stage Dockerfile, one image (`wealthdb:latest` by default),
used unchanged for development, testing, and production. See §12.4
for why there are no separate stages.

The image tag replaces `latest` in every image name the tree builds,
runs and removes: the engine, the collectors, their shared bases and
Metabase. It resolves in this order:

1. `WEALTHDB_IMAGE_TAG`, where it is set;
2. the first line of `.image-tag` at the repository root, a git-ignored
   file, so each checkout keeps its own;
3. `latest`.

A malformed `.image-tag` is an error, never a fall back to `latest`.
One helper decides, `shared/wrappers/image-tag.sh`:

- the Makefile reads it and exports the result;
- every wrapper sources it;
- each Dockerfile built on a shared base takes the tag as a build
  argument for its `FROM`.

A second checkout that writes its tag into `.image-tag` then tests its
own images from any shell. It leaves every other checkout's images
untouched.

### 12.1 Image

Base: the official `golang` bookworm image whose tag
`wealthdb/Dockerfile` pins (glibc, full Go toolchain, gcc/g++ for
CGO). The Dockerfile:

1. Copies `go.mod` / `go.sum` and runs `go mod download` in its own
   layer for cache friendliness.
2. Copies the source.
3. Builds the binary at `/usr/local/bin/wealthdb`, linking in the
   version stamp (below). The Go build cache lives in a BuildKit cache
   mount, so a build whose only change is the stamp relinks rather than
   recompiles.

The build context is `wealthdb/`, which holds no `.git`, so the binary
cannot read its own version from git. `./wealthdb build` reads it on
the host instead and passes it as build args, which the Dockerfile
links into `internal/version`:

- `WEALTHDB_TAG` — the release tag (`v*`) HEAD sits on, set only when
  the engine tree (`wealthdb/`) has no uncommitted or untracked change;
- `WEALTHDB_BASE` — the nearest release tag at or below HEAD;
- `WEALTHDB_COMMIT` — HEAD's abbreviated hash, with `-dirty` when the
  engine tree has changes.

`wealthdb version` prints the tag alone when there is one and
`<base> nightly <commit>` otherwise, so a build between releases never
passes for a release. A binary built without the stamp (a plain
`go build`, `go test`) falls back to the toolchain's own VCS stamp, and
to `devel nightly unknown` without one. A release is therefore just a
tag: nothing in the source names the version.

`./wealthdb build` skips the build when nothing changed:

- It hashes `wealthdb/`, the stamp and the base image's id.
- When the hash matches the image's `wealthdb.inputs` label, it prints
  `build: wealthdb:<tag> unchanged` and builds nothing.
- Any extra argument, such as `--no-cache`, always builds.
- A host without `python3` cannot compute the hash, so it always builds.

The collector wrappers and `make base-images` skip a build the same way
([collectors/README.md](../../collectors/README.md#build-scaffolding)).

Entry point is the production binary. The `./wealthdb-test`
wrapper overrides with `--entrypoint go` to run tests in the same
image. See §12.5.

DuckDB's Go driver (`github.com/duckdb/duckdb-go/v2`) requires CGO
and glibc — the `bookworm` base satisfies both. Alpine / musl was
rejected for this reason.

### 12.2 Volume mounts

The host wrapper mounts host paths to the **same paths** inside the
container, so the gold DB path resolves identically on both sides and
no path translation is needed. The data root is `$WEALTHDB_DATA_ROOT`
when set, else `${XDG_DATA_HOME:-~/.local/share}/wealthdb`. Two bind
mounts:

```
$XDG_CONFIG_HOME/wealthdb.cfg → $XDG_CONFIG_HOME/wealthdb.cfg   config file
<data root>/                  → <data root>/                    gold + silver data tree
```

The container also gets `HOME`, `XDG_CONFIG_HOME` and
`WEALTHDB_DATA_ROOT` from the host, so `~`, `$HOME` and
`${WEALTHDB_DATA_ROOT}` in the config file, and the engine's default
config path, land on the same absolute paths inside and outside.

The host-side `wealthdb` shell wrapper does the mounting. Two shapes:

**Writer host** (runs `wealthdb config`, `wealthdb init`, `wealthdb load`,
`wealthdb reset`):

```sh
CFG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}"
DATA_ROOT="${WEALTHDB_DATA_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/wealthdb}"
mkdir -p "$CFG_DIR" "$DATA_ROOT"
[ -f "$CFG_DIR/wealthdb.cfg" ] || : > "$CFG_DIR/wealthdb.cfg"
TTY=""; [ -t 0 ] && [ -t 1 ] && TTY="-it"
docker run --rm $TTY \
    -e HOME="$HOME" \
    -e XDG_CONFIG_HOME="$CFG_DIR" \
    -e WEALTHDB_DATA_ROOT="$DATA_ROOT" \
    -u "$(id -u):$(id -g)" \
    -v "$CFG_DIR/wealthdb.cfg:$CFG_DIR/wealthdb.cfg" \
    -v "$DATA_ROOT:$DATA_ROOT" \
    "wealthdb:$WEALTHDB_IMAGE_TAG" \
    "$@"
```

The empty config file lets the mount exist before `wealthdb config`
writes it (§4.9). A TTY is allocated only when the host shell has one:
`wealthdb config` needs it, and a cron or piped run skips it.

**Reader host** (only the read commands, such as `wealthdb holdings
positions` and `wealthdb status`, against a read-only mount of another
host's gold tree):

```sh
docker run --rm \
    -e HOME="$HOME" \
    -e XDG_CONFIG_HOME="$CFG_DIR" \
    -e WEALTHDB_DATA_ROOT="$DATA_ROOT" \
    -u "$(id -u):$(id -g)" \
    -v "$CFG_DIR/wealthdb.cfg:$CFG_DIR/wealthdb.cfg:ro" \
    -v "/mnt/shared/wealthdb:$DATA_ROOT:ro" \
    "wealthdb:$WEALTHDB_IMAGE_TAG" \
    "$@"
```

The `:ro` flag on the data-root mount makes the gold DB file
unwriteable inside the container; `wealthdb`'s mode detection (§4.10)
picks this up and refuses (RW) subcommands with a clear message.

**MCP server** (`wealthdb mcp start`, mcp/DESIGN.md §4): the reader
shape, detached, with both mounts `:ro`, a read-only root file system,
a tmpfs at `/tmp` for DuckDB's spill, no capabilities, and the bearer
token as a read-only file. The image is the same; the command is
`mcp-serve`.

### 12.3 Build and first-run commands

```sh
./wealthdb build               # docker build; skipped when nothing changed
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

From the repo root, `make test-wealthdb` (or `make test` for the
whole suite) is the top-level entry: it builds the image when its
inputs changed and runs `go test ./...` in one step. Under the hood it
calls the `./wealthdb-test` wrapper described here.

Tests run **inside the container**, so the host stays clean of Go
toolchain, DuckDB headers, SQLite headers, and the rest. The
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

The `cmd/wealthdb` tests run in parallel (`t.Parallel`): each builds its
gold under its own `t.TempDir()` and talks to the command through
`Run`'s readers and writers, never the process's own. The exceptions
stay serial on purpose — a test that sets an environment variable, and
one that times a persist into a reader's wall-clock window.

**Reload verification.** `TestTaxonomyInvariants`
(internal/gold/taxonomy_invariants_test.go) checks a real gold, not a
fixture, and is skipped unless `WEALTHDB_GOLD_VERIFY` names one. It
asserts that every position and instrument carries an admitted
(asset class, vehicle) pair. With `WEALTHDB_GOLD_BEFORE` naming a copy
taken before a `reload -a`, it also asserts that the reload moved no
money: the market value per source, account and snapshot, and the row
counts per source, are identical. Run it after any adapter change.
`./wealthdb-go` forwards both variables and mounts each file's
directory read-only, so the copies sit where Docker can see them:

```sh
cp "$GOLD" ~/gold-check/before.db && wealthdb reload -a && cp "$GOLD" ~/gold-check/after.db
WEALTHDB_GOLD_VERIFY=~/gold-check/after.db WEALTHDB_GOLD_BEFORE=~/gold-check/before.db \
  ./wealthdb-test ./internal/gold/ -run TestTaxonomyInvariants -v -count=1
```

`make lint` runs `gofmt -l` and `go vet ./...` through `./wealthdb-go`
on the same toolchain (gofmt as `go run cmd/gofmt`, the wrapper's
entrypoint being `go`). It reads the mounted source, so it needs the
image to exist but never rebuilds it. The repo's pre-commit hook
(`.githooks/pre-commit`, installed by `make hooks`) runs the same target
on an export of the staged tree, so a commit is linted as committed.

#### Wrapper shape

`./wealthdb-test` is a thin alias over `./wealthdb-go`, which owns the
container geometry and runs any `go` subcommand in the image.
`./wealthdb-go` runs `docker run --entrypoint go` on the engine image
(§12). It mounts the repo and the Go caches under
`${XDG_CACHE_HOME:-~/.cache}/wealthdb/go` at the same paths inside, and
runs in the repo directory. The script is `wealthdb/wealthdb-go`.

#### The image toolchain owns the module graph

The repo is mounted read-write, so `go get -u` / `go mod tidy` run
through this wrapper write `go.mod` and `go.sum` from inside the
container — which is how `make update` refreshes them. The image ships
`GOTOOLCHAIN=local`, so a dependency requiring a newer Go fails with
`go.mod requires go >= X` instead of raising the `go` directive past the
version `wealthdb/Dockerfile` pins; the base-image tag stays the single
declaration of the toolchain version, and a host Go (any version, or
none) cannot influence the result.

#### One image, not two

`./wealthdb-go` uses the production wrapper's image (§12), with
`--entrypoint go`. The single image carries the full Go toolchain so
`go test` works against the mounted source tree; no separate builder
image is needed.

#### Cache locations

Go's build cache (`GOCACHE`) and module cache (`GOMODCACHE`) live
on the host under `${XDG_CACHE_HOME:-~/.cache}/wealthdb/go/` so that incremental
re-runs are fast across container restarts. The cache dir is
isolated from any host-side Go toolchain — running tests via the
wrapper never pollutes a host `$GOPATH`.

#### No host Go required

A user with only Docker installed can run the entire test suite — and
`make update` — end-to-end. This is the same contract as `./wealthdb`
itself.

## 13. Further mechanisms and open questions

This section holds mechanisms that cut across the sections above
(§13.2, §13.8 to §13.10) and the questions still open.

### 13.1 Cross-silver instrument canonicalisation

The exposure pass keys every instrument to a security
(`instrument_keys`, `security_aliases`, §10.14). The exposure report
therefore reads one security across sources. The other reports keep
one `instruments` row per source. They join on `isin` where they need
one view.

### 13.2 FX-rate precedence among silvers

When several silvers publish the same pair, precedence is
**configured per source**, refined with a day-bucket tiebreak so a
reference source never overrides an account source on the days they
overlap. Precedence is **data, not runtime state**: it rides on a
stamped gold column that the `fx_daily` view reads.

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
  the `fx_norm` / `fx_daily` views read that column directly.
- **Exact timestamp** (`snapshot_at` DESC) breaks any remaining tie.

Out of scope: non-snapshotted FX sources (e.g. ECB reference rates,
or a manually maintained `fx_overrides` table for currencies no silver
covers; `fred` covers the major ones).

### 13.3 acquisition_date backfill

Adapters set `acquisition_date` only where silver states it (§10.4).
For holding-period queries to work across every source, the rest would
have to be derived, for example from the earliest `buy` or
`transfer_in` of each position. On the sources the lot engine fills,
`lots_at` already gives each open lot's acquisition date.

### 13.4 Lot engine: open items

The lot engine (§7.5) leaves out what docs/LOTS.md §11 lists.

Silver also states a few cost facts gold does not project yet:

- schwab-web's `cost_basis_methods`, the method a Gain/Loss Report
  prints per account;
- the K-1 gain lines of angellist and carta, and carta's fund fee,
  gain and carry lines;
- ubs-web's `last_purchase_date`;
- a basis in the reference currency (UBS's average buy rate,
  Swissquote's CHF P&L);
- the purchase fees UBS and Swissquote leave out of their basis, as
  fee legs.

### 13.5 A silver's own kind

Nothing checks a source's configured kind against its silver, so a
mismatch shows only as that adapter's read error. A silver that
published its own kind in `schema_meta` would let the loader check the
pairing up front. That needs a coordinated change with the collectors.

### 13.6 Concurrent loads

Gold is single-writer, and the write mutex of §4.10 is what makes
that true rather than an assumption. DuckDB's own file lock does
not cover it: the lock lasts only as long as a handle is open, and
`compact` and `reload -a` hold no handle across their
rebuild-and-swap, so two writers could each report success while
one of them wrote into the inode the other's rename unlinked.

The contract is therefore: a write command takes an advisory
`flock` on `<gold_db>.wealthdb.lock` for its whole duration, and a
second write command that finds it held exits 5 immediately rather
than racing or queueing. Concurrent readers (read-only mode,
§4.10) take no mutex and are unaffected — though a read-only handle
open on the file does fail a read-write open for as long as it
lives, which is why the model commands retry their store.

What remains open is queueing rather than refusing: a blocking
take would suit a cron that would rather wait than fail. Refusing
is the current contract because the holder may be an hour-long
model run, and a silent hour of blocking is worse than an exit
code.

### 13.7 Silver tables gold does not project

Several silver tables exist that no adapter projects into gold. The
per-bank adapter docs (§6.7) list them. Across banks:

- **`schwab.user_preference`**: Schwab UI and streamer config. It has
  no portfolio relevance.
- **`schwab.open_orders`**: open orders. They are operational, not
  portfolio state, so gold reports no open orders.
- **`ubs.account_holders`**: client and legal-owner metadata. Gold has
  no client dimension, so it does not report who legally owns an
  account.
- **`ubs.pending_securities`**: MT537 pending-settlement snapshots.
  Many rows are "no activity" markers (`ACTI//N`). The rest are
  transient: a position shows in `holdings` once it settles. Gold
  reports no pending settlement.
- **`ubs.portfolio_performance`**: TDPOPF monthly performance rollups.
  Gold computes returns itself (§10.9) and does not read the bank's.
- **`ubs.cash_account_pricing`**: TDCAPI interest-rate config. Gold
  reads UBS's credit rates from the web feed's `account_rates`
  instead (adapters/ubs.md §3).
- **`swissquote.documents`**: PDF document index. Bronze holds the
  binaries. Gold stores no document metadata.
- **Cost facts**: §13.4 lists the ones gold does not project.

A `plaid` Item can also report loans that are not on a home, such as
auto, student and personal loans and lines of credit. They stay in
silver too (`accounts`, `liabilities`, `transactions`). Gold has no
account kind for them (adapters/plaid.md §3 says why), so it reports
none of them.

### 13.8 Account taxonomy (kind / tax_wrapper / management_style) and nickname

Several distinct kinds of accounts typically coexist at the
same bank — personal brokerage, managed wealth account, UTMA /
ESA / IRA / 529 / Säule 3a wrappers for tax purposes, separate
cash accounts. Filtering positions and net-worth roll-ups by
these dimensions is more useful than slicing by raw account ID.
Independently, a `nickname` lets the CLI render
something more recognisable than the bank's identifier.

The `accounts` table carries three orthogonal classifier columns
plus the free-text descriptors:

- **`account_kind`** — the technical container the bank exposes:
  `brokerage`, `cash`, `safekeeping`, `custody`, `overlay`,
  `crypto`, `mortgage`, `card`, `donor_advised_fund`, `other` (with
  `crypto_exchange` / `crypto_self_custody` reserved for a future
  adapter that distinguishes them). Required; every adapter
  stamps this. A `donor_advised_fund` holds irrevocably donated
  charitable assets, a kind of its own so a deployment can include or
  exclude DAF balances from net worth. A `mortgage` is a
  real-property-backed liability whose outstanding principal sits as
  a negative-value position. A `card` is a revolving-credit
  liability: it carries no position, and its outstanding balance is
  negative cash on the account. An `overlay` is a synthetic
  per-portfolio account for positions the bank attributes to the
  portfolio directly (UBS forward contracts). A `custody` account is
  a platform's holding of private-market interests (Carta,
  AngelList, EquityZen). Its holder self-directs it. `crypto` covers
  both exchange and self-custody, because wallet names do not
  reliably tell them apart.
- **`tax_wrapper`** — the tax / regulatory registration.
  Nullable; defaults to `taxable_personal` at render time.
  Values cover US (`traditional_ira`, `roth_ira`, `sep_ira`,
  `simple_ira`, `401k`, `403b`, `457b`, `529`, `coverdell_esa`,
  `hsa`, `charitable`, `custodial_utma`, `custodial_ugma`,
  `trust_grantor`, `trust_non_grantor`, `trust_charitable`) and
  Switzerland (`pillar_2`, `vested_benefits` /
  Freizügigkeitskonto, `pillar_3a`), plus generic
  `taxable_personal`, `taxable_joint`, `foundation`, `other`.
  `charitable` pairs with the `donor_advised_fund` kind.
- **`management_style`** — who places trades. Nullable;
  defaults to `self_directed` at render time. Values:
  `self_directed`, `advisory`, `discretionary`, `automated`.
- **`account_category`** — free-text bank-supplied descriptor,
  supplementary to the enums. It keeps verbatim strings the
  structured enums don't capture (UBS's "Custody / Cash-Custody",
  etc.).

Examples of adapter-supplied values:

- **UBS** populates `account_category` from
  `cash_accounts.AcctTpDesc` for cash legs, and from
  `safekeeping_accounts.AcctTpDesc` + `AcctSubTypeDesc`
  (concatenated as `"Type / Sub"` when the sub is present) for
  safekeeping legs. `tax_wrapper` is derived from `AcctTpCd`
  via an explicit known-code switch (every known PSN code
  maps to `taxable_personal`); unknown codes log a one-shot
  WARN and leave the column nil. UBS PSN doesn't expose a
  per-account nickname, so `nickname` stays NULL unless the
  config-side override fills it in. `management_style` is not
  surfaced by silver and stays nil.
- **Schwab** populates `nickname` from the api silver's
  `accounts.nickname` (the label set in the Schwab UI). The api's
  `securitiesAccount.type` is CASH/MARGIN, not a wrapper. The web
  feed's statement registration label sets `account_category` and the
  tax wrapper ([adapters/schwab.md §7](adapters/schwab.md)).
- **Swissquote** populates `account_category` and `tax_wrapper`
  from `silver.accounts.account_product` (the per-account
  product label scraped from the eBanking account-overview
  page): `Trading`/`Savings` → `taxable_personal`,
  `Säule 3a` → `pillar_3a`, `Freizügigkeit` → `vested_benefits`.
  `nickname` stays NULL; Swissquote doesn't expose one.
- **Fidelity** populates `tax_wrapper` (and, for a charitable
  account, `account_kind`) through `silver.portfolios.kind`:
  - `529` → `tax_wrapper=529`;
  - `trust_managed` → `tax_wrapper=trust_non_grantor`;
  - `daf` → `account_kind=donor_advised_fund`, `tax_wrapper=charitable`.

  `management_style` comes from silver's `accounts.management_style`
  (`automated` for a 529, `discretionary` for a managed trust). Silver
  does not carry per-account registration labels (Roth IRA, Coverdell
  ESA); fidelity-web's DESIGN.md §11.5 describes the path to add them.

Adapters use SQLite PRAGMA-based feature detection where the
silver schema has evolved (e.g. Swissquote's pre-v5 silvers used
`account_type` instead of `account_product`); older silvers
still load with the corresponding columns left NULL.

Config-side override: `account_overrides` (§5.1) sets what an
adapter cannot classify, such as a Schwab IRA whose wrapper no
silver field reaches.

The same block can drop an account outright: `"exclude": true`
removes the dimension row **and every fact keyed to it** — positions,
cash balances and transactions alike. The two must go together,
because a fact whose account gold has no record of is an orphan: it
sits outside every account-scoped filter and belongs to no portfolio.
`portfolio_overrides` takes the same flag at its own grain, dropping
the portfolio's dimension row and every account inside it with their
facts.

This is for an account or a relationship a collector enumerates but
the holder does not hold. A provider whose UI lists its whole product
menu per login reports the products the holder never opened beside the
ones they did, and the scraper cannot tell them apart; such an account
reaches gold as a real account that happens to be empty, which is
exactly how a real account the provider under-reports also looks. The
distinction is the holder's to make and nothing infers it from
emptiness: a provider can report nothing for an account that is not
empty, so an emptiness rule would drop real holdings on exactly the
sources that report them worst. Excluding an account that does hold
something removes that money from every total, so each load prints
what its exclusions dropped. `exclude` may not be combined with a
column override on the same entry: the row the column would be stamped
on is the row the exclusion removes, and config-load refuses the
contradiction rather than picking a winner.

Both grains are enforced the same way, as a sweep over gold once the
streams have drained (`deleteExcluded`), rather than as a filter over
the incoming rows. Neither grain is decidable earlier. A portfolio's
membership lives in the account dimension — a position, a cash balance
and a transaction each name an account and never a portfolio, and an
adapter may emit its facts before the dimension rows that would place
them. An account is decidable per row, but a filter reaches only the
rows a load happens to write, and gold's dimensions are upserted and
never expire: an account already in gold when the exclusion is added
would sit there untouched. The sweep finds a portfolio's facts
*through* the accounts table, so it deletes facts first, then the
accounts, then the portfolio row — the other order would strand
everything behind the accounts it deleted.

Like every other override in this block, an exclusion reaches the rows
a load actually touches: a load with no new silver changes nothing, and
`wealthdb reload <source>` is what re-applies config against history.

`returns_exclude` (§5.5) is the neighbouring instrument and a different
one: it keeps the money in net worth and removes it from the
coarse-grain return math, where `exclude` here removes it from gold
entirely.

`supersession` (§5.1) is the temporal member of the same family: where
`exclude` removes an account from gold outright, this ends one source's
account at a date because something else carries it from there. An
account can outlive its source — a deposit account whose bank is
acquired keeps running under the acquirer's collector, a holding moves
custodian — and gold keys an account on `(silver_source_id,
account_external_id)`, so the two are two accounts and both count.
Dropping the later rows is not enough on its own: gold carries a key
forward until something supersedes it, and a source that simply stops
reporting supersedes nothing (§10.7), so the loader also writes one zero
row AT the date for whatever the account still held — the explicit
closure that absence cannot be. Cutting by date rather than by account
keeps the earlier half of a statement or dump that straddles the
handover, and bounds the series at the END where `inception_overrides`
(§5.4) bounds its start.

What is cut is what the adapter read. An `equity_transfers` (§13.9)
row dated on or after the date is refused by name instead, failing the
load: a ledger row is written by hand, one at a time, asserting a
capital flow nothing else in silver carries, so dropping it silently
would delete a real flow and the returns engine would read the gap as
performance inside the account. The row belongs under the source that
carries the account from the handover on, and saying so is the only
outcome that gets it there.

The instrument dimension has the same escape hatch:
`instrument_overrides`, keyed by `(silver_source_id,
instrument_external_id)`, pins a per-instrument `(asset_class,
vehicle)` pair (validated as an admitted taxonomy pair). The loader
patches both the `instruments` row and every `positions` row
referencing the instrument — position rows carry their own
`asset_class`/`vehicle` copy, so the two must move together.
`transactions` needs no such patch: a trade states a pair only where
its instrument cannot answer (§10.8), so there is no copy of the
instrument's own classification to keep in step. Use it where neither
the source's structured signal nor the ETF name refinement (§6.8) gets
the pair right — the canonical example is an exchange-traded commodity
trust whose security name never mentions the metal or the ETF-ness
(`metal × etf`).

Between the adapter and the override sits the class pass
(`gold.RefineClasses`). It runs first among the passes a load or a
reload ends with. An adapter that implements `silver.ClassRefiner`
(schwab) takes OpenFIGI's security type, from the refdata source's
`security_identities`, over its own pair. The pass reads the type for
each instrument of such a source, patches the `instruments` row and its
`positions` rows as the override does, and leaves a pinned instrument
alone. It runs after every load, so a type a later refdata load brings
reaches a source loaded before it (docs/adapters/schwab.md §4.2).

Two statements in the same family are not overrides of an adapter's
answer but answers an adapter cannot have.

`silver_sources[].taxable_wrapper` names the taxable wrapper a
source's taxable accounts actually sit in. A bank feed says what a
product IS and never who holds it — a jointly held account and a
personally held one are the same product — so an adapter emits the
generic `taxable_personal`, and whose it is belongs to the deployment.
Stated once per source rather than once per account, it reaches an
account opened later on the load that first sees it. It rewrites
`taxable_personal` and nothing else: a retirement, trust or custodial
wrapper the adapter had positive evidence for is not a blanket rule's
to overrule, and an account with no wrapper at all is a gap for the
coverage canary to report rather than one to guess at. It applies
before both override maps, so a named `account_overrides` entry still
wins.

`transaction_instruments` links a row (a securities trade, or an income or
withholding row of a security) whose feed named an instrument nothing in
the product could resolve. It is keyed by the TOKEN the adapter looked up
and failed on, which the adapter states on the row (`instrument_hint`,
§10.8) rather than the loader digging it out of a payload whose shape
differs per feed — one mechanism for every source, and the source-blind
layer stays source-blind. A row the adapter already resolved is never
touched, and a token nothing states any more is a silent no-op (that is
what an adapter learning to resolve it looks like). An entry closes every
row of its source that states the token, in any account and of any kind,
and the row's own coarse taxonomy pair is dropped as the link lands, the
instrument it names answering better. It states the identity only, so it
composes with `instrument_overrides`: pin the link here, pin its
classification there.

Selectable columns: `wealthdb holdings accounts -C
silver_source,account,account_kind,tax_wrapper,management_style,...`.
The three structured classifiers are part of the default
column set; the descriptor columns (`account_category`,
`account_nickname`) are registered but opt-in.

The reports take no row filters (`wealthdb help`): a view is picked,
then its output filtered.

### 13.9 Equity-transfer ledger

Securities transferred *into* a tracked account (e.g. shares
transferred in from another custodian at appreciated value) are a
capital inflow at their market value on the transfer date —
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
double-counted — while a mid-life transfer is booked in full. A row
dated on or after the account's `supersession` (§5.1) date is refused
and fails the load, rather than dropped the way the adapter's own
rows are (§13.8). The ledger is source-agnostic; any source's
transfers are just rows with that `silver_source_id`.

That includes the positions-only source. `manual` collects no
transactions, but value can reach it from another tracked vehicle
with no bank in between, and the selling source books only its own
half. A row
against `manual`'s single account (blank `instrument` and
`quantity`: a bare valued movement) supplies the other half, and
that source's returns policy admits exactly the two ledger kinds so
the claim's arrival is funded rather than read as performance. The
runbook is in `collectors/manual/DESIGN.md` §6.

An exit paid in shares — a private-market vehicle distributing the listed
stock it received, which lands in a brokerage account — is two rows, one
per side, on the same day at the same value: a `transfer_out` against the
vehicle's source and a `transfer_in` against the receiving account, each
carrying the quantity and the ticker. The day is the one the shares land
in the receiving account, whose own transfer record names it and the share
count; the value is that day's close times the quantity. On it the vehicle
realizes its gain from its last mark, the receiving account takes the
shares as capital, and an aggregate holding both accounts nets the pair.
Two dates, or each side's own figure, would leave the difference as
capital from nowhere. Each arrival day is its own pair.
The vehicle's own mark follows its statements, so its value catches up at
the next one; over any window holding the whole exit the totals agree.
The vehicle's source must admit the two ledger kinds in its returns
policy (angellist and manual do). Cost basis is the vehicle's entry, on
both rows, for reference:

```
angellist, lp-account, 2098-03-04, out, 500, 10000, 42000, USD, XMPL, Example Co SPV: 500 XMPL to the brokerage
schwab,    Brokerage,  2098-03-04, in,  500, 10000, 42000, USD, XMPL, Example Co SPV: 500 XMPL received
```

### 13.10 Spending and income pins ledgers

Some rows on a cash account cannot be classified from anything gold
holds. An FX roll settling as a bare withdrawal carries no descriptor
for a rule to match and no counter-leg for the matcher to pair; a
subscription booked as a plain debit looks exactly like a large
purchase. Every deterministic tier reads either the narrative or the
account graph, and such a row offers neither — the only thing that
identifies it is *which* row it is.

The optional `spending.pins` CSV ledger (config §5) records those
corrections one transaction at a time. Columns: `silver_source_id,
account, occurred_at (YYYY-MM-DD), amount, currency, spend_detailed,
asset_class, note` — and no others; an unknown header fails the parse.
`asset_class` is optional. **`income.pins` is the same ledger for the income family**, with
`income_detailed` in place of `spend_detailed` and validated against the
income vocabulary — a pin identifies a transaction the same way
whichever question is being answered about it, so only the value column
and its taxonomy differ. A pin naming a spelling the vocabulary retired
fails the parse, and the error names the value to write instead
(docs/INCOME.md §2). `account` accepts either the gold `account_external_id` or an
account nickname, resolved by the same `gold.NewAccountResolver` the
equity-transfer ledger uses. `amount` is the amount as gold stores it
— canonical sign, so a debit is negative — and matches within a cent,
the same absolute floor the transfer matcher applies, so a figure
copied from a two-decimal statement describes a four-decimal row.
`spend_detailed` may be any valid value, vendored or delta, in the
taxonomy's own spelling — the same set a config rule may place. Two
ledger rows on one transaction key must agree on the value AND on
`asset_class`; disagreeing on either is refused naming both lines,
because file order would otherwise decide it in silence. The
difference between the two is scope, not vocabulary: a pin names one
transaction key, a rule fires on every narrative its pattern matches.
`asset_class` is optional and says what the capital went INTO, for a
pin whose value is that family's one investing verdict — `investment`
on the spending side, `capital_return` on the income one. It is the
exposure dimension alone (TAXONOMY.md §2) less `cash` and `other`, and
on any other verdict it fails the parse rather than being stored where
nothing reads it. It exists because the cash flow statement's investing
class is otherwise the instrument's, and a bank payment order names
none (CASHFLOW.md §4). `note` is free text — why the row is pinned, for
whoever reads the ledger next — and nothing carries it into gold.

Those two are the only columns beyond the required ones, and the header
is checked against that closed set: an unknown column fails the parse
naming it. A misspelled `asset_class` would otherwise be a silent
no-op that still applied its verdict.

A pin applies to **every** gold transaction matching (source, account,
day, amount, currency). The key is deliberately not unique: two
identical rows on one day — the two legs of the same roll — are
indistinguishable by design, and gold's `transaction_external_id` is
opaque, adapter-specific and not something a person can read off a
statement, so the ledger never keys on it. Two ledger rows describing
the same transactions must agree (a contradiction fails the parse,
naming both lines); rows that agree collapse to one.

Mechanics (`internal/spending/pins.go`): both ledgers are parsed before
gold is opened for writing, so a stale or malformed pin fails before
anything is migrated or loaded. The deterministic pass
resolves the ledger against the whole of `transactions` and writes
each match with provenance `manual`, after every other tier — a pin
beats the matcher, the rules and the provider. Because the ledger is
config-sourced, the pass owns `manual` rows exactly as it owns the
derived ones: it clears the whole overlay and re-stamps, so a pin
removed from the ledger is gone on the next pass, `reload` and a
fresh-file rebuild need no carry-across, and `reset <source>` costs
nothing the next load does not restore. A pin that describes nothing
is *counted* — in the pass result and on the `spending:` summary that
`load`, `reload` and `categorize` print — never an error, because the
row it names may simply not have loaded yet; an account the resolver
does not know counts the same way, since an unloaded source has no
accounts at all. An ambiguous nickname is an error, as it is for the
equity ledger.
