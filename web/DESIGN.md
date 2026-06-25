# web — design notes

The optional Metabase BI server. Five decisions worth recording.

## 1. Host-side, not a Go subcommand

The `wealthdb` wrapper forwards subcommands *into* the `wealthdb:latest`
container, which has no Docker socket — so it can't manage a sibling
Metabase container. `web` is therefore handled **host-side**: the
wrapper special-cases the `web` verb (like `build`) and execs
[`web/web`](web), which drives `docker` on the host. The Go engine
stays pure (CLI/query only; no daemon).

The one thing the host script needs from the engine is config it can't
easily parse itself (`wealthdb.cfg` is JSON, validated in Go): it calls
`wealthdb web-config`, a hidden read-only subcommand that prints the
resolved `web.enabled`, `web.port`, and `gold_db` as shell-evalable
`KEY=VALUE` lines. One source of truth (the Go parser), no `jq`
dependency, no bash JSON parsing.

## 2. Read-only snapshot, not the live gold file

DuckDB is single-writer across processes: one read-write handle OR
multiple read-only handles, never both. Metabase's JDBC pool holds a
read-only connection open continuously, which would block every
`wealthdb load` (read-write) for as long as the server is up.

So Metabase never touches the live file. `web start` copies the gold
DB to `…/web/snapshot/wealthdb.db` and mounts *that* read-only;
`web refresh` re-copies and restarts Metabase to pick it up. Loads are
never blocked; dashboards show data as of the last refresh (run
`web refresh` after `wealthdb load`). The copy is atomic (temp + `mv`)
and refuses to run if a `.wal` sidecar is present (gold mid-write / not
checkpointed), so we never capture a torn copy.

## 3. The driver: glibc base, version pin, readable JAR (load-bearing)

Three things must all be right or DuckDB silently won't work — each one
cost a debugging round:

- **Engine version.** The gold DB is DuckDB **1.5.x** storage format
  (the engine links `duckdb-go/v2 v2.10504.0`). The driver bundles its
  own DuckDB; an older one can't open a 1.5 file. Pin driver **1.5.3.0**
  (DuckDB 1.5.3, targets Metabase 59), sha256-verified.
- **JAR must be readable.** `ADD`-from-URL writes the JAR root-only
  (0600); Metabase then *silently skips* `/plugins` and the driver never
  registers. `ADD --chmod=0644` fixes it.
- **glibc, not musl.** The driver's JDBC native (`libduckdb_java.so`) is
  glibc-built and needs `libstdc++.so.6`. The official Metabase image is
  **Alpine/musl**, where the native *hard-crashes the JVM* (gcompat does
  not save it). So we build Metabase from its official `metabase.jar` on
  `eclipse-temurin:21-jre-noble` (Ubuntu/glibc, already ships libstdc++6)
  rather than `FROM metabase/metabase`.

The smoke test (provision + `SELECT count(*) FROM positions` *through the
driver*) is the canary that all three are right.

## 4. Loopback-only, dual-stack publish

Published on `127.0.0.1:PORT` **and** `[::1]:PORT` → container `:3000`
(two `-p` flags). Dual-stack so an `ssh -L PORT:127.0.0.1:PORT` *or* a
`[::1]` tunnel both reach it — avoiding the IPv4/IPv6 loopback mismatch
that bit the DuckDB UI. If the daemon can't bind IPv6, `web start`
falls back to IPv4-only with a warning (`WEALTHDB_WEB_BIND=v4` to
force). Never bound to a public interface; auth is Metabase's own.

## 5. H2 metadata on a bind-mounted volume

Metabase's own metadata (dashboards, users, the saved DuckDB data
source) lives in embedded **H2** at `…/web/metabase/`, bind-mounted
into the container. One container, no second Postgres — fits "no heavy
tooling on metal." Fine for a single local instance; `web stop`
preserves it so a restart keeps everything.

## 6. Provisioning (skip the setup wizard)

A private loopback service shouldn't make you fill in a "tell us about
your company" wizard. Metabase's declarative config file
(`MB_CONFIG_FILE_PATH`) can create users/databases — but that's a
Pro/EE feature, a silent no-op on OSS. So `web start` provisions over the
OSS **setup API** ([`provision.py`](provision.py), stdlib only): create
the admin (`POST /api/setup`, which also finishes the wizard), pre-add
the gold DuckDB database (`POST /api/database`, `read_only`), and create
the pre-defined report models (`POST /api/card`, `type: "model"`) as native-query
shims over the gold **multi-currency** report macros — `SELECT * FROM
report_x_multi(…)` (migration 0024), which build on the same line bases the
CLI's single-currency macros use and emit one value-column set per currency
(USD/CHF/EUR), so the models track command output by construction and bake in
no data. Two families, in a `wealthdb (pre-defined)` collection: the `_latest`
snapshot reports (+ all-time `report_transactions`), and the daily `_history`
reports (migration 0022) for time-series charts. Idempotent — re-running
updates models in place and archives retired names. The admin password comes from
`WEALTHDB_WEB_ADMIN_PASSWORD` (e.g. `~/.secrets/wealthdb-web.env`) or is
generated once and saved chmod 600; `WEALTHDB_WEB_NO_PROVISION=1` opts
back into the browser wizard.

The container runs as root (temurin default), so it reads the mode-600
gold snapshot without us having to loosen the snapshot's perms.

## Testing

[`test_web.sh`](test_web.sh) (`make test-web`) unit-tests the pure
helpers with no Docker: the dual-stack `-p` flag construction, the
read-only snapshot mount in the `docker run` args, the `.wal` guard, and
the generated-password complexity. The Go side (`web` config block,
validation, the `web-config` emitter) is covered by `go test ./...`
(`make test-wealthdb`). End-to-end (build → start → provision → query
through the driver) is the manual smoke test in §3.
