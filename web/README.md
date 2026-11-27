# web — Metabase BI server (optional)

A local [Metabase](https://www.metabase.com/) for running and
visualizing analytics over the **gold** DuckDB database. It is optional.
It runs in a container (no host Java or Metabase), or, where the engine
is a program on the computer, as a Java program beside it
([NATIVE.md](../wealthdb/docs/NATIVE.md)). It only reads the data. It
listens on this computer only: from another one it is reached over an
SSH tunnel, and always through Metabase's own login.

It comes with eleven dashboards: Wealth Overview, Allocation, Returns,
Gains, Spending, Income, Cash Flow, Exposure, Yield, Data Freshness and
Private Markets.
Each but Gains has a privacy twin. It shows shares (%) instead of
amounts, or hides the money where the figures are ratios already
(Returns, the Private Markets table). Allocation and Private Markets
read their figures as of a picked day.

Part of the **wealthdb** suite — see [the architecture
overview](../DESIGN.md). Run it via the main wrapper:

```sh
wealthdb web start | stop | status | restart | refresh | logs | serve
```

## Why a snapshot

DuckDB is single-writer across processes. A long-lived Metabase
connection would hold a lock that makes every `wealthdb load` fail at
open; a load does not queue behind the server. So Metabase never reads
the live gold file. `wealthdb web start` serves it a read-only
**snapshot copy**, and `wealthdb web refresh` copies it again when
gold changed. Loads never contend with the server, and the dashboards
show data as of the last refresh. See [DESIGN.md](DESIGN.md) §2.

## Enable it

Turn the dashboards on in the setup:

```sh
wealthdb config dashboards
```

It writes the `web` block of `wealthdb.cfg`, the only config the
dashboards need: `"web": { "enabled": true, "port": 3000 }`. `port`
defaults to 3000. The dashboards open in the config's
`default_currency` when it is USD, CHF, EUR or GBP, and in USD
otherwise; each dashboard's Currency picker switches between the four.

`wealthdb web start` and `wealthdb web refresh` say when gold holds
amounts in a currency of money with no exchange rate to the config's
currency. The dashboards then show nothing of those amounts. The line
names the currencies and the command that adds a source of exchange
rates
([DESIGN.md §10.6](../wealthdb/docs/DESIGN.md#106-currency-conversion-with-historic-fx-rates)).

Operational knobs are environment variables, not config: the image,
the container name, the data dir, the address family to bind, and the
admin login. `wealthdb web help` lists them.

A config that is not the default file is a setup of its own
([DESIGN.md §4.2](../wealthdb/docs/DESIGN.md#a-setup-of-its-own)):

- Its server is named `wealthdb-metabase-<name>`, `<name>` from the
  config's folder.
- Its files, the generated admin password among them, are in `web/` in
  that folder.
- Its env file is `wealthdb-web-<name>.env` in the secrets folder.
- `WEALTHDB_WEB_CONTAINER`, `WEALTHDB_WEB_DATA_DIR` and
  `WEALTHDB_WEB_ENV_FILE` each still win.

A start never takes over a server of that name that belongs to another
setup. It refuses, says so, and removes nothing.

## Quick start

```sh
# 1. Build the image (Metabase-from-JAR on glibc + pinned DuckDB driver),
#    or download Metabase for a native server.
wealthdb web build         # or: make build-web

# 2. Turn the dashboards on (see above), then start them. This snapshots
#    gold, runs Metabase, and AUTO-PROVISIONS it: creates the admin,
#    pre-adds the gold database, and creates the report models mirroring
#    the CLI plus the dashboards — no "tell us about your company" wizard.
wealthdb web start
#    -> prints the admin login. The password comes from
#       WEALTHDB_WEB_ADMIN_PASSWORD, or is generated once and saved to
#       $XDG_DATA_HOME/wealthdb/web/admin-password.txt (chmod 600).

# 3. From a laptop, forward the port and open it (IPv4 loopback):
ssh -L 3000:127.0.0.1:3000 <this-host>
open http://127.0.0.1:3000/        # log in as admin@wealthdb.local

# 4. The dashboards are in the "wealthdb (pre-defined)" collection, and
#    the "gold" DuckDB database is there to query. After each data load,
#    refresh the snapshot. Refresh ends with the same login line:
wealthdb load -a && wealthdb web refresh
```

## Security

- It listens on loopback only (`127.0.0.1:PORT`; a container also on
  `[::1]:PORT`) — never on a public interface. Reach it via an SSH
  port-forward.
- Authentication is Metabase's own login. `web start` auto-creates the
  admin on first run, so the unauthenticated first-run wizard never sits
  exposed. Set `WEALTHDB_WEB_ADMIN_PASSWORD` (e.g. in
  `~/.secrets/wealthdb-web.env`) or use the generated one saved to
  `admin-password.txt` (chmod 600); change it in the UI anytime.
- Metabase opens the snapshot **read-only**. In a container the
  snapshot is also mounted read-only.

## Layout

```
web/
├── web            # host lifecycle script (start/stop/status/refresh/logs/build)
├── provision.py   # idempotent Metabase setup: admin, gold DB, models, cards, dashboards (stdlib)
├── metabase.py    # the Metabase API client provisioning and the checks share
├── sqlkit.py      # the vocabulary of the card SQL: currencies, tags, viz, the card maker
├── models.py      # the report models
├── pickers.py     # every dashboard picker
├── layout.py      # a dashboard's declaration and its grid
├── colors.py      # each source's colour
├── retired.py     # the retired model, card and dashboard names
├── dashboards/    # one file per dashboard's cards; __init__.py declares the dashboards
├── tile_timings.py  # times each tile's query on the DuckDB the driver bundles
├── Dockerfile     # Metabase (from JAR, glibc base) + pinned DuckDB driver → /plugins
├── test_web.sh    # unit tests: lifecycle script, then provision.py (make test-web)
├── test_provision.py  # unit tests: provisioning definitions, client, tile timings (no Metabase)
├── requirements-test.txt  # their duckdb module, in the web/.venv make test-web builds
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
├── snapshot/wealthdb-<n>.db   # the same file, under the name Metabase reads it by
├── spill/                     # DuckDB spill space for capped queries
├── source-colors.json         # each source's chart colour, kept across refreshes
├── admin-password.txt         # generated admin password (chmod 600), unless set via env
├── plugins/                   # the folder Metabase loads its drivers from (native server)
├── web.pid                    # the server's process (native server)
└── web.log                    # its log, where launchd does not keep it (native server)
```

## Privacy

The repo is publishable. The Metabase **H2 metadata DB** can hold the
SQL of saved questions (no raw source data), and the **snapshot** is a
copy of gold — both live under `$XDG_DATA_HOME`, outside the repo.
Never commit either, and do not bake real queries/dashboards into the
image. See [AGENTS.md](AGENTS.md).
