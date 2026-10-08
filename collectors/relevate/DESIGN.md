# relevate — design (REST-only)

A read-only scraper for Relevate's customer portal at
`portal.pens-expert.ch`. **Replays the Airlock IAM authentication
flow and the `/middlelayer/v2/` REST API directly from Python** —
no browser, no Xvfb, no VNC. CLI-only; mTAN code is prompted on
stdin during login.

Lands per-portfolio JSON + per-document PDFs into a versioned
bronze tree, then parses them into a queryable SQLite silver
database. Part of the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions.

Sections:

1. [How the portal works](#1-how-the-portal-works)
2. [Architecture](#2-architecture)
3. [`login.py` (REST auth)](#3-loginpy-rest-auth)
4. [`download.py` (REST dump)](#4-downloadpy-rest-dump)
5. [`load.py` (silver loader)](#5-loadpy-silver-loader)
6. [Bronze layout](#6-bronze-layout)
7. [Silver schema](#7-silver-schema)
8. [Identity strategy](#8-identity-strategy)
9. [Container + wrapper architecture](#9-container--wrapper-architecture)
10. [What we do NOT do](#10-what-we-do-not-do)
11. [Open questions](#11-open-questions)

---

## 1. How the portal works

The portal is driven entirely over its REST surface; the
container is slim Python + `requests`, no browser. What the
portal looks like on the wire:

**Auth stack — Airlock IAM:**
- Vendor: Ergon Informatik's Airlock IAM (Swiss-banking-standard
  reverse-proxy gateway). Confirmed by cookies `AL_SESS-S`,
  `AL_LoginFromNewDevice`, the `CSRFT759-S` CSRF cookie, and the
  asset bundles served from `/auth/ui/assets/airlock/`.
- 2FA factor: **mTAN** (SMS code to the registered mobile
  number), confirmed on that device; the URL `/auth/ui/app/auth/
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
   `phoneNumber` comes back in full; login.py masks it
   client-side before echoing it to stderr so the target device
   is identifiable without leaking digits.
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
header is a red herring: the SPA stringifies a JS
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

**Account taxonomy:**

- The portal can surface multiple FZ portfolios (vested benefits /
  Pillar 2 / Freizügigkeit) under one login. Both product keys are
  observable:
  - `product.key = FZPF` ("PensFree").
  - `product.key = FZI` ("Independent").
  Both products surface the same interface, fees, and investment
  menu in the SPA — the holder picks from a small set of pre-
  built strategies in either case, and the foundation doesn't let
  the holder choose individual securities or funds. No human
  manager or advisor is in the loop (robo-advisor shape, not
  self-directed brokerage).
- Portfolios are CHF-denominated.
- The SPA exposes a `/dashboard/3a/depots` URL, but
  `/portfolio/investment-overview` returns only the products a
  given login actually holds. `download.py` walks whatever the
  overview lists — a login holding 3a surfaces it there; no
  separate 3a probe.

**Document corpus:**

- Recognisable types from the fileName field: Quarterly Reports,
  Quarterly Fee Statements, FZ Credit Notes (contribution
  arrivals), Pension Agreement, Pension Plan, Investor Profile,
  Leaving Statement.
- `documentType` is a numeric enum (observed values: 0, 1, 3, 4,
  7, 9). `category` is a separate numeric enum (0, 1, 2). The
  enum-to-name mapping isn't exposed in the responses we've
  captured; the silver loader derives `doc_kind` from fileName
  needles instead (`DOC_KIND_PATTERNS` in load.py, German +
  English).

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
                                                     $XDG_DATA_HOME/wealthdb/relevate/<UTC-ts>/
                                                     (gitignored bronze tree)
                                                              │
       load.py ────────────────────────────────────────────►  $XDG_DATA_HOME/wealthdb/relevate/relevate.db
                                                              (SQLite silver)
                                                              │
                                                              ▼
                                          wealthdb gold (out of scope — sibling component: wealthdb/)
```

The Python scripts (login / download / load / prune +
pdf_parsers) run inside the same Docker image
(`wealthdb/base-python` + requests / pypdf / pytest). No browser,
no Xvfb, no VNC.

Stack:

- **Python `requests`** for HTTP. Synchronous; the workload is
  read-only and modest (low tens of API calls per portfolio + a
  few dozen document GETs).
- **`requests.cookies.RequestsCookieJar`** persisted as JSON for
  the Airlock cookies (`AL_SESS-S`, `AL_LoginFromNewDevice`,
  `CSRFT759-S`).
- **No `httpx`, no async**, no scheduling library. Simpler is
  better for a tool invoked by hand.

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
   number; the phone number comes back in full and is masked
   client-side before being surfaced on stderr.
5. Display the masked phone number on stderr and read the OTP
   from stdin via `input()`. Retry on rejection up to
   `--max-otp-attempts` (default 3); each retry accepts the next
   code that arrived (mTANs are usually
   single-use but Airlock typically allows multiple-in-flight).
6. `POST /auth/rest/public/authentication/mtan/otp/check` with
   body `{"otp": "..."}` and the same `X-CSRFT759` +
   `X-Continue-Flow: true` headers. Returns HTTP 200. Server-side
   the existing `AL_SESS-S` session record is promoted from
   anonymous → authenticated; **the cookie value does not change**
   across the pre/post-auth exchanges.
7. `GET /auth/rest/protected/self-service/ui/configuration/portal`
   — landmark probe to confirm the session is in fact live
   before persisting. 200 = good.
8. Persist the cookie jar to `~/.secrets/relevate-state.json`
   (chmod 0600), atomic via `mv tmp final`:
   ```json
   {
     "minted_at": "2026-01-01T12:00:00+00:00",
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
   load-bearing; the SPA's Google Analytics cookies (`_ga`,
   `_gcl_au`, `_ga_*`) are JS-set and never appear in the
   `requests` cookie jar.

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
  user prompt (root AGENTS.md §2).
- `--max-otp-attempts` — retry budget for a mistyped OTP
  (default 3). Attempts can pause indefinitely; `input()`
  blocks on stdin without a deadline. This
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
`--check` triggers a fresh mTAN — it runs only on explicit
request (root AGENTS.md §2).

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
   print "run `./relevate login` first".
2. Configure session headers to mirror the SPA: realistic Chrome
   UA, `Sec-Ch-Ua-*` client hints, `X-Same-Domain: 1`, and the
   placeholder `Authorization: bearer undefined`. No `X-CSRFT759`
   on GETs — Airlock only enforces double-submit on POSTs.
3. Probe `/auth/rest/protected/self-service/ui/configuration/portal`.
   Non-200 → exit; report that the session is dead.
4. Create the run dir `/data/<UTC-ts>/`. Open the
   `Manifest` (run.json writer); it is born `status: "in-progress"`
   and flushed at construction, then flushed after every artefact so
   a Ctrl-C run leaves an inspectable partial manifest. The terminal
   status is stamped by `finish()` at the end.
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
     - Iterate from the window's start year to the current
       year, saving `deposits-YYYY.json` per year. Year is the
       smallest granularity `/deposits` accepts, so the start
       year of the `--lookback` window is the closest analog of
       the fleet's date window (current year by default, since
       `--lookback` defaults to today − 90 days). This is the
       path the download always takes — it derives the years
       from `--lookback` — so the fallbacks below serve direct
       callers of `do_download`.
     - Else if `firstInvestmentDate` is plausible (parses to
       a year ≥ 1900, not the `0001-01-01` sentinel Relevate
       uses for "unknown"): the current year only, saved as
       `deposits-YYYY.json`.
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
   not `--no-documents`):
   - `GET /middlelayer/v2/documents` → `documents/index.json`.
   - Each in-window `documents[].id` is routed through the shared
     `collectorkit.docdedup` download-avoidance engine (keyed
     `(doc_id,)`), chosen per document kind (the `fileName`-derived
     label `load.py` parses on, so a parsed kind is never
     mis-linked):
     - executed-once immutable kinds `load` never parses (fee
       statements, pension agreements/plans, investor profiles,
       account-opening docs) → **link**: an identical PDF from a
       prior complete run is hardlinked into this run dir and the
       fetch is skipped (a hardlink error falls through to a real
       fetch — a doc degrades to a fetch, never to a miss);
     - parsed / tax-adjacent kinds (quarterly reports, credit notes,
       leaving statements) and any unrecognised kind → **fetch-verify**
       (the safe default): always `GET
       /middlelayer/v2/document/{id}`, then content-compare to the
       prior copy — a byte-identical one is hardlinked (disk
       reclaimed), a re-issue keeps its fresh bytes.
     `application/pdf` 200 → `documents/<id>.pdf`. Any other
     content-type → `documents/<id>.unexpected.<ext>` (yields no PDF;
     counted as a per-document error). The index is rebuilt statelessly
     from the COMPLETE prior runs' on-disk PDFs each run; the current
     in-progress run is excluded so it never seeds itself.
     `--documents-force` bypasses the index (always fetch, no hardlink
     reuse). A hardlink is a real in-run file, so each run dir stays
     self-contained and the loader needs no cross-run fallback.
8. Final `run.json` flush via `finish()`, which stamps the terminal
   `status`: `"complete"` for a finished walk (set even when some
   endpoints errored), `"dry-run"` for a `--dry-run` walk (stamped
   in a throwaway temp dir — a dry run leaves nothing under bronze),
   `"incomplete"` when the master enumeration failed and no work
   could run. Exit 0 if no errors recorded, 2 if at least one
   endpoint failed (the run dir is still usable; the loader skips
   manifest entries flagged with errors).

The `status` field is the fleet-uniform completeness signal: a run
dir is a *complete* dump (`status == "complete"`), or a *non-complete*
one (an `"in-progress"` marker from a crashed walk, a `"dry-run"`
shell, an `"incomplete"` abort, or no `run.json` at all). `load` skips
a dump whose status is anything but `"complete"`, so a partial capture
never reaches silver; `prune` reclaims non-complete run dirs (§6). The
`ended_at` / `dry_run` fields are written alongside `status` and are the
terminal signal `prune` falls back to for a statusless manifest
(complete iff `ended_at` was stamped on a non-`dry_run` run).

Manifest shape (as written):

```json
{
  "tool": "relevate.download",
  "schema_version": 1,
  "started_at": "...", "status": "complete", "ended_at": "...",
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
    "count_in_index": NN,
    "count_in_window": NN, "count_outside_window": NN,
    "total": NN,
    "fetched": NN, "linked": NN, "verified": NN,
    "changed": NN, "errors": NN, "other": 0,
    "unexpected_content_type": [],
    "files": [...]
  },
  "errors": [],
  "files": ["accounts/...", "portfolios/abc123/...", ...]
}
```

### 4.2 CLI surface

```
download.py [--state-path PATH] [--bronze-dir DIR]
            [--dry-run] [--debug]
            [--mode {all, accounts, portfolios, documents}]
            [--no-documents] [--documents-force]
            [--lookback PRESET|YYYY-MM-DD]
            [--limit-portfolios N]
            [--limit-documents N]
            [--verbose]
```

Date-window contract: `--lookback` is the one window flag — a
named preset (`1w`/`4w`/`3m`/`6m`/`1y`/`2y`/`5y`/`all`) or an ISO
date naming the window's start, which runs from there to today.
Its start year bounds the `/deposits` year iteration
(`year_from = since.year`, `year_to = until.year`), since the
endpoint takes year granularity only. The same window filters the
per-PDF fetch by `createDate` (the full index is still written for
traceability).

Iteration discipline: the
`--mode`, `--no-documents`, and `--limit-*` flags let one phase be
re-run cheaply while iterating on the silver loader or
investigating a specific portfolio's response shape.

`--dry-run` hits only the two master listing endpoints
(`investment-overview` + `documents`), records the work-list
counts in the manifest, and exits. Root AGENTS.md §2 explicitly
allows running this without user prompt.

### 4.3 Idempotency + dedup

- The bronze run dir is timestamped per invocation — never
  rewritten across runs.
- Document fetches are download-avoidant across runs via the shared
  `collectorkit.docdedup` engine (§4.1 step 7): an immutable,
  unparsed doc identical to a prior complete run is hardlinked in
  (fetch skipped); everything parsed / tax-adjacent / unrecognised
  is fetch-verified (always fetched, a byte-identical copy still
  hardlinked to reclaim disk). The mode is chosen off the same
  `fileName`-derived kind `load.py` parses on, so a parsed doc is
  never mis-linked; `--documents-force` bypasses the index. The
  freshness window (default 35 days, keyed on `createDate`) holds a
  just-issued immutable doc out of the index so a same-period
  correction is re-fetched rather than linked.
- Cross-run silver dedup is still the loader's job (content-hash on
  ingest); the download-time hardlinking is a bronze-side disk +
  fetch-avoidance win that leaves each run dir self-contained.

### 4.4 Errors + retry policy

Current implementation is conservative: log + record in
`manifest.errors`, continue to the next work item. No retry
loop.

- HTTP 401 → session probably expired (the initial probe should
  have caught this; if not, it's mid-run). Record and continue —
  individual endpoints may have shorter-lived auth than the
  landmark.
- HTTP 5xx → record and continue. No retry layer; a
  retry-with-backoff layer would slot in here if needed.
- HTTP 429 → record and continue. Same: not observed; defer.
- Document GET returning HTML or `application/json` instead of
  `application/pdf` → save under `documents/<id>.unexpected.<ext>`
  and record in `manifest.documents.unexpected_content_type`.

## 5. `load.py` (silver loader)

Mechanical projection of bronze JSON into the silver SQLite
schema (§7), with migrations applied on each invocation. Idempotent
on re-runs: tracks which `dump_runs.snapshot_at` have been ingested
and short-circuits.

The loader ingests each unseen bronze run in one transaction
(accounts / cash_balances / positions / instruments /
performance_points / documents), then runs two cross-dump PDF
phases: `load_historical_snapshots` (quarterly-report holdings →
`historical_position_snapshots` / `historical_cash_balances`) and
`load_credit_note_transactions` (credit-note events,
`source='credit_note_pdf'`).

## 6. Bronze layout

```
$XDG_DATA_HOME/wealthdb/relevate/                     (= /data inside container)
├── 20260101T120000Z/                        one bronze dump per run
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
  same pattern as ubs-web's `<sha256-prefix>`. Keeps
  raw external IDs out of any path string that might leak into
  shell history, ps output, error messages.
- **Document filenames key on Relevate's own document `id`**
  (small integer). Stable across runs; idempotent fetch.
- **`run.json`** is written incrementally so a SIGINT-killed
  run still leaves a partial manifest for inspection. Because it is
  flushed from run-dir creation (born `status: "in-progress"`), its
  mere *presence* does not mean the walk finished — the completeness
  signal is `status == "complete"` (or, for a pre-`status` dump,
  `ended_at` stamped on a non-`dry_run` run).

**Pruning.** `prune` (a thin wrapper over the shared, unit-tested
`collectorkit.prune` engine) reclaims whole **non-complete** run dirs
— crashed walks and `--dry-run` shells — across the bronze tree, plus
the one debug artefact a run dir can hold: the `screenshots/` HTTP trace
a `download --debug` writes (`debug_subdirs`). relevate drives no
browser, so there are no DOM dumps or Playwright traces beside it. Every
other file in a *complete* dump is kept: the JSON payloads and document
PDFs (the latter read cross-dump by `load_historical_snapshots` and
`load_credit_note_transactions` via `documents.bronze_path`) are faithful
bronze captures and load inputs, never prune targets. Non-run entries at the bronze root (`manual/`,
`relevate.db`) and symlinks are never touched, and an unreadable or
corrupt `run.json` is UNKNOWN and skipped. An in-flight guard
(`--min-age-hours`, default 1, keyed on the newest write in the dir)
protects a long backfill whose slug is old but whose files are fresh;
a whole-dir deletion rechecks completeness + quiescence immediately
before `rmtree`. Deleting a non-complete dump surfaces on the next
`load --force` rebuild.

## 7. Silver schema

SQLite + JSON1. Migrations live in `migrations/`; the loader
reads `MAX(silver_schema_version)` from `schema_meta` and applies
any newer files in order. Migration discipline matches the
sibling collectors: every change lands as a new numbered file, no
backward-compatible drift, silver DBs always conform to the
latest schema.

Storage conventions:

- Unix-seconds-UTC integers for all timestamps.
- Stable filter columns promoted; rest in `payload TEXT` JSON.
- Snapshot tables monotemporal on `snapshot_at` (PK starts with
  it, so the implicit B-tree is the as-of index).
- Event tables (`transactions`) keyed by stable external id;
  INSERT OR REPLACE so re-loading a window converges.
- Documents content-deduped via PRIMARY KEY `content_sha256`.

### 7.1 Tables (as built — see `migrations/`)

| Table | PK | Purpose |
|---|---|---|
| `schema_meta` | `silver_schema_version` | Migration version registry. |
| `dump_runs` | `snapshot_at` | One row per ingested bronze run; carries the `run.json` manifest verbatim in `payload`. `dry_run` flag preserves provenance. |
| `accounts` | `(snapshot_at, account_external_id)` | One row per (snapshot, Relevate portfolio). Promotes product_key, currency, portfolio_proposal_id, is_active, first_investment_date, contact_id; rest in payload. |
| `cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | One row per (snapshot, account, currency, kind). `balance_kind` ∈ {cash, invested, current, securities, saving, investment, virtual, target_inv, target_sav} — derived from the matching `portfolios[i]` fields. |
| `positions` | `(snapshot_at, account_external_id, instrument_external_id)` | One row per (snapshot, account, modelportfolio position). **TARGET allocation**, not actual unit holdings. Promotes isin, asset_class, country_code, allocation, trading_price. |
| `instruments` | `instrument_external_id` | Slow-changing master data; upsert advances `last_seen_at`. Cross-portfolio dedup'd on `security.id`. |
| `performance_points` | `(snapshot_at, account_external_id, value_date)` | Daily time series from `/portfolio/{id}/performance`. One point per calendar day of the portfolio's history since inception. `value_date` is Unix seconds at the day's midnight UTC. |
| `transactions` | `transaction_external_id` | `/deposits` returns no rows for FZ accounts, so the API side stays empty; `load_credit_note_transactions` parses the credit-note PDFs and lands the contribution events here with `source='credit_note_pdf'`. |
| `documents` | `content_sha256` | Content-deduped index of PDFs on disk. `first_seen_at` is the earliest dump that captured the content; `last_seen_at` advances on subsequent dumps. Promotes numeric `document_type_code` and `category_code` (enum-to-name mapping not exposed; `doc_kind` is derived from fileName needles — `DOC_KIND_PATTERNS`, `'other'` only for unrecognised names). |
| `historical_position_snapshots` | `(snapshot_at, account_external_id, isin)` | Per-quarter holdings parsed from the quarterly-report PDFs (migration 0002). |
| `historical_cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | Per-quarter cash/valuation figures from the same reports (migration 0002). |
| `parser_generations` | `scope` | Which generation of the PDF parsing logic produced the rows a pass is holding (`collectorkit.srcfp` fingerprint). A moved generation drops them before the pass re-derives, so a re-parse replaces rather than accumulates. |

#### 7.1.1 `accounts.product_key` is forensic, not behavioural

The observed values `FZI` (product_name `Independent`) and
`FZPF` (product_name `PensFree`) look like two product lines
with potentially different investment menus, fees, or
operational characteristics. They aren't.

PensExpert operates these as **two legally separate
foundations** that surface the IDENTICAL investment-product
menu, IDENTICAL fee schedule, and IDENTICAL strategy-
configuration UI. The two-foundation structure exists as a
Swiss regulatory workaround: vested-benefits law caps a
customer's vested-benefits assets at two custody accounts when
transferring, so PensExpert runs `Independent` and `PensFree`
side-by-side under one roof to support that without forcing
customers to a third-party foundation. A customer's assets can
land in either or both; the customer experiences no
behavioural difference between them.

The column is surfaced verbatim because it's forensically
useful: "which foundation does this account live in",
legal-entity attribution, cross-checks against the credit-note
PDFs that name the issuing Stiftung. Both foundations classify
identically, so `product_key` is not a behavioural
discriminator — a downstream consumer should treat FZI and FZPF
the same. How gold interprets this column is owned by the
wealthdb relevate adapter — see [the canonical
model](../../DESIGN.md) and the adapter source
[`wealthdb/internal/silver/relevate/`](../../wealthdb/internal/silver/relevate/).

### 7.2 Load semantics verified

The projected row shape:

- one `accounts` row per (portfolio, snapshot);
- one `cash_balances` row per (account, populated balance kind);
- one `positions` row per (modelportfolio position, non-dry-run
  snapshot);
- one `instruments` row per distinct security across the
  modelportfolios;
- ~N `performance_points` per (portfolio, non-dry-run snapshot);
- `transactions` may be empty for FZ products (`/deposits` can
  return no rows);
- `documents` content-deduped across the dumps.

Each `positions` row carries an `asset_class` label (from
`security.assetClass.name`) and an ISIN. Re-running `load`
against the same bronze is a no-op (the `dump_runs.snapshot_at`
PK is the idempotency anchor).

### 7.3 What's deliberately NOT a table

- **`fx_rates`**: portfolios are CHF-denominated and
  `currentValue` comes back in `currency.currencyCode = "CHF"`.
  If a future Relevate product surfaces foreign-currency holdings
  the loader will materialise an `fx_rates` table in a follow-on
  migration; doing it now would be empty-table speculation.
- **`fees`**: `/portfolio/{id}/fees` returns a single `{value,
  isPercent, displayValue}` object per portfolio. The raw JSON
  stays on disk under `portfolios/<slug>/fees.json` in the
  bronze tree; the loader doesn't project it into silver in v1.
  A future migration can promote it into the `accounts` row's
  payload or its own table once gold consumers care.
- **`investment_allocation`**: the
  `/portfolio/{id}/investment/allocation` endpoint's
  `{totalValuation, investmentValuation, savingValuation}` is
  redundant with the matching `cash_balances` rows the loader
  already derives from `investment-overview`. The raw response
  stays in bronze.

## 8. Identity strategy

The silver-side identity choices for the columns the loader
promotes. How gold interprets these columns (the
`tax_wrapper` / `management_style` / `account_kind` mapping) is
owned by the wealthdb relevate adapter — see [the canonical
model](../../DESIGN.md) and the adapter source
[`wealthdb/internal/silver/relevate/`](../../wealthdb/internal/silver/relevate/).

### 8.1 `account_external_id`

**Use `portfolios[].externalId`** (the `NNNN.NNNNNN.N` form,
e.g. `1234.567890.0`). It's the foundation-issued account number
and is stable across sessions. The `id` field is the
foundation's internal DB key — fine as a join column but not
appropriate as the canonical external identifier.

### 8.2 `instrument_external_id`

**Prefer ISIN** if `modelportfolio.positions[].security.isin` is
non-null (wealthdb gold's `instruments.isin` is indexed for
cross-bank joins). Otherwise use `security.id` as an
adapter-scoped identifier.

## 9. Container + wrapper architecture

### 9.1 Image

`wealthdb/base-python` + `requirements.txt` (requests, pypdf,
pytest). No browser stack — the toolkit replays REST calls and
parses PDFs. Build takes a few seconds on a warm base.

### 9.2 Wrapper subcommand surface

```
./relevate <verb>

build       Build the Docker image.
login       login.py — mint or refresh the cookie jar.
download    download.py — fetch bronze from /middlelayer/v2/.
load        load.py — parse bronze into silver SQLite.
prune       prune.py — reclaim non-complete dumps from the bronze tree.
sh|bash     Interactive shell in the container.
help        Show usage.
```

The wrapper sources `~/.secrets/relevate.env`, manages the mount
points, allocates a TTY when both stdin and stdout are TTYs, and
refuses to evict a running container without
`RELEVATE_FORCE_REPLACE=1`.

### 9.3 Mounts

The standard `/secrets` (`~/.secrets`) and `/data`
(`$XDG_DATA_HOME/wealthdb/relevate`) bind-mounts follow the shared collector
convention — see [collectors/README.md](../README.md). On top of
those, relevate bind-mounts a third, tool-specific path:

| Container path | Host default | Purpose |
|---|---|---|
| `/debug` | `~/.cache/wealthdb/debug/relevate` | opt-in scratch logs / traces |

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
  for a code from the phone. No SMS-receiver integration,
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

1. **Session TTL.** Airlock typically defaults to 30 min idle
   / 8 h absolute. Determine empirically by running
   `login.py --check` periodically after a fresh login and
   noting when it flips to DEAD.
2. **Session-renewal mechanism.** No `/refresh` endpoint appears
   on the wire, but the SPA may issue keepalives that haven't
   been captured. If Airlock has an idle-extending touch that
   can be issued without a fresh mTAN, hook it in.
3. **`documentType` + `category` enum mapping.** The numeric
   values 0/1/3/4/7/9 (type) and 0/1/2 (category) — derive
   labels by correlating with fileName patterns, or look for a
   config endpoint that exposes the mapping.
4. **Pillar 3a presence.** Endpoint `/dashboard/3a/depots`
   exists; investment-overview omits 3a entries for FZ-only
   logins. Confirm whether the absence is "the login doesn't have
   any" or "the API filters them out unless explicitly requested
   via a separate endpoint".
5. **Actual unit holdings.** Modelportfolio gives target
   allocation; investment-overview gives total currentValue.
   Is there an endpoint that returns the actual units held
   (e.g., `/positions`, `/holdings`)? If not, silver's
   `positions` table holds target allocations — fine for
   vested benefits but worth confirming.
6. **OTP retry behaviour.** Does Airlock invalidate the mTAN
   after one bad guess, or accept the next attempt? `login.py`
   allows 3 attempts; verify a fat-finger can in fact be
   corrected before the session locks.

