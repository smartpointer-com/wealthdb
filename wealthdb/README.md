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

Minimum viable CLI is in. `wealthdb init`, `wealthdb load <id> | -a`,
and `wealthdb positions` work end-to-end against Schwab, UBS, and
Swissquote silver databases. Multi-currency rendering, the
interactive `wealthdb config` wizard, and the remaining
subcommands (`reset`, `status`, `snapshots`) land in later
milestones per [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md).

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
