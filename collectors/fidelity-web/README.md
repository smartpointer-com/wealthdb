# fidelity-web

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Fidelity with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Fidelity offers no read-only sub-scope, so nothing but this codebase's own
discipline restricts the session to reading. If malicious code were ever
introduced into this repository, its dependency chain, or the container
images it runs, it could act on your accounts with your full authority and
cause **irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Fidelity's terms of
service; verifying that your use is permitted is likewise your
responsibility.

**No warranty; no liability.** This software is provided “AS IS”, without
warranty of any kind, express or implied, including but not limited to the
implied warranties of merchantability, fitness for a particular purpose,
title, and non-infringement. To the maximum extent permitted by applicable
law, **SmartPointer AG and the contributors accept no responsibility for,
and shall not be liable for, any claim, damages, or other liability** —
whether in an action of contract, tort, or otherwise — arising from, out
of, or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds or other
assets, credential or data compromise, account suspension or termination,
and any direct, indirect, incidental, special, consequential, or punitive
damages. Your use is entirely at your own risk. See
[LICENSE](../../LICENSE) for the governing terms. This software is not
affiliated with, endorsed by, or sponsored by Fidelity or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

fidelity-web reads Fidelity (USA) brokerage data from the
`www.fidelity.com` client site. It drives Camoufox, a stealth-patched
Firefox, through the sign-in and a read-only walk. It downloads:

- portfolio positions, and the open and closed tax lots behind them;
- transaction history;
- statement and tax-form PDFs;
- the balances and performance pages, as HTML;
- a Fidelity Charitable donor-advised fund (DAF), when the login has one.

`load` parses the downloads into a SQLite silver database. The wealthdb
Fidelity adapter
([`wealthdb/internal/silver/fidelity/`](../../wealthdb/internal/silver/fidelity/))
reads that database into gold.

Part of the **wealthdb** suite. See [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold model and
[collectors/README.md](../README.md) for the shared collector
conventions. [DESIGN.md](DESIGN.md) covers the Fidelity-specific design,
and [AGENTS.md](AGENTS.md) the read-only surface.

## Why a web scraper

Fidelity's OFX Direct Connect host does not resolve. Fidelity Access
(Akoya) serves businesses only. The client site is the one channel open
to an individual.

Akamai Bot Manager guards the sign-in. Stock Playwright Chromium is
blocked at credential submit. Camoufox with a macOS fingerprint, a
geoip-matched locale and humanised cursor movement gets through. See
[DESIGN.md §1.1](DESIGN.md#11-why-a-web-scraper) and
[§6](DESIGN.md#6-anti-bot-configuration-camoufox--behavioural-mimicry).

## Sessions and MFA

- Fidelity ends a session when its Firefox process exits. So `download`
  signs in, walks, logs out and exits in one run. `login` is a no-op
  that says so.
- A fresh sign-in asks for a 6-digit code. `download` prompts for it on
  stdin.
- The run ticks "Trust this browser" at the code prompt. The profile dir
  (`~/.secrets/fidelity-web-profile/`) then skips the code for about 30
  days.
- A fresh profile dir on a new IP needs one sign-in by hand over VNC
  (`vnc-login`). Akamai wants one real human click. Its trust cookie
  then lets scripted sign-ins through. When that cookie expires, the
  next scripted sign-in can be blocked, and another `vnc-login`
  re-seeds it.
- `download --check` reports whether the stored session is alive. It
  submits no credential. A closed Firefox ends the session, so it
  usually reports DEAD.

## Build

```sh
make build-fidelity-web    # the Docker image and the host venv
make test-fidelity-web
```

fidelity-web is a hybrid collector. `download`, `vnc-login` and
`explore` run in the Docker image. `load`, `prune` and `recompress` run
on a host venv built from `requirements.txt`. `./fidelity-web build`
builds the image alone. The image builds on the shared
`wealthdb/base-camoufox`
([`shared/images/base-camoufox.Dockerfile`](../../shared/images/base-camoufox.Dockerfile)),
which pins Camoufox and the Playwright release that matches it.

## Credentials

`download.py` reads two env vars inside the container:

- `FIDELITY_WEB_USERNAME` — the Fidelity login username. Treat it as
  sensitive.
- `FIDELITY_WEB_PASSWORD` — the Fidelity login password.

The unprefixed names (`FIDELITY_USERNAME` / `FIDELITY_PASSWORD`) are
read as a fallback. Credentials go in `~/.secrets/fidelity-web.env`,
which the container reads as `/secrets/fidelity-web.env`. See
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

## Run

```sh
./fidelity-web vnc-login --mode none -v    # first sign-in on a fresh profile, by hand
./fidelity-web download                     # every phase, last 90 days
./fidelity-web download --mode positions    # positions, lots and the DAF pool snapshot
./fidelity-web download --mode activity --lookback 2022-06-01   # activity since a date
./fidelity-web download --lookback all      # everything Fidelity still serves
./fidelity-web download --check             # is the stored session alive?
./fidelity-web load                         # bronze → silver
./fidelity-web prune --dry-run              # preview the disk reclaim
```

`./fidelity-web help` lists the verbs, and `download --help` the flags.
A few behaviours worth knowing:

- `--mode` picks one phase: `positions`, `activity`, `documents`,
  `balances` or `performance`. `all` (the default) runs every phase.
  Every positions-bearing mode also reads the DAF, so there is no
  DAF-only mode
  ([DESIGN.md §12.3](DESIGN.md#123-silver--gold-model)).
- `--lookback` sets where the window starts. The default is 90 days
  back. Activity comes as one CSV per window of at most 30 days
  ([DESIGN.md §8.4](DESIGN.md#84-activity--orders)). Documents are
  filtered by year, from the year the window starts.
- `--refresh-lots` fetches the lots of every position, not only of new
  and changed ones. Each run fetches at most 200 lot tables; later runs
  fetch the rest.
- `--dry-run` still signs in and walks the site. It writes nothing to
  bronze.
- `--debug` saves an HTML and PNG capture at each landmark under
  `<run>/screenshots/`. `load` never reads them.
- `download` exits 4 when a phase came back short. The dump stays
  loadable ([DESIGN.md §2.1](DESIGN.md#21-coverage-status-and-the-exit-code--three-different-questions)).

### VNC sign-in

`vnc-login` pre-fills the credentials and waits for the human to click
"Log in" and enter the code over VNC. The wrapper picks a free port in
5900-6000 and prints it with a single-use VNC password. The wait
defaults to one hour (`--mfa-timeout`). Without `--mode none`, the walk
runs after the sign-in lands.

### Discovery sessions

`./fidelity-web explore` opens the sign-in over VNC and records a
human-driven session under the debug dir: network log with response
bodies, click log, downloads and DOM snapshots. It maps surfaces the
walk does not cover; [DESIGN.md §12](DESIGN.md#12-donor-advised-fund-extension)
records the DAF map it produced. It reuses the download profile dir.
Recording stops when the browser window closes. `download --explore`
is different: it only adds DOM inventories along the scripted walk.

### Mounts

The wrapper mounts `~/.secrets` as `/secrets` and the data dir as
`/data` ([collectors/README.md](../README.md)). It adds two more:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/debug` | `~/.cache/wealthdb/debug/fidelity-web` | opt-in screenshots and traces |
| `/app` | the collector dir | code edits take effect on the next run, with no rebuild |

`FIDELITY_WEB_SECRETS_DIR`, `FIDELITY_WEB_DATA_DIR` and
`FIDELITY_WEB_DEBUG_DIR` override the host paths.

## Output

```
$XDG_DATA_HOME/wealthdb/fidelity-web/
├── 20260524T120000Z/       one dump per download run
│   ├── run.json            config, accounts, per-phase results, coverage
│   ├── positions/          Overview and DividendView CSVs, all accounts
│   ├── lots/               the lot step's responses and index
│   ├── activity/           one CSV per date window, all accounts
│   ├── documents/          statement and tax-form PDFs
│   ├── balances/           balances page HTML
│   ├── performance/        performance page HTML
│   ├── daf/                the DAF phase's JSON, CSV exports and PDFs
│   └── screenshots/        only with --debug or --explore
├── supplied-statements/    statement PDFs supplied out-of-band
├── manual/                 hand-dropped documents; not parsed
└── fidelity-web.db         silver SQLite
```

The HTML and CSV exports and the lot step's files are zstd-compressed
as they land. PDFs, `run.json` and the DAF JSON stay raw.
[DESIGN.md §2](DESIGN.md#2-bronze-layout) has the full layout, and
[§4](DESIGN.md#4-silver-schema) the silver tables.

`load` runs on the host, so it can run while a `download` container
works. It reads every finished dump not yet in silver, plus the PDFs
under `supplied-statements/`
([DESIGN.md §4.5](DESIGN.md#45-historical-reconstruction--three-pdf-sources)).
Re-running converges. `load --force` rebuilds silver from bronze.

## Reclaiming disk

```sh
./fidelity-web prune --dry-run        # print the plan, delete nothing
./fidelity-web prune
./fidelity-web recompress --dry-run   # plan the backlog sweep
./fidelity-web recompress
./fidelity-web load --force           # silver must come out unchanged
```

`prune` removes `screenshots/` from complete dumps, whole unfinished
dumps, and old entries of the `/debug` cache. It never touches a `load`
input. See
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).

`recompress` compresses the plain HTML and CSV files that complete
dumps still hold. It rewrites `load` inputs, so it is
manual only: review the plan first, then check that `load --force`
leaves silver unchanged
([DESIGN.md §2](DESIGN.md#2-bronze-layout)).
