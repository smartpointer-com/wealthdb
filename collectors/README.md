# collectors

A **collector** is a self-contained toolkit that pulls one
financial source (a bank, broker, or pension provider) into the
pipeline. Each collector owns the **bronze** and **silver** layers
for its source; the gold engine under [`../wealthdb/`](../wealthdb/)
reads every collector's silver and merges them into one canonical
store. For the full bronze → silver → gold model see
[../DESIGN.md](../DESIGN.md).

This file covers what is **common** to all collectors. Each
subdirectory's own `README.md` covers only what is specific to that
source (its auth method, what it produces, its quirks).

> [!WARNING]
> Collectors hold **fully privileged financial-account
> credentials**, and the browser-driven ones **impersonate a human
> user**. Before configuring any credential, read the security &
> liability disclaimer in the [repo README](../README.md) and in
> the README of each collector you intend to use. Use requires a
> prior independent security audit and is entirely at your own
> risk; SmartPointer AG accepts no liability.

## The lifecycle

Every collector exposes the same four steps:

| Step | Produces | What it does |
| --- | --- | --- |
| `login` | a session/token in `~/.secrets/` | Authenticate; usually prompts for MFA. Where `download` mints its own session, `login` is a no-op that says so. When a credential is missing, `login` says what to set up instead (see [The setup declaration](#the-setup-declaration--setuptxt)). |
| `download` | **bronze** under `$XDG_DATA_HOME/wealthdb/<source>/<UTC-ts>/` | Fetch raw artefacts (JSON / CSV / XLS / PDF / zip), exactly as the source returns them. |
| `load` | **silver** `$XDG_DATA_HOME/wealthdb/<source>/<source>.db` | Parse bronze into a source-shaped SQLite. Idempotent — already-loaded dumps are skipped. |
| `prune` | reclaimed disk | Delete debug artefacts from complete dumps + whole non-complete dumps, plus aged-out entries of the host-side debug cache. Never touches a `load` input. See [Debug artefacts, run status, and pruning bronze](#debug-artefacts-run-status-and-pruning-bronze). |

Bronze is immutable raw capture; silver is the parsed, queryable
form and the **input contract** to gold. One silver DB per source.

## Two runtimes

A collector's runtime is set by whether it ships a Dockerfile (the
[Runtime column](#the-collectors) below records which each one is):

| Runtime | Marker | Invocation |
| --- | --- | --- |
| **Host venv** | a `requirements.txt`, no Dockerfile — no browser and no container | A wrapper runs the collector's `.py` under its `.venv`. |
| **Docker** | a Dockerfile + `entrypoint.sh` — browser-based scrapers run headed inside the container | A host wrapper script drives `docker run`: `./<tool> {build,login,download,load}`. `prune` runs on the host. |

Two collectors are **hybrid**, and a `.host-venv` marker tells the
Makefile to build both halves and to test on the host venv.
`schwab-api`'s weekly OAuth `login` runs in a Camoufox/VNC container
(Schwab's 7-day refresh token needs an interactive browser grant) while
`download`/`load` run on the host venv. `fidelity-web` splits the same
way for a different reason: its session lives only as long as the
browser, so the container is where a run happens and `vnc-login` seeds
the profile dir, while the file-only verbs run on the host.

## Conventions shared across collectors

**Credentials** live in `~/.secrets/<source>.env` (chmod `0600`,
never committed). The file is a bash script. The wrappers and the
collectors both read it through bash, so bash's quoting rules apply:

- Single quotes keep every character as written. Use them around a
  password.
- A single quote inside a value is written `'\''`.
- Inside double quotes, `$`, backticks and `\` are special.
- An unquoted space ends the value, and an unquoted ` #` starts a
  comment.

Credentials reach a tool via env vars only, never a `--password`
flag. Each tool's README lists its specific variable names. See the
repo-root [AGENTS.md](../AGENTS.md) §3 for the full authentication
policy.

**Session state** (cookie jars, token bundles, browser profiles)
also lives under `~/.secrets/` (`<source>-state.json`,
`<source>-token.json`, or `<source>-profile/`), chmod `0600`.
A browser profile keeps only session state — cookies, keys, certs,
prefs; its one cache no pref can disable (Firefox's `startupCache/`)
is relocated to a non-sensitive per-profile cache dir under
`${XDG_CACHE_HOME:-~/.cache}/wealthdb/startupcache` (override with
`WEALTHDB_STARTUPCACHE_DIR`), so no cache bytes accrue next to the
session cookie.

**Data layout** is uniform:

```
$XDG_DATA_HOME/wealthdb/<source>/
├── 20260101T120000Z/      one bronze dump per run (UTC timestamp)
│   └── …                  raw artefacts
└── <source>.db            silver SQLite
```

**Docker mounts** (the containerised collectors): the wrapper
bind-mounts `~/.secrets → /secrets` and `$XDG_DATA_HOME/wealthdb/<source> →
/data`, so inside the container credentials are at
`/secrets/<source>.env` and bronze/silver at `/data`. It also mounts the
browser's startup cache dir at `/cache/startupcache`.

**Caches.** A load keeps its caches in
`${XDG_CACHE_HOME:-~/.cache}/wealthdb/<source>`, on the host as in the
container:

- the wrapper mounts that dir at `/cache/collector` and points
  `WEALTHDB_CACHE_DIR` at it;
- a load that runs on the host finds the same dir through
  `collectorkit.extract.cache_root`;
- `WEALTHDB_CACHE_DIR` names one collector's cache root, never a dir
  that several collectors share: a load sweeps the entries it does not
  use.

The dir holds extracted statement text and parsed rows, so it is kept
mode `0700`. It is derived data: deleting it costs one slower load.

**Session discipline:** `login --check` probes the stored session
without a new MFA push. Where a session persists, `download --dry-run`
walks with it but exports nothing. The one-shot collectors keep no
session between runs, so their `download` signs in itself: schwab-web,
chase, raiffeisen_at, amex, fidelity-web and firstcitizens. There a dry
run signs in too. Which dry runs an agent may start without asking is
set by root [AGENTS.md](../AGENTS.md) §2 and each collector's own
`AGENTS.md`; amex is the case that matters (see the exceptions below).

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
collector is usually the best starting point — pick it via the
archetype table in
[../NEW-COLLECTOR-PROMPT.md](../NEW-COLLECTOR-PROMPT.md), which also
carries the build playbook and the kickoff-prompt template for
building a collector with a coding agent.

Each collector is identified by a kebab-case `<name>` (e.g. `acme-bank`)
and an upper-snake `ENV_PREFIX` (e.g. `ACME_BANK`); both thread through
the wrapper, the env file, and the image name.

```
collectors/<name>/
├── <name>                 the wrapper — what wealthdb-collect dispatches to (executable)
├── setup.txt              what the first sign-in needs: credentials, steps, next commands
├── collector.py           the declaration: SPEC, a collectorkit.spec.Collector
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
└── AGENTS.md              source-specific agent rules (allowed UI surface, etc.)
```

### The declaration — `collector.py`

Each collector declares itself once, as `SPEC` in its `collector.py`.
`SPEC` is a `collectorkit.spec.Collector`. It names:

- the collector's name and env prefix, as its wrapper sets them;
- what a download signs in with, and whether it may run with no person
  present;
- whether a download honours `--lookback` or always fetches the whole
  history;
- the debug subdirs and globs that `prune` reclaims from a complete run.

The file imports only the standard library and collectorkit, so a tool
reads it without the collector's own dependencies. This prints one JSON
object per collector:

```sh
python -m collectorkit.spec describe collectors/fred collectors/amex
```

The collectorkit suite checks each declaration against its wrapper's
`NAME` and `ENV_PREFIX` and against its download's window. It also
checks that the default paths in its scripts are the ones `spec.paths()`
derives from the name. A collector without a `collector.py` fails that
suite.

### The wrapper — the `wealthdb-collect` contract

`wealthdb-collect <name> <verb> [flags]` resolves the repo and execs
`collectors/<name>/<name> <verb> [flags]` unchanged; a source appears in
`wealthdb-collect list` when `collectors/<name>/<name>` is executable.
The wrapper therefore:

- accepts the verbs **`login`**, **`download`**, **`load`**, **`prune`**
  (plus `build` and `help`; the Docker wrappers other than schwab-api
  also `sh`). `login` may be a no-op ([`ubs-psn`](ubs-psn/), key-based)
  or fold into `download` (one-shot scrapers), but the verb is still
  accepted so an orchestrator's `login → download → load` never trips.
  `prune` runs host-side (a file walk needs no container), so it can
  reclaim disk while a `download` is mid-flight.
- honours the **uniform directory-override flags** on every verb, with
  this precedence (highest first):

  | | flag | per-collector env | fleet env | default |
  | --- | --- | --- | --- | --- |
  | secrets | `--secrets-dir` | `${PREFIX}_SECRETS_DIR` | `WEALTHDB_SECRETS_DIR` | `~/.secrets` |
  | data (bronze) | `--data-dir` | `${PREFIX}_DATA_DIR` | `WEALTHDB_DATA_ROOT/<name>` | `$XDG_DATA_HOME/wealthdb/<name>` |
  | silver DB | `--silver-db` | `${PREFIX}_SILVER_DB` | — | `<data-dir>/<name>.db` |

  (`$XDG_DATA_HOME` defaults to `~/.local/share` per the XDG Base
  Directory spec, so the default data root is `~/.local/share/wealthdb`.)

- forwards `--lookback` to every `download` — the one window flag, accepted
  everywhere; see the date contract under
  [Collector CLI conventions](#collector-cli-conventions). A full-history
  source accepts it for uniformity but warns it cannot narrow its fetch,
  rather than silently ignoring or rejecting it.

That precedence is not hand-written. It lives once, in
[`shared/wrappers/paths-lib.sh`](../shared/wrappers/paths-lib.sh), which
both wrapper libraries source. A wrapper sources the matching library
and sets a small config block.

A **Docker** wrapper sources
[`shared/wrappers/wrapper-lib.sh`](../shared/wrappers/wrapper-lib.sh)
and sets `NAME` and `ENV_VARS` (env vars forwarded into the container
with `-e`). `ENV_PREFIX` is `NAME` in upper snake case unless the
wrapper sets it. It may opt into these knobs:

- `HAS_DEBUG`: a `/debug` mount for `--screenshot-dir` and `--trace`.
- `HAS_SAFETY`: refuse to clobber a live container mid-MFA.
- `VNC_SUBCOMMANDS`: the verbs that get a forwarded VNC port.
- `HAS_ENV_FILE`: source the credentials env file on the host, so the
  `ENV_VARS` it sets are forwarded.
- `SILVER_SUBCOMMANDS`: the verbs that read `--silver-db`, in place of
  `load`.

A wrapper with a config file calls `wrapper_source_cfg` first. The
wrapper then calls `wrapper_init`, prints its help when
`wrapper_wants_help`, and ends with `wrapper_main "$@"`. A wrapper whose
`login` folds into `download` calls `wrapper_login_folds` before that.
A bare `login` then prints a notice and starts no container.
`wrapper_main` and `wrapper_login_folds` run the setup check first. `wrapper_main`
runs `build`, and runs `prune` on the host through
`wrapper_host_prune`; fidelity-web and schwab-api route `prune`
themselves. The library bind-mounts the dirs listed under Docker mounts
and Caches, plus the checkout over `/app` (and `/silver` and `/debug`
when applicable), and runs as `--user $(id -u):$(id -g)`. It runs only
an image built on this computer: a missing image stops the run with
`wealthdb-collect <name> build`, and every run passes `--pull=never`,
so no registry is asked for an image of that name.

A **host-venv** wrapper (for tools that need no browser) sets `NAME`,
sources [`shared/wrappers/host-lib.sh`](../shared/wrappers/host-lib.sh)
and calls these:

- `host_begin VERB... -- "$@"` prints the banner as help and rejects an
  unknown verb. It then sets `VERB`, resolves `SECRETS_DIR`,
  `DATA_DIR`, `SILVER_DB` and `FORWARD_ARGS`, and runs the setup check.
- `host_source_env_file [FILE]` sources `<secrets>/<name>.env`. Given
  FILE, it sources another source's `<secrets>/<FILE>.env`, overridden
  by `<FILE>_ENV_FILE` in upper snake case.
- `host_run SCRIPT [ARGS...]` execs the `.py` on the collector's `.venv`
  with ARGS, then `FORWARD_ARGS`.
- `host_login_probe SCRIPT [ARGS...]` is the probe in the `login` of a
  source with no session. `--check` runs SCRIPT. Otherwise it returns,
  and the wrapper prints its notice.

### The setup declaration — `setup.txt`

Each collector states what its first sign-in needs in `setup.txt`,
beside its wrapper. It names the env file, each variable to put in it,
what to do at the provider first, how the sign-in runs, and the
commands that come after `login`. Before a verb that signs in, the
wrapper reads it and checks that the credentials are in place.

The file is plain text, one fact per line. A line is a keyword, one
space and a value. Blank lines and lines that start with `#` are
skipped. Bash 3.2 reads it with no other tool. It is data, never
sourced.

| Keyword | Value | Meaning |
| --- | --- | --- |
| `envfile` | `FILE.env [FALLBACK.env...]` | A file the sign-in reads from the secrets folder. The first name that exists is read. Several lines: several files. With none, `<name>.env`. |
| `var` | `NAME need kind shape label` | A variable. `need` is `required` or `optional`. `kind` is `secret` or `plain`. `shape` is the placeholder of the template, with no space. `label` is the rest of the line. |
| `alias` | `NAME OTHER` | `OTHER` is accepted in place of `NAME`. |
| `flag` | `NAME --flag` | `--flag VALUE` on the command line gives `NAME`. |
| `keyfile` | `FILE label` | A file that must be in the secrets folder, such as an SSH key. |
| `session` | `PATH` | A stored session in the secrets folder. When it exists, a sign-in may need no credential, so the check never stops the verb. |
| `checks` | `VERB...` | The verbs the check runs before. |
| `skip` | `FLAG...` | Flags that turn the check off for that run. |
| `provider` | text | A step at the provider before the first sign-in. One line each. |
| `docs` | `SECTION` | The section of the collector's README.md with more. |
| `signin` | text | How the sign-in runs, in plain words. |
| `first` | text | The first step of a collector with nothing to sign in to. |
| `then` | command | A command after `login`, in order. It is listed with what it does, taken from its verb (`download`, `load`, `link`, `wealthdb load`). A note after three spaces and `# ` adds a line. A part in `[ ]` that holds `<name>` is kept, with the name in place, when the verb run named one with `--item`; else it is left out. |

An example, for a collector that signs in with an email and a password:

```
var ACME_BANK_USERNAME required plain <email> The email you sign in with
var ACME_BANK_PASSWORD required secret <password> Your password
checks login
signin Acme Bank sends a code by SMS. Type it in the terminal.
then wealthdb-collect acme-bank download
then wealthdb-collect acme-bank load
then wealthdb load -a
```

The check ([`shared/wrappers/setup-lib.sh`](../shared/wrappers/setup-lib.sh))
runs before each verb of `checks`, in both wrapper families:

- It resolves the secrets folder and the env file as the wrapper does:
  `--secrets-dir`, `${PREFIX}_SECRETS_DIR`, `WEALTHDB_SECRETS_DIR`,
  `~/.secrets`. `${FILE}_ENV_FILE` (`VIAC_ENV_FILE` for `viac.env`)
  names the file where the wrapper reads it on the host: a host-venv
  wrapper, or a Docker wrapper with `HAS_ENV_FILE`.
- A variable is there when it is set and not empty in the environment
  or in an env file, under its name or an alias, or when its flag is
  given.
- A value that is still exactly its `shape`, the placeholder of the
  template, does not count. It stops the verb even for an optional
  variable, since the provider would receive it. The instructions list
  its line as one that still holds the placeholder. The value is
  compared where the file is sourced and never printed.
- It reads an env file the way root [AGENTS.md](../AGENTS.md) §3 sets:
  `bash -n`, then `source` in a subshell. Only the names of the
  variables leave the subshell. No value is ever printed.
- A required variable or a key file that is missing, or a missing
  secrets folder of a Docker collector, prints the instructions and
  stops the verb with exit status **78**. No collector uses 78 for
  anything else. The instructions give the provider steps, the
  commands that make the folder (`chmod 700`) and the file
  (`chmod 600`), a template of the lines to add, how the sign-in runs,
  and `Then run: wealthdb-collect <name> <verb>`.
- An env file that `bash -n` rejects is reported as not valid, with
  the fix and without its lines, and the verb stops with 78.
- An env file that is missing while every variable is optional prints
  a short note on what it would hold, and the verb goes on.
- A complete setup prints nothing before the verb runs.
- A `login` that ends well, or the `link` of a collector that links one
  login at a time, closes with the commands that follow it: the
  `then` lines, numbered, between two rules. So the first command a
  setup names always says what comes next. It is printed for a person
  only, when stderr is a terminal, and a scheduled run's output stays
  as it is. `WEALTHDB_SETUP_NEXT=1` prints it without a terminal, `0`
  never. A help flag and a session probe (`--check`) do not get it.
- On a terminal, the block and the setup's instructions are coloured
  as `wealthdb config` colours: titles bold, commands to run cyan,
  what each does dimmed. `NO_COLOR` or `TERM=dumb` turns the colours
  off; `WEALTHDB_SETUP_COLOR=1` or `0` decides them.

A message or a help text names a collector's command as
`wealthdb-collect <name> <verb>`, never as `./<name>`: the reader's
working directory is not known.

The check is skipped for `--help`, `-h`, `--check` (a session probe),
`--env-file` (a file named on the command line), the flags of `skip`,
and every verb that `checks` does not name. A dry run of a download
that signs in is checked. A dry run of a download that does not sign
in is never in `checks`.

`shared/wrappers/tests/test_setup_check.sh` holds each declaration to
its collector. Every variable must be one the wrapper forwards
(`ENV_VARS`) or sources on the host, and every credential name in the
collector's code must be declared.

### Collector CLI conventions

The same concept wears the same flag, default, and env var on every
collector — the principle of least surprise, and the one precedent a new
collector author copies. The **default no-optional-flags path always pulls
everything**: `wealthdb-collect <source> download` (then `load`) fetches all
documents and all detail with nothing to remember; the `--no-*` flags below
are the escape, not opt-ins. The canonical spelling per concept:

| Concept | Canonical spelling | Default |
| --- | --- | --- |
| bronze root (every verb that touches it) | `--bronze-dir` | resolved data dir |
| documents / heavy detail | fetched by default; opt out with `--no-documents` (and the same `--no-<detail>` pattern for other heavy passes) | **on** |
| force reload (on `load`) | `--force` — rebuild silver from all bronze; on the load frame the old silver stays in place until the new one is complete; on the load frame and the loaders that call `rebuild.runs_without_bronze`, a warning first names each loaded run whose bronze is gone | off |
| session probe | `login --check`: exit `0` = credential or session alive, nonzero = not. See [login.py — the session](#loginpy--the-session) for what it reads and for the five collectors that differ. | — |
| exit status (`download`, `login --check`) | `collectorkit.cli`: `0` done, `1` failed, `2` usage error, `3` no verdict (a probe that could not run), `6` a person has to sign in first. A full `login` may give its own codes. `78`: the setup check stopped the verb (see [The setup declaration](#the-setup-declaration--setuptxt)). | — |
| human-MFA wait | `--mfa-timeout SECONDS` | per source; see the list below |
| MFA-page-appear wait | `--mfa-page-timeout SECONDS` | per source |
| login-diagnostics dir | `--screenshot-dir` (login); `--debug-dir` on an `explore` harness, and on `prune`, which reclaims that dir | None |
| `--mode` "everything" token | `all` | `all` |
| login identity env | `${PREFIX}_USERNAME` | — |
| other credentials env | under `${PREFIX}_*`, unless a share is deliberate and documented | — |
| env file | `/secrets/<source>.env` (container) / `~/.secrets/<source>.env` (host) | — |
| env-file override | `--env-file` flag; a host-venv wrapper also honours a `${PREFIX}_ENV_FILE` env var | — |
| credential precedence | the env file wins over an inherited shell env value | — |
| session-state file | `<source>-state.json` / `<source>-token.json` / `<source>-profile/` | — |
| load-only bronze layout | `<data-dir>/bronze/` (the svb shape) | — |
| credential value on argv | never — env-var fallback only, never a `--password VALUE` flag | — |

(The directory-override flags `--secrets-dir` / `--data-dir` /
`--silver-db` and their `${PREFIX}_*` / fleet-env forms are in the wrapper
contract table above. `--secrets-dir` / `--data-dir` hold on every verb;
`--silver-db` is read only by `load` — plus cointracking's `fetch-prices` —
and is **rejected** on any other verb rather than silently dropped. The
`${PREFIX}_SILVER_DB` env var is not symmetric with the flag: it is ambient
config naming where silver lives, so a verb with no use for it ignores it
rather than failing.)

The `--mfa-timeout` defaults, in seconds:

- 300 (five minutes): schwab-api, schwab-web, fidelity-web, chase,
  amex, firstcitizens, swissquote, ubs-web. That includes a sign-in
  done by hand over VNC.
- raiffeisen_at: the pushTAN timeout the server declares.
- 3600: plaid's `link` verb. That wait is the lifetime of the hosted
  Link page, not of an MFA code.

The other collectors read a code from stdin and set no wall clock.

**Unknown or inapplicable flags fail; they are never ignored.** The one
exception is uniformity: a flag the fleet orchestrator forwards to every
source must PARSE everywhere, so `--lookback` is accepted by every
`download` and a collector that cannot narrow its fetch warns rather than
rejecting. A wrapper declares its silver-consuming verbs with
`SILVER_SUBCOMMANDS` (default `(load)`).

**There is exactly one window flag: `--lookback`.** Every `download`
accepts it, so a fleet orchestrator can hand the same flag to every source. It
takes either a named preset (`1w`, `4w`, `3m`, `6m`, `1y`, `2y`, `5y`,
`all`) or an ISO date (`2020-01-01`), and it names a **starting point**:
the window always runs from there to today, and everything the source
offers inside it — transactions, documents, snapshots — is fetched. The
default is ≈ 90 days; `all` reaches back 30 years.

There is deliberately **no upper-bound flag and no per-facet window**. A
second window knob is what lets a preset or a bare year silently override
the resolved window, and a per-facet window multiplies the ways a run can
under-fetch without saying so. One flag, one window, one answer to "what
did this run cover".

- **Bounded collectors** honour the window.
- **Full-history collectors** (a passive SPA capture, a full export whose
  downstream replay needs every row, an SFTP drop) accept the *same* flag
  for uniformity but structurally cannot narrow the fetch. They **warn
  loudly** that it has no effect and pull their complete history — a
  superset of any window — rather than either silently ignoring it or
  rejecting it at parse time.
- A collector whose source cannot express the window exactly maps it to
  the narrowest thing that covers it. relevate's year-granularity
  endpoint widens the window to whole years without a warning; a
  collector that has to cap the window **warns when it caps**.

Both tiers wire the one shared group,
[`collectorkit.cli`](../shared/collectorkit/collectorkit/cli.py)
`add_standard_args(parser, verb=…)` + `resolve_standard(args, …)`, so a
standard optional flag always parses cleanly — it never argparse-rejects.
Every standard flag is honoured everywhere it can be. Where a source
structurally cannot narrow a fetch, the collector says so at runtime
(`warn_lookback_ignored`) rather than ignoring the flag in silence.

**Deliberate exceptions** (recorded so they read as intentional, not drift):

- **cointracking** silver is a `.duckdb` file (window-function balance
  replay, `DECIMAL(38,18)` amounts) — a recorded one-off; its `--silver-db`
  default is that file, not `<data-dir>/<name>.db`.
- **schwab-web** and **schwab-api** deliberately share
  `SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD` (one consent login for two
  silvers whose identifier spaces are disjoint). schwab-api's OAuth app
  keys are `SCHWAB_CLIENT_ID` / `SCHWAB_CLIENT_SECRET`.
- **relevate** and **viac** name the login id `RELEVATE_LOGIN` /
  `VIAC_LOGIN`, and **cointracking** reads its Binance key as
  `BINANCE_API_KEY`. Renaming them would break existing env files.
- **ubs-web** reads the bank-level `ubs.env` (the contract number)
  when its own `ubs-web.env` does not exist.
- **refdata** reads FINRA's API client from `finra.env`, beside
  `refdata.env`. Its variables are `FINRA_CLIENT_ID` and
  `FINRA_CLIENT_SECRET`, and `FINRA_ENV_FILE` overrides the path. Only
  `login` and `download` source it. The download keeps it from the
  engine it asks for the wanted list.
- **viac** `--no-transaction-documents` opts out of only the per-event
  receipt PDFs — a narrower concept than `--no-documents`; viac's document
  centre always downloads on its date window.
- **plaid** keeps one token file per linked login (a Plaid Item),
  `plaid-token-<item>.json`, instead of a single `<source>-token.json`.
  One collector serves many Items, and an access token Plaid shows only
  once is never rewritten by a later link. Its data dir holds one tree
  per Item, `<data-dir>/<item>/<UTC-ts>/`, rather than run dirs at the
  top, so the `dedup` sweep (below) does not reach its runs. Its silver
  is one database per Item too, `<data-dir>/<item>/<item>.db`, so
  `load --silver-db` needs the one `--item` it is for. A sign-in has a
  verb of its own, `link --item NAME`: it makes a new Item, billed by
  Plaid, or renews one, so no orchestrator runs it. Its `login` opens no
  page. It settles the sign-ins a stopped `link` left open, and with
  none it does nothing, so `login → download → load` runs it safely. Its
  `link`, `login` and `download` also take `--sandbox`, which switches a
  run to Plaid's test institutions and the Items made there. Its
  `download --refresh` asks Plaid to fetch investments from the
  institution first. Plaid bills that per call on a paid plan, so a
  Production run makes it only with the opt-in in the collector's own
  `$XDG_CONFIG_HOME/plaid.cfg`.
- **amex** spends its whole verb surface out of one small sign-in budget.
  A handful of sign-ins draws a captcha. So two fleet defaults are
  withdrawn. `login --check` reads the device-trust cookie instead of
  calling the source (see [login.py](#loginpy--the-session)). And
  `download --dry-run` still signs in: it skips the exports, not the
  sign-in. That takes the dry run out of the run-without-asking set of
  root [AGENTS.md](../AGENTS.md) §2 ([amex/AGENTS.md](amex/AGENTS.md) §0).

### login.py — the session

`login.py` authenticates and persists session state to
`<secrets>/<name>-state.json` (cookies/CSRF) or `<name>-token.json`
(OAuth) at chmod `0600`, via
[`collectorkit.session`](../shared/collectorkit/collectorkit/session.py)
(`save_state`, `secure_file`, `load_state`). Secrets come **only** from env
vars (sourced from `<secrets>/<name>.env`), never a `--password` flag, and
an **OTP / 2FA code is read from stdin, never accepted on argv** (equityzen's
`--totp` is a documented non-interactive escape hatch — a precedent to weigh,
not the pattern to copy).

Which **identity** surface to copy for a new collector:

- a **browser / session** collector takes **no identity flag** — the login
  id lives in the env file (`${PREFIX}_USERNAME`), same as the password;
- an **API / key** collector may expose a *non-secret* identity flag
  (`--client-id`, `--contract-number`) with an env-var fallback — the
  value-with-env-fallback pattern via
  [`collectorkit.envfile`](../shared/collectorkit/collectorkit/envfile.py)
  `resolve_credential`, never the `--x-env NAME` indirection.

A `--check` mode probes the stored session without a new MFA push. It never
merely asserts that the credential is *present*. A key that exists but is
rejected is exactly what it exists to catch. So it makes the cheapest
authenticated call the source allows and maps the answer onto its exit code.
Where a static credential is the session (fred's API key, ubs-psn's RSA
key), that call is the cheapest authenticated read. schwab-api reports
its token file's age. It then refreshes the access token from that file
the way `download` does and calls `/accounts/accountNumbers`. It opens
no browser and fires no MFA. Five collectors differ:

- **fidelity-web** has no session to probe. Its session lasts as long as
  the browser, so its `login` is a no-op that keeps an orchestrator's
  `login → download → load` from failing.
- **schwab-web** has no session to probe either. `download` signs in and
  keeps nothing for a later run, so its `login` is the same no-op.
- **amex** spends a sign-in on its cheapest authenticated call, and
  sign-ins are its scarce resource. Its `--check` reads the device-trust
  cookie from the profile's Firefox jar on disk instead. It opens no
  browser and touches no network. Exit `0` means *this device is
  registered*, not *the credential is alive*. It cannot see a rotated
  password or a trust revocation made server-side; only a `download`
  sign-in can.
- **curves** holds no credential. **refdata** holds one: FINRA's API
  client, for its `finra` provider. Its wrapper sources it from
  `<secrets>/finra.env`. Their `--check` asks each source for its
  cheapest answer and maps it onto the exit code. For `finra` that
  answer is a token, and no data is asked.

The full authentication policy is in root [AGENTS.md](../AGENTS.md) §3.

#### Browser launches

A collector that drives a browser keeps its session in a persistent profile
dir (`<secrets>/<name>-profile`, Camoufox/Firefox) or an ephemeral context
plus a `storage_state` JSON file (Chromium). Either way the launch options
come from
[`collectorkit.launch`](../shared/collectorkit/collectorkit/launch.py) —
never a hand-rolled pref dict or arg list:

- **Camoufox / Playwright Firefox** — `firefox_user_prefs=launch.firefox_prefs()`
- **Playwright Chromium** — `args=launch.chromium_args(<collector's own flags>)`
- **A stock browser binary started from an `entrypoint.sh`** takes neither.
  It reads `<profile>/user.js`, so the profile is seeded from
  `launch.firefox_user_js(**overrides)` by a small Python seeder the
  entrypoint calls (angellist's `fxprofile.py` is the reference) — prefs
  never belong in the shell script.

The shared set disables the disk cache, history, favicons, telemetry
persistence, and the password manager, keeping the profile down to session
state (cookies, keys, certs, prefs) rather than an unbounded cache of
authenticated responses. The one regenerable cache no pref switches off,
Firefox's build-scoped `startupCache/`, is not disabled (that would slow
every launch) but relocated: `launch.prepare_profile_dir(<profile>)` — the
one call every persistent-profile prep site makes — symlinks it out to a
per-profile dir under the cache root, so the profile under `~/.secrets`
holds no cache bytes. The docker wrappers mount that cache root at
`/cache/startupcache`, so the relocated bytes land on the host; the
in-profile symlink there reads as dangling on macOS (host and container
paths differ) — a pointer, not data. A profile signed into by hand
(angellist's) overrides the password manager back on, so Firefox autofills
the saved login. The in-memory cache stays on, and IndexedDB (`storage/`)
is deliberately untouched — SPAs keep real session state there. None of it
is observable to web content, so it is safe on the stealth paths. Debugging
uses `--trace`, which is a richer artefact than a cache blob store.
A launch site that skips the helper — including an entrypoint that
hand-writes prefs — fails `make test-collectorkit`.

### download.py — bronze

`download.py` writes raw artefacts, exactly as the source returns them,
into a fresh UTC-stamped run dir under `--bronze-dir` (the resolved data dir),
via [`collectorkit.bronze`](../shared/collectorkit/collectorkit/bronze.py)
(`ts_slug` / `run_dir` for the directory; `atomic_write_bytes` /
`atomic_write_json` so an interrupted run leaves no half-written file).
The shared window flag comes from
[`collectorkit.cli`](../shared/collectorkit/collectorkit/cli.py)
(`add_standard_args(verb="download")` + `resolve_lookback`). A `--dry-run` mode walks the
source but exports nothing; where the collector keeps no session between
runs, that walk signs in first.

### load.py — silver

`load.py` parses each bronze run into source-shaped tables. Silver is the
**input contract to gold**: its columns follow the source's shape, not
gold's, and are documented in the collector README.

A loader states what is its own in a
[`collectorkit.loader`](../shared/collectorkit/collectorkit/loader.py)
`LoaderSpec`: its flags, its migrations, how one run becomes rows, and
the passes around the runs. `loader.main` does the rest:

- It opens silver and applies the migrations. A migration is an
  `NNNN_*.sql` file. Each one ends by inserting its own version into
  `schema_meta`.
- It loads each run that silver has not loaded. Each run is one
  transaction, and its `dump_runs` row is written last. A run that fails
  leaves no rows behind, and the next load reads it again.
- A load that finds no new run changes nothing.
- `--force` builds a fresh silver beside the old one from every run. It
  puts the new one in place only when the load succeeds
  ([`collectorkit.rebuild`](../shared/collectorkit/collectorkit/rebuild.py)).
- `--force` reads only the bronze that exists. The rows of a loaded run
  whose run dir is gone, a pruned one say, are not in the new silver. So
  a warning names each such run before the rebuild starts
  (`rebuild.runs_without_bronze`).

A loader that is not on the frame uses the same parts of
[`collectorkit.silver`](../shared/collectorkit/collectorkit/silver.py)
itself: `open_db`, `apply_migrations`, one `transaction` per run, and
`reset` for `--force`.

An error inside a run ends the load. A load also stops on purpose
(`rebuild.stop`) for a silver newer than the code, or for an invariant
the loader checks. Silver stays as it was. A new column that only bronze
can fill for old rows does not stop a load. It adds an entry to the
loader's `REBUILD_ADVICE` (`rebuild.rebuild_advice`), which asks for one
`load --force`. The advice also counts the loaded runs whose bronze is
gone, whose rows that rebuild drops.

Rows parsed from a document carry their document. Each scope records the
parser generation that derived each document
([`collectorkit.derivations`](../shared/collectorkit/collectorkit/derivations.py)).
So a load parses only new documents, and a parser change re-derives its
own scope. Each parse is cached by the document's hash and the parser's
fingerprint
([`collectorkit.parsecache`](../shared/collectorkit/collectorkit/parsecache.py)),
so `load --force` reads a parsed document from the cache.

**Cost basis** is stated as printed, in columns rather than only in the
payload, so a reader needs no JSON. That covers a holding's cost basis,
average cost, unrealized gain and acquisition date, and a trade's
quantity, price and fees. Lots have two tables:

- `open_lots` holds the lots of a holding at a snapshot;
- `closed_lots` holds realized lots, one row per lot a tax form, a
  statement or a closed-positions page prints, tagged with its
  `document_kind`.

Both use schwab-web's column names and values for the figures they share
([schwab-web/DESIGN.md §9](schwab-web/DESIGN.md#9-cost-basis-and-tax-lots)).
A lot that two documents print stays in silver twice, once per document.

A loader extracts a document once per engine with
[`collectorkit.extract`](../shared/collectorkit/collectorkit/extract.py)
(`pdfium`, `pdfplumber`, `pdftotext_layout`, `pdftotext_bbox`).
`extract.cached` keys the result on the document and the engine. The
parser reads the pages and words as a
[`collectorkit.words`](../shared/collectorkit/collectorkit/words.py)
`Extracted`, and never imports `extract`. So the two change apart:

- a parser change parses the cached text again and extracts nothing;
- an engine change moves the extraction key, so each document is
  extracted and parsed again;
- the parser's fingerprint covers its own code only (`python=False`),
  and its generation adds the extraction key.

Some parsers extract for themselves, through the whole-document readers
[`collectorkit.pdf`](../shared/collectorkit/collectorkit/pdf.py)
(pypdfium, pdfplumber, OCR) and
[`collectorkit.pdftotext`](../shared/collectorkit/collectorkit/pdftotext.py)
(poppler's `pdftotext -layout`).

Other pieces that more than one source shares:

- [`collectorkit.money`](../shared/collectorkit/collectorkit/money.py):
  an amount in its display form (`$1,234.56`, `(12.50)`);
- [`collectorkit.statement_period`](../shared/collectorkit/collectorkit/statement_period.py):
  a period printed as two month-name dates;
- [`collectorkit.statement`](../shared/collectorkit/collectorkit/statement.py):
  a card statement's summary amounts, its row dates, and the checks
  that its figures add up;
- [`collectorkit.channels`](../shared/collectorkit/collectorkit/channels.py):
  where an export's rows end and a statement's begin, and the one-to-one
  match of rows two channels both carry;
- [`collectorkit.k1`](../shared/collectorkit/collectorkit/k1.py): the
  face page of a Schedule K-1;
- [`collectorkit.series`](../shared/collectorkit/collectorkit/series.py):
  the loader of a collector of dated series;
- [`collectorkit.parser_cli`](../shared/collectorkit/collectorkit/parser_cli.py):
  the command line a statement parser offers on its own
  (`python pdf_parsers.py FILE.pdf [--json-out PATH]`, one JSON array of
  what each file yields).

A source that prints money or dates another way keeps its own reader
beside its parser.

A parser's fingerprint (`collectorkit.srcfp`) covers every collectorkit
module it imports and the package `__init__`, so a code change to one of
them re-parses the documents of every collector whose parser imports it.
A shared helper therefore gets a module of its own and is never
re-exported from `__init__`.

Two tools prove that a change keeps what silver holds. Both print names,
counts and file hashes, never a value; `--help` shows their flags.

- [`parse_diff.py`](../shared/collectorkit/tools/parse_diff.py) runs a
  parser function from two checkouts over every stored document and
  names the documents whose result changes. Run it before a change
  inside a parser closure.
- [`silver_diff.py`](../shared/collectorkit/tools/silver_diff.py)
  compares two silvers by table, row and column counts. Run it after a
  loader change, over silvers built from the same bronze.

```sh
python3 -I shared/collectorkit/tools/parse_diff.py \
    --old ../wealthdb-base/collectors/svb --new collectors/svb \
    --bronze "$BRONZE" --python collectors/svb/.venv/bin/python \
    pdf_parsers_svbdep:parse_svbdep_statement_pdf
python3 -I shared/collectorkit/tools/silver_diff.py old/svb.db new/svb.db
```

Two more prove that a restructuring kept the code and the calls:

- [`move_check.py`](../shared/collectorkit/tools/move_check.py)
  `OLD NEW...` checks that a module split moved every function and
  class unchanged.
- [`collectorkit.testing`](../shared/collectorkit/collectorkit/testing.py)
  records the calls a browser walk makes. A trace stored as a fixture
  pins their order. `COLLECTORKIT_RECORD_TRACES=1` writes the fixtures
  anew after an intended change to a walk.

### Debug artefacts, run status, and pruning bronze

A bronze run dir holds **only what `load` reads**, plus the run's
manifest (`run.json`, and any always-on provenance sidecar the
collector's own docs name — e.g. ubs-psn's `listing.json`, the pre-pull
SFTP listing). Diagnostics a run
writes for troubleshooting — screenshots, HTML/DOM dumps, Playwright
traces, failure captures — are not `load` inputs and are governed by one
uniform convention:

- **`--debug` (default off).** No debug artefact lands in a bronze run
  dir unless `download` (or `login`) is passed `--debug`. Left on for
  every nightly run, per-landmark captures dwarf the structured data and
  become the bulk of the tree; off by default, a routine dump writes only
  what `load` reads. A collector's `--explore` (where it has one) implies
  `--debug`. External diagnostics that already write **outside** bronze
  (`--screenshot-dir`, `--trace` into a `/debug` mount) keep their own
  flags — the invariant is only that nothing debug-related lands in a
  bronze run dir uninvited. `prune` reclaims that external dir too (see
  below), so opting into a trace still costs nothing permanently.

- **Captures carry credentials, and one class of them cannot be
  cleaned.** Everything a harness writes itself goes through
  [`collectorkit.debugcap`](../shared/collectorkit/collectorkit/debugcap.py):
  `secret_redactor(username, password)` masks every wire spelling of a
  credential the harness knows — raw, percent-encoded (either hex case),
  JSON-escaped, and the HTML-entity spellings a serialized DOM carries.
  A base64 or other opaque re-encoding is the spelling it cannot
  recognise, and it does not pretend to. `scrub_dom()` blanks password
  inputs out of a serialized DOM before it is written, because a sign-in
  form carries the typed password in a `value` attribute.
  `redact_headers()` / `redact_url()` / `redact_body()` mask a session
  cookie, a query-string token or a sign-in form's password field by
  NAME — the only way to reach a value no caller could know — and every
  harness routes its `network.jsonl` headers, URLs and POST bodies
  through them. A run holding no password, only a lifted session, builds
  its mask from the jar instead (`session_redactor()`, or `SessionMask`
  where the jar belongs to a persistent browser profile): that session is
  what such a run could leak, and an SPA prints it into the markup a
  capture serialises. The collectorkit suite pins each of these on the
  BEHAVIOUR rather than on a file name — a module that serialises a page
  fails `make test-collectorkit` without `scrub_dom()`, a call to
  `capture_page()` fails without a `redact=`, and a harness that records
  a HAR fails without `redact_har()`, with no per-collector exemptions.
  Playwright writes `network.har` for itself, and writes it whole: the
  sign-in POST as text *and* as parsed params, every header, the cookie
  jar. Nothing passed at record time narrows that, so the file is
  rewritten once the context close has flushed it — every harness calls
  `debugcap.redact_har()`, registered on the exit path so a walk that
  raised is cleaned too. A base64 response body is the one part dropped
  rather than masked there, because no value-based pass can see inside
  it and leaving it would put a whole body in a file that reads as
  redacted. A trace is the one artefact nothing rewrites:
  `trace.zip` / `trace-chunks/` are opt-in everywhere and a zip of
  driver-written blobs, and a trace cannot be redacted after the fact —
  its DOM snapshots store every input's value. A path that types a
  credential therefore records its sign-in with snapshots off, and a
  trace is handled as holding a cleartext credential either way.

- **`run.json` `status` lifecycle.** A `download` writes
  `{"status": "in-progress"}` when it creates the run dir, then
  atomically overwrites `run.json` with the terminal manifest carrying
  `"status": "complete"` (or `"dry-run"`) at the end.
  `collectorkit.bronze.open_run` writes both manifests, and a new
  download uses it. So a run dir is a
  **complete** dump (`status == "complete"`) or a **non-complete** one (an
  `in-progress` marker from a crashed walk, a `dry-run` shell, or no
  `run.json` at all). A manifest with no `status` field is classified by
  each collector from its own terminal signal: the manifest itself, or a
  terminal artefact where the collector writes no `run.json`. `load`
  takes a run whose status is `complete` or absent and skips every other
  status (`collectorkit.bronze.classify_run`; `prune` applies the same
  function). `prune` reclaims the runs `load` skips. Two collectors depart from this rule on
  purpose:
  - **ubs-psn** loads every zip in a run dir, whatever the run's
    status, and its `prune` never deletes a run dir that holds a zip.
    UBS deletes a queue file the moment it is fetched, so the zips of a
    crashed download are the only copy.
  - **ubs-web** has a second loadable status, `partial`. A `download
    --only PASS` run covers one pass and finalises as `partial`. `load`
    reads it, and `prune` keeps it.

- **`prune`** deletes, across every timestamped run dir under the bronze
  root: the collector's nominated **debug-artefact subdirs from complete
  dumps**, and **whole non-complete dumps**. It runs host-side (like
  `load`), previews with `--dry-run`, and guards in-flight downloads with
  `--min-age-hours` (default 1) keyed on the newest write in the dir, so a
  multi-hour backfill whose slug is old but whose files are fresh is
  protected.

- **`prune --debug-dir`** extends the same reclaim to the **host-side
  debug cache**: the `/debug` mount source (`~/.cache/wealthdb/debug/<source>`, or
  `${PREFIX}_DEBUG_DIR`) that `--screenshot-dir` / `--trace` write to.
  That dir lives outside bronze, holds no `load` input, and nothing else
  reclaims it — left alone it grows for the life of the checkout. Each
  entry directly under it (a loose screenshot, a whole Playwright trace
  bundle) is removed once quiet for `--min-age-hours`, so the captures of
  a login happening right now survive. The wrappers pass the flag for the
  collectors that have a debug dir (`HAS_DEBUG=1`); a dir that was never
  written is a no-op, since debug output is opt-in and a routine run
  leaves none. A `--debug-dir` overlapping the bronze tree is refused —
  the cache is reclaimed with no completeness check, which is right for a
  cache and would be fatal for a run dir.

The safety envelope is **identical everywhere** because it lives in one
place — [`collectorkit.prune`](../shared/collectorkit/collectorkit/prune.py),
a reviewed, unit-tested engine. Each collector with run dirs ships a
**thin `prune.py`** whose config is `prune.spec_config(SPEC)`. That
config holds:

- the debug subdirs and globs that `collector.py` names (none for a
  collector that writes no debug artefact);
- an `is_complete(run_dir, meta)` predicate. It is the lenient rule
  (`prune.lenient_classification`) unless the collector passes its own,
  as `prune.spec_config(SPEC, is_complete)`. Its own predicate builds
  on `prune.status_classification`, which encodes the `status`
  lifecycle plus a per-collector legacy fallback.

The engine guarantees, for every collector:

- a **`load` input is never deleted** — inside bronze the only paths
  removed are the configured debug subdirs of *complete* dumps and whole
  *non-complete* run dirs; a complete dump's data is out of scope by
  construction, and the debug cache holds no `load` input at all;
- an **unreadable or corrupt manifest is UNKNOWN and never deleted** — an
  I/O error or corrupt bytes is an environmental failure, not proof of
  incompleteness, so it is skipped before the collector predicate runs;
- **symlinks are never followed or deleted**, and nothing at the bronze
  root that is not a timestamped run dir (a silver `.db`, a shared
  `manual/` tree, supplied inputs such as raiffeisen_at's `supplied/`) is
  ever touched;
- a whole-dir deletion **rechecks completeness + quiescence immediately
  before `rmtree`**, closing the window between planning and deletion.

**`wealthdb-collect dedup`** (no source) reclaims disk another way. It
makes byte-identical files share one copy, across the complete, quiescent
dumps of every collector or of one `--source`, and previews with
`--dry-run`. No content changes, but a sweep runs after `load`, never
between `download` and `load`: a containerised load can read a stale
file-share cache of a swept file. `wealthdb-collect help` describes it.

Load-only collectors have no `<UTC-ts>/` run-dir layout, and the two ship
**different on-disk shapes** (both user-visible, do not move the data):
[`manual`](manual/) keeps its CSVs flat in the data dir, while [`svb`](svb/)
takes its statement PDFs in a `<data-dir>/bronze/` subdir. Each documents a
`prune` that is a justified no-op (nothing to reclaim; the inputs are the
only copy).

### Build scaffolding

The [Makefile](../Makefile) auto-discovers collectors (immediate subdirs
of `collectors/`); **the presence of a Dockerfile decides the runtime.**

- A **Docker** collector has a `Dockerfile` + `entrypoint.sh`. The
  Dockerfile `FROM`s a shared base
  ([`shared/images/`](../shared/images/)). Each base ships
  `collectorkit` and the shared entrypoint engine at
  `/opt/entrypoint-lib.sh`. `wealthdb/base-python` is for REST and
  no-browser collectors. `wealthdb/base-playwright` adds Chromium and
  Xvfb/x11vnc, so a browser can run headed and be driven over VNC.
  `wealthdb/base-camoufox` adds a stealth-patched Firefox to that.
  `requirements.txt` is copied and `pip install`ed, and nothing more:
  the wrapper mounts the checkout over `/app`, so a run uses the
  checkout's code. A collectorkit test (`test_collector_images.py`)
  checks the rule. `entrypoint.sh` maps the subcommand to the right
  script. Each verb is one line,
  `run plain|xvfb|vnc:LABEL SCRIPT [FIXED...] -- "$@"`, from
  [`shared/images/entrypoint-lib.sh`](../shared/images/entrypoint-lib.sh).
  A `login` that folds into `download` calls `login_folds` for its
  session probe, then prints its notice.
  `make build-<name>` runs `<wrapper> build`, and the bases come from
  `make base-images`. A build is skipped when the image's
  `wealthdb.inputs` label matches a hash of its inputs and its base
  image's id. The inputs are the Dockerfile and `requirements.txt`. An
  extra argument to `build` always builds. A host without `python3`
  always builds.
- A **host-venv** collector has only `requirements.txt` (no Dockerfile).
  `make build-<name>` creates `.venv` and installs `requirements.txt`
  plus `-e shared/collectorkit`.

`make build-collectors` / `make all` builds everything.

### The Camoufox browser

`wealthdb/base-camoufox` carries one pinned build of the Camoufox
browser. `CAMOUFOX_BUILD` in
[`base-camoufox.Dockerfile`](../shared/images/base-camoufox.Dockerfile)
names it.

- The build takes the browser from a local archive when there is one:
  `${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/camoufox/camoufox-cache-<build>-lin.<arch>.tgz`.
  `<arch>` is `arm64` or `amd64`.
- Without the archive, the build downloads the browser from the
  Camoufox releases on GitHub.
- The build checks the archive against a SHA-256 checksum. The
  Dockerfile pins one per architecture, next to `CAMOUFOX_BUILD`. The
  checksum covers what the archive holds: each entry's path, the mode
  of each file and directory, each file's content and each link's
  target. Any other checksum stops the build.
- An architecture with no pinned checksum takes no archive. An archive
  of it stops the build. `make camoufox-archive` prints the checksum to
  pin.
- `make camoufox-archive` writes the archive again. It reads the browser
  from the image `wealthdb/base-camoufox:keep-<build>`. That is a
  base-camoufox built with the browser (by `make all`), kept under a
  tag of its own
  (`docker tag wealthdb/base-camoufox:latest wealthdb/base-camoufox:keep-<build>`).
  An image without the browser writes nothing. The target checks the
  new archive against the pinned checksum.
  `CAMOUFOX_KEEP=<image>` reads another image.
- The archive also holds the uBlock Origin add-on. Without the archive,
  the build downloads it with the browser. The build always downloads
  the GeoLite2 location database.
- `make all`, `make build-<name>` and `make update` build the image
  with the browser.
- `make test-<name>` and CI build base-camoufox without the browser
  (`CAMOUFOX_BROWSER=none`). The unit tests run on fixtures and never
  start a browser. A sign-in in such an image stops at once with a
  message.
- A test build never replaces a base-camoufox that carries the browser:
  it rebuilds that one with the browser.

### Unit tests

Tests live in `tests/` (or `test_*.py`); `make test-<name>` builds the
collector when its inputs changed and runs **pytest** — in-container for
Docker collectors, in the `.venv` for host ones. At minimum they cover
**bronze → silver**: a *synthetic* bronze dump (no real IDs/balances —
see root [AGENTS.md](../AGENTS.md) §4) and assertions that `load.py`
projects the expected silver rows; pure-stdlib `unittest` works too.
A test that needs a PDF draws its own with
[`collectorkit.testpdf`](../shared/collectorkit/collectorkit/testpdf.py),
which only tests import.
Each loader also runs `collectorkit.incremental.check` over its fixture
runs: the runs loaded one load at a time must build the silver that one
`load --force` builds.
`make test` / `make test-collectors` runs the whole suite, and tests
must pass before any commit. `make lint` runs ruff over every collector
with the rules in the root `ruff.toml`; it has to be clean too. The
pre-commit hook (`make hooks`) refuses a commit whose staged tree fails
the lint or the privacy check (root [AGENTS.md](../AGENTS.md) §4).
A wrapper's help text and an entrypoint's command lines are pinned in
`shared/wrappers/tests/golden/`. After a deliberate change, `make
test-wrappers UPDATE_GOLDEN=1` rewrites them; review the diff before
committing it.
Fixtures are where real data most often slips in, and no pattern
catches all of it: run the privacy review
([../.claude/skills/wealthdb-pii-review/SKILL.md](../.claude/skills/wealthdb-pii-review/SKILL.md))
over a branch before it is pushed.

### The gold adapter

A collector stops at silver. A source reaches the canonical store
through a Go **adapter** under
[`../wealthdb/internal/silver/`](../wealthdb/internal/silver/) that reads
the silver DB and emits canonical snapshot/transaction batches, mapping
into the canonical enums (`account_kind`, `tax_wrapper`,
`management_style`, `asset_class` and the transaction kind, `TxKind`)
and the transaction sign convention. The adapter registers its `silver.Source` from
`init()` via `silver.Register`, and `internal/silver/all` imports it;
several adapters also carry a design doc under
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
| [`fidelity-web`](fidelity-web/) | Fidelity web | scraped session + 2FA | hybrid: Docker (Camoufox) + host venv |
| [`chase`](chase/) | Chase retail banking (checking + savings) | scraped session + 2FA | Docker (Camoufox) |
| [`firstcitizens`](firstcitizens/) | First Citizens retail banking (checking + savings) | Q2 REST + terminal 2FA (persistent device trust) | Docker (Camoufox login + REST download) |
| [`raiffeisen_at`](raiffeisen_at/) | Austrian Raiffeisen retail banking, Mein ELBA (checking + savings) | Camoufox login + pushTAN, then REST | Docker (Camoufox login + REST fetch); bank-supplied listings in `supplied/` backfill the deep past |
| [`amex`](amex/) | American Express card portal (credit + charge cards) | scraped session + one-time passcode (login folds into `download`), then REST | Docker (Camoufox sign-in + REST fetch) |
| [`relevate`](relevate/) | Relevate / Pensexpert (Pillar 2) | REST + mTAN | Docker |
| [`viac`](viac/) | VIAC (Pillar 3a) | REST + mTAN | Docker |
| [`cointracking`](cointracking/) | Crypto aggregator | scraped session + 2FA | Docker (Camoufox) |
| [`angellist`](angellist/) | AngelList LP portal (SPVs / fund deals) | scraped session | Docker (Camoufox) |
| [`carta`](carta/) | Carta (private holdings / cap table) | scraped session | Docker (Camoufox) |
| [`equityzen`](equityzen/) | EquityZen (pre-IPO secondary SPVs) | scraped session | Docker (Camoufox) |
| [`svb`](svb/) | SVB brokerage, deposit and mortgage statements (historical sideload) | none — load-only | host venv |
| [`manual`](manual/) | Private holdings with no portal (CSV) | none — manual entry | host venv |
| [`fred`](fred/) | FRED / US Fed H.10 and H.15 (historic FX rates, Treasury yields) | API key | host venv |
| [`curves`](curves/) | ECB and SNB data portals (the euro and franc government yield curves) | none — public reads | host venv |
| [`refdata`](refdata/) | Public reference data: what each fund holds and states about itself (issuers' holdings files and fund pages, SEC N-PORT and N-MFP filings, fund reports, a local inbox), and what each security is, with each company's reported figures (SEC EDGAR, GLEIF, OpenFIGI), and the distributions of each fund and share (issuers' fund pages, SEC EDGAR, SIX, JPX, FINRA) | FINRA API client id + secret (`finra.env`) for its `finra` provider, off by default; the rest are public reads | host venv |
| [`plaid`](plaid/) | Banks, brokers and card issuers through the Plaid aggregator | Plaid app keys + one access token per linked institution (sign-in on Plaid's hosted page) | host venv |

One silver kind has no collector. `synthetic` is written by a generator,
not downloaded: [`demo/generate.py`](../demo/generate.py) writes the demo household
in it, and tests build fixtures with it
([`synthetic.md`](../wealthdb/docs/adapters/synthetic.md)).

Agent ground rules shared by every collector are in the repo-root
[AGENTS.md](../AGENTS.md); each subdirectory's `AGENTS.md` adds
only source-specific rules.
