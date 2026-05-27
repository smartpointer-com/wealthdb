# relevate-dump — design (REST-only)

A read-only scraper for Relevate's customer portal at
`portal.pens-expert.ch`. **Replays the Airlock IAM authentication
flow and the `/middlelayer/v2/` REST API directly from Python** —
no browser, no Xvfb, no VNC. CLI-only; mTAN code is prompted on
stdin during login.

Lands per-portfolio JSON + per-document PDFs into a versioned
bronze tree, then parses them into a queryable SQLite silver
database. Future `wealthdb` integration consumes the silver as the
`relevate` adapter source.

Sections:

1. [How the portal works](#1-how-the-portal-works)
2. [Architecture](#2-architecture)
3. [`login.py` (REST auth)](#3-loginpy-rest-auth)
4. [`download.py` (REST dump)](#4-downloadpy-rest-dump)
5. [`load.py` (silver loader)](#5-loadpy-silver-loader)
6. [Bronze layout](#6-bronze-layout)
7. [Silver schema](#7-silver-schema)
8. [Identity strategy + wealthdb gold bridge](#8-identity-strategy--wealthdb-gold-bridge)
9. [Container + wrapper architecture](#9-container--wrapper-architecture)
10. [What we do NOT do](#10-what-we-do-not-do)
11. [Open questions](#11-open-questions)

---

## 1. How the portal works

The portal was originally mapped via a one-shot VNC-driven
Playwright harness that captured every request / response / DOM
snapshot during a manual login. The REST flow it revealed proved
stable on first run; the harness has since been removed and the
container is now slim Python + `requests`. What that discovery
taught us:

**Auth stack — Airlock IAM:**
- Vendor: Ergon Informatik's Airlock IAM (Swiss-banking-standard
  reverse-proxy gateway). Confirmed by cookies `AL_SESS-S`,
  `AL_LoginFromNewDevice`, the `CSRFT759-S` CSRF cookie, and the
  asset bundles served from `/auth/ui/assets/airlock/`.
- 2FA factor: **mTAN** (SMS code to the user's registered mobile
  number). The user confirms this; the URL `/auth/ui/app/auth/
  flow/b2c/mtan` and the API endpoint
  `/auth/rest/public/authentication/mtan/otp/check` both match.

**Auth endpoints (all under `/auth/rest/`, all serving
`application/vnd.api+json`):**

The auth flow is **three steps**, not two: `/b2c/access` is the
flow-init probe, then `/password/check` for credentials, then
`/mtan/otp/check` for the OTP.

- `POST /auth/rest/public/authentication/applications/b2c/access`
   — flow-init probe. **Empty body**, sent with `X-CSRFT759`
   header. Returns **401** with a JSON:API envelope describing
   the current flow state (`meta.nextAuthStep`,
   `errors[].code`). 401 is *expected* — it's Airlock saying
   "authenticate first".
- `POST /auth/rest/public/authentication/password/check`
   — credentials submission. Body
   `{"username": "...", "password": "..."}`. On 200, Airlock
   sends the mTAN and returns
   `data.attributes.{nextAuthStep, phoneNumber, resendPossible}`.
   `phoneNumber` is masked (12 chars) — handy to echo to the
   operator before they look at their phone.
- `POST /auth/rest/public/authentication/mtan/otp/check`
   — OTP verification. Body `{"otp": "..."}`. On 200 the
   existing `AL_SESS-S` cookie is promoted server-side from
   anonymous → authenticated. **No bearer token in the response
   body** — `data.attributes` is `{}`.
- `GET /auth/rest/protected/self-service/ui/configuration/portal`
   — post-auth probe; returns the portal target redirect tree
   (a `portalGroups[].portalTargets[]` structure). Useful as a
   session-alive landmark.
- `DELETE /auth/rest/public/authentication` — observed in the
   logout flow; returns 204.

**Authorization for application API — cookie-only, no bearer:**

The `/middlelayer/v2/` REST endpoints' `Authorization: bearer X`
header in Phase-1 was a red herring: the SPA stringifies a JS
`undefined` and sends literally `Authorization: bearer undefined`
(rest_len=9, prefix `un`, suffix `ed`). Airlock ignores it; the
real session is carried by the `AL_SESS-S` cookie alone. We
mirror the same `bearer undefined` value defensively in case
Airlock checks for header presence, but we have no token to
extract or refresh.

CSRF is **double-submit-cookie**: the `CSRFT759-S` cookie value
(22 chars, NOT httpOnly so JS can echo it) is also sent as the
`X-CSRFT759` request header on every state-changing POST. The
auth POSTs require it; the middlelayer GETs do not.

**Application API — `/middlelayer/v2/`:**

| Endpoint | Method | Notes |
|---|---|---|
| `/portfolio/investment-overview` | GET | **Master list.** All portfolios under the contact, with full metadata: `portfolios[].{id, externalId, contactId, contactGroupId, currency, currentValue, investedAmount, currentPerformance*, isActive, portfolioTypeId, portfolioStatusId, product.{id, key, name, externalId, ...}}` plus top-level `cashAmount`, `ahvStatus`, `externalContactId`. |
| `/Portfolio/{id}/deposits` | GET | Transactions / deposits. Returned `{year:null, maxDepositPerYear:null, transactions:[]}` without query params — needs a `?year=YYYY` query param (TBC). |
| `/portfolio/{id}/performance` | GET | Daily time series: `values[].{date, value, amount, cashBalance, securitiesBalance, cashFlow, deposits, payouts, profit, profitAll, profitVirtual, amountVirtual}` plus `currency`, `externalPortfolioId`, `isEntireDuration`. |
| `/portfolio/{id}/fees` | GET | Fee breakdown. |
| `/portfolio/{id}/investment/allocation` | GET | Allocation summary: `totalValuation`, `investmentValuation`, `savingValuation`. |
| `/Portfolio/proposal/{proposalId}/modelportfolio` | GET | Target model portfolio with `positions[].{security.{isin, name, assetClass, country, tradingPrice, tradingUnit, faceValue}, allocation}` and `riskCategory.{min, max, expectedReturn}`. The `proposalId` is `portfolios[].portfolioProposalId` from the overview. |
| `/documents` | GET | Document index. Returns `{documents[], maintenance}`. Each document: `{id, externalId, fileName, documentType (enum), category (enum), createDate, foundation, foundationId, validTill, dmsDocumentMetadataId, ownerId, namePreInsurer, isUploaded, sP6_SHOW_RELEVATE, externalContactId, tenantId, description}`. |
| `/document/{id}` | GET | Direct PDF fetch (`application/pdf`). The download surface. |
| `/document/generate/contract/pensionfundregulations?tId=N` | GET | On-demand contract regeneration; also returns a PDF. |
| `/portfolio/services`, `/compliance/products`, `/contact/{messages, notification, language, risk-protection}`, `/sso/{claims, sync}` | GET | Ancillary endpoints. Probe-only for now; `/contact/messages` may carry foundation announcements worth preserving in bronze. |

**Account taxonomy (observed):**

- Three portfolios visible under one login, all FZ products
  (vested benefits / Pillar 2 / Freizügigkeit):
  - `product.key = FZPF` ("PensFree") — foundation-managed
    discretionary. Two of three portfolios.
  - `product.key = FZI` ("Independent") — self-directed
    safekeeping. One of three.
- All three CHF-denominated, `isActive=true`,
  `portfolioTypeId=0`, `portfolioStatusId=0`.
- The URL `/dashboard/3a/depots` exists in the SPA, but
  `/portfolio/investment-overview` returned no Pillar 3a
  portfolios in the observed account. `download.py` still calls the
  endpoint defensively — other Relevate users may have 3a.

**Document corpus (observed):**

- 26 documents in the index. Recognisable types from the
  fileName field: Quarterly Reports, Quarterly Fee Statements,
  FZ Credit Notes (contribution arrivals), Pension Agreement,
  Pension Plan, Investor Profile, Leaving Statement.
- `documentType` is a numeric enum (observed values: 0, 1, 3, 4,
  7, 9). `category` is a separate numeric enum (0, 1, 2). The
  enum-to-name mapping isn't exposed in the responses we've
  captured; the silver loader will need to derive it (likely by
  correlating with fileName patterns).

## 2. Architecture

```
                ┌─ ~/.secrets/relevate.env    (chmod 0600)
                │     export RELEVATE_LOGIN=...
                │     export RELEVATE_PASSWORD=...
                │
                │  ┌─ ~/.secrets/relevate-state.json   (chmod 0600)
                │  │    persisted Airlock cookies (AL_SESS-S,
                │  │    CSRFT759-S, AL_LoginFromNewDevice)
                │  │
                ▼  ▼
       login.py ──────────►  Airlock IAM REST   (/b2c/access -> /password/check -> /mtan/otp/check)
                                   ▲      │
                                   │      └─ mTAN prompt on stdin
                                   │
       download.py ────────►  /middlelayer/v2/   ─► writes JSON + PDFs into
                                                     ~/wealthdb/relevate/<UTC-ts>/
                                                     (gitignored bronze tree)
                                                              │
       load.py ────────────────────────────────────────────►  ~/wealthdb/relevate/relevate.db
                                                              (SQLite silver)
                                                              │
                                                              ▼
                                          wealthdb gold (out of scope — separate repo)
```

All three Python scripts run inside the same Docker image
(`python:3.12-slim-bookworm` + `requests`). No browser, no Xvfb,
no VNC.

Stack:

- **Python `requests`** for HTTP. Synchronous; the workload is
  read-only and modest (low tens of API calls per portfolio + 26
  document GETs).
- **`requests.cookies.RequestsCookieJar`** persisted as JSON for
  the Airlock cookies (`AL_SESS-S`, `AL_LoginFromNewDevice`,
  `CSRFT759-S`).
- **No `httpx`, no async**, no scheduling library. Simpler is
  better for a tool the user invokes by hand.

## 3. `login.py` (REST auth)

### 3.1 Flow

Three steps. `/b2c/access` is the flow-initialisation probe (not
the credentials endpoint — credentials go to the separate
`/password/check`). There is **no bearer token at all**: the
SPA's `Authorization` header literally carries the string
`"bearer undefined"` (a JS `undefined` stringified). The session
is 100% cookie-based.

The script reproduces what the Angular SPA does:

1. Read `RELEVATE_LOGIN` and `RELEVATE_PASSWORD` from env (the
   wrapper sourced them from `~/.secrets/relevate.env`).
2. `GET /auth/ui/app/auth/flow/b2c/password?lang=en` — primes the
   cookie jar. Airlock sets `AL_SESS-S` (68 chars, httpOnly,
   secure), `CSRFT759-S` (22 chars, NOT httpOnly so JS can echo
   it), and `AL_LoginFromNewDevice` (185 chars, device-trust
   token).
3. `POST /auth/rest/public/authentication/applications/b2c/access`
   with empty body. Sends the `X-CSRFT759` header (value =
   `CSRFT759-S` cookie value, double-submit pattern). Returns
   HTTP **401** with a JSON:API envelope describing the current
   flow state (`meta.nextAuthStep`, `errors[].code`). The 401 is
   *expected* — it's Airlock saying "you need to authenticate".
4. `POST /auth/rest/public/authentication/password/check` with
   body `{"username": "...", "password": "..."}` and the
   `X-CSRFT759` + `X-Continue-Flow: true` headers. Returns HTTP
   200 with `data.attributes.{nextAuthStep, phoneNumber,
   resendPossible}`. Airlock has sent the mTAN to the registered
   number; the (masked) phone number comes back so we can
   surface it to the operator.
5. Display the phone number on stderr and read the OTP from
   stdin via `getpass` (input hidden). Retry on rejection up to
   `--max-otp-attempts` (default 3); on each retry the user can
   enter the next code that arrived (mTANs are usually
   single-use but Airlock typically allows multiple-in-flight).
6. `POST /auth/rest/public/authentication/mtan/otp/check` with
   body `{"otp": "..."}` and the same `X-CSRFT759` +
   `X-Continue-Flow: true` headers. Returns HTTP 200. Server-side
   the existing `AL_SESS-S` session record is promoted from
   anonymous → authenticated; **the cookie value does not change**
   (confirmed by Phase-1 cookie-length stability across the
   pre/post-auth snapshots).
7. `GET /auth/rest/protected/self-service/ui/configuration/portal`
   — landmark probe to confirm the session is in fact live
   before persisting. 200 = good.
8. Persist the cookie jar to `~/.secrets/relevate-state.json`
   (chmod 0600), atomic via `mv tmp final`:
   ```json
   {
     "minted_at": "2026-05-27T13:08:30+00:00",
     "issuer": "portal.pens-expert.ch",
     "schema_version": 1,
     "cookies": [
       {"name": "AL_SESS-S", "value": "...", "domain": "portal.pens-expert.ch",
        "path": "/", "secure": true, "expires": null, "rest": {"HttpOnly": null}},
       {"name": "CSRFT759-S", "value": "...", "domain": "portal.pens-expert.ch",
        "path": "/", "secure": true, "expires": null, "rest": {}},
       {"name": "AL_LoginFromNewDevice", "value": "...", "...": "..."}
     ]
   }
   ```
   Only the Airlock-domain cookies + the device-trust cookie are
   load-bearing; the Google Analytics cookies (`_ga`, `_gcl_au`,
   `_ga_*`) are persisted too for completeness but the API
   doesn't care about them.

No bearer extraction step. No token decoding. Just cookies.

### 3.2 CLI surface (as built)

```
login.py [--state-path PATH] [--check] [--max-otp-attempts N]
         [--verbose]
```

- `--state-path` — defaults to `/secrets/relevate-state.json`
  inside the container (host `~/.secrets/relevate-state.json`).
- `--check` — **no credentials submitted, no mTAN push.** Loads
  the existing state file, restores the cookie jar, hits the
  landmark probe, prints `ALIVE` / `DEAD` / `MISSING` on stdout.
  Exit codes: 0 (ALIVE), 1 (MISSING), 2 (DEAD). Allowed without
  user prompt (CLAUDE.md §2).
- `--max-otp-attempts` — retry budget when the user mistypes the
  OTP (default 3). The operator can pause indefinitely between
  attempts; `getpass` blocks on stdin without a deadline. This
  is the "no immediate-response interactive flows" rule honoured
  by NOT having a timeout, rather than by setting a long one.
- No `--password` flag, ever. Credentials only via env.

### 3.3 Session reuse

Airlock IAM session cookies have a sliding TTL — server-side
configuration, not exposed on the wire we observed. Most Airlock
deployments default to 30 min idle / 8 h absolute, but the
device-trust cookie (`AL_LoginFromNewDevice`) is usually much
longer (30 d nominal).

`login.py --check` is the canonical "is my session still alive"
probe. The wrapper documents that running `login.py` without
`--check` triggers a fresh mTAN — only do it on user request.

### 3.4 Custom request headers

Mirroring what the SPA sends, in order to look like a polite
browser-class consumer to Airlock:

| Header | Value | Where |
| --- | --- | --- |
| `User-Agent` | recent Chrome on macOS | every request |
| `Accept` | `application/vnd.api+json, application/json, */*` | every request |
| `Accept-Language` | `en-US,en;q=0.9` | every request |
| `Origin` | `https://portal.pens-expert.ch` | every request |
| `Referer` | login-flow URL | every request |
| `Sec-Ch-Ua*`, `Sec-Fetch-*` | matching Chrome's client hints | every request |
| `X-Same-Domain` | `1` | every request |
| `X-CSRFT759` | `CSRFT759-S` cookie value (22 chars) | every POST |
| `X-Continue-Flow` | `true` | `/password/check`, `/mtan/otp/check` |
| `Content-Type` | `application/json` | every POST that has a body |

### 3.5 What is NOT `login.py`'s job

- It does NOT call any `/middlelayer/v2/` endpoint beyond the
  bare-minimum probe (`/configuration/portal` from the auth
  rest tree, not middlelayer). Data extraction is `download.py`.
- It does NOT prompt for credentials interactively. They come
  from env. Refuse-and-exit (64) if missing.

## 4. `download.py` (REST dump)

### 4.1 Flow

1. Load `/secrets/relevate-state.json` and restore the cookie
   jar into a `requests.Session`. Missing file → exit `64`,
   print "run `./relevate-dump login` first".
2. Configure session headers to mirror the SPA: realistic Chrome
   UA, `Sec-Ch-Ua-*` client hints, `X-Same-Domain: 1`, and the
   placeholder `Authorization: bearer undefined`. No `X-CSRFT759`
   on GETs — Airlock only enforces double-submit on POSTs.
3. Probe `/auth/rest/protected/self-service/ui/configuration/portal`.
   Non-200 → exit; tell the user the session is dead.
4. Create the run dir `/data/<UTC-ts>/`. Open the
   `Manifest` (run.json writer); flush after every artefact so a
   Ctrl-C run leaves an inspectable partial manifest.
5. **Accounts phase** (run when `--mode` ∈ `{all, accounts,
   portfolios}`):
   - `GET /middlelayer/v2/portfolio/investment-overview` →
     `accounts/investment-overview.json`.
   - In `all` mode also fetch the small ancillary user-scope
     endpoints into `accounts/*.json`: `/compliance/products`,
     `/contact/messages`, `/contact/notification`,
     `/contact/risk-protection` (returns 204 No Content for
     accounts without protection set up — saved as a tiny
     `{_no_content: true, _status: 204}` marker), `/sso/claims`.
     `/portfolio/services` and `/contact/language` were dropped
     after the first real run showed them 400-ing /
     405-ing without parameters we could synthesise. Each
     endpoint is independent; real failures are recorded but
     don't abort the run.
6. **Portfolios phase** (run when `--mode` ∈ `{all, portfolios}`):
   for each `portfolios[i]` in the overview, in order:
   - Resolve the deposits-iteration policy:
     - If `--year-from` is set: iterate that explicit range
       (`--year-to` defaults to current year), saving
       `deposits-YYYY.json` per year.
     - Else if `firstInvestmentDate` is plausible (parses to
       a year ≥ 1900, not the `0001-01-01` sentinel Relevate
       uses for "unknown"): iterate from that year to current.
     - Else: single call to `/deposits` with no `?year=` param,
       saved as `deposits.json`. The first real run revealed
       that for the FZ products observed so far, the endpoint
       returns the same empty `{transactions:[]}` envelope
       regardless of `?year=YYYY` — actual transaction data
       arrives via the credit-note PDFs that the documents
       phase fetches. One call records "asked, empty" for the
       silver loader.
   - `GET /middlelayer/v2/portfolio/{id}/performance` →
     `portfolios/<slug>/performance.json`.
   - `GET /middlelayer/v2/portfolio/{id}/fees` →
     `portfolios/<slug>/fees.json`.
   - `GET /middlelayer/v2/portfolio/{id}/investment/allocation` →
     `portfolios/<slug>/investment-allocation.json`.
   - If `portfolioProposalId` is non-null:
     `GET /middlelayer/v2/Portfolio/proposal/{pid}/modelportfolio`
     → `portfolios/<slug>/modelportfolio.json`.
   - The slug is `sha256(externalId)[:16]`. The raw `externalId`
     stays inside the JSON files (under the bronze tree, which is
     gitignored); it never appears in path strings.
7. **Documents phase** (run when `--mode` ∈ `{all, documents}` and
   not `--skip-documents`):
   - `GET /middlelayer/v2/documents` → `documents/index.json`.
   - For each `documents[].id`, `GET /middlelayer/v2/document/{id}`.
     `application/pdf` 200 → `documents/<id>.pdf`. Any other
     content-type → `documents/<id>.unexpected.<ext>`. Already-
     downloaded files are skipped (idempotent within a run).
8. Final `run.json` flush. Exit 0 if no errors recorded, 2 if at
   least one endpoint failed (the run dir is still usable; the
   loader skips manifest entries flagged with errors).

Manifest shape (as written):

```json
{
  "tool": "relevate-dump.download",
  "schema_version": 1,
  "started_at": "...", "ended_at": "...",
  "mode": "all", "dry_run": false,
  "state_minted_at": "...",
  "accounts": [
    {
      "id": NNNN, "slug": "<sha256-prefix>",
      "product_key": "FZPF", "product_name": "PensFree",
      "product_offer_id": NNNN,
      "currency": "CHF", "proposal_id": NNNN,
      "is_active": true,
      "first_investment_date": "0001-01-01T00:00:00",
      "portfolio_type_id": 0, "portfolio_status_id": 0,
      "deposits_year_window": null
    }
  ],
  "documents": {
    "count_in_index": 26,
    "fetched": 26, "skipped": 0,
    "unexpected_content_type": [],
    "files": [...]
  },
  "errors": [],
  "files": ["accounts/...", "portfolios/abc123/...", ...]
}
```

### 4.2 CLI surface

```
download.py [--state-path PATH] [--dest DIR]
            [--dry-run]
            [--mode {all, accounts, portfolios, documents}]
            [--skip-documents]
            [--year-from YYYY] [--year-to YYYY]
            [--limit-portfolios N]
            [--limit-documents N]
            [--verbose]
```

Iteration discipline: the
`--mode`, `--skip-documents`, and `--limit-*` flags let an operator
re-run one phase cheaply while iterating on the silver loader or
investigating a specific portfolio's response shape.

`--dry-run` hits only the two master listing endpoints
(`investment-overview` + `documents`), records the work-list
counts in the manifest, and exits. CLAUDE.md §2 explicitly
allows running this without user prompt.

### 4.3 Idempotency + dedup

- The bronze run dir is timestamped per invocation — never
  rewritten across runs.
- Within a run, per-document fetch skips if
  `documents/<id>.pdf` already exists (defensive against a
  re-invocation with the same `--dest` on the same timestamp,
  which would only happen via an explicit `--dest` override).
- Cross-run dedup is the silver loader's job (content-hash on
  ingest).

### 4.4 Errors + retry policy

Current implementation is conservative: log + record in
`manifest.errors`, continue to the next work item. No retry
loop.

- HTTP 401 → session probably expired (the initial probe should
  have caught this; if not, it's mid-run). Record and continue —
  individual endpoints may have shorter-lived auth than the
  landmark.
- HTTP 5xx → record and continue. Relevate hasn't 5xx-ed yet;
  if it starts, a retry-with-backoff layer goes here.
- HTTP 429 → record and continue. Same: not observed; defer.
- Document GET returning HTML or `application/json` instead of
  `application/pdf` → save under `documents/<id>.unexpected.<ext>`
  and record in `manifest.documents.unexpected_content_type`.

## 5. `load.py` (silver loader)

Mechanical projection of bronze JSON into the silver SQLite
schema (§7), with migrations applied on each invocation. Idempotent
on re-runs: tracks which `dump_runs.snapshot_at` have been ingested
and short-circuits.

Out of scope here — the loader can be written against the schema
in §7 and one good bronze run, both of which exist.

## 6. Bronze layout

```
$HOME/wealthdb/relevate/                     (= /data inside container)
├── 20260527T142500Z/                        one bronze dump per run
│   ├── run.json                             manifest
│   ├── accounts/
│   │   ├── investment-overview.json         master list
│   │   ├── services.json
│   │   ├── compliance-products.json
│   │   ├── sso-claims.json
│   │   └── contact-messages.json
│   ├── portfolios/
│   │   └── <sha256-16>/                     dir per portfolio, slug hides externalId
│   │       ├── deposits-2018.json
│   │       ├── deposits-2019.json
│   │       ├── ...
│   │       ├── performance.json
│   │       ├── fees.json
│   │       ├── investment-allocation.json
│   │       └── modelportfolio.json
│   └── documents/
│       ├── index.json
│       ├── <docId>.pdf
│       └── <docId>.unexpected.{html,json}   only if API returned non-PDF
├── 20260815T120000Z/
│   └── ...
├── manual/                                  ad-hoc user-supplied PDFs
└── relevate.db                              silver SQLite (default path)
```

Path conventions:

- **Account / portfolio slugs are `sha256(externalId)[:16]`** —
  same pattern as ubs-web-dump's `<sha256-prefix>`. Keeps
  raw external IDs out of any path string that might leak into
  shell history, ps output, error messages.
- **Document filenames key on Relevate's own document `id`**
  (small integer). Stable across runs; idempotent fetch.
- **`run.json`** is written incrementally so a SIGINT-killed
  run still leaves a partial manifest for inspection.

## 7. Silver schema

The schema is now grounded in the observed REST response
shapes rather than guessed. SQLite + JSON1; JSON payloads
preserve the raw response for audit and to absorb future
field-level drift.

```sql
CREATE TABLE schema_meta (...);              -- migration version tracking
CREATE TABLE dump_runs (
    snapshot_at      INTEGER PRIMARY KEY,    -- Unix seconds UTC
    run_dir          TEXT NOT NULL,
    status           TEXT NOT NULL           -- 'ok' | 'partial' | 'failed'
);

CREATE TABLE accounts (
    -- One row per Relevate portfolio.
    account_external_id    TEXT PRIMARY KEY, -- portfolios[].externalId (NNNN.NNNNNN.N form)
    portfolio_internal_id  INTEGER,          -- portfolios[].id (small int; opaque)
    contact_id             INTEGER,          -- portfolios[].contactId
    contact_group_id       INTEGER,
    product_key            TEXT,             -- 'FZPF', 'FZI', ...
    product_name           TEXT,             -- 'PensFree', 'Independent', ...
    product_external_id    TEXT,
    product_offer_id       INTEGER,
    tax_wrapper            TEXT NOT NULL,    -- 'vested_benefits' for every FZ* product
    management_style       TEXT,             -- 'discretionary' for FZPF, 'self_directed' for FZI
    base_currency          TEXT NOT NULL,    -- 'CHF' observed
    portfolio_type_id      INTEGER,
    portfolio_status_id    INTEGER,
    portfolio_proposal_id  INTEGER,
    is_active              INTEGER,          -- 0/1
    first_investment_date  TEXT,             -- ISO yyyy-mm-dd
    first_seen_at          INTEGER NOT NULL,
    last_seen_at           INTEGER NOT NULL,
    payload                TEXT               -- the full portfolios[i] JSON object
);

CREATE TABLE positions (
    -- Per-portfolio target allocation snapshot (from modelportfolio).
    -- Relevate doesn't expose actual unit holdings on the
    -- /middlelayer/v2/ endpoints we've mapped — it exposes the
    -- MODEL/target allocation + portfolio currentValue. If a
    -- future endpoint surfaces actual holdings this table extends.
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT NOT NULL,
    position_key          TEXT NOT NULL,     -- modelportfolio.positions[].security.id
    instrument_external_id TEXT,             -- = position_key
    isin                  TEXT,
    asset_class           TEXT,              -- security.assetClass.name
    asset_class_external  TEXT,              -- security.assetClass.externalId
    country_code          TEXT,
    currency              TEXT NOT NULL,
    allocation            REAL,              -- positions[].allocation (target %, 0..1 or 0..100)
    trading_price         REAL,
    payload               TEXT,
    PRIMARY KEY (snapshot_at, account_external_id, position_key)
);

CREATE TABLE cash_balances (
    -- Per-portfolio cash + total balances, one row per
    -- investment-overview snapshot.
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT NOT NULL,
    currency              TEXT NOT NULL,
    balance_kind          TEXT NOT NULL,     -- 'cash', 'invested', 'total', 'current_value'
    amount                REAL NOT NULL,
    payload               TEXT,
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);

CREATE TABLE transactions (
    -- From /Portfolio/{id}/deposits per year. Schema TBC until we
    -- see a populated body (the Phase-1 capture got empty arrays).
    transaction_external_id TEXT PRIMARY KEY,
    occurred_at           INTEGER NOT NULL,
    account_external_id   TEXT NOT NULL,
    kind                  TEXT NOT NULL,     -- 'deposit','withdrawal','fee','interest','rebalance',...
    currency              TEXT NOT NULL,
    gross_amount          REAL,
    net_amount            REAL,
    payload               TEXT
);

CREATE TABLE performance_points (
    -- /portfolio/{id}/performance daily time series.
    -- One row per (portfolio, date).
    snapshot_at           INTEGER NOT NULL,  -- ingest snapshot, NOT the value date
    account_external_id   TEXT NOT NULL,
    value_date            TEXT NOT NULL,     -- ISO yyyy-mm-dd
    value                 REAL,
    cash_balance          REAL,
    securities_balance    REAL,
    cash_flow             REAL,
    deposits              REAL,
    payouts               REAL,
    profit                REAL,
    profit_all            REAL,
    profit_virtual        REAL,
    currency              TEXT,
    payload               TEXT,
    PRIMARY KEY (snapshot_at, account_external_id, value_date)
);

CREATE TABLE instruments (
    -- Securities seen in modelportfolios. Cross-portfolio dedup'd
    -- on ISIN if present, else on security.id.
    instrument_external_id TEXT PRIMARY KEY,
    isin                  TEXT,
    name                  TEXT,
    asset_class           TEXT,
    country_code          TEXT,
    currency              TEXT,
    first_seen_at         INTEGER NOT NULL,
    last_seen_at          INTEGER NOT NULL,
    payload               TEXT
);

CREATE TABLE documents (
    document_external_id  TEXT PRIMARY KEY,  -- documents[].id (small int)
    account_external_id   TEXT,              -- if document is per-portfolio
    foundation_id         INTEGER,
    document_type_code    INTEGER,           -- documents[].documentType enum
    document_type_label   TEXT,              -- derived
    category_code         INTEGER,           -- documents[].category enum
    category_label        TEXT,              -- derived
    file_name             TEXT,
    create_date           TEXT,
    valid_till            TEXT,
    bronze_path           TEXT NOT NULL,     -- relative to bronze root
    sha256                TEXT NOT NULL,
    payload               TEXT
);

CREATE TABLE fx_rates (
    -- Empty in v1 — all portfolios observed are CHF. The table
    -- exists so wealthdb gold's adapter contract is satisfied
    -- even before we see foreign-currency holdings.
    snapshot_at           INTEGER NOT NULL,
    base_currency         TEXT NOT NULL,
    quote_currency        TEXT NOT NULL,
    mid_rate              REAL NOT NULL,
    payload               TEXT,
    PRIMARY KEY (snapshot_at, base_currency, quote_currency)
);
```

Note `positions` models the **target allocation** (model
portfolio), not actual unit holdings. Vested-benefits funds
typically don't surface unit-level holdings to the customer —
the portfolio's value is opaque-NAV-derived. If an actual-
holdings endpoint is discovered we extend; otherwise this is the
right granularity for wealthdb gold.

## 8. Identity strategy + wealthdb gold bridge

### 8.1 `account_external_id`

**Use `portfolios[].externalId`** (the `NNNN.NNNNNN.N` form,
e.g. `NNNN.NNNNNN.N`). It's the foundation-issued account number
and is stable across sessions. The `id` field is the
foundation's internal DB key — fine as a join column but not
appropriate as the canonical external identifier.

### 8.2 `instrument_external_id`

**Prefer ISIN** if `modelportfolio.positions[].security.isin` is
non-null (wealthdb gold's `instruments.isin` is indexed for
cross-bank joins). Otherwise use `security.id` as an
adapter-scoped identifier.

### 8.3 `tax_wrapper`

Hard-coded to `'vested_benefits'` for any portfolio whose
`product.key` starts with `FZ` (currently `FZPF`, `FZI`). When
Pillar 3a portfolios materialise (`product.key` starting with
`3A` or similar — TBD), map them to `'pillar_3a'`. The adapter
catches anything else as `'other'` and the user fixes via
wealthdb config-side override.

### 8.4 `management_style`

- `FZPF` → `'discretionary'` (foundation-managed)
- `FZI` → `'self_directed'`
- Anything else → `'other'`

### 8.5 Future `relevate.md` adapter doc

Mirrors `swissquote.md`. Goes in
`~/github/wealthdb/docs/adapters/relevate.md` once `load.py`
exists and has produced one good silver. Out of scope here.

## 9. Container + wrapper architecture

### 9.1 Image

`python:3.12-slim-bookworm` + `requests`. Image is ~80 MB —
nothing else is installed because the toolkit replays REST calls
only. Build takes a few seconds.

### 9.2 Wrapper subcommand surface

```
./relevate-dump <verb>

build       Build the Docker image.
login       login.py — mint or refresh the cookie jar.
download    download.py — fetch bronze from /middlelayer/v2/.
load        load.py — parse bronze into silver SQLite.
sh|bash     Interactive shell in the container.
help        Show usage.
```

The wrapper sources `~/.secrets/relevate.env`, manages the mount
points, allocates a TTY when both stdin and stdout are TTYs, and
refuses to evict a running container without
`RELEVATE_FORCE_REPLACE=1`.

### 9.3 Mounts

| Container path | Host default | Purpose |
|---|---|---|
| `/secrets` | `~/.secrets` | env file + cookie jar state file |
| `/data` | `~/wealthdb/relevate` | bronze runs + silver DB |
| `/debug` | `~/.cache/relevate-debug` | opt-in scratch logs / traces |

All three are bind-mounted RW. No ports published — the
container is purely an HTTP client.

## 10. What we do NOT do

- **No mutations.** No POST / PUT / DELETE to any
  `/middlelayer/v2/` endpoint. No call to
  `/dashboard/depot/{id}/investment/allocation/change` or any
  other `*/change` URL — these exist in the SPA but are
  explicitly off-limits. The CLI must never accept a flag that
  would trigger a write.
- **No 2FA bypass.** Every fresh `login.py` invocation prompts
  for a code from the user's phone. No SMS-receiver integration,
  no TOTP-secret storage, no email-poll-and-extract.
- **No `--password` flag** anywhere. Env-only for secrets.
- **No scheduling.** Cron / launchd / Actions are out of scope —
  they can't survive the mTAN gate, would burn the session
  cookie's lifetime, and add a stealth-traffic signature to a
  customer portal we're a polite guest on.
- **No PII in tracked files.** The repo is publishable; the
  account-external-id `NNNN.NNNNNN.N` pattern is on the
  pre-commit grep list along with AHV `756.NNNN.NNNN.NN`.

## 11. Open questions

Resolved during the initial portal-mapping work:

- ✓ 2FA mechanism — mTAN via SMS (user-confirmed).
- ✓ Auth flow is three-step: `/b2c/access` (probe, 401) →
  `/password/check` (creds, 200, mTAN sent) → `/mtan/otp/check`
  (OTP, 200, session promoted).
- ✓ Session model — Airlock IAM, cookie-only. No bearer token
  exists; the SPA's `Authorization` header literally carries
  `"bearer undefined"`. Cookie value (`AL_SESS-S`, 68 chars) is
  promoted server-side, doesn't rotate.
- ✓ CSRF — double-submit-cookie. `CSRFT759-S` cookie value
  echoed as `X-CSRFT759` header on every POST. 22 chars,
  stable within a session.
- ✓ POST body shapes — `/b2c/access` is empty,
  `/password/check` is `{username, password}`,
  `/mtan/otp/check` is `{otp}`.
- ✓ Response shapes — JSON:API envelope (`meta`, `data` with
  `type/id/attributes`, `errors[]`). `/password/check` returns
  the masked phone number so we can show the operator which
  device to look at.
- ✓ Anti-bot posture — vanilla `requests` with mimicked Chrome
  headers should suffice. No Akamai / Camoufox / JS-challenge
  observed at the auth surface.
- ✓ `account_external_id` shape — `portfolios[].externalId`
  (`NNNN.NNNNNN.N`).
- ✓ Number of accounts — 3 (observed account, this session).
- ✓ Export availability — REST API returns JSON for all
  observed pages; documents return `application/pdf` directly.
- ✓ Document types — quarterly reports, fee statements, credit
  notes, agreements, plans, investor profile, leaving statement
  (and probably more we haven't seen).

Outstanding — to be resolved during silver-loader work or in
follow-up:

1. **Session TTL.** Airlock typically defaults to 30 min idle
   / 8 h absolute. Determine empirically by running
   `login.py --check` periodically after a fresh login and
   noting when it flips to DEAD.
2. **Session-renewal mechanism.** No `/refresh` endpoint was
   observed on the wire, but the SPA may issue keepalives we
   didn't see. If Airlock has an idle-extending touch we can
   issue without a fresh mTAN, hook it in.
3. **`/deposits` query param.** Every observed year returned the
   same empty `{transactions:[]}` envelope for these FZ accounts.
   Either the endpoint isn't transaction-bearing for this
   product, or transactions are accessed via a different path.
   The credit-note PDFs cover the actual contribution data.
4. **`documentType` + `category` enum mapping.** The numeric
   values 0/1/3/4/7/9 (type) and 0/1/2 (category) — derive
   labels by correlating with fileName patterns, or look for a
   config endpoint that exposes the mapping.
5. **Pillar 3a presence.** Endpoint `/dashboard/3a/depots`
   exists; investment-overview returned no 3a entries for this
   user. Confirm whether the absence is "user doesn't have
   any" or "API filters them out unless explicitly requested
   via a separate endpoint".
6. **Actual unit holdings.** Modelportfolio gives target
   allocation; investment-overview gives total currentValue.
   Is there an endpoint that returns the actual units held
   (e.g., `/positions`, `/holdings`)? If not, silver's
   `positions` table holds target allocations — fine for
   vested benefits but worth confirming.
7. **OTP retry behaviour.** Does Airlock invalidate the mTAN
   after one bad guess, or accept the next attempt? `login.py`
   currently allows 3 attempts; verify the user can in fact
   correct a fat-finger before the session locks.

