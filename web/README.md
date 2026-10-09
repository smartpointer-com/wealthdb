# web — Metabase BI server (optional)

A local, dockerized [Metabase](https://www.metabase.com/) for running
and visualizing analytics over the **gold** DuckDB database. Optional,
fully dockerized (no host Java/Metabase), read-only against your data,
and reachable only over an SSH tunnel + Metabase's own login.

It comes with eight dashboards: Wealth Overview, Allocation, Returns,
Gains, Spending, Income, Cash Flow and Data Freshness. Each but Gains
has a privacy twin that shows shares (%) instead of amounts.

Part of the **wealthdb** suite — see [the architecture
overview](../DESIGN.md). Run it via the main wrapper:

```sh
wealthdb web start | stop | status | restart | refresh | logs
```

## Why a snapshot

DuckDB is single-writer across processes: a long-lived Metabase
connection would hold a lock that makes every `wealthdb load` fail at
open — a load does not queue behind the server. (Between two engine
commands the exclusion is the engine's own write mutex, a
`<gold_db>.wealthdb.lock` sidecar beside the database — an expected
file, safe for a copy to skip.) So Metabase never reads
the live gold file — `wealthdb web start` serves it a read-only
**snapshot copy**, and `wealthdb web refresh` re-copies it. Loads never
contend with the server; the dashboards show data as of the last
refresh. See [DESIGN.md](DESIGN.md).

## Enable it

Add a `web` block to `wealthdb.cfg` (the only config it needs):

```json
"web": { "enabled": true, "port": 3000 }
```

`port` defaults to 3000. The dashboards open in the config's
`default_currency` when it is USD, CHF, EUR or GBP, and in USD
otherwise; each dashboard's Currency picker switches between the four.

Operational knobs are env overrides (not in
the config file): `WEALTHDB_WEB_IMAGE`, `WEALTHDB_WEB_CONTAINER`,
`WEALTHDB_WEB_DATA_DIR`, `WEALTHDB_WEB_BIND` (`both`|`v4`|`v6`).

## Quick start

```sh
# 1. Build the image (Metabase-from-JAR on glibc + pinned DuckDB driver).
wealthdb web build         # or: make build-web

# 2. Enable it in wealthdb.cfg (see above), then start it. This snapshots
#    gold, runs Metabase, and AUTO-PROVISIONS it: creates the admin,
#    pre-adds the gold database, and creates the report models mirroring
#    the CLI plus the dashboards — no "tell us about your company" wizard.
wealthdb web start
#    -> prints the admin login. A generated password is saved to
#       $XDG_DATA_HOME/wealthdb/web/admin-password.txt (chmod 600);
#       or set WEALTHDB_WEB_ADMIN_PASSWORD yourself.

# 3. From your laptop, forward the port and open it (IPv4 loopback):
ssh -L 3000:127.0.0.1:3000 <this-host>
open http://127.0.0.1:3000/        # log in as admin@wealthdb.local

# 4. The dashboards are in the "wealthdb (pre-defined)" collection, and
#    the "gold" DuckDB database is there to query. After each data load,
#    refresh the snapshot:
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
├── provision.py   # idempotent Metabase setup: admin, gold DB, models, cards, dashboards (stdlib)
├── Dockerfile     # Metabase (from JAR, glibc base) + pinned DuckDB driver → /plugins
├── test_web.sh    # unit tests: lifecycle script, then provision.py (make test-web)
├── test_provision.py  # unit tests for provision.py's definitions (no Metabase)
├── README.md      # this file
├── DESIGN.md      # rationale: snapshot, glibc base, version pin, provisioning
├── AGENTS.md      # ground rules for agents
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
image. See [AGENTS.md](AGENTS.md).
