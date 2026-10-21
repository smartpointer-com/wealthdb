# wealthdb

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **The wealthdb suite handles fully privileged financial-account
> credentials. Read this disclaimer in full before configuring any
> credential anywhere in the suite.**

The `wealthdb` gold engine itself only reads local silver databases
and holds no credentials — but the [collectors](../collectors/)
that feed it sign in to banks, brokerages, and pension providers
with your credentials and your multi-factor confirmations. The
web-scraping collectors **impersonate a human browser user**
(a stealth-hardened browser session), and the API collectors hold
**write-capable credentials**; in both cases the session is fully
privileged — the same login a human uses to move money — and no
provider offers a read-only sub-scope. Nothing but the codebase's
own discipline restricts the collectors to reading. If malicious
code were ever introduced into this repository, its dependency
chain, or the container images it runs, it could act on your
accounts with your full authority and cause **irreversible
financial damage, up to the total loss of the assets reachable
from those credentials**.

**You are solely responsible for a thorough, independent security
audit** of this code, its dependency chain, and its runtime images
**before** entrusting the suite with credentials, and again after
every update or rebuild. If you cannot perform such an audit, do
not hand this software real credentials. Automated access may
additionally breach a provider's terms of service; verifying that
your use is permitted is likewise your responsibility.

**No warranty; no liability.** This software is provided “AS IS”,
without warranty of any kind, express or implied, including but
not limited to the implied warranties of merchantability, fitness
for a particular purpose, title, and non-infringement. To the
maximum extent permitted by applicable law, **SmartPointer AG and
the contributors accept no responsibility for, and shall not be
liable for, any claim, damages, or other liability** — whether in
an action of contract, tort, or otherwise — arising from, out of,
or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds
or other assets, credential or data compromise, account suspension
or termination, and any direct, indirect, incidental, special,
consequential, or punitive damages. Your use is entirely at your
own risk. See [LICENSE](../LICENSE) for the governing terms. This
software is not affiliated with, endorsed by, or sponsored by any
financial institution; nothing in this repository is financial,
legal, or tax advice.

## Overview

The gold engine of the suite. It reads the silver databases the
[collectors](../collectors/) produce, one per source. It projects them
into one canonical DuckDB schema. It reads that schema back as the
suite's reports: holdings as of any date, time- and money-weighted
returns, realized and unrealized gains, exposure after looking through
every fund, what the holdings yield, spending, income and the
household's cash flow statement. The repo-root [README](../README.md)
describes what each report answers.

It runs as a CLI and as an MCP server over the same reports
(`mcp-serve`, run and managed by `wealthdb mcp` from [../mcp/](../mcp/)).
The optional Metabase dashboards live in [../web/](../web/). The engine
runs in one Docker image, with no host-side Go toolchain.

## Commands

The commands fall into four groups, the same as in `wealthdb help`:

- **Reports** read the database and change nothing: `holdings`,
  `returns`, `gains`, `exposure`, `yield`, `transactions`, `spending`,
  `income`, `cashflow`, `status` and `snapshots`.
- **Set up and load** write it. `config` writes the config file and
  `init` creates the database. `load`, `reset` and `reload` bring a
  source's silver in or take it out. `compact` reclaims space. `lots
  rebuild` replays the trades into lots where a source states no cost
  basis; every load does it too.
- **Enrich** asks the configured model. `categorize` places the
  merchants and payers no rule could place, and `resolve-symbols`
  fills in missing tickers. `categorizations` and `resolutions` list
  the stored answers.
- **Other**: `web` and `mcp` run the dashboards and the MCP server,
  `mcp-serve` is the MCP server itself, and `version` prints the
  version.

Every report prints a table, CSV or JSON. Money is shown in the
configured currency or another one, at historic FX rates, and a report
can hide amounts for a screen others may see. `wealthdb help <command>`
gives a command's views and flags.

An adapter ships for every collected source: Swiss and US banks and
brokerages, pension providers, crypto, private markets, and reference
data (FX rates, yield curves, what funds hold). See
[`../collectors/README.md`](../collectors/README.md) for the sources and
[`internal/silver/`](internal/silver/) for their adapters.

Each account has a kind, a tax wrapper and a management style.
Adapters fill what silver carries, and the config fills the rest
([docs/DESIGN.md §13.8](docs/DESIGN.md)).

## Quickstart

Docker is the one requirement; there is no host-side Go toolchain.
From the repo root:

```sh
make build-wealthdb                    # build the engine image (~2 min the first time)
make install                           # put wealthdb on PATH (~/.local/bin)
wealthdb config                        # interactive setup wizard
wealthdb init                          # create the gold DB
wealthdb load -a                       # merge every configured silver
wealthdb holdings positions            # consolidated positions, in the default currency
wealthdb holdings positions -x CHF     # the same, in CHF
wealthdb holdings positions -f csv     # CSV for scripting
wealthdb returns accounts 2025         # per-account TWR for 2025, by quarter
wealthdb returns global --method both  # whole-portfolio TWR and MWR since the first snapshot
wealthdb gains summary 2025            # realized and unrealized gains in 2025, by month
wealthdb gains check 2025              # the rebuilt cost basis against what the statements say
wealthdb exposure sectors              # sector weights, every fund looked through
wealthdb exposure coverage             # what the exposure figures rest on
wealthdb status -v                     # how current each source is
```

The `config` wizard asks for:

1. the gold DB path (default `${XDG_DATA_HOME:-~/.local/share}/wealthdb/wealthdb.db`);
2. the default output currency (default `USD`; `-x` overrides it per
   query);
3. one or more silver sources, each with a short id (used by `load`,
   `reset` and `snapshots`), the source kind (`schwab`, `ubs`,
   `swissquote`, `fidelity`, …) and the path to its silver database.

It writes the result to `${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg`
(another path with `-c <path>`). [docs/DESIGN.md §5](docs/DESIGN.md)
describes every field.

## Where the data goes

The gold database and the collectors' silver databases live under one
data root: `$WEALTHDB_DATA_ROOT` when set, else
`${XDG_DATA_HOME:-~/.local/share}/wealthdb`. The engine reads silver
and never writes it. Reports print to standard output.

## Build and run

From the repo root, the `Makefile` drives builds and tests
(`make build-wealthdb`, `make test-wealthdb`). Underneath, from this
directory, are the per-component wrappers.

All commands run inside a single Docker image. The host wrapper
mounts the config file and the data root at the same paths inside the
container, so paths in the config resolve the same on both sides.

```sh
./wealthdb build               # build the engine image (wealthdb:latest, or the checkout's tag)
./wealthdb <subcommand> ...    # run wealthdb in the container
./wealthdb-test ./...          # run `go test` inside the container
./wealthdb-go mod tidy         # any `go` subcommand, same toolchain
```

[docs/DESIGN.md §12](docs/DESIGN.md) describes the container model
and the read-only sharing pattern.

## Documentation

- **[docs/DESIGN.md](docs/DESIGN.md)** — gold-layer architecture,
  schema, CLI, plugin contract, load semantics, query patterns,
  package layout.
- **[docs/RETURNS-NOTES.md](docs/RETURNS-NOTES.md)** — TWR / MWR
  method and rationale.
- **[docs/GAINS.md](docs/GAINS.md)** — realized and unrealized gains:
  what each figure is, and the quality flags.
- **[docs/EXPOSURE.md](docs/EXPOSURE.md)** — exposure: the
  look-through, the views and the quality flags.
- **[docs/YIELD.md](docs/YIELD.md)** — yield: what each holding earns,
  its duration and rate risk, and the views.
- **[docs/LOTS.md](docs/LOTS.md)** — the lot engine: how a cost basis
  no source states is rebuilt from the trades, and how far to trust it.
- **[docs/SPENDING.md](docs/SPENDING.md)**,
  **[docs/INCOME.md](docs/INCOME.md)**,
  **[docs/CASHFLOW.md](docs/CASHFLOW.md)** — the enrichment engine's
  three readings: how a transaction gets its category, its payer, and
  its place in the cash flow statement.
- **[docs/TAXONOMY.md](docs/TAXONOMY.md)** — the 2-D
  `asset_class` × `vehicle` taxonomy.
- **[docs/adapters/](docs/adapters/)** — per-source adapter design,
  one file per adapter that needs one.
