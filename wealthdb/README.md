# wealthdb

The gold layer of a personal-portfolio data pipeline. Reads
per-bank silver SQLite databases (produced by sibling `*-dump`
repositories — [schwab-dump](https://github.com/ptu/schwab-dump),
[ubs-psn-dump](https://github.com/ptu/ubs-psn-dump),
[swissquote-dump](https://github.com/ptu/swissquote-dump)) and
projects them into a canonical, cross-bank DuckDB schema queryable
through the `wealthdb` CLI.

Single Docker image; no host-side Go toolchain required. CLI only,
no web UI.

## Status

All non-interactive subcommands are in. `wealthdb init`, `load`,
`reset`, `positions`, `status`, and `snapshots` work end-to-end
against Schwab, UBS, and Swissquote silver databases. `positions`
renders in table / csv / csv_plain / json with selectable columns
and multi-currency value conversion. The interactive
`wealthdb config` first-time-setup wizard lands in M11 and
MT535 SWIFT-tag parsing for UBS quantity/market_value in M12 per
[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md).

## Build and run

All commands run inside a single Docker image (no host-side Go
toolchain needed).

```sh
./wealthdb build               # build the wealthdb:latest image
./wealthdb <subcommand> ...    # run wealthdb in the container
./wealthdb-test ./...          # run `go test` inside the container
```

The `wealthdb` wrapper mounts `$HOME/.config/wealthdb.cfg` and
`$HOME/wealthdb/` into the container at identical paths so `~`
expansion works the same on both sides. See
[docs/DESIGN.md §12](docs/DESIGN.md) for the full container model.

## Documentation

- **[docs/DESIGN.md](docs/DESIGN.md)** — gold-layer architecture,
  schema, CLI, plugin contract, load semantics, and query patterns.
- **[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)** — Go package
  layout, dependency direction, testing strategy, implementation
  roadmap.
- **[docs/adapters/](docs/adapters/)** — per-bank adapter design
  ([schwab](docs/adapters/schwab.md), [ubs](docs/adapters/ubs.md),
  [swissquote](docs/adapters/swissquote.md)).
