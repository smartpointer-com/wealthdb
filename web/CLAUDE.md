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

## 2. Loopback only

Publish on `127.0.0.1` and `[::1]` only. Never bind a public interface
(`0.0.0.0`, a LAN IP, a host name). The server is reached over an SSH
port-forward; auth is Metabase's own login.

## 3. No baked-in content; no secrets

- Do **not** bake canned dashboards, saved questions, or source data
  into the image or the repo. The user builds dashboards in the UI;
  keeping the image content-free also keeps source data out of git.
- The **one allowed exception** is the report models that `provision.py`
  creates at runtime over the API, in a dedicated `wealthdb (pre-defined)`
  collection: `report_{global,portfolios,accounts,positions}_latest`,
  `report_transactions`, and the daily-history `report_{global,portfolios,
  accounts,positions}_history`. They are content-free shims — each is just
  `SELECT * FROM report_x(…)` over the gold report macros (the same macros
  the CLI commands use), lightly wrapped to fix column types for BI use
  (the macros' VARCHAR money columns cast back to DECIMAL, epoch columns
  to TIMESTAMP), so they bake in no data and track the CLI output by
  construction. Provisioning is idempotent (updates in place, archives
  retired names). The gold DuckDB connection is likewise added at runtime
  by `provision.py`, never baked into the image.
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

## 5. Provisioning is API-based and idempotent

`web/provision.py` skips the setup wizard by creating the admin, adding
the gold DB, and creating the pre-defined report models over the OSS setup API.
Keep it idempotent (safe on every start — it skips the admin, the DB,
and any model that already exists by name). Do **not** switch to
Metabase's config-file provisioning — it's Pro/EE-only and a silent
no-op on OSS. Never hard-code a password; take it from env or generate
+ save chmod 600.
