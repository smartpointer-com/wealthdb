# wealthdb

A personal wealth-management data suite. It pulls holdings,
transactions, and documents from every configured bank and pension
provider, normalises them into one canonical store, and answers
"what is held, anywhere, as of when?" from a single CLI.

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **The wealthdb suite handles fully privileged financial-account
> credentials. Read this disclaimer in full before configuring any
> credential anywhere in the suite.**

The [collectors](collectors/) sign in to banks, brokerages, and
pension providers with your credentials and your multi-factor
confirmations. The web-scraping collectors **impersonate a human
browser user** (a stealth-hardened browser session), and the API
collectors hold **write-capable credentials**; in both cases the
session is fully privileged — the same login a human uses to move
money — and no provider offers a read-only sub-scope. Nothing but
the codebase's own discipline restricts the collectors to reading.
If malicious code were ever introduced into this repository, its
dependency chain, or the container images it runs, it could act on
your accounts with your full authority and cause **irreversible
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
own risk. See [LICENSE](LICENSE) for the governing terms. This
software is not affiliated with, endorsed by, or sponsored by any
financial institution; nothing in this repository is financial,
legal, or tax advice.

## Why wealthdb

An AI agent becomes genuinely useful when it can answer questions
over a complete financial picture — and genuinely dangerous when
the way to get there is handing it banking credentials. A fully
privileged e-banking login in the hands of a probabilistic,
prompt-injectable system is a standing invitation for irreversible
damage. wealthdb exists to make that trade unnecessary; its
layered security model is the main reason it was built:

1. **Credentials are handled only by static, reviewable code.**
   The collectors are deterministic scripts — auditable line by
   line, human-triggered, with MFA challenges answered by a
   person. No agent drives a banking session, and no agent ever
   sees a credential.
2. **The collectors only read.** Their contract is navigate,
   filter, export — no code path submits a form, places an order,
   or changes a setting, and no CLI flag can enable one.
3. **The data lands locally.** Everything is parsed into local
   databases and consolidated into one queryable gold store;
   nothing is sent to any third-party service.
4. **Agent access is read-only by construction.** The `wealthdb`
   CLI's `--read-only` flag forces read-only access to the gold
   DB, so the surface exposed to an agent is consolidated,
   local, read-only queries — and nothing else.

The result is a clean separation: an agent can answer "what is
held, anywhere, as of when?" while no agent is ever given write
access to the financial data — let alone the banking credentials
that produced it.

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
| [`collectors/`](collectors/) | One bronze+silver collector per source — banks, brokerages, pensions, crypto, reference data | Docker or Python venv | **see [collectors/README.md](collectors/README.md)** for the full list |

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
make install    # symlink wealthdb + wealthdb-collect into ~/.local/bin
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
`login` / `download` / `load` verbs, whether it runs in Docker (the
web/REST ones) or on a host venv. Drive the whole fleet through the
`wealthdb-collect` dispatcher:

```sh
make install                          # symlink wealthdb + wealthdb-collect into ~/.local/bin (BINDIR)

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
offers in it. Without it, downloads default to a 90-day window. The
uniform flag is what lets a single orchestration script — a cron job,
a shell loop — drive the whole fleet through `wealthdb-collect` and
forward one `--lookback` to every collector.

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
