# wealthdb

A personal-portfolio gold-layer CLI. Reads the per-source silver
SQLite databases produced by the sibling
[collectors](../collectors/) — one per bank, pension, and crypto
source — and projects them into a canonical cross-bank DuckDB
schema queryable through the `wealthdb` CLI.

CLI only; the optional Metabase BI server lives in
[../web/](../web/). Single Docker image; no host-side Go toolchain
required.

## Status

All planned v1 functionality is in. The CLI ships with:

| Subcommand | Purpose |
| --- | --- |
| `wealthdb config` | Interactive first-time setup wizard. |
| `wealthdb init` | Create the gold DuckDB at the configured path. |
| `wealthdb load <id>\|-a` | Merge new silver snapshots into gold. |
| `wealthdb reset <id>\|-a` | Purge a silver source's data from gold. |
| `wealthdb reload <id>\|-a` | Reset then load (use after upgrading wealthdb). |
| `wealthdb holdings <view>` | Point-in-time portfolio views: `positions`, `accounts`, `portfolios`, `sources`, `global` — each with currency conversion and `-d`/`-f`/`-x`/`-p` (and `-C` columns on all but `global`). |
| `wealthdb returns <view>` | Time-weighted (TWR) & money-weighted (MWR/XIRR) returns by `accounts`, `portfolios`, `sources`, `global` over a window. `--method`, `--period {monthly\|quarterly\|annual\|total}`, `--annualize`, `--netting`, `--inception`; historic FX, after fees & taxes. Account-grain is exact; coarse grains are best-effort — read the `quality` column. |
| `wealthdb transactions` | Print transactions over a date range, oldest first (`-r` reverses to newest first). |
| `wealthdb status [<id>] [-v]` | Report gold state vs each silver source. |
| `wealthdb snapshots <id>\|-a` | List snapshots gold has loaded for a silver. |
| `wealthdb resolve-symbols` | Back-fill missing instrument tickers via a local LLM (configured under `symbol_resolution.model`); applies any `symbol_resolution.overrides` first. `--overrides-only` skips the LLM round-trip. |
| `wealthdb resolutions` | Dump the `symbol_resolutions` lookup table for inspection. |
| `wealthdb help [<subcommand>]` | Help. |

Adapters ship for every source: Schwab (API + web), UBS (PSN feed
+ web), Swissquote, Fidelity (web; SVB historical statements load
through it), VIAC and Relevate (Swiss pension), CoinTracking
(crypto), Carta, AngelList and EquityZen (private markets), the
manual CSV source, and FRED (reference FX).

Accounts carry a three-dimensional taxonomy: `account_kind`
(technical container — brokerage / cash / safekeeping / custody /
overlay / crypto / mortgage / other, with crypto_exchange /
crypto_self_custody reserved),
`tax_wrapper` (taxable_personal / IRA / Roth / 529 / coverdell_esa /
custodial_utma / custodial_ugma / pillar_3a / vested_benefits /
trust / DAF / HSA / …; covers US + Switzerland),
and `management_style` (self_directed / advisory / discretionary /
automated). Adapters populate what silver carries; config-side
`account_overrides` fills the rest.

Future work lives in [docs/DESIGN.md §13](docs/DESIGN.md).
Notable items: market-data feeds (§13.8), instrument-name
enrichment for Schwab equity, broader crypto-source coverage.

## Quickstart

You need Docker. No host-side Go toolchain.

```sh
git clone <this repo>
cd wealthdb
./wealthdb build                       # one-time, ~2 min on first run
./wealthdb config                      # interactive setup wizard
./wealthdb init                        # create the gold DB
./wealthdb load -a                     # merge every configured silver
./wealthdb holdings positions          # print consolidated positions (default table format, USD)
./wealthdb holdings positions -x CHF   # render values in CHF
./wealthdb holdings positions -f csv   # CSV output for scripting
./wealthdb returns accounts 2025       # per-account TWR, quarterly, for 2025
./wealthdb returns global --method both # whole-portfolio TWR + MWR since inception
./wealthdb status -v                   # quick health check across all silvers
```

The `config` wizard walks you through:

1. Gold DB path (default `$XDG_DATA_HOME/wealthdb/wealthdb.db`).
2. Default output currency (default `USD`; can be overridden per
   query with `-x`).
3. One or more silver sources — for each: a short id (used by
   `load`/`reset`/`snapshots`), the source kind (`schwab`, `ubs`,
   `swissquote`, `viac`, `cointracking`, …), and the path to the
   silver SQLite.

It writes the result to `${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg`
(overridable with `-c <path>`).

## Build and run

From the repo root, the `Makefile` drives builds and tests
(`make build-wealthdb`, `make test-wealthdb`); the commands below
are the underlying per-component wrappers.

All commands run inside a single Docker image; the host wrapper
bind-mounts `$XDG_CONFIG_HOME/wealthdb.cfg` and `$XDG_DATA_HOME/wealthdb/` at
identical paths inside the container so `~`/`$HOME` resolution
matches both sides.

```sh
./wealthdb build               # build the wealthdb:latest image
./wealthdb <subcommand> ...    # run wealthdb in the container
./wealthdb-test ./...          # run `go test` inside the container
```

See [docs/DESIGN.md §12](docs/DESIGN.md) for the container model
and read-only sharing pattern.

## Documentation

- **[docs/DESIGN.md](docs/DESIGN.md)** — gold-layer architecture,
  schema, CLI, plugin contract, load semantics, query patterns,
  package layout.
- **[docs/RETURNS-NOTES.md](docs/RETURNS-NOTES.md)** — TWR / MWR
  method and rationale.
- **[docs/TAXONOMY.md](docs/TAXONOMY.md)** — the 2-D
  `asset_class` × `vehicle` taxonomy.
- **[docs/adapters/](docs/adapters/)** — per-source adapter design
  ([carta](docs/adapters/carta.md),
  [cointracking](docs/adapters/cointracking.md),
  [schwab](docs/adapters/schwab.md), [ubs](docs/adapters/ubs.md),
  [swissquote](docs/adapters/swissquote.md)).
