# viac-dump

A read-only scraper for [VIAC](https://viac.ch)'s Pillar-3a
customer portal at `app.viac.ch`. **Replays the auth flow and the
`/rest/web/` REST API directly from Python** — no browser, no
Xvfb, no VNC. CLI-only; the SMS mTAN is prompted on stdin during
login.

Lands per-portfolio JSON + per-document PDFs into a versioned
bronze tree, then (Phase 4) parses them into a queryable SQLite
silver database. Future `wealthdb` integration consumes the
silver as the `viac` adapter source.

Sibling projects:
[swissquote-dump](https://github.com/ptu/swissquote-dump),
[ubs-web-dump](https://github.com/ptu/ubs-web-dump),
[ubs-psn-dump](https://github.com/ptu/ubs-psn-dump),
[schwab-web-dump](https://github.com/ptu/schwab-web-dump),
[schwab-api-dump](https://github.com/ptu/schwab-api-dump),
[fidelity-web-dump](https://github.com/ptu/fidelity-web-dump),
[relevate-dump](https://github.com/ptu/relevate-dump) (the
closest analogue — Swiss vested-benefits, same Airlock-shaped
auth stack). The shared three-layer (bronze / silver / gold)
model is documented in
[`schwab-api-dump/DESIGN.md`](https://github.com/ptu/schwab-api-dump/blob/main/DESIGN.md);
this repo's own [DESIGN.md](DESIGN.md) covers VIAC-specific
decisions.

## Status

| Phase | Verb | Status |
| --- | --- | --- |
| Persistent session minting | `login` | implemented (pure httpx) |
| Bronze scrape | `download` | implemented (pure httpx) |
| Silver loader | `load` | implemented |

Phase 1 discovery (mapping the SPA's REST surface) ran via a
short-lived VNC-driven Playwright harness; that scaffolding has
been removed now that the API shape is locked in. If VIAC ever
rotates the CSRF cookie name or the per-endpoint `/N-N` version
suffixes, the right move is to spin up a fresh capture in a
throwaway branch rather than carry discovery scaffolding in
the main toolkit indefinitely.

## Why this design

VIAC does not expose any retail-accessible read API for personal
Pillar-3a data:

- **OpenWealth / PSD2** — both are B2B-only.
- **Aggregators (Plaid, TrueLayer, Tink, Powens, …)** — no
  meaningful coverage of Swiss Pillar-3a providers.
- **Email feeds** — VIAC sends "new document available"
  notifications with no payload.

The remaining channel is the `app.viac.ch` SPA. Discovery
showed the underlying backend is plain JSON over cookies +
double-submit-cookie CSRF — so the toolkit replays it with
`httpx` directly, no browser needed.

A 2FA approval (SMS mTAN) is required on every fresh login.
Unattended cron is therefore impossible; this toolkit is
human-triggered.

## Account composition

VIAC's `/rest/web/wealth/portfolio-inventory` returns three
arrays:

- `p3a[]` — Pillar-3a portfolios (the primary product line).
  Per-portfolio strategy / positions / fees endpoints exist.
- `pvb[]` — Pillar-2 vested-benefits portfolios. Typically
  PASSIVE state while still actively employed; the
  toolkit records them in `accounts` but doesn't fetch
  per-portfolio detail until the pvb endpoint surface is mapped.
- `inv[]` — VIAC's non-retirement investment product line.
  Empty for users who only hold retirement assets.

The first dotted segment of the portfolio number encodes the
product line: `3.*` is Pillar-3a, `2.*` is PVB, `1.*` is INV.

## Container build

The toolkit ships as a Docker image (slim Python + `httpx`)
and runs entirely inside the container.

```sh
git clone <this repo>
cd viac-dump
./viac-dump build         # one-time, ~30 s on first build
```

## Run

The repo ships a thin `viac-dump` shell wrapper around `docker
run` that mounts two host paths into the container:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/secrets` | `~/.secrets` | `viac.env` (credentials), `viac-state.json` (cookies + CSRF metadata) |
| `/data` | `~/wealthdb/viac` | bronze artefacts + silver DB |

```sh
./viac-dump login --check                       # cheap session-alive probe; no mTAN push
./viac-dump login                               # mints a fresh session; SMS goes to your phone
./viac-dump download --dry-run                  # walk JSON endpoints, skip PDFs
./viac-dump download                            # full bronze dump (default-tier PDFs only)
./viac-dump download --with-transaction-documents  # also pull the ~950 per-event TRANSACTION PDFs
./viac-dump load                                # parse bronze → silver SQLite
```

Override the host mounts via env: `VIAC_SECRETS_DIR`, `VIAC_DATA_DIR`.

### Credentials

`login.py` reads two env vars sourced from `/secrets/viac.env`:

- `VIAC_LOGIN` — your mobile number in **E.164 form including
  the country code** (e.g. `+417XXXXXXXX` for a Swiss number).
  The web UI hides the country code behind a drop-down; the
  API does not.
- `VIAC_PASSWORD` — VIAC login password.

```sh
# ~/.secrets/viac.env (chmod 600, never committed)
# Treated as a bash script (`source`d via bash). SINGLE quotes
# around values containing $/!/backtick — double quotes let
# `source` do $-expansion and silently mangle the password.
export VIAC_LOGIN=+417XXXXXXXX
export VIAC_PASSWORD='your-password-with-$pecial-chars'
```

Passwords are NEVER accepted as CLI flags
(see [CLAUDE.md §3](CLAUDE.md)).

## Bronze layout

```
<bronze-dir>/                            e.g. ~/wealthdb/viac/
├── 20260527T150000Z/                    one bronze dump per run
│   ├── run.json                         manifest: timestamp, flags, document counts
│   ├── customer.json                    /rest/web/customer/current/<N-N>
│   ├── wealth/
│   │   ├── portfolio-inventory.json     master list (p3a + pvb + inv)
│   │   ├── summary.json                 daily NAV time series across all wealth
│   │   └── allocation.json              cross-portfolio allocation breakdown
│   ├── positions/<portfolio-num>/
│   │   ├── strategy.json                strategy / risk level / custody bank
│   │   ├── assets.json                  current holdings per fund
│   │   └── fees.json                    fee config
│   ├── transactions/all.json            every transaction keyed by portfolio
│   ├── documents/
│   │   ├── index.json                   document catalogue (~1019 entries)
│   │   └── <docid>.pdf                  PDF binaries (gating below)
│   └── (manual/ ... user-uploaded artefacts, ingested by load.py)
└── viac.db                              silver SQLite (default name)
```

**Document gating** — `download.py` classifies the index by
`type` and applies the `--with-transaction-documents` flag:

- **Default**: download the ~58 non-TRANSACTION docs (statements,
  Pillar-3a Bescheinigungen, contracts, investment profiles) plus
  the ~9 `SECURITY_FUSION` TRANSACTION docs (the only place the
  old→new ISIN mapping lives).
- **`--with-transaction-documents`**: also download the ~950
  per-event TRANSACTION PDFs (TRADE_REPORT, DIVIDEND,
  FEE_CHARGE, INTEREST, …).

**Cross-run dedup** — PDFs are hard-linked from prior bronze
runs when the document number matches, so a re-run only fetches
genuinely-new documents.

## Read-only

See [CLAUDE.md §1](CLAUDE.md). VIAC's portal exposes mutation
surfaces (initiate a contribution, change strategy, change
beneficiary, request a withdrawal). This toolkit is **read-only**
and never POSTs / PUTs / DELETEs anything beyond the auth flow.
Same contract as the sibling repos.
