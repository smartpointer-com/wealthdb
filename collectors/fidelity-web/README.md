# fidelity-web

A toolkit for ingesting Fidelity (USA) brokerage data by driving
the `www.fidelity.com` client UI under Camoufox (a stealth-patched
Firefox) to export portfolio positions, transaction history, and
the archived PDF document set (statements + tax forms), then (in
subsequent scripts) parsing the raw downloads into a queryable
SQLite silver database for downstream tools to consume.

Part of the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. [DESIGN.md](DESIGN.md) here covers Fidelity-specific
design decisions.

## Status

Login, bronze fetch, and silver loader are operational.

| Component | Status |
| --- | --- |
| [`download.py`](download.py) login + logout | one-shot: Camoufox + Akamai trust + Fidelity device-trust + CLI-MFA prompt; best-effort logout before context teardown |
| [`download.py`](download.py) positions | implemented (Overview + DividendView CSVs, all accounts) |
| [`download.py`](download.py) activity | implemented (consolidated CSV per date-window; preset 'Past 90 days' or Custom-tab `--lookback` window bisected into ≤93-day chunks, clamped to Fidelity's ~4-year retention) |
| [`download.py`](download.py) documents — tax forms | implemented (multi-year via `#options-select-TimeFilter`; one click per form by unique anchor id) |
| [`download.py`](download.py) documents — statements | implemented (per-row popover → "Download as PDF" via popup-tab + `context.request`, "Download as CSV" via canonical download event; scroll-into-view + JS-click fallback for rows below the fold). |
| [`download.py`](download.py) balances | implemented as HTML capture only — no direct export; per-account values are in `data-testid$='-totalaccountvalue-label'` for silver to scrape. The actions menu's 'Create Balance Letter' is a multi-step wizard; deferred. |
| [`download.py`](download.py) performance | implemented as HTML capture only — Fidelity offers no structured export here (pure Highcharts UI + collapsible info tiles). Silver loader either scrapes return % from DOM text or accepts the gap. |
| [`load.py`](load.py) / [silver schema](migrations/0001_initial.sql) | implemented (positions + activity + documents loaders; 529 vs `trust_managed` portfolio classification; ticker-coverage validation pass). |
| [`pdf_parsers.py`](pdf_parsers.py) + [migration 0004](migrations/0004_historical_position_snapshots.sql) | implemented — 529 statement-PDF parser back-fills `historical_position_snapshots` for any quarter the statement archive covers. Accounts whose statements Fidelity does not serve (see [DESIGN.md §4.5](DESIGN.md)) are back-filled from `pdf_parsers_supplied.py` instead. |
| `wealthdb` Fidelity adapter | implemented — see [`wealthdb/internal/silver/fidelity/`](../../wealthdb/internal/silver/fidelity/) |

The current open punch list lives in [DESIGN.md §11](DESIGN.md).

## Why this design

Fidelity's OFX Direct Connect endpoint at `ofx.fidelity.com` is
**NXDOMAIN** (retired) as of 2026-05-15. The official 2026
consumer path is Fidelity Access / Akoya, which is B2B-only and
not accessible to an individual reading their own data. Web
scraping is the only realistic surface for a personal-archive
toolkit, and Fidelity's signin flow is protected by Akamai Bot
Manager — vanilla Playwright Chromium gets blocked at credential
submit. Camoufox-patched Firefox with macOS-mode fingerprinting,
geoip-derived locale, and humanised cursor trajectories gets
through. See [DESIGN.md §6](DESIGN.md) for the empirical
escalation chain.

A 2FA approval (Symantec VIP / Google Authenticator / similar
TOTP-style code) is required on every truly-fresh login. Once
the operator ticks "Trust this browser," subsequent logins from
the same Camoufox profile dir skip MFA for the duration of
Fidelity's device-trust cookie (~30 days nominal).

The first-ever login from a fresh profile dir on a new IP needs
a one-time **VNC handoff** so Akamai sees a real human-generated
mousedown/mouseup when the Log-In button is clicked. After that,
the trust cookies in the profile dir allow scripted auto-clicks
to pass. See [DESIGN.md §6](DESIGN.md) / `vnc-login` subcommand.

## Account composition

Fidelity's account selector groups accounts under section labels
(`Education` for 529 sleeves, `Authorized` for trust accounts under a third-party investment manager, `Fidelity Charitable®
Giving` for DAFs, etc.). The toolkit auto-excludes the DAF by
account-id length (Fidelity uses a shorter id for it than for
brokerage / trust / 529 accounts) and dumps everything else into
one bronze run.

Silver classifies each section label into a stable
`portfolios.kind`:

- `529` — 529 College Investing Plan participant accounts
- `trust_managed` — Trust accounts under a third-party investment manager (Fidelity-as-custodian; manager places trades)
- `other` — anything else, kept as a fall-through so future
  Fidelity labels don't need a schema migration

A trust can be a separate tax entity, so this `portfolios.kind`
split is not cosmetic. How gold consumes it is the adapter's
concern — see [the canonical model](../../DESIGN.md) and the adapter source
[`wealthdb/internal/silver/fidelity/`](../../wealthdb/internal/silver/fidelity/).

See [DESIGN.md §1.2](DESIGN.md) for the account-category model
in more depth; §1.3 for third-party managers.

## Operational model

Fidelity binds its session to the Firefox-process lifetime
(confirmed empirically; same model as Schwab). Login and scrape
share one continuous Camoufox process — `./fidelity-web
download` does login → walk → logout → exit in one shot. The
device-trust cookie in the profile dir lets subsequent runs skip
the MFA prompt for ~30 days; once it expires the next run
prompts on stdin for a fresh 6-digit code.

### Build

```sh
git clone <this repo>
cd collectors/fidelity-web
./fidelity-web build       # one-time, ~10 min on first build
                                # (~700 MB of that is the Camoufox-
                                # patched Firefox binary fetch)
```

fidelity-web is a **hybrid** collector: `download` / `vnc-login` run in
the Docker image, while `load` / `prune` / `recompress` run on a host
venv (they parse + decompress bronze in Python, no browser).
`make build-fidelity-web` builds both halves — the image and the
`.venv` (from `requirements.txt`, with `collectorkit` editable-
installed). `make test-fidelity-web` runs the suite in that venv.

Base image is `mcr.microsoft.com/playwright/python:v1.59.0-noble`
(used for OS-level deps only; the bundled Playwright browsers are
unused). Camoufox brings its own Firefox at
`$HOME/.cache/camoufox/`, fetched at image-build time. The Python
`playwright` package is pinned to **1.49.0** to match Camoufox's
juggler-protocol revision — do NOT bump without also bumping
`camoufox[geoip]`.

### Run

The wrapper drives `docker run` with the shared
`~/.secrets → /secrets` and `$XDG_DATA_HOME/wealthdb/fidelity-web → /data`
mounts (see [collectors/README.md](../README.md)). On top of
those it adds two Fidelity-specific mounts:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/debug` | `~/.cache/fidelity-web-debug` | opt-in screenshots / traces |
| `/app` | repo dir | edits to `download.py` picked up by the next spawn — no rebuild during iteration |

Override host paths via env: `FIDELITY_WEB_SECRETS_DIR`,
`FIDELITY_WEB_DATA_DIR`, `FIDELITY_WEB_DEBUG_DIR`.

#### First-ever login (one-time VNC handoff)

For a fresh profile dir on a new IP, Akamai's behavioural-detection
needs a real human click at credential submit. Use `vnc-login` to
hand off:

```sh
rm -rf ~/.secrets/fidelity-web-profile/
./fidelity-web vnc-login --mode none -v
```

`entrypoint.sh` prints a fresh single-use VNC password and the
host-side port at startup. The wrapper walks `5900-6000` to find a
free TCP port (so a prior crashed container, macOS Screen Sharing,
or another VNC server doesn't collide); whichever port lands is
printed in the banner. Tunnel + connect with any VNC client (macOS:
Finder → ⌘K → `vnc://localhost:<port>`), click "Log in" in the
Camoufox window, complete 2FA, tick "Trust this browser" if you
want subsequent runs to skip MFA. The script auto-detects post-auth
and exits; the profile dir now holds the trust cookies.

Drop the `--mode none` to also run the walk after the VNC-driven
login lands.

#### Routine dumps

Once the profile dir is seeded, every run is one-shot:

```sh
./fidelity-web download --mode all     # positions + activity + documents + balances + performance
./fidelity-web download --mode positions
./fidelity-web download --mode activity
./fidelity-web download --mode documents
./fidelity-web download --mode balances    # balances.html (no CSV export)
./fidelity-web download --mode performance # HTML snapshot (no structured export)
./fidelity-web download --mode activity \
  --lookback 2022-06-01                  # Custom-range backfill from that date to
                                          # today (chunked into ≤93-day windows,
                                          # clamped to Fidelity's ~4-year retention)
./fidelity-web download --lookback 1y    # shared window flag: a preset
                                          # (1w/4w/3m/6m/1y/2y/5y/all) or an ISO date;
                                          # also widens the documents scope
                                          # (stmts + tax-forms, at year granularity)
./fidelity-web download --dry-run      # walk + enumerate, no artefact writes
./fidelity-web download --check        # validate session, no walk
./fidelity-web download --debug        # also save per-landmark HTML+PNG captures
                                        # under <run>/screenshots/ (off by default;
                                        # diagnostic-only, prunable; --explore implies it)
```

Each invocation spins up Camoufox, logs in (auto-MFA-skip via the
device-trust cookie or stdin prompt for the 6-digit code), runs
the walk, attempts a clean logout, and exits. Bronze lands in a
fresh `<bronze-dir>/<UTC-ts>/`.

#### Loading into silver

`load.py` walks every `<bronze-dir>/<UTC-ts>/` subdir, applies
any pending schema migrations, and inserts new dumps into the
silver SQLite DB. Idempotent on the synthetic `activity_id` and
the `content_sha256` document key, so re-running converges.

```sh
./fidelity-web load                                # defaults: bronze + silver under $XDG_DATA_HOME/wealthdb/fidelity-web
./fidelity-web load --silver-db /tmp/fidelity.db  # override the silver path
./fidelity-web load -v                             # DEBUG logging
```

Runs host-side on the collector's venv (no Docker, no Camoufox), so
it can execute in parallel with a `download` container if needed. The
venv (built by `./fidelity-web build` / `make build-fidelity-web`)
carries `zstandard` — the loader decompresses the zstd-compressed
HTML/CSV bronze in Python, since the SQLite silver has no engine to
stream `.zst` natively — and `pdfplumber` for statement parsing. Bronze
is resolved by on-disk variant, so a plain (pre-compression) tree and a
`.zst` tree load to identical silver. After every run the loader
validates that positions and non-cash transactions have a ticker and
logs how many accounts each classified portfolio holds; failures are
logged as warnings.

#### Reclaiming disk

```sh
./fidelity-web prune --dry-run   # print the deletion plan, delete nothing
./fidelity-web prune             # delete it
```

`prune` removes two things across the bronze tree: `screenshots/`
from complete dumps (debug captures, written only by
`download --debug` / `--explore`; never a load input), and whole
non-complete run dirs (a `--dry-run` shell, or a walk that crashed
before writing a terminal `run.json`). Load inputs of complete
dumps are never touched, so silver stays reproducible; deleting a
non-complete dump surfaces on the next `load --force` rebuild.

It also reclaims the `/debug` cache outside bronze
(`~/.cache/fidelity-web-debug`, or `$FIDELITY_WEB_DEBUG_DIR`) — the
screenshots and traces the container writes there, which nothing else
clears out. Runs host-side like `load`, and one in-flight guard
(`--min-age-hours`, default 1, keyed on recent write activity) covers
both: it keeps `prune` from removing a download that is still running,
or the captures of one.

#### Compressing the pre-compression backlog

`download` zstd-compresses every HTML/CSV export as it lands
(`balances.html.zst`, `positions_*.csv.zst`, `activity_*.csv.zst`),
and `load` reads the `.zst` and plain forms alike (it decompresses in
Python). Run dirs written before compression existed can be converted
once with the `recompress` verb, which replaces each plain compressible
file inside a **complete** dump with a compressed twin — the original is
unlinked only after the twin has been decompressed and sha256-verified
against it, and an interrupted sweep is safe to re-run. PDFs and
`run.json` are left untouched. Unlike `prune` this rewrites load
inputs, so it is strictly manual: never schedule it, review the plan
first, and verify afterwards with `load --force` (silver must come out
identical).

```sh
./fidelity-web recompress --dry-run   # print the sweep plan, rewrite nothing
./fidelity-web recompress             # convert complete dumps, with byte accounting
./fidelity-web load --force           # convergence check: silver must be unchanged
```

### Credentials

`download.py` reads two env vars inside the container:

- `FIDELITY_USERNAME` — Fidelity login username (treat as sensitive).
- `FIDELITY_PASSWORD` — Fidelity login password.

Credentials go in `~/.secrets/fidelity-web.env` (sourced
automatically as `/secrets/fidelity-web.env` inside the container);
see [collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

### Session lifecycle

- The session lives only for the duration of one `download`
  invocation: login → walk → logout → exit. No keep-alive,
  no idle-timeout concerns.
- Device-trust cookie has a long lifetime (~30 days nominal);
  during that window subsequent runs skip the MFA prompt.
  Beyond it, MFA fires again on the next run.
- Akamai trust cookie has a separate lifetime; if it expires,
  the next scripted login may get bot-blocked and need another
  one-shot VNC handoff to re-seed.
- `download --check` is mostly diagnostic — it'll report DEAD on
  a profile dir whose Fidelity session has expired even if the
  cookies on disk look intact (session ≠ profile-dir cookies,
  per §5 of [DESIGN.md](DESIGN.md)).

## Bronze layout

```
<bronze-dir>/                              e.g. $XDG_DATA_HOME/wealthdb/fidelity-web/
├── 20260524T120000Z/                      one bronze dump per trigger
│   ├── run.json                           manifest: accounts, per-phase results (not compressed)
│   ├── positions/
│   │   ├── positions_summary.csv.zst      Overview view (all accounts)
│   │   └── positions_dividend.csv.zst     DividendView (all accounts)
│   ├── activity/
│   │   └── activity_<since>__<until>.csv.zst  one CSV per date-window
│   │                                       (consolidated across accounts;
│   │                                       Account Number column inside)
│   ├── documents/
│   │   ├── <fidelity-supplied-filename>.pdf  (PDFs never compressed)
│   │   └── …
│   ├── balances/
│   │   └── balances.html.zst              full-page DOM (no CSV export)
│   ├── performance/
│   │   └── performance.html.zst           full-page DOM (no CSV export)
│   └── screenshots/                       only with download --debug / --explore
│       └── <ts>-<label>.{html,png}        per-landmark diagnostics (prunable, not compressed)
├── 20260525T120000Z/
│   └── …
├── manual/                                user-uploaded artefacts (documents that arrive out-of-band)
└── fidelity-web.db                        silver SQLite (default location)
```

HTML/CSV load inputs are zstd-compressed in place as they land
(`.zst`; plain in pre-compression dumps — both forms load, and the
loader decompresses in Python). PDFs and `run.json` stay raw. See
[DESIGN.md](DESIGN.md) §2 for the compression + convergence contract
and the manual `recompress` backlog sweep.

`run.json` keys `account_dimensions` by `sha256(account_external_id)[:16]`,
so `ls` of a bronze dir + a glance at the manifest does not
expose the 9-digit Fidelity account numbers. The canonical
mapping lives inside each CSV's `Account Number` column and
in tax-form filenames (Fidelity-supplied); bronze itself is
gitignored.

The `manual/` directory is for bronze artefacts produced
out-of-band — most notably advisor reports from the outside
investment manager (performance attribution, fee accruals, IPS
/ mandate documentation) that Fidelity-as-custodian does not
surface. The (planned) silver loader will ingest `manual/` on
every run using the same dedup-by-hash mechanism as
auto-fetched documents.

## Relationship to a hypothetical Fidelity API source

`schwab-api` exists alongside `schwab-web` because
Schwab publishes a retail Trader API; for Fidelity in 2026 there
is no equivalent api-side toolkit. A direct institutional feed,
if one ever materialises, would live in its own collector; The
`wealthdb` Fidelity adapter would then merge the two silvers the
same way the Schwab adapter merges api + web.

For now, this is the only Fidelity-side silver source planned.
