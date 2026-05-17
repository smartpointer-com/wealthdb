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

Design phase. No code yet — see [docs/DESIGN.md](docs/DESIGN.md)
for the architecture and [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)
for the Go-level plan.

## Documentation

- **[docs/DESIGN.md](docs/DESIGN.md)** — gold-layer architecture,
  schema, CLI, plugin contract, load semantics, and query patterns.
- **[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)** — Go package
  layout, dependency direction, testing strategy.
- **[docs/adapters/](docs/adapters/)** — per-bank adapter design
  ([schwab](docs/adapters/schwab.md), [ubs](docs/adapters/ubs.md),
  [swissquote](docs/adapters/swissquote.md)).
