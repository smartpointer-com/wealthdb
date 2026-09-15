# wealthdb: Design

## 1. Audience and scope

This document describes the design of `wealthdb` — the **gold**
layer of the personal-portfolio data pipeline whose bronze and silver
layers are owned by the per-source collectors under `collectors/`
(see [the repo-root DESIGN.md](../../DESIGN.md) for the full fan-in).

It is intended to be read alongside
[schwab-api/DESIGN.md](../../collectors/schwab-api/DESIGN.md),
which establishes the three-layer (bronze/silver/gold) model and the
per-broker silver-schema conventions reused here. This document covers
only what gold adds.

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
  independently. The daily history macros go one grain finer, resolving
  the active snapshot per (source, account) so a run that covered part of
  a source carries the rest (§10.7).
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
  outside the window in which the single upstream owner's
  `wealthdb load` holds the write lock. See §4.10.
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
wealthdb reload  <id> | -a            (RW)    Reset then load; -a builds a fresh compact gold and swaps it in.
wealthdb compact                      (RW)    Rewrite the gold DB into a fresh file to reclaim dead space.
wealthdb holdings <view> [flags]      (RO)    Point-in-time views: positions, accounts, portfolios, sources, global.
wealthdb returns <view> [flags]       (RO)    TWR & MWR/XIRR returns: accounts, portfolios, sources, global.
wealthdb transactions [flags]         (RO)    Print transactions over a date range.
wealthdb spending <view> [flags]      (RO)    Spending reports: summary, categories, transactions.
wealthdb income   <view> [flags]      (RO)    Income reports: summary, types, transactions.
wealthdb status  [<id>]               (RO)    Report gold state vs each silver source.
wealthdb snapshots <id> | -a          (RO)    List snapshots gold has loaded (one silver, or all).
wealthdb resolve-symbols              (RW)    Back-fill missing instrument ticker symbols via the configured LLM.
wealthdb resolutions                  (RO)    Dump the symbol_resolutions table (LLM + manual-override tickers).
wealthdb categorize [spending|income] (RW)    Categorise the unplaced merchants and payers via the configured LLM.
wealthdb categorizations [spending|income] (RO) Dump the model-derived verdict stores; --forget SIG removes one (RW).
wealthdb version                      (RO)    Print the wealthdb version.
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
| `-c`, `--config` | `${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg` | Path to config file. Tilde (`~`) and `$HOME` are expanded. |
| `-r`, `--read-only` | off | Force read-only access even when the gold DB is writeable. Useful for ad-hoc safety during exploration ("I'm running queries on prod and don't want to accidentally mutate anything"). Without this flag, mode is auto-detected from filesystem permissions (§4.10). |

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
| `-x`, `--currency` | value of `default_currency` in the config file | ISO 4217 output currency for value columns (e.g. `USD`, `CHF`). The short form `-x` is mnemonic for "(currency) exchange"; `-c` is deliberately not used here so it stays reserved for the top-level `--config` flag (§4.2). |
| `--with-cash` | off | Also emit one synthetic row per account+currency with non-zero cash (`asset_class = 'cash'`). |
| `-p`, `--privacy` | off | Redact identifying and monetary columns (see below). |

**Privacy classes.** `-p` is per-column, and every read-only view
that offers the flag draws from the same four classes (everything
else is left alone):

| Class | Renders as | Carries |
| --- | --- | --- |
| account-id | `****1234` — length preserved, a short tail and an IBAN country code left legible so rows stay distinguishable | account / portfolio / relationship / transaction ids. Applied only to identifier-shaped values: alphanumeric with a digit. A purely-alphabetic label (a bank-assigned `Education` / `Authorized`) and any value with a space or a paren are taxonomy or display text and pass through. |
| free-text | `***` — the whole cell, in every format | values that can carry a person's name and never look like an identifier: statement narratives and their folds (`counterparty`, `description`, `merchant_signature` and the `merchant` built from it in §4.12), and customer-chosen labels such as a cointracking portfolio name. Shape-blind by design — the narratives worth hiding are multi-word, so a shape test would pass exactly them. The `(no portfolio)` sentinel is a structural marker, not a name, and stays legible. |
| quantity | `***` (table) / empty (csv) / key dropped (json) | share counts. |
| money | `*****.**` (table) / empty (csv) / key dropped (json) | every monetary amount. |

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
`-c` (default `${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg`) by walking
through:

1. **Gold DB path** — default `$XDG_DATA_HOME/wealthdb/wealthdb.db`. The wizard
   confirms the parent directory exists but does **not** create
   the gold DB itself — that's `wealthdb init`'s job.
2. **Default output currency** — default `USD`. ISO 4217 only;
   validated for ISO 4217 shape (three uppercase ASCII letters).
3. **First silver source** — `id` (slug matching `^[A-Za-z0-9_-]+$`),
   `kind` (any registered adapter kind, or `auto`), and
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
soften this — see §4.13.)

#### Non-interactive fallback

In a pipe / non-TTY environment, `wealthdb config` refuses to run
("wealthdb config requires an interactive terminal; run with `docker
run -it` or write the JSON directly per §5"). This is preferable
to silently consuming stdin and producing an empty config.

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

A second open of the same file, read-write *or* read-only, fails
immediately with DuckDB's conflicting-lock IO error rather than
blocking or queueing: `gold.Open` surfaces it from `db.Ping()` and
the dispatcher exits 5 (`ExitOpenFailed`). One read-write handle OR
several read-only handles, never both — which is why `wealthdb web`
serves Metabase a snapshot copy instead of the live file
(web/DESIGN.md §2).

#### The gold write mutex

DuckDB's lock covers only the window a handle is open, and two
write commands hold no handle across their work: `compact` and
`reload -a` read the outgoing file, build a replacement, and
rename it over the live path. A verdict or a load written into the
outgoing file inside that window lands in the inode the rename
unlinks, and both commands report success.

So every write command — `load`, `reset`, `reload`, `compact`,
`categorize`, `resolve-symbols`, `web-materialize` — takes an
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
not shut out for the length of a run. Re-taking the handle to
store what the model answered can lose the race against a reader
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
| RO subcommand on non-existent DB | 3 | `wealthdb: gold database '<path>' does not exist. Run 'wealthdb init' first (requires write access).` |
| `init` on existing DB | 4 | `wealthdb: gold database '<path>' already exists. Use 'wealthdb reset -a' to clear data, or delete the file manually if you really want a fresh DB.` |
| DuckDB open fails mid-run | 5 | the driver's own error, passed through whole and prefixed by the stage that hit it: `wealthdb: ping duckdb "<path>": <driver error>` (or `open duckdb` / `migrate gold`) |
| Write subcommand while another holds the write mutex | 5 | `wealthdb: '<cmd>' cannot write '<path>': another wealthdb write command (load / reset / reload / compact / categorize / resolve-symbols / web-materialize) is running and holds '<path>.wealthdb.lock'. Wait for it to finish and re-run.` |
| Silver DB unreadable during load | 6 (reserved) | not emitted today — a silver open failure returns a plain error and exits 1 |

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

Exit codes 2–6 are distinct so callers (shell scripts, CI) can
discriminate; 6 is allocated but unused — nothing returns it, and a
silver open failure exits 1 with the plain error (`errs.ExitSilverIO`
carries the same note). Exit code 1 is reserved
for unexpected runtime errors (panics, etc.) — and for the
failures no row above claims.

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

Both take an optional family positional. Nothing named runs BOTH, in
order — spending, then income — as two plans and two summaries against
one model; a name runs that one alone. `--forget SIG` with no family
removes the signature from both verdict stores, one counterparty being
able to sit in each with a different verdict. See docs/INCOME.md §7.


`categorize` is the model tier of both families. The deterministic
tiers run on every `load`; what they leave behind is a set of
counterparty signatures whose category follows from nothing but the
counterparty's name, and this is what asks a model about them. Work is
priced PER SIGNATURE and the verdicts land in a global store —
`spend_merchant_categories` for spending, `income_payer_categories` for
income — so a merchant met by several cards, or a payer paying into
several accounts, is asked about once and answered once.

A normal run re-asserts every deterministic verdict first — the same
pass `load` runs — and then asks about what is left. `--dry-run` opens
gold read-only and therefore *cannot* run that pass, so its plan is
computed against the enrichment as of the last load and says so.

The backlog goes to the model in **batches** of `--batch` signatures
(default 40: what a local model answers well inside the transport's
five-minute call ceiling; a backlog sent whole in one call times out).
Every batch carries the taxonomy and the anchor block, retries on its
own feedback up to `--max-attempts`, and has its accepted verdicts
**stored as it completes**: a run that dies at batch 30 of 50 has kept
29 batches' worth of paid answers, and a re-run asks only about what is
still unanswered, because the backlog excludes whatever the store now
covers. Each batch's accepted verdicts become the newest anchors for the
batch after it, capped by `--max-anchors`. The batch plan — count,
sizes, anchors, the first prompt's size in characters and estimated
tokens, the best- and worst-case call count — prints before the first
call, and one progress line prints per batch. A dry run asks the model
exactly as a real run does, batch by batch (the precedent's shape: it is
the only way to see verdicts without writing them), so the plan is its
cost signal.

Two independent gates decide what leaves the machine, and neither can
be relaxed by the other: the family's `categorization.context` decides
how much of a candidate is described, and the fence — rails, IBANs,
masked contacts, wordless and filing-only keys, and by default
person-shaped keys on non-card accounts — decides which signatures are
candidates at all. See docs/SPENDING.md §5, and
docs/INCOME.md §7 for the one difference — an `income.categorization`
block inherits `spending.categorization` WHOLE when absent, so a
household that configured one tier gets one tier, and a stricter income
context is a deliberate act.

Every run ends, per family, with the categorisation rate per source,
the provider-map misses (a card issuer's categorical vocabulary moving,
bar the residual bucket its map marks untranslatable; a bank's
untranslated booking types are rails, not misses — SPENDING.md §3), the
matched internal-transfer pairs (both legs — the audit surface for what
the matcher removed from the base), the largest unmatched legs including
the cross-currency shapes the matcher structurally cannot pair, and a
stratified sample of what is still uncategorised.

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
| `[FROM [TO]]` | trailing twelve months | Positional window, same grammar as `transactions` (`YYYY` / `YYYY-MM` / `YYYY-MM-DD`, an explicit pair, `-` for an open bound). May appear before or after flags. |
| `--period` | `monthly` | `daily` \| `weekly` \| `monthly` \| `quarterly` \| `annual` \| `total`. Maps onto the `date_trunc` part `report_spending_*` buckets by; `total` collapses the window into one bucket. |
| `--level` | `primary` | `primary` \| `detailed` — the category vocabulary `categories` groups by. The other views ignore it. |
| `-f`, `--format` | `table` | `table` \| `csv` \| `csv_plain` \| `json`. |
| `-C`, `--columns` | `default` | Per-view column set: names, `default`, `all`, or a `+ADD,-REMOVE` delta. |
| `-x`, `--currency` | `default_currency` | ISO 4217 output currency; historic FX at each line's `occurred_at`. |
| `-p`, `--privacy` | off | Redact account IDs, counterparties, and amounts. |

**The window default is trailing-twelve-months, not since-inception.**
The returns window defaults to inception because a return is a
cumulative fact about the whole history; a spending report is read
against recent habit, and a window reaching back past the day a
source's card ledger begins covers a cash-only population — a `total`
over it would silently answer a different question.

**No row-filter flags**, deliberately, matching `holdings positions`
and `transactions`: filtering by merchant, category or account belongs
to `-f json` plus a downstream filter, to SQL, or to the dashboard. A
filter flag on a report whose numbers are shares of a bucket would also
have to decide whether the denominator moves with the filter, and every
answer to that is wrong for some reader.

**Privacy classes** (§4.6) are the substantive per-column decision here:

- `merchant` takes the **free-text class**, like the narrative it was
  named from. The transfer fence gates candidacy for the merchant STORE
  (docs/SPENDING.md §5), not the column: a wire, ACH or P2P narrative
  is refused a verdict, has no store name, and falls back to the
  signature folded from that narrative (migration 0054), so a payment
  to a person surfaces its payee here on the non-private view. The
  store's own names carry the same exposure by a slower route — it is
  append-only across signature revisions and across widenings of the
  fence itself, and a stored verdict is applied by signature for as
  long as it is there, so a name bought while the fence was narrower
  outlives the fence that would now refuse it. The column is empty on a
  delta line besides (migration 0048, docs/SPENDING.md §7): a gift or
  an own-account move a rule or a pin placed shows no name, whatever
  the store holds for its signature. A card bill shows the issuer it
  was paid to (migration 0052) — an institution rather than a
  narrative, and redacted with the column all the same.
- `counterparty`, `description` and `merchant_signature` take the same
  class, and reach it more directly: they are the raw narrative and its
  fold, published for every row the fence let through *and* every row
  it stopped. The class masks the cell whole (`***`) without asking
  what the value looks like. A shape test is the wrong
  instrument here: the narratives that carry a person's name are
  multi-word, so any identifier heuristic passes precisely them.
- The account's external id and display name take the account-id class,
  so a card whose display name is its masked number redacts like any
  account id.
- Amount columns take the money class; category, provenance and the
  `share` percentage stay legible, like the returns percentages.

Exit codes follow §4.10's table: `2` for a usage error (unknown view,
invalid `--period` / `--level` / `-x`, unknown column), `3` when the
gold DB does not exist. `spending` is `(RO)` and never needs write
access.

### 4.13 `wealthdb income <view>`

The read surface over the income population (docs/INCOME.md, and §10.11
for the macros underneath) — §4.12 read in the other direction. Same
positional grain, same flags, same window default; three differences,
each a decision recorded in docs/INCOME.md §10.

| Flag | Default | Meaning |
| --- | --- | --- |
| `<view>` | required | `summary` \| `types` \| `transactions`. No `payers` view: payers rank on the dashboard only, as merchants do. |
| `[FROM [TO]]` | trailing twelve months | As §4.12. A positional year is the idiom for the calendar-year report: `wealthdb income types 2025 --period annual`. |
| `--period` | `monthly` | As §4.12. |
| `--level` | **`detailed`** | The one default that differs from spending's. The income taxonomy has ONE vendored primary, so the primary level folds every vendored type and extension into `INCOME` beside the deltas. |
| `-C/--columns` | `default` | `withheld` is available on `summary` and off by default: tax deducted at source, shown beside the income it was taken from and never subtracted from `net_income`. |
| `-f`, `-x`, `-p` | as §4.12 | `-p` masks `payer`, `payer_signature`, `counterparty` and `description` whole; types, provenance and `share` stay legible. |

`income` and `reversals` are positive magnitudes and `net_income` is
the difference, mirroring `spend` / `refunds` / `net_spend`; the
`transactions` view keeps canonical signs.

`wealthdb transactions` carries `payer`, `income_primary` and
`income_detailed` behind `-C`, beside the spending trio. A row can hold
both — a deposit the matcher paired is `internal_transfer` in each
overlay — and that view is the one surface showing a transaction from
both sides at once.

### 4.14 Future subcommands (sketch only)

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

JSON file. Path defaults to `wealthdb.cfg` under the XDG config dir
(`$XDG_CONFIG_HOME`, falling back to `~/.config`) and is
overridable with `-c` / `--config`. The `.cfg` extension is a
convention; the content is JSON. Tilde (`~`) and `$HOME` are
expanded in both the `--config` argument and the path values
inside the file.

Conventional host layout, produced by `wealthdb config`'s defaults:

```
$XDG_CONFIG_HOME/wealthdb.cfg          config file (this file)
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
| `account_overrides` | object | Optional. Nested map keyed by `silver_source_id` (outer) and `account_external_id` (inner) carrying user-supplied per-account `nickname`, `category`, `tax_wrapper`, and/or `management_style` strings, or `exclude: true` to drop the account and every fact keyed to it (a sweep over gold after the load's streams drain, §13.9). All inner fields are optional but at least one must be set per entry, and `exclude` may not be combined with a column override. `tax_wrapper` and `management_style` values are validated against the canonical enums (`internal/canonical/enums.go`) at config-load time. The loader applies overrides AFTER the adapter stamps its own values, so config wins on overlap. |
| `portfolio_overrides` | object | Optional. Portfolio-grain counterpart of `account_overrides`. Nested map keyed by `silver_source_id` (outer) and `portfolio_external_id` (inner); the override applies to every account whose `portfolio_external_id` matches — e.g. a whole crypto portfolio inside an IRA / trust / Stiftung wrapper. Accepts `tax_wrapper` (unlike `account_overrides`, which also takes nickname / category / management_style) or `exclude: true`, which drops the portfolio and every account inside it with their facts; one of the two must be set and they may not be combined. A per-account `tax_wrapper` override still wins over a portfolio one. See §13.9. |
| `instrument_overrides` | object | Optional. Nested map keyed by `silver_source_id` (outer) and `instrument_external_id` (inner) pinning a per-instrument taxonomy pair. Each entry sets both `asset_class` (the exposure) and `vehicle` (the wrapper); both are required and validated as an admitted taxonomy pair (§7.2, docs/TAXONOMY.md) at config-load time. For holdings the adapter's structured signals and name heuristics misclassify — e.g. an exchange-traded commodity trust whose security name doesn't give away what it holds (`metal × etf`). The loader applies overrides AFTER the adapter classifies, to both the instrument dimension and every position row referencing it, so config wins on overlap. See §13.9. |
| `inception_overrides` | object | Optional. Pins the returns-window START date per source / portfolio / account so an entity's track record begins at its first real capital rather than a tiny pre-history dust base. Three grain-keyed maps (`sources`, `portfolios`, `accounts`), values `YYYY-MM-DD` (UTC). Consumed by the returns engine at query time — it stamps no gold column. See §5.4. |
| `returns_exclude` | object | Optional. Omits whole accounts or portfolios from HIGHER-grain return aggregates (`sources`, `global`) while still reporting them at their own grain — e.g. keep holdings tracked in a shared login that belong to another person out of the source/global returns. Two grain-keyed maps (`portfolios`, `accounts`), each keyed by `silver_source_id` to a list of external ids; a listed source id must name a declared silver source. Returns only — holdings / net-worth are unaffected. See §5.5. |
| `returns_policy_overrides` | object | Optional. Per-source adjustments to the registered ReturnsPolicy, keyed by `silver_source_id` (not adapter kind). `flow_regime` replaces the source's flow classification with a named regime's canonical kind sets (`"flow_complete"` \| `"crypto_partial"` \| `"nav_only"`); `accounts_grain` sets the per-account display mode (`"normal"` \| `"blanked"` \| `"hidden"`). Unset fields keep the registered policy's values. See §5.6. |
| `returns_hide` | object | Optional. Suppresses accounts' or portfolios' OWN return rows at every grain while their values and flows keep contributing to every aggregate — the display mirror of `returns_exclude`. Same grain-keyed shape (`portfolios`, `accounts` per `silver_source_id`). See §5.7. |
| `returns_transfer_matching` | object | Optional, off by default. Enables the cross-source transfer matcher: an external leg whose counterparty leg exists in ANOTHER source (opposite sign, same native currency, equal amount within `tolerance_pct`, within `window_days`) nets out of every return aggregate containing BOTH legs, while finer grains keep counting each leg. Fields: `enabled` (bool), `window_days` (0–30, default 5), `tolerance_pct` (0–5, default 0.5). See §5.8. |
| `income` | object | Optional. Groups the income feature's per-deployment knobs — `accounts`, `rules[]`, `pins`, `categorization` — in `spending`'s shapes. Two blocks spending has are deliberately absent: there is one internal-transfer matcher and one transfer-override ledger, and both families read them (docs/INCOME.md §1). Absent ⇒ every account counts, no rules and no pins apply, and the model tier inherits `spending.categorization`. |
| `income.accounts` | object | Optional. The income account scope, in `spending.accounts`' shape and stamped into gold's `income_account_scope`. Its own table on purpose: an account excluded from spending because its outflows double-count something is not thereby an account whose inflows are not income. |
| `income.rules[]` | array | Optional, default empty. As `spending.rules[]`, with the value field named **`type`** and validated against the INCOME vocabulary — a spending value here fails the load naming `income.rules[i].type`. |
| `income.pins` | string | Optional. Path to the income pins ledger: the spending ledger's format with an `income_detailed` column. See §13.11. |
| `income.categorization` | object | Optional. As `spending.categorization`, `fence_person_names` included. **Absent ⇒ inherits `spending.categorization` whole** — one household, one local model, one answer to what may leave the machine. Whole-block rather than per-field: a half-inherited endpoint is a configuration nobody wrote down, and a half-inherited fence would be one that quietly turned itself off. `context` additionally accepts `payer`, the income spelling of the narrowest level. |
| `spending` | object | Optional. Groups the spending feature's per-deployment knobs. Absent ⇒ every account counts, the internal-transfer matcher runs on its defaults, no rules and no pins apply, and `wealthdb categorize` refuses for want of a model. See docs/SPENDING.md. |
| `spending.accounts` | object | Optional. Account-scope overrides, keyed by `silver_source_id` in the `returns_exclude` shape, with `include` / `exclude` lists of account ids. An account may not appear in both. Stamped into gold's `spend_account_scope` by every enrichment pass, so removing an entry removes its effect. An entry naming an account gold does not hold scopes nothing; the pass counts such entries and the load summary reports how many. Which way round the overrides bite is the scope rule — see docs/SPENDING.md §1. |
| `spending.internal_transfer_matching` | object | Optional. Knobs for the matcher that pairs the two legs of an own-account move so neither counts as spending: `window_days` (0–30, default 5) and `tolerance_pct` (0–5, default 0.5). Deliberately the same defaults as `returns_transfer_matching` — one matching core, one banding. |
| `spending.rules[]` | array | Optional, default empty. The deployment's own entries in the rule tier, each `{ "match": <regex>, "category": <spend_detailed> }`. `match` is compiled case-insensitively at load and tested against `counterparty`, the full `description` (memo included) and `provider_category` — the issuer's own filing of the row — each on its own; among config rules the first written wins. Matching the issuer's filing is how a rule reaches a class of merchant the descriptor never names, and is what makes the provider an input to the rule tier rather than a tier that outranks it. `category` may be **any** valid `spend_detailed` value, vendored or delta, in the taxonomy's case-sensitive spelling. An invalid pattern, one matching the empty string, or an unknown category fails the load naming `spending.rules[i]` and the text. Consulted after the built-in rules and below the matcher and the pins, with provenance `rule`. Deployment-specific: lives in the user's config, never in the repository. See docs/SPENDING.md §3, *Config-supplied rules*. |
| `spending.pins` | string | Optional. Filesystem path to a CSV ledger of per-transaction category pins — the top of the precedence lattice, for the row nothing else can classify. Columns `silver_source_id, account, occurred_at (YYYY-MM-DD), amount, currency, spend_detailed, note`; `account` is a gold `account_external_id` or a nickname, resolved as `equity_transfers` resolves it; `spend_detailed` may be any valid value, vendored or delta; `note` is free text kept for the ledger's own readability and is not carried into gold. `~` / `$HOME` / `${VAR}` expanded, a relative path resolved against the config file's directory; a missing file is a no-op. Re-stamped by every enrichment pass, so removing a row removes its effect. See §13.11. |
| `spending.categorization` | object | Optional. Configures `wealthdb categorize`: the model endpoint (`model`), how much of a transaction reaches it (`context`), and the per-merchant narrative cap (`descriptor_samples`). Absent ⇒ the subcommand refuses; the deterministic tiers are unaffected and keep running on load. |
| `spending.categorization.model` | object | Optional. LLM endpoint asked for a category per merchant signature. Same shape and same API support as `symbol_resolution.model` (`baseUrl`, `api`, `apiKey`, `name`). |
| `spending.categorization.context` | string | Optional. `"merchant"` (default) sends merchant signatures only; `"descriptor"` adds the raw statement narratives; `"transaction"` adds date, amount, account kind and nearby-transaction signatures. The default is the most private level by decision, not by accident. Independent of the fence, which gates candidacy at every level. |
| `spending.categorization.descriptor_samples` | integer | Optional, 0–20, default 3. Caps the raw narratives sent per merchant at the two context levels that send any. Range-checked even at the `merchant` level, where nothing reads it. |
| `spending.categorization.fence_person_names` | boolean | Optional, **default `true`**. Keeps a signature that is a bare person's name — two or more all-letter tokens with no organisation marker — off a non-card account out of the model tier, at every context level. Turn it off only when the model endpoint is on this machine: a person's name is the one signature shape that is PII by itself, and every other arm of the fence reads a marker beside the name rather than the name. Card rows are exempt by construction (most card merchants are two plain words). `income.categorization` inherits it with the rest of the block. See docs/SPENDING.md §5. |
| `symbol_resolution` | object | Optional. Groups the knobs for `wealthdb resolve-symbols`: the LLM endpoint (`model`) and the ticker-mapping override list (`overrides`). Both inner fields optional; the subcommand fails if `model` is unset and `--overrides-only` wasn't passed. |
| `symbol_resolution.model` | object | Optional. LLM endpoint used to back-fill missing instrument tickers (`baseUrl`, `api`, `apiKey`, `name`). Only the OpenAI-compatible Chat Completions API (`api: "openai-completions"`) is supported today. |
| `symbol_resolution.overrides[]` | array | Optional. User-authored ticker-mapping overrides applied at the start of every run under `model_name='manual-override'`. Each entry keys on `silver_source_id` + `lookup_kind` (`instrument_external_id` or `name`) + `lookup_value`; set `symbol` to correct a ticker, or `delete: true` to suppress a row where no real ticker exists. |

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
  **global** grain is never anchored (v1).
- **Semantics:** `winFrom = max(data-inception, configured-inception, --from)`
  — it can only move an anchor LATER, never earlier (an inception before an
  entity's data is a no-op). The opening base `V0` becomes the entity's
  carry-forward value on that date. Truncation is tagged
  `configured_inception` (replacing `since_data_inception`). Composes with
  the policy `Inception` mode (e.g. UBS `first-real-snapshot`) as `max`.
  MOIC is unaffected (time-insensitive); this re-anchors the TWR/MWR chain.
  Ledger flows dated before the resolved inception fall outside every
  window; the `flows_before_inception` quality flag surfaces that exclusion
  (and, conversely, marks the entities where this override is the
  documented remedy — a transaction backfill reaching further back than
  the value spine supports).
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
  grain shows every non-hidden account (§5.7), and an excluded PORTFOLIO still
  shows its own portfolios row (only an excluded ACCOUNT drops from its
  portfolio there). At
  the sources and global grains, an excluded account, and every account of an
  excluded portfolio, are omitted from the aggregate.
- **Returns only.** Holdings / net-worth views are unaffected; excluding
  someone's holdings from your net worth is a separate owner-dimension task.
- Absent ⇒ nothing excluded, byte-identical to before.

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
  deployment re-surfaces a hidden source's rows.
- **Partial semantics:** unset fields keep the registered policy's values;
  code-side knobs without a config field (onboarding scope, conduit kinds,
  inception mode, …) are never touched.
- Applied identically by `wealthdb returns` and the materialized
  `report_returns` behind the web dashboard. Absent ⇒ registered policies
  apply unchanged, byte-identical to before.

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
- **Policy composition:** per-source policies already hide whole conduit
  sources (`AccountsGrainHidden` — the deposit-bank collectors register it
  by default); this block covers deployment-specific ids on top. An id in
  both `returns_hide` and `returns_exclude` is excluded from the math AND
  shows nowhere.
- **Returns only.** Holdings / net-worth views are unaffected.
- Absent ⇒ nothing hidden beyond policy, byte-identical to before.

### 5.8 Cross-source transfer matching (opt-in)

Per-source flow classification can never pair the two legs of a
cross-custodian move: the sending source books a real withdrawal/journal, the
receiving one a real deposit, and each is correct at its own boundary — but an
aggregate containing BOTH accounts double-represents the move (the
`unmatched_transfers=N` population is dominated by exactly these legs).
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
- **Limitations (v1):** same-native-currency pairs only (a CHF→USD
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

```go
package silver

type Adapter interface {
    Kind() string                                       // "schwab", "ubs", "swissquote"
    // Open attaches to the silver source(s) in spec. Single-file
    // adapters read spec.Path; merged adapters (UBS = web + PSN)
    // read spec.Subsources and align identities via spec.Relationships.
    Open(ctx context.Context, spec OpenSpec) (Connection, error)
}

type OpenSpec struct {
    Path          string             // single-file adapters
    Subsources    []Subsource        // merged adapters (UBS = ubs-web + ubs-psn)
    Relationships []RelationshipPair
}

type Subsource struct {
    Kind string
    Path string
}

// RelationshipPair pairs a web banking_relationship_id with a PSN
// relationship_id under one label so downstream sees a single key.
type RelationshipPair struct {
    Label            string
    WebID            string
    PSNID            string
    PSNStartOverride int64 // Unix seconds UTC; 0 = auto-detect the web↔PSN cutover
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
    Portfolios   []PortfolioChange
}

type TransactionStream interface {
    Next(ctx context.Context) (batch TransactionBatch, more bool, err error)
    Close() error
}

type TransactionBatch struct {
    Transactions []TransactionChange
}

// The *Change types mirror the gold-table column shapes one-to-one.
// They live in internal/canonical so an adapter imports only the
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

`silver.Register(&Adapter{})` is called from each backend
package's `init()` (the name comes from the adapter's `Kind()`).
Backends live under `internal/silver/<source>`. The registered set is
not restated here — it would drift the moment one is added. Two places
carry it, and both are checked by the build: the blank-import block in
`cmd/wealthdb/main.go` below, and the `silver_kind` CHECK on
`silver_sources` (§7.2), which a load validates every source against.

```go
import (
    _ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/schwab"
    _ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/ubs"
    _ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/swissquote"
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
- [adapters/carta.md](adapters/carta.md)
- [adapters/cointracking.md](adapters/cointracking.md)
- [adapters/chase.md](adapters/chase.md)
- [adapters/amex.md](adapters/amex.md)

(Adapters without a dedicated doc here are described inline
where they diverge from the gold-side contract above.)

Each adapter doc is self-contained for the engineer writing or
maintaining that adapter. New bank adapters add a new file in
the same directory.

### 6.8 Cross-cutting adapter rules

A few rules apply to every adapter regardless of bank:

- **Unrecognised `kind` / `transaction_type` values fall through
  to gold `kind = 'other'`** with the raw source value preserved
  in `payload`, rather than failing the load. `wealthdb status -v`
  reports the count of `other` rows per silver source so taxonomy
  drift is visible. The one adapter that departs is **amex**, which
  kinds a row with no usable direction by the sign of its amount
  (`purchase` / `card_payment`) and keeps the raw value in
  `payload.source_kind`: on a card ledger `other` reaches neither the
  spending base nor the internal-transfer matcher, so the row would
  vanish from both rather than surface as backlog
  ([adapters/amex.md](adapters/amex.md) §5).
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
-- and recreates the table).
CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex'
    )),
    silver_path         TEXT    NOT NULL,            -- as observed at last load
    high_watermark      BIGINT  NOT NULL,            -- plugin's logical change number after the last load
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL,
    fx_priority         INTEGER                      -- config's FX precedence, stamped on load; §10.6 / §13.2
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
--   'crypto'        — crypto holding bucket (cointracking); wallet
--                     display names don't reliably distinguish
--                     exchange from self-custody, so one kind covers
--                     both. 'crypto_exchange' / 'crypto_self_custody'
--                     stay reserved for a future adapter that surfaces
--                     the distinction at source.
--   'mortgage'      — real-property-backed liability; outstanding
--                     principal sits as a negative-value position
--   'card'          — revolving-credit liability; unlike a mortgage it
--                     carries no position, its outstanding balance
--                     being negative cash on the account
--   'donor_advised_fund'
--                   — irrevocably donated charitable assets; its own
--                     kind so DAF balances can be included in or
--                     excluded from net worth by kind
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
-- `counterparty` and `provider_category` were added by gold migration
-- 0038, both nullable — a non-card source leaves them unset.
-- `counterparty` is not merely informational: it is the input to the
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

- **Views.** ~~The first cut keeps gold as tables only.~~ Landed, in
  the shape the reasoning predicted: the views exist because a query
  needed them, not ahead of one. They serve the optional Metabase
  server and are named `web_*` for that reason (§10.10), so nothing
  in the engine depends on one.
- **Materialised positions-as-of cache.** The as-of query in §10 runs
  fast on the index; no cache needed at personal scale.
- **A surrogate `account_id` / `instrument_id` integer key.** Tuple
  PKs `(silver_source_id, account_external_id)` are noisier in
  joins but eliminate a whole class of "how do I generate a stable
  surrogate" bugs. DuckDB has no SERIAL/SEQUENCE that fits well.
- **Audit columns on dimensions** beyond `first_seen_at` /
  `last_seen_at`. `payload` carries the rest.
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
           fold dimensions: batch.Portfolios, batch.Accounts,
                            batch.Instruments (one record per entity — §8.4)
           insert facts:    batch.Positions, batch.CashBalances, batch.FxRates
           if not more: break
       upsert the folded dimension records

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

Two whole-file steps then run **after** the per-source loop, outside
its transactions, because neither is a per-source fact: the
config-driven FX source precedence is stamped into `silver_sources`
(§13.2), and the deterministic spending pass re-asserts every spend
verdict in gold (§10.10, docs/SPENDING.md §3 — an own-account move's
two legs routinely arrive from two different sources, so neither is
recognisable until both have landed). A failed FX stamp is a warning:
it degrades a conversion. A failed spending pass is an error: it
leaves own-account moves counted as spending, which is a wrong answer
rather than a degraded one. `reload` runs both the same way, on both
its in-place and fresh-file paths.

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
field-less record exists. (The one-directional form of this guard
silently dropped attributes only an older observation carries on
every full rebuild — schwab-web's statement-derived `tax_wrapper`,
dated at its silver snapshot, lost to newer wrapper-less api rows.)
The seen-at range expands in both directions regardless —
re-emitting an older snapshot pulls `first_seen_at` backward.

(An earlier sketch used a single `WHERE EXCLUDED.last_seen_at >=
accounts.last_seen_at` at the end of the SET clause; that gated
the whole UPDATE including `first_seen_at` expansion, defeating
the union semantics. Per-column CASE keeps the two concerns
independent.)

The loader folds each load's dimension emissions down to one record
per entity before the upsert runs (`gold.ChangeAccumulator`).
Adapters re-emit accounts / instruments alongside every snapshot
they walk — tens of thousands of emissions folding onto a few
hundred entities — and executing the guard once per emission made
dimension upserts dominate load time. The fold applies records in
arrival order with the guard semantics above. Because both fold and
SQL are symmetric-coalescing per column, the folded single upsert
stores exactly what record-by-record upserts would — emission order
no longer affects which attributes survive.

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
DELETE FROM symbol_resolutions   WHERE silver_source_id = ?;
DELETE FROM spend_txn_enrichment WHERE silver_source_id = ?;
DELETE FROM transactions         WHERE silver_source_id = ?;
DELETE FROM fx_rates             WHERE silver_source_id = ?;
DELETE FROM cash_balances        WHERE silver_source_id = ?;
DELETE FROM positions            WHERE silver_source_id = ?;
DELETE FROM instruments          WHERE silver_source_id = ?;
DELETE FROM accounts             WHERE silver_source_id = ?;
DELETE FROM portfolios           WHERE silver_source_id = ?;
DELETE FROM load_audit           WHERE silver_source_id = ?;
DELETE FROM silver_sources       WHERE silver_source_id = ?;
COMMIT;
```

The order matters because of foreign keys. The silver database
itself is untouched. The watermark is removed with the
`silver_sources` row, so a follow-up `wealthdb load <id>` starts
fresh (treating `high_watermark` as -1).

`symbol_resolutions` and the two enrichment tables — `spend_txn_enrichment`
and `income_txn_enrichment` — go with the rest because all are per-source
derived data — resolved tickers keyed to the source's instruments,
enrichment derived from the transactions being deleted — and the next
resolve or enrichment pass regenerates them. Four tables deliberately
survive, two per family: the verdict stores `spend_merchant_categories`
and `income_payer_categories` are global knowledge keyed by signature,
with no source column and verdicts that were paid for, and the scope
stamps `spend_account_scope` and `income_account_scope` are configuration
stamped into gold and re-stamped whole by every enrichment pass — the
`fx_priority` precedent rather than source data. Lifecycle table:
docs/SPENDING.md §8. The invariant is pinned by
`TestResetClearsEnrichmentKeepsMerchantStore` and
`TestResetClearsBothOverlays`.

`wealthdb reset -a` runs the same per silver source. A full rebuild is
`wealthdb reload -a`: every source-derived table is rebuilt into a fresh
compact file, which is swapped over the live path with both verdict
stores carried across (`carryVerdictStore`, once per entry in
`paidStores`). Two tables do not
survive that swap — `symbol_resolutions`, which
`wealthdb resolve-symbols` writes and no load does, and
`report_returns`, which `web-materialize` writes (§10.9) — so the
rebuilt file carries neither until a resolve and a materialization
follow it. `reload -a --in-place` keeps the live file instead, forgoing
the compaction and leaving `report_returns` alone; the per-source reset
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
spine via an at-or-before ASOF join — so per-day compute stays cheap. A day an
entity has no line on at all is omitted (a day it is observed at zero on is
not: that emits an explicit 0 row). `report_global_history` is
`Σ report_accounts_history`. The Metabase models wrap the **multi-currency**
variants of these (§10.8), not the single-currency macros directly.

The active snapshot is resolved **per key per day** — migration `0051`,
`hist_active_pos()` / `hist_active_cash()`. Each key is ASOF-joined onto the day
spine on its own snapshot series, so it contributes its own most recent
observation at or before the day and the day's total is the sum across keys. The
key is `(source, account)` for positions and `(source, account, currency)` for
cash, where every balance row is its own observation. One active snapshot per
*source* (the 0022 rule) is only right for a source that writes every account in
one run; where a deposit run, a card run and a statement backfill each land under
their own `snapshot_at`, it reported whichever run was last and dropped every
account the others carried — a card-only day read as that card's negative balance
alone. Four rules complete the picture:

- **Zero is an observation.** The cash line bases keep `amount = 0` rows (they
  used to be filtered as noise), so an account paid down to zero carries the
  zero, not the balance before it, and a zero line converts to 0 in every
  reporting currency instead of following the FX chain — no rate exists for a
  currency nobody quotes, and a NULL there would blank the whole entity-day's
  sum rather than just that line. Positions need no filter of their own: the
  carry unit is the account's whole snapshot, so a holding absent from that
  account's next snapshot is sold rather than carried, and an explicit $0
  position row (`silver.ClosureMarkerBatch`) reads as zero. The per-position
  series (`report_positions_history`) keeps the plain FX chain, as its
  point-in-time twin does — a display row has no sum for a NULL to poison.
- **A later run that re-covers the key's company ends it.** Absence is evidence
  of closure exactly when the run that produced it was in a position to report
  the key: a key leaves the series at the first later snapshot of its source
  that covers every OTHER key observed alongside it in its own last snapshot
  *and still reported at that snapshot*. Company that left in the same run is
  not company the source could re-cover, so it is not required — otherwise two
  accounts closing in one nightly dump would each hold the other's ending open.
  A nightly full dump therefore ends a closed account on the next dump, as the
  per-source rule did, whether one account closed or several; a card-only or
  deposit-only run covers none of another run's keys and ends nothing. A key
  observed alone has no company to re-cover.
- **The source's clock bounds the rest.** A key nothing ever re-covers stops
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
  peer set — indistinguishable from closure at this grain, and what the
  per-source rule this replaces did with that shape too.

What follows for readers of these macros:

- **`history@today` reconciles with `report_*(MAX)` on totals**, not on rows,
  for a source that writes every account in one run. The point-in-time reports
  (§10.1, migration 0021) carry a row for every account in the dimension — 0
  when the source's latest snapshot gave it no lines — while the history emits
  rows only for entity-days that have lines.
- **Where runs are partial the two differ by design**: the history carries every
  account, while the point-in-time reports read one latest snapshot per source
  and so value only the accounts of the source's last run. `wealthdb holdings global` and
  `wealthdb holdings positions` read the point-in-time macros, so for such a source the
  CLI headline sits below a chart built on the history. Pulling the CLI onto the
  same per-account resolution is a known follow-up; nothing in gold depends on
  the two disagreeing.
- **`report_positions_history.snapshot_at`** is the account's own active
  snapshot, so it varies per account within a source-day where it used to hold
  one value per (source, day).
- **Cost:** the day spine is crossed with keys rather than with sources, so its
  cardinality grows by the accounts-per-source factor.

### 10.8 Multi-currency reports (Metabase)

The Metabase models need a value column **per reporting currency** (USD, CHF,
EUR) so the currency is picked by picking a column — Metabase native
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
- **`account_kind` on the transaction macros.** `report_transactions` and
  `report_transactions_multi` carry the owning account's `account_kind`
  (migration 0039), so a consumer can fence a kind out of a chart — the web's
  income and fee charts exclude `card`, whose finance charges and annual fees
  are `interest` / `fee` transactions that would otherwise read as investment
  income and portfolio costs. NULL when the transaction's account is absent
  from `accounts` (a LEFT JOIN), so a fence must keep NULLs.
- **Merchant and spend category on the transaction macros.** The same two
  macros also carry `merchant_name`, `spend_primary` and `spend_detailed`
  (migration 0042), resolved by `spend_txn_categories()` (re-issued by 0050,
  which resolves the model tier's provenance, and by 0052 and 0054) — the
  overlay's precedence lattice, extracted so `spending_lines_base` and the
  transaction reports share one definition of it (§10.10, docs/SPENDING.md §3). NULL for
  every row the enrichment pass does not reach. Unlike the spending reports,
  these keep a row the matcher called an own-account move and show what it was
  categorised as: `wealthdb transactions` is the whole ledger. `merchant_name`
  resolves in three steps (docs/SPENDING.md §7): a delta row shows its issuer
  label or nothing (migrations 0048 and 0052), otherwise the store's name for
  the signature, otherwise the row's own `merchant_signature` verbatim
  (migration 0054).
- **Lockstep on the transaction macros.** `gold.TransactionsBetween` runs
  `SELECT *` with a positional scan, so every column added to these macros must
  land in `gold.TransactionRow` and the scan list in the same change (0039,
  0042, 0071 and 0075 each made that edit). The returns flow loaders are safe by
  construction: they project named columns.
- **`transactions.check_number`** (migration 0075) is the number written on a
  paper cheque drawn on the holder's own account, kept so a row can be matched
  against the holder's own paper records. An adapter sets it **only on an
  outflow**: the field names an outgoing payment, and the sign test is what
  keeps it meaning that, because a bank may put its own instrument reference in
  the same silver column on an incoming credit and a deposit export may head
  that column "Check or Slip #". Deliberately not a transaction kind — a cheque
  is an instrument, not a distinct economic event — and deliberately a column
  rather than `payload`, which holds what could *not* be canonicalised.
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

- **SQL assembles**, reusing existing macros — no new migration. The per-account
  carry-forward value series comes from `report_accounts_history(p_ccy)` (§10.7;
  its ASOF-inner join already omits pre-inception days, so a boundary before an
  account's first snapshot reads as NULL — "not yet alive", not 0, and its
  per-account carry means a run that covered only part of a source no longer
  punches holes in the spine). External
  flows come from `report_transactions(from,to,p_ccy)` (`net_amount` converted at
  `occurred_at`). The source's adapter kind (`silver_sources.silver_kind`) and a
  `DISTINCT snapshot-day per account` query complete the inputs.
- **Go computes** (`internal/returns`, pure + unit-tested): Modified-Dietz per
  sub-period, geometric chaining, XIRR (Newton + bisection), the per-adapter flow
  policy, and the synthetic onboarding/closure mechanics. The gold orchestration
  is `internal/gold/returns.go` (`RunReturns`), aggregating the per-account spine
  to every grain.

Method and conventions (see `docs/RETURNS-NOTES.md` for the full rationale and
the per-adapter flow classification):

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
  crypto = fiat flows only; carta/equityzen = boundary deposit/withdrawal
  only; manual = NAV-only). `value_outccy`
  already carries the canonical sign, so Dietz `F_i = +value_outccy` and XIRR
  `cf = -value_outccy` with no per-kind exception.
- **Historic-FX**: FX movement is part of the return. **Net
  of fees and taxes paid** (after-tax) — costs stay inside the value series.
- **Mortgage / net-negative entities** are excluded from coarse rollups and shown
  as a separate `nonpositive_base` liability line.
- **Credit cards (`account_kind='card'`) are returns-invisible**: no value, no
  flow, no row, at any grain. A card is a spending instrument, not an
  investment. The exclusion is engine-level and kind-keyed (the loader seam
  `appendSeries`), never a `ReturnsPolicy` knob — policies default to a no-op,
  so a card from an unregistered source would otherwise leak. Consequence: with
  cards loaded the returns global no longer equals `report_global`; the gap is
  the card balances, which net worth still counts, plus — for a source whose
  runs are partial — the accounts the per-account value spine carries and the
  per-source `report_global` omits (§10.7). The checking-side leg
  of a card payment stays a real external withdrawal — the money left the
  returns-visible system. See `docs/RETURNS-NOTES.md`, "Credit cards are
  returns-invisible".
- **Account-grain is exact**; coarse grains are best-effort (heuristic transfer
  netting, synthetic onboarding for staggered inception). **Returns are NOT
  additive across grains** — `global == Σ accounts + hidden plumbing` is a
  *value* identity, not a return identity (hidden conduit rows, §5.7, are in
  global but emit no accounts-grain row).
- **Staggered inception (corrected semantics).** When a constituent joins a
  coarse aggregate *mid-window* (debut `d > winFrom`), its arrival is booked once
  as a synthetic onboarding inflow of its **full** first-snapshot value at `d`,
  and **all of its own external flows dated `≤ d` are subsumed** (dropped from the
  aggregate flow series) — they happened while the aggregate value series did not
  yet reflect the account, so counting them too would double-count the capital and
  drive the chained TWR below −100%. A constituent already alive at `winFrom`
  keeps every in-window flow and gets no onboarding. The symmetric **closure**
  case: flows a closing constituent dates inside its terminal zero-carry tail
  are subsumed as strays under the default per-source `ClosureScope` (the
  zeroing value drop is the exit signal), while `ClosureLedgerExact` sources
  (carta / equityzen) keep them as the real, dated exit proceeds
  (RETURNS-NOTES.md §"Netting and closure"). The netting interaction is handled by running
  transfer/journal netting over the *full* candidate set (pre-debut legs included)
  **before** subsumption, so genuine internal pairs annihilate and only a
  constituent's own surviving pre-debut/closure capital is subsumed — never
  orphaning a phantom outflow whose sibling sits on an already-alive account
  (whose value series does reflect it). Onboarding still legitimately recognizes
  *untracked pre-existing* capital (a late account with no funding transactions at
  all, e.g. a custody account whose backfill carries no transactions), which is NOT a double-count.
  See `docs/RETURNS-NOTES.md`, "Staggered-inception subsumption".
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
  `docs/RETURNS-NOTES.md`, "Pluggable per-source policy".

The honesty surface is the **`quality` column**: every n/a carries a reason, and
every approximation is tagged (`since_data_inception`, `configured_inception`,
`partial_window`, `staggered_inception`, `accounts_grain_meaningless`,
`empty_bucket`/`carried_forward`, `boundary_same_snapshot`, `stale_snapshot`,
`dropped_while_nonzero`, `dietz_degenerate`, `nonpositive_base`, `mwr_no_flows`,
`mwr_no_sign_change`, `mwr_nonunique`, `mwr_no_converge`, `mwr_incomplete_flows`,
`mwr_negative_net_capital`, `unmatched_transfers=N`, `cross_source_netted=N`, `journal_present`, `nav_only`,
`nav_only_capital_call_risk`, `crypto_unclassified_transfers`,
`unknown_adapter_policy`, `fx_clamped_flow`, `pre_fx_history`,
`flows_before_inception`, `after_tax`). Cross-grain note: `global == Σ accounts + hidden
plumbing` is a **value** identity (verified by reconciliation test; hidden
conduit rows, §5.7, are in global but emit no accounts row), and **returns
are not additive across grains**.

### 10.10 Spending reports

The spending family (docs/SPENDING.md) charts the population
`spending_lines_base(from, to)` defines — the scoped accounts, spend-side
kinds, category resolved, own-account moves and capital deployed
(`internal_transfer`, `investment`) removed. A card bill on a card wealthdb
does not itemise stays in as `card_spend` (migration `0046`), its own
primary: an unpaired card payment is generic card spend, not an own-account
move, and only the matcher — which has seen the card's own leg — may net it
out. A cash gift or family support stays in as `gift` (migration `0047`),
likewise its own primary: spending with no merchant behind it, placed by a
config rule or a pin. Migration `0042` adds
three reports over it, each with the `_multi` sibling §10.8 describes
(single-currency VARCHAR money for the CLI, DECIMAL USD/CHF/EUR for the web):

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
  `quarter` | `year`), or NULL for the single `total` bucket. The part is a
  macro parameter, which DuckDB accepts — but `total` is not a `date_trunc`
  part and the engine binds the call even on a CASE branch it discards, so the
  substitution happens **inside** the call. The timestamp comes from `epoch_ms`,
  not `to_timestamp`, which is TIMESTAMPTZ and would truncate in the session's
  zone rather than UTC.
- **Shares over magnitudes.** A category's share is `|net_spend|` over the
  bucket's `Σ|net_spend|`, so a net-positive category cannot shrink the
  denominator and push the others past 100%; a bucket's shares sum to 1.
- **`(uncategorized)` is a label.** The backlog is materialised **before** the
  GROUP BY at both levels, so it groups like any other category instead of
  rendering as a blank row. At transaction grain the category stays NULL —
  `web_spending` labels it at the view.
- **No merchant on a delta line, except a card bill.** `merchant_name` is
  refused on a line whose resolved category is a delta (migration `0048`,
  docs/SPENDING.md §7), whatever the merchant store holds for its signature.
  The one exception is a card bill, which names the ISSUER it was paid to
  (migration `0052`): the bill's own narrative names the payer's bank or the
  holder, so the issuer the built-in card rule matched is the only handle on
  which card the money went to, and it is stored on the enrichment row rather
  than read from the merchant store. `web_spending` inherits the column; the
  dashboard's merchant rankings exclude the delta categories, so an issuer
  never ranks as a merchant.
- **Every other line names its merchant, store row or not.** The store is the
  model tier's alone and the model tier is the weakest scope, so a line a
  provider, a rule, the matcher or a pin placed is never a model candidate and
  never acquires a store row — and a line nothing placed has none by
  construction. `merchant_name` therefore falls back to the line's own
  `merchant_signature` (migration `0054`, docs/SPENDING.md §7), VERBATIM: the
  cell is then the value of the `merchant_signature` column beside it, the
  fold's upper case distinguishes a derived name from a model-written one, and
  identity is the only rendering stable by construction, so a report groups one
  signature as one merchant. (`initcap` does not exist in this DuckDB.) An
  empty or blank signature still renders nothing. The candidacy gates that keep
  a bare booking code or the provider's own filing away from the model fence
  the STORE and not this column, so such a fold surfaces, and ranks, under
  itself; the merchant rankings widen from the lines the store named to every
  non-delta line carrying a signature. Every consumer inherits the column by
  name — the three spending reports and their `_multi` siblings,
  `report_transactions`, `web_spending`, both CLI views — and none was
  re-issued.
- **Reconciliation.** Σ categories == the summary bucket, for every period and
  level. The identity is structural (one base, one sign split) and therefore
  cannot catch a wrong population: an over-eager internal-transfer match removes
  a row from both sides at once and the month is silently cheaper. The layered
  populations are what surfaces that — `spend_enrichment_population` is
  deliberately the layer **before** the exclusion, so a removed row is still
  there carrying the tier that removed it (docs/SPENDING.md §6, the matched-pair
  canary).

Migration `0043` adds the two serving views, in the 0032 shape (an `epoch_ms`
TIMESTAMP reduction Metabase syncs like a table; re-issued in that form by
migration `0049`):

- **`web_spending`** — transaction grain over
  `report_spending_transactions_multi`, with the account's identity for the
  picker and both category levels for the breakdowns, NULL rendered as
  `(uncategorized)` here.
- **`web_card_balances_history`** — a card account's owed balance per UTC day,
  carried forward, in the three reporting currencies
  (`report_card_balances_history_multi()`). Its grain is finer than the
  account-history macros': one row per (source, account, **currency**), carrying
  the native balance alongside the converted trio, which is what the card cards
  chart. It was also the first macro to ASOF-join each (source, account,
  currency) onto the day spine independently — migration `0043`, because the
  account-history macros then gated each day on one active snapshot per source
  and lost a card behind a deposit-move day (and the deposits behind a statement
  closing). Migration `0051` made that resolution the rule everywhere and
  matched this view's cash grain, so the two now resolve a day the same way;
  this view keeps the card-grain series. They still differ at the end of one:
  0043 bounds nothing, so a card its source stops reporting keeps its last owed
  balance to the end of this view's spine, where the account-history macros end
  it (§10.7). Zero balances are kept in both: a paid-off card really is at zero,
  and dropping the row would leave the series owing money forever.

Both views label an account through the shared `account_label` macro
(migration `0063`): `<name> (<source> <kind>)`, where the name falls back
to the account's id when an adapter leaves it unset (`0061`) — a name on
its own says neither which institution the account belongs to nor whether
the money moved through a deposit account or a card, and names collide
across sources. The name comes first so a chart truncating a long label
clips the annotation rather than the identity. The bare name and the id
stay projected beside it.

### 10.11 Income reports

The spending macros of §10.10 read in the other direction, over the
income population. `income_scoped_accounts()`,
`income_enrichment_population(f, t)` and `income_lines_base(f, t)`
(migration `0070`), the FX pair `income_lines_outccy` /
`income_lines_multi` and the three reports `report_income_summary`,
`report_income_types` and `report_income_transactions` (`0071`), with
`0073` re-issuing the resolution and `0074` the types report's
multi-currency form.

One asymmetry is left standing: spending publishes a `_multi` sibling
for all three of its reports, income for two. There is no
`report_income_summary_multi`, because nothing reads either summary
`_multi` and an unread macro is a shape to keep true for nothing
(docs/INCOME.md §11).

Four things differ from the spending set, and only four:

- **The kind floor is read OVER the verdict store**, where spending's is
  read under its merchant store (`0073`). Spending asks the model who
  was paid, and a name is finer than a kind; income asks what KIND of
  income a receipt is, and the data answers that itself on every
  admitted kind but `deposit`. `categorize income` is restricted to that
  one kind for the same reason (docs/INCOME.md §3, decision 13).
- **The resolution adds a step.** `income_txn_categories()` resolves the
  payer as the INSTRUMENT where a line carries one — a dividend, a
  coupon, a staking reward arrive with an instrument and no narrative —
  before falling through to the store's name and then the signature.
  A delta line carries no payer at all.
- **One floor arm places a delta.** `distribution` floors to
  `capital_return`, so such a row leaves the base with no verdict
  written anywhere (docs/INCOME.md §1).
- **The summary carries `withheld`**, a memo with no spending twin: the
  window's `tax` rows on the same accounts, bucketed and converted like
  the lines, FULL JOINed by bucket and never subtracted. Full, so that
  neither side drops a bucket the other has: a bucket whose only
  booking was a tax row keeps its row, with `txn_count 0` and an empty
  income column (docs/INCOME.md §5).

`report_transactions` was re-issued in `0071` to carry `payer_name`,
`income_primary` and `income_detailed` after the spending trio. It is
read by a POSITIONAL scan (`gold.TransactionRow`), so that macro, the
row struct and the scan list move together in one change.

`web_income` (`0072`) is `web_spending`'s shape over the income lines:
one row per (line, reporting currency), epoch-ms timestamps, the shared
account-label macro, and `(uncategorized)` on the type columns — but
NOT on the payer, which a delta line has none of by construction.

## 11. Repository layout

```
wealthdb/
├── README.md                       — user-facing usage
├── Dockerfile                      — single-stage; Go toolchain + binary in one image (ENTRYPOINT is the binary)
├── wealthdb                        — host-side wrapper around `docker run` (production binary)
├── wealthdb-go                     — host-side wrapper around `docker run go ...` (§12.5)
├── wealthdb-test                   — thin alias: `wealthdb-go test ...` (§12.5)
├── go.mod / go.sum
├── docs/
│   ├── DESIGN.md · RETURNS-NOTES.md · SPENDING.md · TAXONOMY.md
│   └── adapters/                   — per-bank adapter design (amex, carta, chase, cointracking, schwab, swissquote, ubs)
├── cmd/
│   └── wealthdb/                   — CLI entry point + one cmd_<subcommand>.go per subcommand
├── internal/
│   ├── canonical/                  — change types + enums (asset_class, vehicle, …); zero deps
│   ├── silver/                     — adapter interface + registry, one package per source:
│   │   │                             amex angellist carta chase cointracking equityzen
│   │   │                             fidelity firstcitizens fred manual raiffeisen_at
│   │   │                             relevate schwab swissquote ubs viac
│   │   └── <source>/               — impl (snapshots/transactions/classmap) + co-located policy.go
│   ├── gold/                       — DuckDB schema, writer, queries, report macros
│   │   └── migrations/             — 0001…NNNN SQL, //go:embed-ed by schema.go
│   ├── returns/                    — source-agnostic TWR/MWR math + pluggable per-source policy
│   ├── spending/                   — the deterministic spend-enrichment pass (§10.10, docs/SPENDING.md)
│   ├── loader/                     — the §8 silver→gold load orchestration (the only silver↔gold bridge)
│   └── config/ · pathmode/ · wizard/ · output/ · errs/ · version/
└── (Dockerfile, wrappers per above)
```

**Dependency direction** is strictly one-way — anything lower may
import anything higher, never the reverse:

```
canonical                    (zero deps)
   ↑
silver (interface)           imports canonical
   ↑
silver/<source> adapters     import silver + canonical
   ↑
gold                         imports canonical — NOT silver
   ↑
loader                       imports gold + silver (the bridge)
   ↑
cmd/wealthdb                 imports loader + gold + config + wizard + output
```

**`gold` never imports `silver`** (a compile-time test in
`internal/gold` guards it): the writer takes canonical `*Change`
values, not adapter handles, and `loader` is the sole place silver
and gold meet. Migrations live under `internal/gold/migrations/`
because Go's `//go:embed` can't traverse up the tree. The
`internal/` prefix keeps every package unimportable downstream —
wealthdb is an application, not a library.

## 12. Container / build / run

Single-stage Dockerfile, single image tag (`wealthdb:latest`),
used unchanged for development, testing, and production. See §12.4
for why we don't separate stages.

### 12.1 Image

Base: `golang:1.26-bookworm` (glibc, full Go toolchain, gcc/g++
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
$XDG_CONFIG_HOME/wealthdb.cfg → $XDG_CONFIG_HOME/wealthdb.cfg   config file
$XDG_DATA_HOME/wealthdb/      → $XDG_DATA_HOME/wealthdb/        gold + silver data tree
```

The container also sets `HOME` and `XDG_CONFIG_HOME` to match the
host's so that `~` / `$HOME` references in the config file — and the
engine's default config-path resolution — land on the same absolute
paths inside and outside.

The host-side `wealthdb` shell wrapper does the mounting. Two shapes:

**Writer host** (runs `wealthdb config`, `wealthdb init`, `wealthdb load`,
`wealthdb reset`):

```sh
CFG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}"
mkdir -p "$CFG_DIR" "$XDG_DATA_HOME/wealthdb"
[ -f "$CFG_DIR/wealthdb.cfg" ] || : > "$CFG_DIR/wealthdb.cfg"
docker run --rm -it \
    -e HOME="$HOME" \
    -e XDG_CONFIG_HOME="$CFG_DIR" \
    -u "$(id -u):$(id -g)" \
    -v "$CFG_DIR/wealthdb.cfg:$CFG_DIR/wealthdb.cfg" \
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
    -e XDG_CONFIG_HOME="$CFG_DIR" \
    -u "$(id -u):$(id -g)" \
    -v "$CFG_DIR/wealthdb.cfg:$CFG_DIR/wealthdb.cfg:ro" \
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

From the repo root, `make test-wealthdb` (or `make test` for the
whole suite) is the top-level entry: it rebuilds the image and runs
`go test ./...` in one step. Under the hood it calls the
`./wealthdb-test` wrapper described here.

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

#### Wrapper shape

`./wealthdb-test` is a thin alias over `./wealthdb-go`, which owns the
container geometry and runs ANY `go` subcommand in the image:

```sh
#!/bin/sh
# wealthdb-go — run a `go` subcommand inside the wealthdb container
set -eu
REPO="$(cd "$(dirname "$0")" && pwd)"

# Persist Go's caches across invocations so incremental builds
# stay fast — bind-mount the host's cache dirs (or dedicated
# named-volumes; the host paths are simpler to inspect).
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/go"
mkdir -p "$CACHE/go-build" "$CACHE/go-mod" "$CACHE/gopath"

docker run --rm \
    -u "$(id -u):$(id -g)" \
    -e HOME="$HOME" \
    -e GOCACHE="$CACHE/go-build" \
    -e GOMODCACHE="$CACHE/go-mod" \
    -e GOPATH="$CACHE/gopath" \
    -v "$REPO:$REPO" \
    -v "$CACHE:$CACHE" \
    -w "$REPO" \
    --entrypoint go \
    wealthdb:latest \
    "$@"
```

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

`./wealthdb-go` uses the same `wealthdb:latest` image as the
production wrapper, just with `--entrypoint go`. The single image
carries the full Go toolchain so `go test` works against the
mounted source tree; no separate builder image is needed.

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
  authoritative bank-reported figure.

This is a sketch only; the source and ingest design will be
fleshed out when the feature is scheduled.

### 13.9 Account taxonomy (kind / tax_wrapper / management_style) and nickname

Several distinct kinds of accounts typically coexist at the
same bank — personal brokerage, managed wealth account, UTMA /
ESA / IRA / 529 / Säule 3a wrappers for tax purposes, separate
cash accounts. Filtering positions and net-worth roll-ups by
these dimensions is more useful than slicing by raw account ID.
Independently, a `nickname` lets the CLI render
something more recognisable than the bank's identifier.

The `accounts` table carries three orthogonal classifier columns
plus the free-text descriptors (migrations 0002 promoted
`account_category`/`nickname`; migration 0008 added the two
structured-enum columns):

- **`account_kind`** — the technical container the bank exposes:
  `brokerage`, `cash`, `safekeeping`, `custody`, `overlay`,
  `crypto`, `mortgage`, `card`, `donor_advised_fund`, `other` (with
  `crypto_exchange` / `crypto_self_custody` reserved for a future
  adapter that distinguishes them). Required; every adapter
  stamps this. A `donor_advised_fund` holds irrevocably donated
  charitable assets — its own kind so deployments can include or
  exclude DAF balances from net-worth calculations. A `card` is a
  revolving-credit liability (migration 0037): unlike a `mortgage`
  it carries no position, its outstanding balance being negative
  cash on the account.
- **`tax_wrapper`** — the tax / regulatory registration.
  Nullable; defaults to `taxable_personal` at render time.
  Values cover US (`traditional_ira`, `roth_ira`, `sep_ira`,
  `simple_ira`, `401k`, `403b`, `457b`, `529`, `coverdell_esa`,
  `hsa`, `charitable`, `custodial_utma`, `custodial_ugma`,
  `trust_grantor`, `trust_non_grantor`, `trust_charitable`) and
  Switzerland (`pillar_2`, `vested_benefits` /
  Freizügigkeitskonto, `pillar_3a`), plus generic
  `taxable_personal`, `taxable_joint`, `foundation`, `other`.
  (`charitable` pairs with the `donor_advised_fund` kind; it
  replaced the never-emitted `daf` value in migration 0036.)
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
  documented in fidelity-web's DESIGN.md §11.5.

Adapters use SQLite PRAGMA-based feature detection where the
silver schema has evolved (e.g. Swissquote's pre-v5 silvers used
`account_type` instead of `account_product`); older silvers
still load with the corresponding columns left NULL.

Config-side override (extends §5): the config accepts
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
double-counted — while a mid-life transfer is booked in full. The
ledger is source-agnostic; any source's transfers are just rows
with that `silver_source_id`.

That includes the positions-only source. `manual` collects no
transactions, but value can reach it from another tracked vehicle
with no bank in between, and the selling source books only its own
half. A row
against `manual`'s single account (blank `instrument` and
`quantity`: a bare valued movement) supplies the other half, and
that source's returns policy admits exactly the two ledger kinds so
the claim's arrival is funded rather than read as performance. The
runbook is in `collectors/manual/DESIGN.md` §6.

### 13.11 Spending and income pins ledgers

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
note`. **`income.pins` is the same ledger for the income family**, with
`income_detailed` in place of `spend_detailed` and validated against the
income vocabulary — a pin identifies a transaction the same way
whichever question is being answered about it, so only the value column
and its taxonomy differ. `account` accepts either the gold `account_external_id` or an
account nickname, resolved by the same `gold.NewAccountResolver` the
equity-transfer ledger uses. `amount` is the amount as gold stores it
— canonical sign, so a debit is negative — and matches within a cent,
the same absolute floor the transfer matcher applies, so a figure
copied from a two-decimal statement describes a four-decimal row.
`spend_detailed` may be any valid value, vendored or delta, in the
taxonomy's own spelling — the same set a config rule may place. The
difference between the two is scope, not vocabulary: a pin names one
transaction key, a rule fires on every narrative its pattern matches.
`note` is free text — why the row is pinned, for whoever reads the
ledger next — and is the one column the parser accepts and ignores:
nothing carries it into gold.

A pin applies to **every** gold transaction matching (source, account,
day, amount, currency). The key is deliberately not unique: two
identical rows on one day — the two legs of the same roll — are
indistinguishable by design, and gold's `transaction_external_id` is
opaque, adapter-specific and not something a person can read off a
statement, so the ledger never keys on it. Two ledger rows describing
the same transactions must agree (a contradiction fails the parse,
naming both lines); rows that agree collapse to one.

Mechanics (`internal/spending/pins.go`): the deterministic pass
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
