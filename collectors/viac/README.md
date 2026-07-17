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
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. The closest sibling is
[relevate](../relevate/) (Swiss vested-benefits, same
Airlock-shaped auth stack); this collector's own
[DESIGN.md](DESIGN.md) covers VIAC-specific decisions.

## Status

| Stage | Verb | Status |
| --- | --- | --- |
| Persistent session minting | `login` | implemented (pure httpx) |
| Bronze scrape | `download` | implemented (pure httpx) |
| Silver loader | `load` | implemented |
| Reclaim bronze disk | `prune` | implemented (non-complete dumps) |

The REST surface is mapped and stable, so the toolkit needs no
browser and carries no discovery scaffolding. If VIAC ever rotates
the CSRF cookie name or the per-endpoint `/N-N` version suffixes,
the fix is a fresh capture against the SPA in a throwaway branch
rather than discovery tooling parked in the main toolkit.

## Why this design

VIAC does not expose any retail-accessible read API for personal
Pillar-3a data:

- **OpenWealth / PSD2** — both are B2B-only.
- **Aggregators (Plaid, TrueLayer, Tink, Powens, …)** — no
  meaningful coverage of Swiss Pillar-3a providers.
- **Email feeds** — VIAC sends "new document available"
  notifications with no payload.

The remaining channel is the `app.viac.ch` SPA, whose backend is
plain JSON over cookies + double-submit-cookie CSRF — so the
toolkit replays it with `httpx` directly, no browser needed.

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
./viac download --no-transaction-documents  # skip the per-event TRANSACTION PDFs (downloaded by default)
./viac download --lookback 1y              # wider window (also: 1w/4w/3m/6m/2y/5y/all, or an ISO date)
./viac load                                # parse bronze → silver SQLite
./viac prune --dry-run                     # preview which non-complete dumps would be reclaimed
./viac prune                               # delete crashed/in-progress walks + --dry-run shells
```

The shared `--lookback` flag scopes the run client-side. It names the
window's start — a preset (`1w` / `4w` / `3m` / `6m` / `1y` / `2y` /
`5y` / `all`) or an ISO date (`YYYY-MM-DD`) — and the window runs from
there to today: the documents walk only fetches PDFs whose `timestamp`
falls in it (the full index is still written to bronze for
traceability), and the transactions filter is applied at silver-load
time using the window recorded in `run.json` (the REST endpoint always
returns the full history, so bronze stays a faithful copy). Old bronze
dumps without the window block fall through to a no-bound load.

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
│   ├── run.json                         manifest: status, timestamp, flags, document counts
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
└── viac.db                              silver SQLite (default name)
```

**Document gating** — `download.py` classifies the index by
`type` and applies the `--no-transaction-documents` opt-out:

- **Default**: download everything — the non-TRANSACTION docs
  (statements, Pillar-3a Bescheinigungen, contracts, investment
  profiles), the `SECURITY_FUSION` TRANSACTION docs (the only place
  the old→new ISIN mapping lives), and the per-event TRANSACTION
  PDFs (TRADE_REPORT, DIVIDEND, FEE_CHARGE, INTEREST, …), which are
  the bulk of the archive.
- **`--no-transaction-documents`**: skip the per-event TRANSACTION
  PDFs, keeping the non-TRANSACTION docs plus the `SECURITY_FUSION`
  TRANSACTION docs (the ISIN-mapping source is always fetched).

**Cross-run download-avoidance** — in-gate PDF fetches run through
the shared `collectorkit.docdedup` engine, keyed by document number
and chosen per document class:

- immutable, unparsed docs (contracts, investment profiles, credit
  notes, communications, and the per-event TRANSACTION receipts) are
  **hard-linked** from a prior run when the document number matches —
  the fetch is skipped (any hardlink error falls through to a real
  fetch, so a doc degrades to a fetch, never to a miss);
- the parsed `INVESTMENT_REPORTING` statements, every `TAX`
  Bescheinigung, and the data-bearing `SECURITY_FUSION` PDF are
  **fetch-verified** — always re-fetched and content-compared, so a
  byte-identical copy is deduped to a hardlink while a re-issue under a
  stable document number keeps its fresh bytes (never served stale);
  an unrecognised type fetch-verifies too (the safe default).

`--documents-force` bypasses the index entirely (fetch every in-gate
PDF, no hardlink reuse) — a first-run confidence check.

## Reclaiming disk

`download` stamps each run's `run.json` with a `status`:
`"in-progress"` when the run dir is created, atomically overwritten
with `"complete"` once the walk finishes (a `--dry-run` writes nothing
under bronze; legacy dry-run shells are still recognised and pruned). A dump is **complete** when `status == "complete"`;
everything else — an `in-progress` marker a crashed walk left behind, a
`dry-run` shell, or no `run.json` at all — is **non-complete**. Dumps
that predate the field carry a statusless manifest and are treated as
complete (it was written only at the end, so its presence alone marked
completion), except a `dry_run: true` shell, which stays non-complete.

`prune` deletes whole non-complete dumps from the bronze tree — crashed
or aborted walks and `--dry-run` shells. From a complete dump it reclaims
one thing: the `screenshots/` HTTP trace a `download --debug` left
behind, which a routine download never writes. Nothing else inside a
complete dump is touched.

```sh
./viac prune --dry-run          # print the plan; remove nothing
./viac prune                    # delete non-complete dumps
./viac prune --min-age-hours 6  # protect anything written in the last 6h
```

`load` skips a non-complete dump too (it keys on the same `status`), so
a crashed walk never leaks a partial snapshot into silver. The in-flight
guard keeps `prune` from deleting a download still in progress: it keys
on the newest write in the dir (default 1h via `--min-age-hours`), so a
multi-hour backfill whose slug is old but whose files are fresh is
protected. A complete dump's load inputs, an unreadable/corrupt
`run.json` (skipped as UNKNOWN), symlinks, and non-run entries at the
bronze root (the silver `viac.db`) are never touched. Document PDFs are
hard-linked across dumps, so deleting a non-complete dump that holds a
link is safe — the inode survives while any complete dump still links it.
After a prune, the next `load --force` rebuild reflects the removal.

## Read-only

See [CLAUDE.md §1](CLAUDE.md). VIAC's portal exposes mutation
surfaces (initiate a contribution, change strategy, change
beneficiary, request a withdrawal). This toolkit is **read-only**
and never POSTs / PUTs / DELETEs anything beyond the auth flow.
Same contract as the sibling collectors.
