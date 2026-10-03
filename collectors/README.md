# collectors

A **collector** is a self-contained toolkit that pulls one
financial source (a bank, broker, or pension provider) into the
pipeline. Each collector owns the **bronze** and **silver** layers
for its source; the gold engine under [`../wealthdb/`](../wealthdb/)
reads every collector's silver and merges them into one canonical
store. For the full bronze → silver → gold model see
[../DESIGN.md](../DESIGN.md).

This file covers what's **common** to all collectors. Each
subdirectory's own `README.md` covers only what's specific to that
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
| `login` | a session/token in `~/.secrets/` | Authenticate; usually prompts for MFA. (Omitted where the runtime mints the session inside `download` — see each tool.) |
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
| **Host venv** | a `requirements.txt`, no Dockerfile — pure-stdlib plus one thin dependency; no container | A wrapper runs the collector's `.py` under its `.venv`. |
| **Docker** | a Dockerfile + `entrypoint.sh` — browser-based scrapers run headed inside the container | A host wrapper script drives `docker run`: `./<tool> {build,login,download,load}`. |

Two collectors are **hybrid**, and a `.host-venv` marker tells the
Makefile to build and test both halves. `schwab-api`'s weekly OAuth
`login` runs in a Camoufox/VNC container (Schwab's 7-day refresh token
needs an interactive browser grant) while `download`/`load` run on the
host venv. `fidelity-web` splits the same way for a different reason:
its session lives only as long as the browser, so the container is
where a run happens and `vnc-login` seeds the profile dir, while the
file-only verbs run on the host.

## Conventions shared across collectors

**Credentials** live in `~/.secrets/<source>.env` (chmod `0600`,
never committed), sourced as a bash script — use **single quotes**
around any value containing `$`, `!`, or backticks so `source`
doesn't mangle it. Credentials reach a tool via env vars only,
never a `--password` flag. Each tool's README lists its specific
variable names. See the repo-root [AGENTS.md](../AGENTS.md) §3 for
the full authentication policy.

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
`/secrets/<source>.env` and bronze/silver at `/data`.

**Session discipline:** `login --check` probes the stored session
without a new MFA push; `download --dry-run` walks with the existing
session but exports nothing — *where a session persists*. On the one-shot
collectors, where `login` folds into `download` because no session survives
the browser (schwab-web, chase, raiffeisen_at, amex, fidelity-web — whose
`login` is an explicit no-op that says so), a dry-run signs in to
do that walk. On amex that sign-in is silent on a trusted device but comes
out of a budget small enough that a handful of them draws a captcha, so
`download --dry-run` is **not** in the run-without-asking set there
([amex/AGENTS.md](amex/AGENTS.md) §0). Agents must not mint real sessions
or run real downloads unless asked — see root [AGENTS.md](../AGENTS.md) §2.

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

### The wrapper — the `wealthdb-collect` contract

`wealthdb-collect <name> <verb> [flags]` resolves the repo and execs
`collectors/<name>/<name> <verb> [flags]` unchanged; a source appears in
`wealthdb-collect list` when `collectors/<name>/<name>` is executable.
The wrapper therefore:

- accepts the verbs **`login`**, **`download`**, **`load`**, **`prune`**
  (plus `build` and `help`; Docker wrappers also `sh`). `login` may be a
  no-op ([`ubs-psn`](ubs-psn/), key-based) or fold into `download`
  (one-shot scrapers), but the verb is still accepted so an
  orchestrator's `login → download → load` never trips. `prune` runs
  host-side like `load` (a file walk needs no container), so it can
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
  source accepts it for uniformity but warns it can't narrow its fetch,
  rather than silently ignoring or rejecting it.

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
| force reload (on `load`) | `--force` — delete the silver DB, then rebuild from all bronze | off |
| session probe | `login --check` — exit `0` = credential/session alive, nonzero = not. Every collector implements it, including the ones with no session to mint: where a static credential IS the session (fred's API key, ubs-psn's RSA key), the probe is the cheapest authenticated read against the source, so a credential that is present but rejected fails here. Two deviate. **fidelity-web** has no `login` verb to probe — its session is the browser's lifetime, so there is nothing to persist and nothing to check; the verb exists only to no-op so an orchestrator's login → download → load does not trip. **amex** (see the exceptions below) is the other, where a sign-in is the scarce resource: its `--check` reads the device-trust cookie out of the profile's own Firefox jar on disk, opening no browser and touching no network, so exit `0` means *this device is registered*, not *the credential is alive*. | — |
| human-MFA wait | `--mfa-timeout SECONDS` | ≥ 600 (swissquote and ubs-web ship 300) |
| MFA-page-appear wait | `--mfa-page-timeout SECONDS` | per source |
| login-diagnostics dir | `--screenshot-dir` (login); `--debug-dir` on an `explore` harness, and on `prune`, which reclaims that dir | None |
| `--mode` "everything" token | `all` | `all` |
| login identity env | `${PREFIX}_USERNAME` | — |
| other credentials env | under `${PREFIX}_*`, unless a share is deliberate and documented | — |
| env file | `/secrets/<source>.env` (container) / `~/.secrets/<source>.env` (host) | — |
| env-file override | `--env-file` flag **and** `${PREFIX}_ENV_FILE` env var | — |
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

**Unknown or inapplicable flags fail; they are never ignored.** The one
exception is uniformity: a flag the fleet orchestrator forwards to every
source must PARSE everywhere, so `--lookback` is accepted by every
`download` and a collector that cannot narrow its fetch warns rather than
rejecting. A wrapper declares its silver-consuming verbs with
`SILVER_SUBCOMMANDS` (default `(load)`), the same convention
`VNC_SUBCOMMANDS` uses.

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
- A collector whose source can't express the window exactly (Schwab's
  preset-driven UI, relevate's year-granularity endpoint) maps it to the
  narrowest thing that covers it, and **warns when it has to cap**.

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
  silvers whose identifier spaces are disjoint).
- **ubs-web** reads the bank-level `ubs.env` (contract number, shared with
  a future ubs-* sibling) as a legacy fallback behind its own
  `ubs-web.env`.
- **viac** `--no-transaction-documents` opts out of only the per-event
  receipt PDFs — a narrower concept than `--no-documents`; viac's document
  centre always downloads on its date window.
- **plaid** keeps one token file per linked login (a Plaid Item),
  `plaid-token-<item>.json`, instead of a single `<source>-token.json`.
  One collector serves many Items, and an access token Plaid shows only
  once is never rewritten by a later link. Its data dir holds one tree
  per Item, `<data-dir>/<item>/<UTC-ts>/`, rather than run dirs at the
  top, so `dedup` does not reach its runs. Its silver is one database
  per Item too, `<data-dir>/<item>/<item>.db`, so `load --silver-db`
  needs the one `--item` it is for. A sign-in has a verb of its own,
  `link --item NAME`: it makes a new Item, billed by Plaid, or renews
  one, so no orchestrator runs it. Its `login` opens no page. It
  settles the sign-ins a stopped `link` left open, and with none it does
  nothing, so `login → download → load` runs it safely. Its `link`,
  `login` and `download` also take `--sandbox`, which switches a run to
  Plaid's test institutions and the Items made there.
- **amex** spends its whole verb surface out of one small sign-in budget,
  so two fleet defaults are withdrawn: `login --check` reads the profile's
  device-trust cookie instead of calling the source (exit `0` = device
  registered, not credential alive), and `download --dry-run` still signs
  in — it skips the exports, not the sign-in — which takes it out of the
  run-without-asking set root [AGENTS.md](../AGENTS.md) §2 grants.

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
merely asserts the credential is *present* — a key that exists but is
rejected is exactly what it exists to catch — so it makes the cheapest
authenticated call the source allows and maps the answer onto its exit code.
The exception is amex, where the cheapest authenticated call is a sign-in
and a sign-in is the scarce resource: `--check` there reports device
registration from the profile's own cookie instead. It cannot see a rotated
password or a trust revocation made server-side; only a `download` sign-in
can. The full authentication policy is in root
[AGENTS.md](../AGENTS.md) §3.

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

`load.py` opens the DB with
[`collectorkit.silver`](../shared/collectorkit/collectorkit/silver.py)
`open_db(--silver-db)`, runs `apply_migrations(conn, migrations/)`, and
parses each bronze run into source-shaped tables. The load is
**idempotent** — already-loaded dumps are skipped (`loaded_snapshots`,
backed by a `dump_runs` table keyed by the run timestamp from
`bronze.parse_run_ts`). Migrations are `NNNN_*.sql` applied in order,
each ending by inserting its own version into `schema_meta`
(`silver_schema_version`). `open_db` leaves transactions to the caller;
`silver.transaction(conn)` makes a block atomic — one per bronze run, so a
failed load leaves no half-loaded run behind. Silver is the **input
contract to gold**: its columns follow the source's shape, not gold's, and
are documented in the collector README.

Document text comes from
[`collectorkit.pdf`](../shared/collectorkit/collectorkit/pdf.py)
(pypdfium, pdfplumber, OCR) or
[`collectorkit.pdftotext`](../shared/collectorkit/collectorkit/pdftotext.py)
(poppler's `pdftotext -layout`, for parsers that read printed columns).
Readings more than one source shares live beside them:
[`collectorkit.money`](../shared/collectorkit/collectorkit/money.py) for an
amount in its display form (`$1,234.56`, `(12.50)`),
[`collectorkit.statement_period`](../shared/collectorkit/collectorkit/statement_period.py)
for a period printed as two month-name dates, and
[`collectorkit.parser_cli`](../shared/collectorkit/collectorkit/parser_cli.py)
for the command line a statement parser offers on its own
(`python pdf_parsers.py FILE.pdf [--json-out PATH]`, one JSON array of what
each file yields). A source that prints money or dates another way keeps
its own reader beside its parser.

A parser's fingerprint (`collectorkit.srcfp`) covers every collectorkit
module it imports and the package `__init__`, so a code change to one of
them re-parses the documents of every collector whose parser imports it.
A shared helper therefore gets a module of its own and is never
re-exported from `__init__`.

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
  `"status": "complete"` (or `"dry-run"`) at the end. So a run dir is a
  **complete** dump (`status == "complete"`), a **non-complete** one (an
  `in-progress` marker from a crashed walk, a `dry-run` shell, or no
  `run.json` at all), or — for a dump that predates this field — a
  statusless manifest that each collector classifies from its original
  terminal signal (the presence of the manifest, or of a terminal
  artefact for collectors that never wrote a `run.json`).

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
a reviewed, unit-tested engine. Each collector ships a **thin `prune.py`**
that hands the engine a `PruneConfig`: the `debug_subdirs` to reclaim
(empty for collectors that write none) and an `is_complete(run_dir, meta)`
predicate (most delegate to `prune.status_classification`, which encodes
the `status` lifecycle plus a per-collector legacy fallback). The engine
guarantees, for every collector:

- a **`load` input is never deleted** — inside bronze the only paths
  removed are the configured debug subdirs of *complete* dumps and whole
  *non-complete* run dirs; a complete dump's data is out of scope by
  construction, and the debug cache holds no `load` input at all;
- an **unreadable or corrupt manifest is UNKNOWN and never deleted** — an
  I/O error or corrupt bytes is an environmental failure, not proof of
  incompleteness, so it is skipped before the collector predicate runs;
- **symlinks are never followed or deleted**, and nothing at the bronze
  root that isn't a timestamped run dir (a silver `.db`, a shared
  `manual/` tree, supplied inputs such as raiffeisen_at's `supplied/`) is
  ever touched;
- a whole-dir deletion **rechecks completeness + quiescence immediately
  before `rmtree`**, closing the window between planning and deletion.

Load-only collectors have no `<UTC-ts>/` run-dir layout, and the two ship
**different on-disk shapes** (both user-visible, don't move the data):
[`manual`](manual/) keeps its CSVs flat in the data dir, while [`svb`](svb/)
takes its statement PDFs in a `<data-dir>/bronze/` subdir. Each documents a
`prune` that is a justified no-op (nothing to reclaim; the inputs are the
only copy).

### Build scaffolding

The [Makefile](../Makefile) auto-discovers collectors (immediate subdirs
of `collectors/`); **the presence of a Dockerfile decides the runtime.**

- A **Docker** collector has a `Dockerfile` + `entrypoint.sh`. The
  Dockerfile `FROM`s a shared base
  ([`shared/images/`](../shared/images/)): `wealthdb/base-python`
  (REST/no-browser, ships `collectorkit`), `wealthdb/base-playwright`
  (Chromium, plus Xvfb/x11vnc and the shared entrypoint bootstrap so a
  browser can be run headed and driven over VNC), or
  `wealthdb/base-camoufox` (that, plus a stealth-patched Firefox).
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
[AGENTS.md](../AGENTS.md) §4) and assertions that `load.py` projects the
expected silver rows; pure-stdlib `unittest` works too. `make test` /
`make test-collectors` runs the whole suite, and tests must pass before
any commit. `make lint` runs ruff over every collector with the rules in
the root `ruff.toml`; it has to be clean too, and the pre-commit hook
(`make hooks`) refuses a commit whose staged tree it fails on.

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
| [`fidelity-web`](fidelity-web/) | Fidelity web | scraped session + 2FA | hybrid: Docker (Camoufox) + host venv |
| [`chase`](chase/) | Chase retail banking (checking + savings) | scraped session + 2FA | Docker (Camoufox) |
| [`firstcitizens`](firstcitizens/) | First Citizens retail banking (checking + savings) | Q2 REST + terminal 2FA (persistent device trust) | Docker (Camoufox login + REST download) — full pipeline through gold, validated live |
| [`raiffeisen_at`](raiffeisen_at/) | Austrian Raiffeisen retail banking, Mein ELBA (checking + savings) | Camoufox login + pushTAN, then REST | Docker (Camoufox login + REST fetch) — full pipeline through gold, validated; bank-supplied transaction listings in `supplied/` backfill the deep past |
| [`amex`](amex/) | American Express card portal (credit + charge cards) | scraped session + one-time passcode (login folds into `download`), then REST | Docker (Camoufox sign-in + REST fetch) — full pipeline through gold, validated live; `login --check` reads the profile only, `download --dry-run` still signs in |
| [`relevate`](relevate/) | Relevate / Pensexpert (Pillar 2) | REST + mTAN | Docker |
| [`viac`](viac/) | VIAC (Pillar 3a / vested benefits) | REST + mTAN | Docker |
| [`cointracking`](cointracking/) | Crypto aggregator | scraped session + 2FA | Docker (Camoufox) |
| [`angellist`](angellist/) | AngelList LP portal (SPVs / fund deals) | scraped session | Docker (Camoufox) |
| [`carta`](carta/) | Carta (private holdings / cap table) | scraped session | Docker (Camoufox) |
| [`equityzen`](equityzen/) | EquityZen (pre-IPO secondary SPVs) | scraped session | Docker (Camoufox) |
| [`svb`](svb/) | SVB brokerage, deposit and mortgage statements (historical sideload) | none — load-only | host venv |
| [`manual`](manual/) | Private holdings with no portal (CSV) | none — manual entry | host venv |
| [`fred`](fred/) | FRED / US Fed H.10 (historic FX rates) | API key | host venv |
| [`plaid`](plaid/) | Banks, brokers and card issuers through the Plaid aggregator | Plaid app keys + one access token per linked institution (sign-in on Plaid's hosted page) | host venv |

One silver kind has no collector. `synthetic` is written by a generator,
not downloaded: [`demo/generate.py`](../demo/generate.py) writes the demo household
in it, and tests build fixtures with it
([`synthetic.md`](../wealthdb/docs/adapters/synthetic.md)).

Agent ground rules shared by every collector are in the repo-root
[AGENTS.md](../AGENTS.md); each subdirectory's `AGENTS.md` adds
only source-specific rules.
