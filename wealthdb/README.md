# wealthdb

A personal-portfolio gold-layer CLI. Reads per-bank silver
SQLite databases (produced by sibling `*-dump` repositories —
[schwab-api-dump](https://github.com/ptu/schwab-api-dump),
[ubs-psn-dump](https://github.com/ptu/ubs-psn-dump),
[ubs-web-dump](https://github.com/ptu/ubs-web-dump),
[swissquote-dump](https://github.com/ptu/swissquote-dump)) and
projects them into a canonical cross-bank DuckDB schema queryable
through the `wealthdb` CLI.

CLI only, no web UI. Single Docker image; no host-side Go
toolchain required.

## Status

All planned v1 functionality is in. The CLI ships with:

| Subcommand | Purpose |
| --- | --- |
| `wealthdb config` | Interactive first-time setup wizard. |
| `wealthdb init` | Create the gold DuckDB at the configured path. |
| `wealthdb load <id>\|-a` | Merge new silver snapshots into gold. |
| `wealthdb reset <id>\|-a` | Purge a silver source's data from gold. |
| `wealthdb reload <id>\|-a` | Reset then load (use after upgrading wealthdb). |
| `wealthdb positions` | Print consolidated positions (table / csv / csv_plain / json) with currency conversion. |
| `wealthdb transactions` | Print transactions over a date range, oldest first (`-r` reverses to newest first). |
| `wealthdb accounts` | Print one row per account with derived positions / cash / total value aggregates. |
| `wealthdb portfolios` | Print one row per portfolio (plus sentinel-NULL row per silver source) with derived value aggregates. |
| `wealthdb status [<id>] [-v]` | Report gold state vs each silver source. |
| `wealthdb snapshots <id>\|-a` | List snapshots gold has loaded for a silver. |
| `wealthdb help [<subcommand>]` | Help. |

Verified end-to-end against real Schwab + UBS + Swissquote
silvers.

Future work (queued for separate milestones) lives in
[docs/DESIGN.md §13](docs/DESIGN.md). Notable items:
account-type categorisation (§13.9), instrument-name enrichment
for Schwab equity, market-data feeds (§13.8).

## Quickstart

You need Docker. No host-side Go toolchain.

```sh
git clone <this repo>
cd wealthdb
./wealthdb build                  # one-time, ~2 min on first run
./wealthdb config                 # interactive setup wizard
./wealthdb init                   # create the gold DB
./wealthdb load -a                # merge every configured silver
./wealthdb positions              # print consolidated positions (default table format, USD)
./wealthdb positions -x CHF       # render values in CHF
./wealthdb positions -f csv       # CSV output for scripting
./wealthdb status -v              # quick health check across all silvers
```

The `config` wizard walks you through:

1. Gold DB path (default `$HOME/wealthdb/wealthdb.db`).
2. Default output currency (default `USD`; can be overridden per
   query with `-x`).
3. One or more silver sources — for each: a short id (used by
   `load`/`reset`/`snapshots`), the bank kind (`schwab`, `ubs`,
   `swissquote`), and the path to the silver SQLite.

It writes the result to `$HOME/.config/wealthdb.cfg` (overridable
with `-c <path>`).

## Build and run

All commands run inside a single Docker image; the host wrapper
bind-mounts `$HOME/.config/wealthdb.cfg` and `$HOME/wealthdb/` at
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
  schema, CLI, plugin contract, load semantics, query patterns.
- **[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)** — Go
  package layout, dependency direction, testing strategy.
- **[docs/adapters/](docs/adapters/)** — per-bank adapter design
  ([schwab](docs/adapters/schwab.md), [ubs](docs/adapters/ubs.md),
  [swissquote](docs/adapters/swissquote.md)).
