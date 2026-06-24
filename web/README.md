# web — Metabase BI server (optional)

A local, dockerized [Metabase](https://www.metabase.com/) for running
and visualizing analytics over the **gold** DuckDB database. Optional,
fully dockerized (no host Java/Metabase), read-only against your data,
and reachable only over an SSH tunnel + Metabase's own login.

Part of the **wealthdb** suite — see [the architecture
overview](../ARCHITECTURE.md). Run it via the main wrapper:

```sh
wealthdb web start | stop | status | restart | refresh | logs
```

## Why a snapshot

DuckDB is single-writer across processes: a long-lived Metabase
connection would hold a lock that blocks `wealthdb load`. So Metabase
never reads the live gold file — `wealthdb web start` serves it a
read-only **snapshot copy**, and `wealthdb web refresh` re-copies it.
Loads are never blocked; the dashboards show data as of the last
refresh. See [DESIGN.md](DESIGN.md).

## Enable it

Add a `web` block to `wealthdb.cfg` (the only config it needs):

```json
"web": { "enabled": true, "port": 3000 }
```

`port` defaults to 3000. Operational knobs are env overrides (not in
the config file): `WEALTHDB_WEB_IMAGE`, `WEALTHDB_WEB_CONTAINER`,
`WEALTHDB_WEB_DATA_DIR`, `WEALTHDB_WEB_BIND` (`both`|`v4`|`v6`).

## Quick start

```sh
# 1. Build the image (Metabase-from-JAR on glibc + pinned DuckDB driver).
wealthdb web build         # or: make build-web

# 2. Enable it in wealthdb.cfg (see above), then start it. This snapshots
#    gold, runs Metabase, and AUTO-PROVISIONS it: creates the admin and
#    pre-adds the gold database — no "tell us about your company" wizard.
wealthdb web start
#    -> prints the admin login. A generated password is saved to
#       $XDG_DATA_HOME/wealthdb/web/admin-password.txt (chmod 600);
#       or set WEALTHDB_WEB_ADMIN_PASSWORD yourself.

# 3. From your laptop, forward the port and open it (IPv4 loopback):
ssh -L 3000:127.0.0.1:3000 <this-host>
open http://127.0.0.1:3000/        # log in as admin@wealthdb.local

# 4. The "gold" DuckDB database is already there — just start querying.
#    After each data load, refresh the snapshot:
wealthdb load -a && wealthdb web refresh
```

## Security

- Published on loopback only (`127.0.0.1:PORT` and `[::1]:PORT` →
  container `:3000`) — never on a public interface. Reach it via an
  SSH port-forward.
- Authentication is Metabase's own login. `web start` auto-creates the
  admin on first run, so the unauthenticated first-run wizard never sits
  exposed. Set `WEALTHDB_WEB_ADMIN_PASSWORD` (e.g. in
  `~/.secrets/wealthdb-web.env`) or use the generated one saved to
  `admin-password.txt` (chmod 600); change it in the UI anytime.
- The gold snapshot is mounted **read-only**; Metabase cannot write to
  your data.

## Layout

```
web/
├── web            # host lifecycle script (start/stop/status/refresh/logs/build)
├── provision.py   # idempotent Metabase setup: admin + pre-add gold DB (stdlib)
├── Dockerfile     # Metabase (from JAR, glibc base) + pinned DuckDB driver → /plugins
├── test_web.sh    # unit tests for the lifecycle script (make test-web)
├── README.md      # this file
├── DESIGN.md      # rationale: snapshot, glibc base, version pin, provisioning
├── CLAUDE.md      # ground rules for agents
├── .gitignore
└── .dockerignore
```

On the host:

```
$XDG_DATA_HOME/wealthdb/web/
├── metabase/                  # Metabase's H2 metadata DB (dashboards, users)
├── snapshot/wealthdb.db       # read-only gold snapshot Metabase serves
└── admin-password.txt         # generated admin password (chmod 600), unless set via env
```

## Privacy

The repo is publishable. The Metabase **H2 metadata DB** can hold the
SQL of saved questions (no raw source data), and the **snapshot** is a
copy of gold — both live under `$XDG_DATA_HOME`, outside the repo.
Never commit either, and don't bake real queries/dashboards into the
image. See [CLAUDE.md](CLAUDE.md).
