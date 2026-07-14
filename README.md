# wealthdb

A personal wealth-management data suite. It pulls holdings,
transactions, and documents from every configured bank and pension
provider, normalises them into one canonical store, and answers
"what is held, anywhere, as of when?" from a single CLI.

The suite is a **monorepo** of two parts:

- **`wealthdb/`** — the **gold** engine: a Go CLI that reads the
  per-source silver databases and projects them into a canonical
  cross-bank DuckDB schema. This is the query surface.
- **`collectors/`** — **bronze + silver** collectors, one per
  source. Each logs in, downloads raw artefacts (bronze), and
  parses them into a source-shaped silver SQLite (silver).

See **[DESIGN.md](DESIGN.md)** for the bronze → silver
→ gold model and how the pieces fit.

## Component map

| Component | Role | Runtime | Source |
| --- | --- | --- | --- |
| [`wealthdb/`](wealthdb/) | Gold engine + `wealthdb` CLI | Go (Docker) | reads all silvers |
| [`web/`](web/) | Optional Metabase BI server (`wealthdb web`) | Docker (Metabase) | reads a read-only gold snapshot |
| [`collectors/schwab-api/`](collectors/schwab-api/) | Schwab holdings/tx | Python venv | Schwab Trader API (OAuth) |
| [`collectors/schwab-web/`](collectors/schwab-web/) | Schwab statements/history | Docker (Camoufox) | client-web scrape |
| [`collectors/ubs-psn/`](collectors/ubs-psn/) | UBS structured feed | Python venv | PSN SFTP (nightly) |
| [`collectors/ubs-web/`](collectors/ubs-web/) | UBS netbanking export | Docker | netbanking scrape |
| [`collectors/swissquote/`](collectors/swissquote/) | Swissquote holdings/tx | Docker | eBanking scrape |
| [`collectors/fidelity-web/`](collectors/fidelity-web/) | Fidelity holdings/tx | Docker (Camoufox) | web scrape |
| [`collectors/relevate/`](collectors/relevate/) | Relevate / Pensexpert (Pillar 2) | Docker | middlelayer REST |
| [`collectors/viac/`](collectors/viac/) | VIAC (Pillar 3a / vested benefits) | Docker | web REST |
| [`collectors/cointracking/`](collectors/cointracking/) | Crypto aggregator (all exchanges + wallets) | Docker (headless Firefox + Camoufox for re-discovery) | web scrape |
| [`collectors/angellist/`](collectors/angellist/) | AngelList LP portal (SPVs / fund deals) | Docker (Camoufox) | web scrape |
| [`collectors/carta/`](collectors/carta/) | Carta (private holdings / cap table) | Docker (Camoufox) | web scrape |
| [`collectors/equityzen/`](collectors/equityzen/) | EquityZen (pre-IPO secondary SPVs) | Docker (Camoufox) | web scrape |
| [`collectors/svb/`](collectors/svb/) | SVB Wealth Advisory (historical sideload) | Python venv | PDF statements (one-shot) |
| [`collectors/manual/`](collectors/manual/) | Private holdings, no portal (CSV) | Python venv | manual entry |
| [`collectors/fred/`](collectors/fred/) | Historic FX rates (reference data) | Python venv | FRED API (US Fed H.10) |

Each component has its own `README.md` (usage), `DESIGN.md`
(internals), and `CLAUDE.md` (agent guidance) at its root.

## Data flow

```
collectors/<source>/         wealthdb/
  download.py  → bronze         load  ─┐
  load.py      → silver  ─────────────┼─→ gold (DuckDB)  →  wealthdb holdings positions
  (one SQLite per source)             │                     wealthdb transactions
                                      │                     wealthdb holdings accounts ...
  silver DBs live under $XDG_DATA_HOME/wealthdb/<source>/, read-only to gold
```

A collector owns its bronze (raw downloads) and silver (parsed,
source-shaped SQLite). The gold engine reads every silver and
merges them into one canonical schema. Sources only meet at gold.

## Build & run

The repo-root `Makefile` orchestrates the whole suite — run it from
the root, no `cd`-ing into subdirectories:

```sh
make            # show the target list
make all        # build everything (gold engine + web + all collectors)
make test       # test everything
make build-<name> / make test-<name>   # one component (e.g. make build-schwab-web)
make install    # symlink wealthdb + wealthdb-collect into ~/bin
make update     # bring deps forward (host venvs, Go modules, base images)
```

Each component also builds independently if you prefer, as shown below.

**Gold engine** (Go; Docker or host toolchain):

```sh
cd wealthdb
./wealthdb build            # build the wealthdb:latest image
./wealthdb config           # first-time setup wizard
./wealthdb load -a          # merge every configured silver into gold
./wealthdb holdings positions        # query
```

**Collectors** — every collector ships a wrapper exposing the same
`login` / `download` / `load` verbs, whether it's a Docker collector
(the web/REST ones) or a host-venv collector (`schwab-api`, `ubs-psn`,
`fred`, `manual`, `svb`). Drive the whole fleet through the `wealthdb-collect`
dispatcher:

```sh
make install                          # symlink wealthdb + wealthdb-collect into ~/bin (BINDIR)

wealthdb-collect list                 # the available collectors
wealthdb-collect viac login           # mint/refresh session (prompts for MFA)
wealthdb-collect viac download        # bronze dump
wealthdb-collect viac load            # bronze → silver
wealthdb-collect schwab-api download  # host-venv collectors look identical
# without installing, the per-collector wrapper works too:
collectors/viac/viac download
```

Docker collectors need their image built first (`make build-<name>`);
host-venv collectors need their `.venv` (`make build-<name>`).

The secrets / bronze / silver directories are never hard-coded —
override them per command or fleet-wide (precedence: **CLI flag >
`${PREFIX}_*` env > `WEALTHDB_*` env > default**):

| location | flag | env var(s) | default |
|---|---|---|---|
| secrets | `--secrets-dir` | `${PREFIX}_SECRETS_DIR`, `WEALTHDB_SECRETS_DIR` | `~/.secrets` |
| bronze  | `--data-dir`    | `${PREFIX}_DATA_DIR`, `WEALTHDB_DATA_ROOT/<name>` | `$XDG_DATA_HOME/wealthdb/<name>` |
| silver  | `--silver-db`   | `${PREFIX}_SILVER_DB` | `<data-dir>/<name>.db` |

`$XDG_DATA_HOME` follows the XDG Base Directory spec: when unset it
falls back to `~/.local/share`, so the out-of-the-box data root is
`~/.local/share/wealthdb`. The gold engine's `gold_db` and silver-source
paths default the same way.

```sh
wealthdb-collect viac load --data-dir /mnt/bronze/viac --silver-db /mnt/silver/viac.db
WEALTHDB_DATA_ROOT=/mnt/bronze wealthdb-collect schwab-api download
```

Every collector accepts the same single window flag, `--lookback`,
taking either a preset (`1w`, `4w`, `3m`, `6m`, `1y`, `2y`, `5y`,
`all`) or an ISO date (`2020-01-01`). It names where to start; the
window runs from there to today and covers everything the source
offers in it. Without it, downloads default to a 90-day window. Orchestration
helpers in `~/bin` (`wealthdb-nightly` for the unattended sources,
`wealthdb-refresh` for the interactive ones) drive the fleet through
`wealthdb-collect` and forward `--lookback` to every collector; see
their `--help`.

## Documentation

- **[DESIGN.md](DESIGN.md)** — suite-wide pipeline,
  layer ownership, canonical model.
- **[CLAUDE.md](CLAUDE.md)** — agent ground rules shared across
  every component (security, PII, read-only access).
- **[wealthdb/docs/DESIGN.md](wealthdb/docs/DESIGN.md)** — deep
  gold-engine design (schema, plugin contract, load semantics).
- Per-component `README.md` / `DESIGN.md` under each directory.

## License

Released under the [MIT License](LICENSE).
Copyright (c) 2026 SmartPointer AG.
