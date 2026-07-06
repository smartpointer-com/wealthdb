# viac

A read-only scraper for [VIAC](https://viac.ch)'s Pillar-3a
customer portal at `app.viac.ch`. **Replays the auth flow and the
`/rest/web/` REST API directly from Python** — no browser, no
Xvfb, no VNC. CLI-only; the SMS mTAN is prompted on stdin during
login.

Lands per-portfolio JSON + per-document PDFs into a versioned
bronze tree, then parses them into a queryable SQLite silver
database.

Part of the **wealthdb** suite — see [the architecture
overview](../../ARCHITECTURE.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. The closest sibling is
[relevate](../relevate/) (Swiss vested-benefits, same
Airlock-shaped auth stack); this collector's own
[DESIGN.md](DESIGN.md) covers VIAC-specific decisions.

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
cd collectors/viac
./viac build         # one-time, ~30 s on first build
```

## Run

The repo ships a thin `viac` shell wrapper around `docker
run` that bind-mounts `~/.secrets` and `$XDG_DATA_HOME/wealthdb/viac` into the
container per the shared collector convention — see
[collectors/README.md](../README.md). Inside the container that
puts `viac.env` (credentials) and `viac-state.json` (cookies +
CSRF metadata) at `/secrets`, and bronze artefacts + silver DB at
`/data`.

```sh
./viac login --check                       # cheap session-alive probe; no mTAN push
./viac login                               # mints a fresh session; SMS goes to your phone
./viac download --dry-run                  # walk JSON endpoints, skip PDFs
./viac download                            # bronze dump (default tier; last 90 days)
./viac download --with-transaction-documents  # also pull the per-event TRANSACTION PDFs
./viac download --lookback 1y              # wider window (also: 1w/4w/3m/6m/2y/5y/all)
./viac load                                # parse bronze → silver SQLite
```

The shared `--since` / `--until` / `--documents-since` /
`--documents-until` flags scope the run client-side: the documents
walk only fetches PDFs whose `timestamp` falls in the window (the
full index is still written to bronze for traceability), and the
transactions filter is applied at silver-load time using the window
recorded in `run.json` (the REST endpoint always returns the full
history, so bronze stays a faithful copy). Old bronze dumps without
the window block fall through to a no-bound load.

Override the host mounts via env: `VIAC_SECRETS_DIR`, `VIAC_DATA_DIR`.

### Credentials

`login.py` reads two env vars from `~/.secrets/viac.env`:

- `VIAC_LOGIN` — your mobile number in **E.164 form including
  the country code** (e.g. `+417XXXXXXXX` for a Swiss number).
  The web UI hides the country code behind a drop-down; the
  API does not.
- `VIAC_PASSWORD` — VIAC login password.

See
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules.

## Bronze layout

```
<bronze-dir>/                            e.g. $XDG_DATA_HOME/wealthdb/viac/
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
│   │   ├── index.json                   document catalogue (one entry per document)
│   │   └── <docid>.pdf                  PDF binaries (gating below)
│   └── (manual/ ... user-uploaded artefacts, ingested by load.py)
└── viac.db                              silver SQLite (default name)
```

**Document gating** — `download.py` classifies the index by
`type` and applies the `--with-transaction-documents` flag:

- **Default**: download the non-TRANSACTION docs (statements,
  Pillar-3a Bescheinigungen, contracts, investment profiles) plus
  the `SECURITY_FUSION` TRANSACTION docs (the only place the
  old→new ISIN mapping lives).
- **`--with-transaction-documents`**: also download the per-event
  TRANSACTION PDFs (TRADE_REPORT, DIVIDEND, FEE_CHARGE, INTEREST,
  …), which are the bulk of the archive.

**Cross-run dedup** — PDFs are hard-linked from prior bronze
runs when the document number matches, so a re-run only fetches
genuinely-new documents.

## Read-only

See [CLAUDE.md §1](CLAUDE.md). VIAC's portal exposes mutation
surfaces (initiate a contribution, change strategy, change
beneficiary, request a withdrawal). This toolkit is **read-only**
and never POSTs / PUTs / DELETEs anything beyond the auth flow.
Same contract as the sibling collectors.
