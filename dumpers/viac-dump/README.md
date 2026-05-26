# viac-dump

A toolkit for ingesting [VIAC](https://viac.ch) Pillar-3a
retirement-account data by driving the `app.viac.ch` customer
portal under Playwright to export per-account positions,
allocations, contribution / transaction history, and the document
archive (annual statements, tax *Bescheinigungen* for Säule-3a
contributions), then parsing the raw downloads into a queryable
SQLite silver database for downstream tools — e.g. local
LLM-based agents and the
[`wealthdb`](https://github.com/ptu/wealthdb) gold layer — to
consume.

Sibling projects:
[swissquote-dump](https://github.com/ptu/swissquote-dump),
[ubs-web-dump](https://github.com/ptu/ubs-web-dump),
[ubs-psn-dump](https://github.com/ptu/ubs-psn-dump),
[schwab-web-dump](https://github.com/ptu/schwab-web-dump),
[schwab-api-dump](https://github.com/ptu/schwab-api-dump),
[fidelity-web-dump](https://github.com/ptu/fidelity-web-dump).
The shared three-layer (bronze / silver / gold) model and the
per-bank conventions reused here are documented in
[`schwab-api-dump/DESIGN.md`](https://github.com/ptu/schwab-api-dump/blob/main/DESIGN.md);
this repo's own [DESIGN.md](DESIGN.md) covers VIAC-specific
decisions.

## Status

Bootstrapping. The current commit is scaffolding only: the
container builds, the host wrapper dispatches subcommands, but
the Python scripts under `/app/` are stubs. Live work proceeds
in three phases:

| Phase | Verb | Status |
| --- | --- | --- |
| 1. VNC-driven discovery | `vnc-explore` | implemented; used to map the REST surface |
| 2. Persistent session minting | `login` | implemented (pure httpx; no browser) |
| 3. Bronze scrape | `download` | implemented (pure httpx; no browser) |
| 4. Silver loader | `load` | stub |

See [DESIGN.md §6](DESIGN.md) for the phase-by-phase plan.

## Why this design

VIAC does not expose any retail-accessible read API for personal
Pillar-3a data:

- **OpenWealth / PSD2** — both are B2B-only (External Asset
  Managers, family offices). VIAC's parent (WIR Group / Terzo)
  has no published retail aggregator path.
- **Aggregators (Plaid, TrueLayer, Tink, Powens, …)** — no
  meaningful coverage of Swiss Pillar-3a providers.
- **Email feeds** — VIAC sends "new document available"
  notifications with no payload; the parseable detail is only
  inside the PDF / JSON behind the customer-portal login.

The remaining channel is the `app.viac.ch` SPA, which surfaces
per-account positions, allocations, contribution history, and the
document archive (annual statements + the high-value Pillar-3a
contribution *Bescheinigungen* that feed the user's tax return).
This toolkit automates that channel.

A 2FA approval (mechanism TBD — in-app TOTP or push via VIAC's
mobile app, to be confirmed in Phase 1) is required on every fresh
login. Unattended cron is therefore impossible; this toolkit is
human-triggered (one biometric tap per fresh session) and reuses
the persisted session across runs until VIAC invalidates it.

## Account composition

VIAC users typically hold several Pillar-3a sub-accounts under
one login:

- One **Vorsorgekonto** (interest-bearing cash account; the
  default landing for new contributions).
- One or more **Vorsorge-Portfolios** (investment sleeves, each
  with a chosen strategy / glide-path).

The toolkit must enumerate and scrape all of them. See
[DESIGN.md §3](DESIGN.md) for the per-account-type identity
strategy and the resulting silver-schema shape.

## Container build

Playwright + Chromium + the Phase 1 VNC plumbing (Xvfb + x11vnc +
fluxbox) are heavy; running them directly on the host pollutes
the OS. This toolkit ships as a Docker image and runs entirely
inside the container.

Base image: `mcr.microsoft.com/playwright/python:v1.59.0-noble`
(Ubuntu Noble, Chromium binary and Playwright OS-level deps
pre-installed at `/ms-playwright/`). The Playwright Python package
is installed via `requirements.txt`, version-pinned to match the
base-image tag — bump both in lockstep.

### Build

```sh
git clone <this repo>
cd viac-dump
./viac-dump build         # one-time, ~3 min on first build
```

(`./viac-dump build` succeeds today but the Python scripts inside
the image are stubs — see Status above.)

### Run

The repo ships a thin `viac-dump` shell wrapper around `docker
run` that mounts three host paths into the container:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/secrets` | `~/.secrets` | `viac.env` (credentials), browser profile dir |
| `/data` | `~/wealthdb/viac` | bronze artefacts + silver DB |
| `/debug` | `~/.cache/viac-dump-debug` | opt-in screenshots / Playwright traces / Phase 1 discovery dumps |

Pass any debug-flag value as `/debug/...` so debug artefacts stay
out of the bronze / silver tree and out of `~/.secrets/`.

```sh
./viac-dump vnc-explore --discovery-dir /debug/discovery-$(date +%Y%m%dT%H%M%SZ)
./viac-dump login --check
./viac-dump login                        # explicit user opt-in required (mints fresh session)
./viac-dump download --dry-run
./viac-dump download                     # explicit user opt-in required (live scrape)
./viac-dump load --silver-db /data/viac.db --bronze-dir /data
```

Inside the container, the scripts live at `/app/`; `/secrets/` is
the credential + profile mount; `/data/` is the bronze + silver
mount; `/debug/` is the opt-in debug mount.

Override any of the host paths via env vars:
`VIAC_SECRETS_DIR`, `VIAC_DATA_DIR`, `VIAC_DEBUG_DIR`.

### Concurrency

The wrapper uses a fixed container name (`viac-dump`) and refuses
to clobber a RUNNING container — important during a Phase 1
`vnc-explore` session that may sit idle for minutes while the
operator completes 2FA on their phone. Set `VIAC_FORCE_REPLACE=1`
to override.

### Credentials

`login.py` will read two env vars inside the container:

- `VIAC_LOGIN` — VIAC username (customer-identifying; treat as
  sensitive even though it's not strictly a secret).
- `VIAC_PASSWORD` — VIAC login password.

The script sources `/secrets/viac.env` automatically.

```sh
# ~/.secrets/viac.env (chmod 600, never committed)
# Treated as a bash script (`source`d via bash). SINGLE quotes
# around values containing $/!/backtick — double quotes let `source`
# do $-expansion and silently mangle the password.
export VIAC_LOGIN=+417XXXXXXXX               # mobile number in E.164 form
                                              # (with country code). The web
                                              # UI's country drop-down isn't
                                              # part of the API call — the
                                              # country code MUST be in here.
                                              # Spaces/dashes inside the
                                              # value are stripped, so
                                              # `+41 79 XXX XX XX` works too.
export VIAC_PASSWORD='your-password-with-$pecial-chars'
```

Passwords are NEVER accepted as CLI flags
(see [CLAUDE.md §3](CLAUDE.md)).

## Bronze layout (planned)

```
<bronze-dir>/                          e.g. ~/wealthdb/viac/
├── 20260526T120000Z/                  one bronze dump per run
│   ├── run.json                       manifest: accounts seen,
│   │                                  per-phase counts, paths
│   ├── positions/
│   │   └── <account>/<...>.{json,html}
│   ├── transactions/
│   │   └── <account>/<...>.{json,html}
│   ├── documents/
│   │   └── <docid>.pdf                annual statements +
│   │                                  Pillar-3a Bescheinigungen
│   └── ...
├── manual/                            user-uploaded bronze artefacts
└── viac.db                            silver SQLite (default name)
```

Bronze and silver paths are independently configurable; the
layout above is the path of least resistance for personal use.
Phase 1 discovery dumps default to `/debug/discovery-<ts>/` and
stay outside the bronze tree.

## Configuration

A small JSON config at `$HOME/.config/viac-dump.cfg` (XDG layout,
not colocated with the data tree) carries defaults for the
bronze / debug / profile-dir paths and any default flag values.
The credentials live separately in `~/.secrets/viac.env`. The
config file is optional — every value has a CLI-flag override.

## Read-only

See [CLAUDE.md §1](CLAUDE.md). VIAC's portal exposes mutation
surfaces (initiate a contribution, change strategy, change
beneficiary, request a withdrawal). This toolkit is **read-only**.
Same contract as the sibling repos.

## Phase 1: VNC-driven discovery

Before any automation can land, the SPA's URLs, JSON endpoints,
and per-account routing need to be mapped. Phase 1 runs Chromium
under Xvfb + x11vnc + fluxbox so the operator can connect with a
VNC client, log in by hand, complete 2FA, and click through every
relevant view while `explore.py` records:

- Every request URL + method + status.
- Every response body for `application/json` (full — VIAC's SPA
  almost certainly returns clean JSON here).
- Every response body for `text/html`.
- Every DOM snapshot at the URLs the user lands on.
- Every download trigger + the resulting file.
- Screenshots at each landmark.
- Cookies + `localStorage` + `sessionStorage` at logical
  waypoints (SPAs commonly stash auth tokens in storage, so
  capturing both layers is important).

All output lands under `--discovery-dir` (no default — must be
user-provided per [CLAUDE.md §3](CLAUDE.md)), conventionally
`/debug/discovery-<UTC-ts>/` on the host.

See [DESIGN.md §6](DESIGN.md) for the phase-by-phase plan
including the human-in-the-loop timeout convention.
