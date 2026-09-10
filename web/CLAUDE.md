# Notes for Claude / coding agents — web component

Shared, repo-wide ground rules (no PII in source, git/commit
conventions) live in the repo-root [CLAUDE.md](../CLAUDE.md). The
web-specific surface below applies on top of those.

## 1. Read-only against gold — always a snapshot

The Metabase server must only ever read a **read-only snapshot** of the
gold DB, never the live file. The snapshot is mounted `:ro`. Never:

- mount the live `gold_db` into the container (it would contend for
  DuckDB's single-writer lock, and a lock conflict makes `wealthdb
  load` fail at open rather than wait);
- open gold read-write from here, or add any write path to gold;
- weaken the `.wal` guard in `_snapshot` (it prevents copying a
  torn / mid-write database).

The one narrow carve-out: before snapshotting, `web refresh` (and the
initial snapshot in `web start`) invokes the **engine's** hidden
`web-materialize` subcommand, which rewrites the `report_returns` table
in live gold (DESIGN.md §8). That is an engine-owned write — the same
class as `wealthdb load` and mutually exclusive with it under the
engine's write mutex (an advisory flock on a `<gold_db>.wealthdb.lock`
sidecar, with DuckDB's own single-writer file lock behind it), a
concurrent load making it fail rather than wait so refresh aborts
*before* the snapshot is touched — not a web write path: the container
and `provision.py` still only ever see the `:ro` snapshot.
Keep it that way: materialization happens in the engine, before
`_snapshot`, never from the container.

## 2. Loopback only

Publish on `127.0.0.1` and `[::1]` only. Never bind a public interface
(`0.0.0.0`, a LAN IP, a host name). The server is reached over an SSH
port-forward; auth is Metabase's own login.

## 3. No baked-in content; no secrets

- Do **not** bake canned content or source data into the image or the
  repo. Keeping the image content-free also keeps source data out of git.
- The **one allowed exception** is what `provision.py` creates at
  runtime over the API, in a dedicated `wealthdb (pre-defined)`
  collection. All of it is content-free *definitions* — MBQL and SQL
  only, never source data — and what it builds is described in
  [DESIGN.md §6](DESIGN.md): the models, the dashboards, their privacy
  twins and how each twin normalises. Read that before changing a card;
  it is not repeated here.
- Provisioning **converges the pre-defined collection to spec on every
  start**: dashboards get their tile layout replaced wholesale. User
  content elsewhere is never touched — card and dashboard matching is
  scoped to the pre-defined collection — so anyone wanting to customise
  a pre-defined card must duplicate it into another collection first.
- Adding a serving view? Register it in `FILTER_FIELD_COLUMNS`. That is
  the single registry both the field-id resolution and the startup probe
  read, and the probe aborts the run rather than converging every card
  to a degraded shape when a view the cards need is missing.
- The gold DuckDB connection is added at runtime by `provision.py`,
  never baked into the image.
- This component has **no credentials**. Don't add a `~/.secrets/*`
  mount or any secret env. Metabase manages its own admin account in
  its H2 metadata DB (under `$XDG_DATA_HOME`, outside the repo).

## 4. The driver is fragile — keep all three invariants

DuckDB silently won't work unless ALL of these hold (each one is a real
trap, see DESIGN.md §3):

- (a) the driver's bundled DuckDB engine is **>= the gold storage
  format** (1.5.x);
- (b) the JAR is **world-readable** in `/plugins` (`ADD --chmod=0644` —
  root-only is silently skipped, no error);
- (c) the base image is **glibc**, not Alpine/musl (the JDBC native is
  glibc and hard-crashes the JVM on musl). We build Metabase from
  `metabase.jar` on `eclipse-temurin` for (c) — do **not** "simplify"
  back to `FROM metabase/metabase` (that image is Alpine).

When bumping pins, re-run the smoke test (provision + `SELECT count(*)
FROM positions` through the driver) and update the sha256.

A fourth invariant, same weight (DESIGN.md §7): **keep the memory
discipline**. DuckDB runs in-process in the Metabase JVM — do not remove
the container/JVM caps in `web/web`, the `memory_limit`/`threads`
connection details in `provision.py`, or the writable spill mount at
`<database_file>.tmp`; and never move DuckDB settings into `init_sql`
(it runs per pooled connection, and re-`SET`ting a used
`temp_directory` breaks every later query).

## 5. Provisioning is API-based and idempotent

`web/provision.py` skips the setup wizard by creating the admin, adding
the gold DB, and creating the pre-defined report models, metrics,
questions and dashboards over the OSS API. Keep it idempotent (safe on
every start — it skips the admin and the DB, and updates any card or
dashboard that already exists by name in place). Do **not** switch to
Metabase's config-file provisioning — it's Pro/EE-only and a silent
no-op on OSS. Never hard-code a password; take it from env or generate
+ save chmod 600.
