# Notes for Claude / coding agents — web component

Shared, repo-wide ground rules (no PII in source, git/commit
conventions) live in the repo-root [CLAUDE.md](../CLAUDE.md). The
web-specific surface below applies on top of those.

## 1. Read-only against gold — always a snapshot

The Metabase server must only ever read a **read-only snapshot** of the
gold DB, never the live file. The snapshot is mounted `:ro`. Never:

- mount the live `gold_db` into the container (it would contend for
  DuckDB's single-writer lock and block `wealthdb load`);
- open gold read-write from here, or add any write path to gold;
- weaken the `.wal` guard in `_snapshot` (it prevents copying a
  torn / mid-write database).

The one narrow carve-out: before snapshotting, `web refresh` (and the
initial snapshot in `web start`) invokes the **engine's** hidden
`web-materialize` subcommand, which rewrites the `report_returns` table
in live gold (DESIGN.md §8). That is an engine-owned write — the same
class as `wealthdb load`, serialized with it by DuckDB's single-writer
lock — not a web write path: the container and `provision.py` still
only ever see the `:ro` snapshot. Keep it that way: materialization
happens in the engine, before `_snapshot`, never from the container.

## 2. Loopback only

Publish on `127.0.0.1` and `[::1]` only. Never bind a public interface
(`0.0.0.0`, a LAN IP, a host name). The server is reached over an SSH
port-forward; auth is Metabase's own login.

## 3. No baked-in content; no secrets

- Do **not** bake canned content or source data into the image or the
  repo. Keeping the image content-free also keeps source data out of git.
- The **one allowed exception** is what `provision.py` creates at
  runtime over the API, in a dedicated `wealthdb (pre-defined)`
  collection — all of it content-free *definitions* (MBQL / SQL only,
  no source data baked in):
  - the report models `report_{global,sources,portfolios,accounts,
    positions}_latest`, `report_transactions`, and the daily-history
    `report_{global,sources,portfolios,accounts,positions}_history`.
    Each is just `SELECT * FROM report_x_multi(…)` over the gold
    multi-currency report macros (migration 0024; built on the same line
    bases the CLI's single-currency `report_x(…)` macros use, so each
    `_<ccy>` column equals the CLI's output for that currency by
    construction). Those macros emit one value-column set per currency
    (USD/CHF/EUR) as DECIMAL, so the wrapper only casts epoch columns to
    TIMESTAMP.
  - the returns models over the **materialized** `report_returns`
    table (migration 0026, rewritten by the engine's `web-materialize`
    on every refresh — see §1's carve-out and DESIGN.md §8):
    `report_returns` is the same kind of cast-only shim;
    `report_returns_redacted` additionally drops the absolute money
    columns and keeps only the sources and global grains (the privacy
    twin's basis, below). The definitions are content-free; the
    table's data lives in gold and reaches Metabase only via the
    snapshot.
  - pre-defined metrics and questions over those models (net worth
    current and over time, income and fees by month, allocation
    breakdowns, TWR/MWR returns, source freshness), and four dashboards
    composing them: **Wealth Overview** and **Allocation** (global
    filters: a time range resp. a required as-of day, plus a source
    picker), **Returns** (a required currency picker plus a start-year
    picker that rescopes the summary figures to the since-<year> window
    — `window_from_year` — so the frequently-null since-inception TWR
    becomes a real number; its per-period and cumulative-growth charts
    are native SQL, split by source with the global grain unioned in as
    a toggleable `(all sources)` line) and **Data Freshness**
    (deliberately unfiltered, so stale sources stay visible).
  - a **privacy twin** of each dashboard (same layout and filters,
    switch links between the two views), whose cards show shares (%)
    instead of money. The twins' charts are native SQL over the gold
    `web_*` serving views (migration 0032), with the dashboard pickers
    landing on them as field filters; each card computes its
    normalization denominator in-query with those filters applied —
    holdings as % of the selected sources' total at the window's end
    (a subset still totals 100), income/fee flows as % of their own
    peak month within the selected window (the tallest bar reads
    100). The scalars are MBQL ratios of sums over `_pct` models that
    pre-scale every monetary column to % of the latest global net
    worth and drop columns that would leak absolute values
    (base-currency totals, quantities, amounts, prices), so a
    drill-through stays leak-free. Still definitions only: the scale
    factors are computed by the queries at run time, never stored.
    The Returns twin redacts instead of normalizing (returns are
    already scale-free ratios): its cards run over
    `report_returns_redacted`, which drops the absolute money
    columns and keeps only the sources and global grains.
  Provisioning is idempotent (updates in place, archives retired names)
  and **converges the pre-defined collection to spec on every start** —
  dashboards get their tile layout replaced wholesale. User content
  elsewhere is never touched (card/dashboard matching is scoped to the
  pre-defined collection); a user who wants to customize a pre-defined
  card or dashboard must duplicate it into another collection first.
  The gold DuckDB connection is likewise added at runtime by
  `provision.py`, never baked into the image.
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
