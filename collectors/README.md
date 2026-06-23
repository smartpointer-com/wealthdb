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
| `download` | **bronze** under `$XDG_DATA_HOME/wealthdb/<source>/<UTC-ts>/` | Fetch raw artefacts (JSON / CSV / XLS / PDF / zip), exactly as the source returns them. |
| `load` | **silver** `$XDG_DATA_HOME/wealthdb/<source>/<source>.db` | Parse bronze into a source-shaped SQLite. Idempotent — already-loaded dumps are skipped. |

Bronze is immutable raw capture; silver is the parsed, queryable
form and the **input contract** to gold. One silver DB per source.

## Two runtimes

| Runtime | Collectors | Invocation |
| --- | --- | --- |
| **Host venv** | `schwab-api`, `ubs-psn`, `fred`, `manual` | A wrapper runs the collector's `.py` under its `.venv` — pure-stdlib plus one thin dependency; no container. |
| **Docker** | `schwab-web`, `ubs-web`, `swissquote`, `fidelity-web`, `relevate`, `viac`, `cointracking`, `angellist`, `carta`, `equityzen` | A host wrapper script drives `docker run`: `./<tool> {build,login,download,load}`. Browser-based scrapers run headed inside the container. |

`schwab-api` is **hybrid**: its weekly OAuth `login` runs in a
Camoufox/VNC container (Schwab's 7-day refresh token needs an
interactive browser grant), while `download`/`load` run on the host
venv. A `.host-venv` marker tells the Makefile to build/test both.

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
$XDG_DATA_HOME/wealthdb/<source>/
├── 20260528T104753Z/      one bronze dump per run (UTC timestamp)
│   └── …                  raw artefacts
└── <source>.db            silver SQLite
```

**Docker mounts** (the ten containerised collectors): the wrapper
bind-mounts `~/.secrets → /secrets` and `$XDG_DATA_HOME/wealthdb/<source> →
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

## Anatomy of a collector

A collector lives in `collectors/<name>/`. Its one hard requirement is
an **executable `collectors/<name>/<name>`** — the wrapper that
`wealthdb-collect` dispatches to; everything else (Python scripts,
migrations, Dockerfile, tests) is convention. The closest existing
collector is usually the best starting point — a REST one
([`viac`](viac/)), a browser one ([`schwab-web`](schwab-web/)), or a
host-venv one ([`schwab-api`](schwab-api/)).

Each collector is identified by a kebab-case `<name>` (e.g. `acme-bank`)
and an upper-snake `ENV_PREFIX` (e.g. `ACME_BANK`); both thread through
the wrapper, the env file, and the image name.

```
collectors/<name>/
├── <name>                 the wrapper — what wealthdb-collect dispatches to (executable)
├── login.py               mints/refreshes the session  (may be a no-op or absent)
├── download.py            fetches bronze
├── load.py                parses bronze → silver
├── migrations/            0001_initial.sql, 0002_*.sql … (silver schema)
├── requirements.txt       Python deps (host venv) OR pip layer (Docker)
├── Dockerfile             Docker collectors only
├── entrypoint.sh          Docker collectors only — maps subcommand → script
├── tests/                 pytest / unittest (bronze→silver at minimum)
├── README.md              what this source produces + its silver columns
├── DESIGN.md              source-specific reverse-engineering notes (optional)
└── CLAUDE.md              source-specific agent rules (allowed UI surface, etc.)
```

### The wrapper — the `wealthdb-collect` contract

`wealthdb-collect <name> <verb> [flags]` resolves the repo and execs
`collectors/<name>/<name> <verb> [flags]` unchanged; a source appears in
`wealthdb-collect list` when `collectors/<name>/<name>` is executable.
The wrapper therefore:

- accepts the verbs **`login`**, **`download`**, **`load`** (plus
  `build` and `help`; Docker wrappers also `sh`). `login` may be a no-op
  ([`ubs-psn`](ubs-psn/), key-based) or fold into `download` (one-shot
  scrapers), but the verb is still accepted so an orchestrator's
  `login → download → load` never trips.
- honours the **uniform directory-override flags** on every verb, with
  this precedence (highest first):

  | | flag | per-collector env | fleet env | default |
  | --- | --- | --- | --- | --- |
  | secrets | `--secrets-dir` | `${PREFIX}_SECRETS_DIR` | `WEALTHDB_SECRETS_DIR` | `~/.secrets` |
  | data (bronze) | `--data-dir` | `${PREFIX}_DATA_DIR` | `WEALTHDB_DATA_ROOT/<name>` | `$XDG_DATA_HOME/wealthdb/<name>` |
  | silver DB | `--silver-db` | `${PREFIX}_SILVER_DB` | — | `<data-dir>/<name>.db` |

  (`$XDG_DATA_HOME` defaults to `~/.local/share` per the XDG Base
  Directory spec, so the default data root is `~/.local/share/wealthdb`.)

- forwards the `download` date-window flags (`--since`, `--until`,
  `--lookback`, `--documents-*`) to the inner `download.py`.

That precedence is not hand-written; it comes from sourcing the matching
shared library and setting a small config block.

A **Docker** wrapper sources
[`shared/wrappers/wrapper-lib.sh`](../shared/wrappers/wrapper-lib.sh),
sets `NAME`, `ENV_PREFIX`, and `ENV_VARS` (env vars forwarded into the
container with `-e`), optionally opts into feature flags — `HAS_DEBUG`
(a `/debug` mount for `--screenshot-dir`/`--trace`), `HAS_APP_MOUNT`
(live-mounted source during iteration), `HAS_SAFETY` (refuse to clobber
a live container mid-MFA), `HAS_VNC` (`vnc-login` port forwarding) —
calls `wrapper_init`, handles `build`/`help` inline, and ends with
`wrapper_main "$@"`. The library bind-mounts `~/.secrets → /secrets` and
`<data> → /data` (plus `/silver`, `/debug`, `/app` when applicable) and
runs as `--user $(id -u):$(id -g)`.

A **host-venv** wrapper (for pure-stdlib-plus-one-dep tools that need no
browser) sources
[`shared/wrappers/host-lib.sh`](../shared/wrappers/host-lib.sh) and calls
`host_resolve_dirs "$@"` (which populates `SECRETS_DIR` / `DATA_DIR` /
`SILVER_DB` / `FORWARD_ARGS`), `host_source_env_file` (which sources
`<secrets>/<name>.env`), and `host_python` (the collector's `.venv`),
then execs the right `.py` with the resolved paths and `FORWARD_ARGS`.

### login.py — the session

`login.py` authenticates and persists session state to
`<secrets>/<name>-state.json` (cookies/CSRF) or `<name>-token.json`
(OAuth) at chmod `0600`, via
[`collectorkit.session`](../shared/collectorkit/collectorkit/session.py)
(`save_state`, `secure_file`, `load_state`). Credentials come **only**
from env vars (sourced from `<secrets>/<name>.env`), never a
`--password` flag;
[`collectorkit.envfile`](../shared/collectorkit/collectorkit/envfile.py)
(`load_env`, `resolve_credential`) resolves a `--client-id` with an
env-var fallback. A `--check` mode probes the stored session without a
new MFA push. The full authentication policy is in root
[CLAUDE.md](../CLAUDE.md) §3.

### download.py — bronze

`download.py` writes raw artefacts, exactly as the source returns them,
into a fresh UTC-stamped run dir under `--dest` (the resolved data dir),
via [`collectorkit.bronze`](../shared/collectorkit/collectorkit/bronze.py)
(`ts_slug` / `run_dir` for the directory; `atomic_write_bytes` /
`atomic_write_json` so an interrupted run leaves no half-written file).
The shared date-window flags come from
[`collectorkit.cli`](../shared/collectorkit/collectorkit/cli.py)
(`add_lookback_args` + `resolve_lookback`). A `--dry-run` mode walks the
source but exports nothing.

### load.py — silver

`load.py` opens the DB with
[`collectorkit.silver`](../shared/collectorkit/collectorkit/silver.py)
`open_db(--silver-db)`, runs `apply_migrations(conn, migrations/)`, and
parses each bronze run into source-shaped tables. The load is
**idempotent** — already-loaded dumps are skipped (`loaded_snapshots`,
backed by a `dump_runs` table keyed by the run timestamp from
`bronze.parse_run_ts`). Migrations are `NNNN_*.sql` applied in order,
each ending by inserting its own version into `schema_meta`
(`silver_schema_version`). Silver is the **input contract to gold**: its
columns follow the source's shape, not gold's, and are documented in the
collector README.

### Build scaffolding

The [Makefile](../Makefile) auto-discovers collectors (immediate subdirs
of `collectors/`); **the presence of a Dockerfile decides the runtime.**

- A **Docker** collector has a `Dockerfile` + `entrypoint.sh`. The
  Dockerfile `FROM`s a shared base
  ([`shared/images/`](../shared/images/)): `wealthdb/base-python`
  (REST/no-browser, ships `collectorkit`), `wealthdb/base-playwright`,
  or `wealthdb/base-camoufox` (headed browser + Xvfb/VNC).
  `requirements.txt` is copied and `pip install`ed first (a cache
  layer), then the scripts + `migrations` + `entrypoint.sh` are copied
  **by name** so stray host artefacts never enter the image;
  `entrypoint.sh` maps the subcommand to the right script.
  `make build-<name>` runs `<wrapper> build`, and the bases come from
  `make base-images`.
- A **host-venv** collector has only `requirements.txt` (no Dockerfile).
  `make build-<name>` creates `.venv` and installs `requirements.txt`
  plus `-e shared/collectorkit`.

`make build-collectors` / `make all` builds everything.

### Unit tests

Tests live in `tests/` (or `test_*.py`); `make test-<name>` rebuilds the
collector and runs **pytest** — in-container for Docker collectors, in
the `.venv` for host ones. At minimum they cover **bronze → silver**: a
*synthetic* bronze dump (no real IDs/balances — see root
[CLAUDE.md](../CLAUDE.md) §4) and assertions that `load.py` projects the
expected silver rows; pure-stdlib `unittest` works too. `make test` /
`make test-collectors` runs the whole suite, and tests must pass before
any commit.

### The gold adapter

A collector stops at silver. A source reaches the canonical store
through a Go **adapter** under
[`../wealthdb/internal/silver/`](../wealthdb/internal/silver/) that reads
the silver DB and emits canonical snapshot/transaction batches, mapping
into the canonical enums (`account_kind`, `tax_wrapper`,
`management_style`, `asset_class`, `tx_kind`) and the transaction
sign convention. The adapter registers itself from `init()` via
`silver.Register` and is imported in `cmd/wealthdb`; several adapters
also carry a design doc under
[`../wealthdb/docs/adapters/`](../wealthdb/docs/adapters/). The
interface, canonical vocabulary, and fixture-test pattern live on the
gold side — see [`../wealthdb/docs/DESIGN.md`](../wealthdb/docs/DESIGN.md)
and an existing adapter doc (e.g.
[`schwab.md`](../wealthdb/docs/adapters/schwab.md)) — rather than being
restated here.

## The collectors

| Collector | Source | Auth | Runtime |
| --- | --- | --- | --- |
| [`schwab-api`](schwab-api/) | Schwab Trader API | OAuth (7-day refresh) | host venv + Docker login |
| [`schwab-web`](schwab-web/) | Schwab client web | scraped session + 2FA | Docker (Camoufox) |
| [`ubs-psn`](ubs-psn/) | UBS PSN feed | SFTP key | host venv |
| [`ubs-web`](ubs-web/) | UBS netbanking | scraped session + QR | Docker |
| [`swissquote`](swissquote/) | Swissquote eBanking | scraped session + push | Docker |
| [`fidelity-web`](fidelity-web/) | Fidelity web | scraped session + 2FA | Docker (Camoufox) |
| [`relevate`](relevate/) | Relevate / Pensexpert (Pillar 2) | REST + mTAN | Docker |
| [`viac`](viac/) | VIAC (Pillar 3a / vested benefits) | REST + mTAN | Docker |
| [`cointracking`](cointracking/) | Crypto aggregator | scraped session + 2FA | Docker (Camoufox) |
| [`angellist`](angellist/) | AngelList LP portal (SPVs / fund deals) | scraped session | Docker (Camoufox) |
| [`carta`](carta/) | Carta (private holdings / cap table) | scraped session | Docker (Camoufox) |
| [`equityzen`](equityzen/) | EquityZen (pre-IPO secondary SPVs) | scraped session | Docker (Camoufox) |
| [`manual`](manual/) | Private holdings with no portal (CSV) | none — manual entry | host venv |
| [`fred`](fred/) | FRED / US Fed H.10 (historic FX rates) | API key | host venv |

Agent ground rules shared by every collector are in the repo-root
[CLAUDE.md](../CLAUDE.md); each subdirectory's `CLAUDE.md` adds
only source-specific rules.
