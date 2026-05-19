# ubs-web-dump

A toolkit for ingesting UBS Switzerland retail e-banking data the
PSN feed does not cover: driving the UBS netbanking web UI under
Playwright to export historic account statements, custody/portfolio
statements, transaction reports, and other eDocuments, then (in
subsequent scripts) parsing the raw downloads into a queryable
SQLite silver database for downstream tools — e.g. local LLM-based
agents and the [`wealthdb`](https://github.com/ptu/wealthdb) gold
layer — to consume.

Sibling projects: [ubs-psn-dump](https://github.com/ptu/ubs-psn-dump),
[swissquote-dump](https://github.com/ptu/swissquote-dump),
[schwab-dump](https://github.com/ptu/schwab-dump). The
[DESIGN.md](https://github.com/ptu/schwab-dump/blob/main/DESIGN.md)
document in `schwab-dump` covers the shared three-layer (bronze/
silver/gold) model and the conventions reused here.

## Why this design

[`ubs-psn-dump`](https://github.com/ptu/ubs-psn-dump) already
ingests UBS Private Standard Network (PSN) SFTP feeds, which carry
the canonical trade confirmations, MT940 cash statements, MT535
holdings, FX rates, and master data. PSN is the right channel for
going-forward data. It has one inflexible limitation: **UBS only
publishes to PSN going forward from the day the agreement is
activated.** Historic data — months or years of statements,
transactions, and tax PDFs already accumulated in the customer's
netbanking archive — is not available over PSN and never will be.

The remaining channel for that historic backfill is the UBS
Switzerland retail netbanking web UI, which exposes:

- **eDocuments archive** — multi-year PDF history of account
  statements, custody/portfolio statements, tax statements,
  trade confirmations, fee notes, corporate-action notices.
- **Transaction history exports** — CSV/PDF exports of cash-account
  transactions for arbitrary date ranges.
- **Position / portfolio statements** — periodic custody-account
  PDFs.

UBS does not expose any retail-accessible read API for any of this:

- **PSD2 / Open Banking** — Switzerland is outside the EU PSD2
  regime; UBS's published APIs target External Asset Managers and
  corporate treasury (OpenWealth, OpenBanking.swiss), not retail
  customers reading their own data.
- **PSN SFTP Pull** — see above; covers going-forward only.
- **PSD2 aggregators (Plaid, TrueLayer, Tink, Powens, etc.)** — no
  meaningful UBS Switzerland retail coverage.
- **Email feeds** — UBS sends "new document available" notifications
  with no payload; the parseable detail is only inside the PDF
  behind the netbanking login.

This toolkit automates the netbanking-web channel under Playwright
so a one-off historic backfill is feasible without manually
clicking through years of archive listings.

A second-factor approval (UBS Access App push or alternative MFA
factor) is required on every fresh login. Unattended cron is
therefore impossible; this toolkit is human-triggered (one
biometric tap per fresh session) but reuses the persisted session
cookie across runs until UBS invalidates it.

## Tools

The architecture follows the `swissquote-dump` template; subcommand
names and roles are the same:

| Script | Status | Purpose |
| --- | --- | --- |
| [`login.py`](login.py) | implemented | Drive headless Chromium through the UBS Nevis login dialog and the Access App QR challenge: fill the contract number, advance through the optional "Login starten" interstitial, fetch the QR PNG from the rendered `<img>` data URL, render it both to the terminal (Unicode half-blocks; Access App scans this directly) and as an upscaled PNG (6×; for SFTP-then-scan on truly headless hosts), watch for QR rotations, poll for the post-auth URL transition (`/workbench/?login` → `/app/OQJ/<N>/ebanking/spa.html`), then persist `storageState.json` at `--state-path` (chmod 0600). `--check` validates an existing state file without a new QR push. |
| [`download.py`](download.py) | implemented | Reuse the persisted session to enumerate **cash** accounts from the homepage, then for each: export the transactions list as CSV (one file per account per window) and SWIFT MT940 enriched (one or more files per account; bisected on the 1000-trx export cap). Export `positions.csv` per portfolio (enumerated from the homepage; one CSV per `portfolioUid`). Walk the documents archive in adaptive windows (bisected on UBS's 999-row display cap) fetching each PDF via the `/api/v1/digital-banking/files/` endpoint. Writes a `run.json` manifest. Credit-card transactions are intentionally skipped — this is a wealth-management toolkit. Read-only — see [CLAUDE.md](CLAUDE.md) §1. |
| `load.py` | planned | Parse bronze artefacts into a queryable SQLite silver database using the schema in [migrations/0001_initial.sql](migrations/0001_initial.sql). Apply pending migrations on startup; load each dump atomically (row-level UPSERT on transactions, content-hash dedup for documents). Idempotent — already-loaded dumps are skipped. |

### Why both CSV and MT940?

UBS offers three transactions exports per cash account: CSV, PDF
(server-rendered, bronze-only), and SWIFT MT940 (light + enriched
variants). This toolkit captures **CSV and the enriched MT940**.
They overlap but neither is a strict superset:

- **CSV gives more per-line detail.** Full beneficiary name +
  address, payment reference (RF/SCOR), counterparty IBAN, cost
  code, four distinct timestamps (Trade date, Trade time, Booking
  date, Value date), clean UTF-8.
- **MT940 carries opening + closing balances.** The CSV does not
  expose statement balances per chunk; MT940's `:60F:` / `:62F:`
  lines do, which is useful as an integrity check.
- MT940 truncates the `:86:` narrative and uses a Latin-1-ish
  encoding (mojibake on non-ASCII names).

Card accounts only render the CSV button (SWIFT MT940 isn't
defined for card transactions), so no MT940 there.

### The two UBS export caps

UBS enforces two hidden caps that `download.py` works around by
bisecting the request window:

| Surface | Cap | Behaviour at the cap | Workaround |
| --- | --- | --- | --- |
| Documents list (`#/documents/bank-documents`) | 999 rows displayed | Banner "Not all documents are displayed right now" | `_walk_window` bisects `[--documents-since..-until]` recursively until each leaf window has < 999 docs |
| MT940 export dialog | 1000 transactions | Info-only dialog "A maximum of 1000 transactions can be exported" with no Export button | `_export_mt940_with_split` reads the rendered trx count for the current period; if > 1000, bisects in half and emits one MT940 file per leaf window. Output filename includes the window: `<kind>_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.mt940` |

Both bisections cap at `WINDOW_MAX_DEPTH = 20` and won't sub-divide
below `WINDOW_MIN_DAYS = 1`.

### Bronze layout

```
<dest>/
└── 20260518T220332Z/                                              one run = one UTC-timestamped dir
    ├── run.json                                                   manifest (accounts, windows, file inventory)
    ├── transactions/
    │   ├── cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.csv         one per account per window
    │   └── cash_<sha256-prefix>_<yyyymmdd>_<yyyymmdd>.mt940       one or more per cash account
    └── documents/
        └── <token-prefix>.pdf                                     content-addressable by UBS token
```

The `<sha256-prefix>` collapses the opaque UBS account-id token to
16 hex chars. UBS account-ids all share a ~34-char per-customer
prefix, so a naive slice would collide silently — hashing avoids it.

### Silver schema and gold-merge contract

Silver lives at `~/wealthdb/ubs-web/ubs-web.db` by default;
companion PSN silver is at `~/wealthdb/ubs.db` (from `ubs-psn-dump`).
Schema in [migrations/0001_initial.sql](migrations/0001_initial.sql);
full design notes including the per-entity gold-merge contract,
identifier conventions, IBAN ↔ PSN AcctId conversion, and the
transaction-splice strategy in [DESIGN.md](DESIGN.md).

## Container build

Playwright + Chromium + PDF tooling is heavy; running it directly
on the host pollutes the OS. This toolkit ships as a Docker image
and runs entirely inside the container.

Base image: `mcr.microsoft.com/playwright/python:v1.59.0-noble`
(Ubuntu Noble, Chromium browser binary and all OS-level deps
pre-installed at `/ms-playwright/`). The Playwright Python package
itself is installed via `requirements.txt`, version-pinned to match
the base-image tag — bump both in lockstep. The Dockerfile pins to
a specific tag so reruns are deterministic.

### Build

```sh
git clone <this repo>
cd ubs-web-dump
./ubs-web-dump build         # one-time, ~10 min on first build
```

(`./ubs-web-dump build` will fail until the Python scripts
referenced in `Dockerfile` exist — see Status above.)

### Run

The repo ships a thin `ubs-web-dump` shell wrapper around
`docker run` that mounts three host paths into the container:

| Container path | Host path (default) | Purpose |
| --- | --- | --- |
| `/secrets` | `~/.secrets` | Playwright `storageState.json`, `ubs.env` |
| `/data` | `~/wealthdb/ubs-web` | bronze artefacts + silver DB |
| `/debug` | `~/.cache/ubs-web-debug` | opt-in screenshots / Playwright traces / ad-hoc QR PNGs |

Pass any debug-flag value as `/debug/...` so debug artefacts stay
out of the bronze/silver tree.

```sh
./ubs-web-dump login --check
./ubs-web-dump login --qr-png /debug/qr.png
./ubs-web-dump download --dry-run --screenshot-dir /debug/download
./ubs-web-dump load --silver-db /data/ubs-web.db --bronze-dir /data
```

Inside the container, `login.py`, `download.py`, and `load.py`
live at `/app/`; `/secrets/` is the credential mount; `/data/` is
the bronze + silver mount; `/debug/` is the opt-in debug-artefact
mount.

Override any of the host paths via env vars:
`UBS_WEB_SECRETS_DIR`, `UBS_WEB_DATA_DIR`, `UBS_WEB_DEBUG_DIR`.

If you prefer to drive `docker run` directly, the equivalent of
`./ubs-web-dump <cmd>` is:

```sh
docker run --rm -it \
    -v ~/.secrets:/secrets \
    -v ~/wealthdb/ubs-web:/data \
    -v ~/.cache/ubs-web-debug:/debug \
    ubs-web-dump:latest \
    <cmd>.py <flags>
```

### Headless remote host

The toolkit is built to run on a headless remote Linux host (e.g.
an always-on home server or a small VPS). The user `scp`s the
bronze directory back to their workstation for analysis. Chromium
runs headless inside the container; the MFA approval happens on
the user's phone, not in any UI on the remote host.

**First-run device verification.** UBS does device fingerprinting
on retail netbanking. The first login from a new IP (i.e. the
remote host's IP) is likely to trigger an extra device-trust step
on top of the normal MFA factor. Plan to do the very first
`login.py` run with `--screenshot-dir` enabled so you can `scp` the
captured screenshots down and see what UBS is asking for.
Alternatively, do the device-trust step once via a manual browser
session on the same outbound IP (e.g. SSH port-forwarding), then
run scripted afterwards.

## Layout (planned)

```
<bronze-dir>/                       e.g. ~/wealthdb/ubs-web/
├── 20260518T210504Z/               one bronze dump per run
│   ├── transactions_<account>.csv  per-account transactions for --since..--until
│   ├── documents/
│   │   ├── <docid>.pdf             eDocuments (account/custody statements,
│   │   │                           tax PDFs, trade confirms, fee notes, ...)
│   │   └── ...
│   └── run.json                    metadata: customer ID, window bounds,
│                                   doc IDs + types seen
├── 20260519T091210Z/
│   └── ...
├── manual/                         user-uploaded bronze artefacts (e.g. PDFs
│   └── ...                         downloaded by hand before this toolkit existed)
└── ubs-web.db                      silver SQLite database (default name)
```

Bronze and silver paths are independently configurable; the layout
above is the path of least resistance for personal use.

The `manual/` directory is for bronze artefacts the user produces
out-of-band — most notably PDFs already downloaded by hand from
the netbanking archive before this toolkit existed. `load.py` will
ingest `manual/` on every run using the same dedup-by-hash
mechanism as the auto-fetched eDocuments.

## Relationship to ubs-psn-dump

Both toolkits load into UBS-shaped silver databases that the
`wealthdb` UBS adapter can consume. They do not share a silver DB
file: `ubs-psn-dump` lands in `ubs.db` (canonical PSN MT/XML),
`ubs-web-dump` lands in `ubs-web.db` (web-scraped statements +
PDF-derived rows). The wealthdb gold layer merges them at the
canonical-layer level — same as the UBS/Schwab cross-bank merge
already in place.

The two silver shapes overlap (both produce transactions, holdings,
account-level snapshots) but differ in fidelity: PSN is the
authoritative source for any date where both feeds carry the same
event; `ubs-web-dump` is the only source for historic dates before
the PSN agreement was activated. Gold-layer reconciliation rules
will be defined in a follow-up wealthdb design note.

## Session lifecycle (expected)

UBS netbanking sessions, like Swissquote's, have two layers:

- **Session cookie** — set by UBS's auth gateway after the login +
  MFA flow completes. `login.py` will persist it into
  `storageState.json`; `download.py` reuses that file directly.
  Lifetime is policy-driven and unconfirmed; expect to re-login at
  least once per working session in practice.
- **MFA gate** — UBS Access App push (or the user's configured
  fallback factor) on every fresh login. Cannot be scripted away.

So the normal rhythm is: run `login.py` once, then `download.py`
as many times as you like during the cookie's lifetime. When
`download.py` reports the session is dead, re-run `login.py`.

Concrete URLs, form selectors, MFA timing characteristics, and
eDocument listing pagination — all TBD pending real netbanking
samples.
