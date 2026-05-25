# fidelity-web-dump

A toolkit for ingesting Fidelity (USA) brokerage data by driving
the `www.fidelity.com` client UI under Camoufox (a stealth-patched
Firefox) to export portfolio positions, transaction history, and
the archived PDF document set (statements + tax forms), then (in
subsequent scripts) parsing the raw downloads into a queryable
SQLite silver database for downstream tools — e.g. local LLM-based
agents and the [`wealthdb`](https://github.com/ptu/wealthdb) gold
layer — to consume.

Sibling projects:
[schwab-api-dump](https://github.com/ptu/schwab-api-dump),
[schwab-web-dump](https://github.com/ptu/schwab-web-dump),
[ubs-psn-dump](https://github.com/ptu/ubs-psn-dump),
[ubs-web-dump](https://github.com/ptu/ubs-web-dump),
[swissquote-dump](https://github.com/ptu/swissquote-dump). See
[`schwab-api-dump/DESIGN.md`](https://github.com/ptu/schwab-api-dump/blob/main/DESIGN.md)
for the shared three-layer (bronze / silver / gold) model;
[DESIGN.md](DESIGN.md) here covers Fidelity-specific design
decisions.

## Status

Login + bronze fetch are operational; the silver loader is not
yet implemented.

| Component | Status |
| --- | --- |
| [`login.py`](login.py) | implemented (Camoufox + Akamai trust + Fidelity device-trust) |
| [`download.py`](download.py) positions | implemented (Overview + DividendView CSVs, all accounts) |
| [`download.py`](download.py) activity | implemented (consolidated CSV per date-window; preset 'Past 90 days' or Custom-tab `--since/--until` window bisected into ≤93-day chunks, clamped to Fidelity's ~4-year retention) |
| [`download.py`](download.py) documents — tax forms | implemented (multi-year via `#options-select-TimeFilter`; one click per form by unique anchor id) |
| [`download.py`](download.py) documents — statements | implemented (per-row popover → "Download as PDF" via popup-tab + `context.request`, "Download as CSV" via canonical download event; scroll-into-view + JS-click fallback for rows below the fold). |
| [`download.py`](download.py) balances | implemented as HTML capture only — no direct export; per-account values are in `data-testid$='-totalaccountvalue-label'` for silver to scrape. The actions menu's 'Create Balance Letter' is a multi-step wizard; deferred. |
| [`download.py`](download.py) performance | implemented as HTML capture only — Fidelity offers no structured export here (pure Highcharts UI + collapsible info tiles). Silver loader either scrapes return % from DOM text or accepts the gap. |
| `load.py` / silver schema | not yet implemented |
| `wealthdb` Fidelity adapter | separate repo (not yet created) |

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

Fidelity's account selector groups accounts under section labels.
Every account surfaces through the same Portfolio / Activity /
Documents pages and is dumped in one bronze run; the DAF is
auto-excluded by account-id length. Silver classifies each label into
a `portfolios.kind`; see [DESIGN.md §1.2](DESIGN.md).

## Operational model

Fidelity binds its session to the Firefox-process lifetime
(confirmed empirically; same model as Schwab). Login and scrape
must share one continuous Camoufox process. The current dev
architecture uses a trigger-file keep-alive: `./fidelity-web-dump
login` holds Camoufox open and polls a trigger file; `./fidelity-
web-dump download` writes the trigger file from the host side and
exits immediately. Once the scraping logic stabilises this will
collapse into a one-shot `download` that does login + scrape +
exit in one go.

### Build

```sh
git clone <this repo>
cd fidelity-web-dump
./fidelity-web-dump build       # one-time, ~10 min on first build
                                # (~700 MB of that is the Camoufox-
                                # patched Firefox binary fetch)
```

Base image is `mcr.microsoft.com/playwright/python:v1.59.0-noble`
(used for OS-level deps only; the bundled Playwright browsers are
unused). Camoufox brings its own Firefox at
`$HOME/.cache/camoufox/`, fetched at image-build time. The Python
`playwright` package is pinned to **1.49.0** to match Camoufox's
juggler-protocol revision — do NOT bump without also bumping
`camoufox[geoip]`.

### Run

The wrapper mounts three host paths into the container:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/secrets` | `~/.secrets` | env file (`fidelity-web.env`), Firefox profile dir |
| `/data` | `~/wealthdb/fidelity-web` | bronze artefacts + silver DB |
| `/debug` | `~/.cache/fidelity-web-debug` | opt-in screenshots / traces |

Plus the repo dir is mounted at `/app` so edits to `download.py`
on the host are picked up by the running container's keep-alive
loop on the next trigger (no rebuild needed during iteration).

Override host paths via env: `FIDELITY_WEB_SECRETS_DIR`,
`FIDELITY_WEB_DATA_DIR`, `FIDELITY_WEB_DEBUG_DIR`.

#### First-ever login (one-time VNC handoff)

For a fresh profile dir:

```sh
rm -rf ~/.secrets/fidelity-web-profile/
./fidelity-web-dump vnc-login --screenshot-dir /debug/login-$(date +%Y%m%dT%H%M%SZ) -v
```

`entrypoint.sh` prints a fresh VNC password at startup; connect
with any VNC client (macOS: Finder → ⌘K → `vnc://localhost:5900`),
click "Log in" in the Camoufox window, complete 2FA, tick
"Trust this browser" if you want subsequent logins to skip MFA.
Script auto-detects post-auth, persists the profile dir, drops
into keep-alive.

#### Subsequent logins

Once the profile dir has the trust cookies, auto-driven login
works:

```sh
./fidelity-web-dump login --screenshot-dir /debug/login-$(date +%Y%m%dT%H%M%SZ) -v
```

This will likely skip MFA entirely. If MFA does fire, you'll get
a stdin prompt for the 6-digit code.

#### Triggering a dump

In a second terminal, with the keep-alive container running:

```sh
./fidelity-web-dump download --mode all     # positions + activity + documents + balances + performance
./fidelity-web-dump download --mode positions
./fidelity-web-dump download --mode activity
./fidelity-web-dump download --mode documents
./fidelity-web-dump download --mode balances    # balances.html (no CSV export)
./fidelity-web-dump download --mode performance # HTML snapshot (no structured export)
./fidelity-web-dump download --mode activity \
  --since 2022-06-01 --until 2025-12-31    # Custom-range backfill (chunked
                                            # into ≤93-day windows, clamped
                                            # to Fidelity's ~4-year retention)
./fidelity-web-dump download --dry-run      # walk + enumerate, no artefact writes
```

The wrapper writes the trigger file and exits in <1s; the running
login container picks it up on its next 2-second poll, runs
`walk()` in-place, and writes a fresh `<bronze-dir>/<UTC-ts>/`.

Ctrl-C the keep-alive terminal when done. Camoufox flushes the
profile dir cleanly on exit.

### Credentials

`login.py` reads two env vars inside the container:

- `FIDELITY_USERNAME` — Fidelity login username (treat as sensitive).
- `FIDELITY_PASSWORD` — Fidelity login password.

The login script sources `/secrets/fidelity-web.env` automatically.

```sh
# ~/.secrets/fidelity-web.env (chmod 600, never committed)
# SINGLE quotes for values containing $/!/backtick (defeats
# host-shell $-expansion when `source` is run).
FIDELITY_USERNAME='your-username'
FIDELITY_PASSWORD='your-password'
```

### Session lifecycle

- Fidelity's idle timeout is ~15 minutes. A keep-alive container
  that goes idle for that long will get redirected to a session-
  timeout modal on the next trigger. `walk()` detects this best-
  effort and aborts the affected phase cleanly; restart the
  container to recover (MFA-less per the device-trust cookie).
- Device-trust cookie has a long lifetime (~30 days nominal);
  beyond that, MFA fires again on next login.
- Akamai trust cookie has a separate lifetime; if it expires, the
  next scripted login may get bot-blocked and need another
  one-shot VNC handoff to re-seed.
- `login --check` is mostly diagnostic — it'll report DEAD on a
  profile that just completed a successful login if Camoufox was
  closed in between (session ≠ profile-dir cookies, per §5 of
  [DESIGN.md](DESIGN.md)).

## Bronze layout

```
<bronze-dir>/                              e.g. ~/wealthdb/fidelity-web/
├── 20260524T120000Z/                      one bronze dump per trigger
│   ├── run.json                           manifest: accounts, per-phase results
│   ├── positions/
│   │   ├── positions_summary.csv          Overview view (all accounts)
│   │   └── positions_dividend.csv         DividendView (all accounts)
│   ├── activity/
│   │   └── activity_<since>__<until>.csv  one CSV per date-window
│   │                                       (consolidated across accounts;
│   │                                       Account Number column inside)
│   ├── documents/
│   │   ├── <fidelity-supplied-filename>.pdf
│   │   └── …
│   ├── balances/
│   │   └── balances.html                  full-page DOM (no CSV export)
│   ├── performance/
│   │   └── performance.html               full-page DOM (no CSV export)
│   └── screenshots/
│       └── <ts>-<label>.{html,png}        per-landmark diagnostics
├── 20260525T120000Z/
│   └── …
├── manual/                                user-uploaded artefacts (documents that arrive out-of-band)
└── fidelity-web.db                        silver SQLite (default location)
```

`run.json` keys `account_dimensions` by `sha256(account_external_id)[:16]`,
so `ls` of a bronze dir + a glance at the manifest does not
expose the 9-digit Fidelity account numbers. The canonical
mapping lives inside each CSV's `Account Number` column and
in tax-form filenames (Fidelity-supplied); bronze itself is
gitignored.

The `manual/` directory is for bronze artefacts that arrive
out-of-band, outside anything Fidelity-as-custodian surfaces.
The (planned) silver loader will ingest `manual/` on every run
using the same dedup-by-hash mechanism as auto-fetched documents.

## Relationship to a hypothetical Fidelity API source

`schwab-api-dump` exists alongside `schwab-web-dump` because
Schwab publishes a retail Trader API; for Fidelity in 2026 there
is no equivalent api-side toolkit. A direct institutional feed,
if one ever materialises, would live in its own collector; The
`wealthdb` Fidelity adapter would then merge the two silvers the
same way the Schwab adapter merges api + web.

For now, this is the only Fidelity-side silver source planned.
