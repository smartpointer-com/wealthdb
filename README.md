# wealthdb

A personal wealth-management data suite. It pulls holdings,
transactions, and documents from every configured bank and pension
provider, normalises them into one canonical store, and answers
"what is held, anywhere, as of when?" from a single CLI.

The suite is a **monorepo** of two parts:

- **`wealthdb/`** — the **gold** engine: a Go CLI that reads the
  per-source silver databases and projects them into a canonical
  cross-bank DuckDB schema. This is the query surface.
- **`collectors/`** — eight **bronze + silver** collectors, one
  per source. Each logs in, downloads raw artefacts (bronze), and
  parses them into a source-shaped silver SQLite (silver).

See **[ARCHITECTURE.md](ARCHITECTURE.md)** for the bronze → silver
→ gold model and how the pieces fit.

## Component map

| Component | Role | Runtime | Source |
| --- | --- | --- | --- |
| [`wealthdb/`](wealthdb/) | Gold engine + `wealthdb` CLI | Go (Docker) | reads all silvers |
| [`collectors/schwab-api/`](collectors/schwab-api/) | Schwab holdings/tx | Python venv | Schwab Trader API (OAuth) |
| [`collectors/schwab-web/`](collectors/schwab-web/) | Schwab statements/history | Docker (Camoufox) | client-web scrape |
| [`collectors/ubs-psn/`](collectors/ubs-psn/) | UBS structured feed | Python venv | PSN SFTP (nightly) |
| [`collectors/ubs-web/`](collectors/ubs-web/) | UBS netbanking export | Docker | netbanking scrape |
| [`collectors/swissquote/`](collectors/swissquote/) | Swissquote holdings/tx | Docker | eBanking scrape |
| [`collectors/fidelity-web/`](collectors/fidelity-web/) | Fidelity holdings/tx | Docker (Camoufox) | web scrape |
| [`collectors/relevate/`](collectors/relevate/) | Relevate / Pensexpert (Pillar 2) | Docker | middlelayer REST |
| [`collectors/viac/`](collectors/viac/) | VIAC (Pillar 3a / vested benefits) | Docker | web REST |

Each component has its own `README.md` (usage), `DESIGN.md`
(internals), and `CLAUDE.md` (agent guidance) at its root.

## Data flow

```
collectors/<source>/         wealthdb/
  download.py  → bronze         load  ─┐
  load.py      → silver  ─────────────┼─→ gold (DuckDB)  →  wealthdb positions
  (one SQLite per source)             │                     wealthdb transactions
                                      │                     wealthdb accounts ...
  silver DBs live under ~/wealthdb/<source>/, read-only to gold
```

A collector owns its bronze (raw downloads) and silver (parsed,
source-shaped SQLite). The gold engine reads every silver and
merges them into one canonical schema. Sources only meet at gold.

## Build & run

Each component builds independently — there is no top-level build.

**Gold engine** (Go; Docker or host toolchain):

```sh
cd wealthdb
./wealthdb build            # build the wealthdb:latest image
./wealthdb config           # first-time setup wizard
./wealthdb load -a          # merge every configured silver into gold
./wealthdb positions        # query
```

**Collectors** — two shapes:

```sh
# Host-venv collectors (schwab-api, ubs-psn): pure-stdlib + a thin dep
cd collectors/schwab-api
.venv/bin/python download.py --token-path ~/.secrets/schwab-api-token.json --dest ~/wealthdb/schwab-api
.venv/bin/python load.py --silver-db ~/wealthdb/schwab-api/schwab-api.db --bronze-dir ~/wealthdb/schwab-api

# Docker collectors (the six web/REST ones): a host wrapper drives docker run
cd collectors/viac
./viac-dump build                 # build the image (wrapper name still carries -dump for now)
./viac-dump login                 # mint/refresh session (prompts for MFA)
./viac-dump download              # bronze dump
./viac-dump load                  # bronze → silver
```

Orchestration helpers in `~/bin` (`wealthdb-nightly` for the
unattended sources, `wealthdb-refresh` for the interactive ones)
run the whole fleet in sequence; see their `--help`.

## Documentation

- **[ARCHITECTURE.md](ARCHITECTURE.md)** — suite-wide pipeline,
  layer ownership, canonical model.
- **[CLAUDE.md](CLAUDE.md)** — agent ground rules shared across
  every component (security, PII, read-only access).
- **[wealthdb/docs/DESIGN.md](wealthdb/docs/DESIGN.md)** — deep
  gold-engine design (schema, plugin contract, load semantics).
- Per-component `README.md` / `DESIGN.md` under each directory.
