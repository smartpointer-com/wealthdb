# collectors

A **collector** is a self-contained toolkit that pulls one
financial source (a bank, broker, or pension provider) into the
pipeline. Each collector owns the **bronze** and **silver** layers
for its source; the gold engine under [`../wealthdb/`](../wealthdb/)
reads every collector's silver and merges them into one canonical
store. For the full bronze → silver → gold model see
[../ARCHITECTURE.md](../ARCHITECTURE.md).

This file covers what's **common** to all collectors. Each
subdirectory's own `README.md` covers only what's specific to that
source (its auth method, what it produces, its quirks).

## The lifecycle

Every collector exposes the same three steps:

| Step | Produces | What it does |
| --- | --- | --- |
| `login` | a session/token in `~/.secrets/` | Authenticate; usually prompts for MFA. (Omitted where the runtime mints the session inside `download` — see each tool.) |
| `download` | **bronze** under `~/wealthdb/<source>/<UTC-ts>/` | Fetch raw artefacts (JSON / CSV / XLS / PDF / zip), exactly as the source returns them. |
| `load` | **silver** `~/wealthdb/<source>/<source>.db` | Parse bronze into a source-shaped SQLite. Idempotent — already-loaded dumps are skipped. |

Bronze is immutable raw capture; silver is the parsed, queryable
form and the **input contract** to gold. One silver DB per source.

## Two runtimes

| Runtime | Collectors | How you run it |
| --- | --- | --- |
| **Host venv** | `schwab-api`, `ubs-psn` | `.venv/bin/python {download,load}.py …` — pure-stdlib plus one thin dependency; no container. |
| **Docker** | `schwab-web`, `ubs-web`, `swissquote`, `fidelity-web`, `relevate`, `viac` | A host wrapper script drives `docker run`: `./<tool>-dump {build,login,download,load}`. Browser-based scrapers run headed inside the container. |

## Conventions shared across collectors

**Credentials** live in `~/.secrets/<source>.env` (chmod `0600`,
never committed), sourced as a bash script — use **single quotes**
around any value containing `$`, `!`, or backticks so `source`
doesn't mangle it. Credentials reach a tool via env vars only,
never a `--password` flag. Each tool's README lists its specific
variable names. See the repo-root [CLAUDE.md](../CLAUDE.md) §3 for
the full authentication policy.

**Session state** (cookie jars, token bundles, browser profiles)
also lives under `~/.secrets/` (`<source>-state.json`,
`<source>-token.json`, or `<source>-profile/`), chmod `0600`.

**Data layout** is uniform:

```
~/wealthdb/<source>/
├── 20260528T104753Z/      one bronze dump per run (UTC timestamp)
│   └── …                  raw artefacts
└── <source>.db            silver SQLite
```

**Docker mounts** (the six containerised collectors): the wrapper
bind-mounts `~/.secrets → /secrets` and `~/wealthdb/<source> →
/data`, so inside the container credentials are at
`/secrets/<source>.env` and bronze/silver at `/data`.

**Session discipline:** `login --check` probes the stored session
without a new MFA push; `download --dry-run` walks with the
existing session but exports nothing. Agents must not mint real
sessions or run real downloads unless asked — see root
[CLAUDE.md](../CLAUDE.md) §2.

## Gold consumes silver — not the other way round

Silver is the contract gold reads; a collector never knows or
dictates what gold does with it. The gold-side mapping (how a
source's silver columns become canonical `account_kind` /
`tax_wrapper` / `management_style`, instrument joins, sign
conventions) is owned by the per-source adapter in
[`../wealthdb/internal/silver/<source>/`](../wealthdb/internal/silver/)
and documented under
[`../wealthdb/docs/adapters/`](../wealthdb/docs/adapters/) where a
design doc exists. A collector's docs describe its **silver
columns** (what they contain, where scraped from); they point to
the adapter for the gold interpretation rather than restating it.

## The collectors

| Collector | Source | Auth | Runtime |
| --- | --- | --- | --- |
| [`schwab-api`](schwab-api/) | Schwab Trader API | OAuth (7-day refresh) | host venv |
| [`schwab-web`](schwab-web/) | Schwab client web | scraped session + 2FA | Docker (Camoufox) |
| [`ubs-psn`](ubs-psn/) | UBS PSN feed | SFTP key | host venv |
| [`ubs-web`](ubs-web/) | UBS netbanking | scraped session + QR | Docker |
| [`swissquote`](swissquote/) | Swissquote eBanking | scraped session + push | Docker |
| [`fidelity-web`](fidelity-web/) | Fidelity web | scraped session + 2FA | Docker (Camoufox) |
| [`relevate`](relevate/) | Relevate / Pensexpert (Pillar 2) | REST + mTAN | Docker |
| [`viac`](viac/) | VIAC (Pillar 3a / vested benefits) | REST + mTAN | Docker |

Agent ground rules shared by every collector are in the repo-root
[CLAUDE.md](../CLAUDE.md); each subdirectory's `CLAUDE.md` adds
only source-specific rules.
