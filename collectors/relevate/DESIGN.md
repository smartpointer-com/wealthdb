# relevate — design (REST-only)

A read-only scraper for Relevate's customer portal at
`portal.pens-expert.ch`. It **replays the Airlock IAM authentication
flow and the `/middlelayer/v2/` REST API directly from Python**. There
is no browser, no Xvfb and no VNC. `login` prompts for the mTAN code on
stdin.

`download` lands per-portfolio JSON and per-document PDFs in a bronze
tree. `load` parses them into a SQLite silver database. Part of the
**wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and
[collectors/README.md](../README.md) for shared collector conventions.

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

The portal is driven entirely over its REST surface. The container is
slim Python + `requests`, no browser. What the portal looks like on the
wire:

**Auth stack — Airlock IAM:**
- Vendor: Ergon Informatik's Airlock IAM (Swiss-banking-standard
  reverse-proxy gateway). The cookies `AL_SESS-S`,
  `AL_LoginFromNewDevice` and `CSRFT759-S` identify it, as do the
  asset bundles served from `/auth/ui/assets/airlock/`.
- 2FA factor: **mTAN**, an SMS code to the registered mobile number.
  The URL `/auth/ui/app/auth/flow/b2c/mtan` and the API endpoint
  `/auth/rest/public/authentication/mtan/otp/check` both match.

**Auth endpoints (all under `/auth/rest/`, all serving
`application/vnd.api+json`):**

The auth flow has **three steps**: `/b2c/access` is the flow-init
probe, `/password/check` takes the credentials, and `/mtan/otp/check`
takes the OTP.

- `POST /auth/rest/public/authentication/applications/b2c/access`
   — flow-init probe. **Empty body**, sent with the `X-CSRFT759`
   header. Returns **401** with a JSON:API envelope describing
   the current flow state (`meta.nextAuthStep`,
   `errors[].code`). The 401 is *expected*: Airlock is saying
   "authenticate first".
- `POST /auth/rest/public/authentication/password/check`
   — credentials submission. Body
   `{"username": "...", "password": "..."}`. On 200, Airlock
   sends the mTAN and returns
   `data.attributes.{nextAuthStep, phoneNumber, resendPossible}`.
   `phoneNumber` comes back in full. `login.py` masks it before
   echoing it to stderr, so the target device is identifiable
   without leaking digits.
- `POST /auth/rest/public/authentication/mtan/otp/check`
   — OTP verification. Body `{"otp": "..."}`. On 200 the
   existing `AL_SESS-S` cookie is promoted server-side from
   anonymous to authenticated. **No bearer token in the response
   body**: `data.attributes` is `{}`.
- `GET /auth/rest/protected/self-service/ui/configuration/portal`
   — post-auth probe. Returns the portal target redirect tree
   (a `portalGroups[].portalTargets[]` structure). It serves as
   the session-alive landmark.
- `DELETE /auth/rest/public/authentication` — the SPA's logout;
   returns 204. The toolkit does not call it.

**Authorization for the application API — cookie-only, no bearer:**

The `Authorization: bearer X` header on the `/middlelayer/v2/` calls is
a red herring. The SPA stringifies a JS `undefined` and sends literally
`Authorization: bearer undefined`. Airlock ignores it; the `AL_SESS-S`
cookie alone carries the session. The toolkit sends the same
`bearer undefined` value so its requests match the SPA's. There is no
token to extract or refresh.

CSRF is **double-submit-cookie**. The `CSRFT759-S` cookie value
(22 chars, not httpOnly so JS can echo it) is also sent as the
`X-CSRFT759` request header on every state-changing POST. The auth
POSTs require it; the middlelayer GETs do not.

**Application API — `/middlelayer/v2/`:**

| Endpoint | Method | Notes |
|---|---|---|
| `/portfolio/investment-overview` | GET | **Master list.** All portfolios under the contact, with full metadata: `portfolios[].{id, externalId, contactId, contactGroupId, currency, currentValue, investedAmount, currentPerformance*, isActive, portfolioTypeId, portfolioStatusId, product.{id, key, name, externalId, ...}}` plus top-level `cashAmount`, `ahvStatus`, `externalContactId`. |
| `/Portfolio/{id}/deposits` | GET | Transactions / deposits, one year per call (`?year=YYYY`). Without the parameter it returns `{year:null, maxDepositPerYear:null, transactions:[]}`. For vested-benefits portfolios the `transactions` list is empty for every year; contributions come from the credit-note PDFs instead. |
| `/portfolio/{id}/performance` | GET | Daily time series: `values[].{date, value, amount, cashBalance, securitiesBalance, cashFlow, deposits, payouts, profit, profitAll, profitVirtual, amountVirtual}` plus `currency`, `externalPortfolioId`, `isEntireDuration`. |
| `/portfolio/{id}/fees` | GET | Fee breakdown. |
| `/portfolio/{id}/investment/allocation` | GET | Allocation summary: `totalValuation`, `investmentValuation`, `savingValuation`. |
| `/Portfolio/proposal/{proposalId}/modelportfolio` | GET | Target model portfolio with `positions[].{security.{isin, name, assetClass, country, tradingPrice, tradingUnit, faceValue}, allocation}` and `riskCategory.{min, max, expectedReturn}`. The `proposalId` is `portfolios[].portfolioProposalId` from the overview. |
| `/documents` | GET | Document index. Returns `{documents[], maintenance}`. Each document: `{id, externalId, fileName, documentType (enum), category (enum), createDate, foundation, foundationId, validTill, dmsDocumentMetadataId, ownerId, namePreInsurer, isUploaded, sP6_SHOW_RELEVATE, externalContactId, tenantId, description}`. |
| `/document/{id}` | GET | Direct PDF fetch (`application/pdf`). The download surface. |
| `/document/generate/contract/pensionfundregulations?tId=N` | GET | On-demand contract regeneration; also returns a PDF. Not called. |
| `/compliance/products`, `/contact/{messages, notification, risk-protection}`, `/sso/claims` | GET | Ancillary endpoints, saved to bronze in `--mode all`. `/contact/risk-protection` answers 204 when no protection is set up. |
| `/portfolio/services`, `/contact/language`, `/sso/sync` | GET | Not called. `/portfolio/services` answers 400 and `/contact/language` 405 without parameters the toolkit cannot supply. |

The casing of `Portfolio` vs `portfolio` differs between endpoints on
the wire; the code keeps each as observed.

**Account taxonomy:**

- One login can hold several vested-benefits (Pillar 2,
  Freizügigkeit) portfolios. The known product keys are:
  - `product.key = FZPF` ("PensFree").
  - `product.key = FZI` ("Independent").
  Both products offer the same interface, fees and investment menu.
  The holder picks one of a small set of pre-built strategies. The
  foundation does not let the holder choose individual securities or
  funds, and no human manager or advisor is in the loop (robo-advisor
  shape, not self-directed brokerage). See §7.1.1.
- Portfolios are CHF-denominated.
- The SPA exposes a `/dashboard/3a/depots` URL, but
  `/portfolio/investment-overview` returns only the products a
  login holds. `download.py` walks whatever the overview lists and
  makes no separate 3a call (§11, question 4).

**Document corpus:**

- The `fileName` field names the document kind: quarterly report,
  quarterly fee statement, credit note (contribution arrival),
  pension agreement, pension plan, investor profile, leaving
  statement, account opening.
- `documentType` and `category` are numeric enums. The responses
  carry no names for their values. The silver loader therefore
  derives `doc_kind` from `fileName` needles (`DOC_KIND_PATTERNS` in
  `load.py`, German and English).

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
                                          wealthdb gold (sibling component: wealthdb/)
```

The Python scripts (login / download / load / prune +
pdf_parsers) run inside one Docker image (`wealthdb/base-python` +
requests / pypdf / pytest). `prune` also runs on the host.

Stack:

- **Python `requests`** for HTTP. Synchronous; the workload is
  read-only and modest (a handful of calls per portfolio plus one per
  in-window document).
- **`requests.cookies.RequestsCookieJar`** persisted as JSON for
  the Airlock cookies (`AL_SESS-S`, `AL_LoginFromNewDevice`,
  `CSRFT759-S`).
- **No `httpx`, no async**, no scheduling library. A tool run by hand
  needs none of them.

## 3. `login.py` (REST auth)

### 3.1 Flow

Three steps. `/b2c/access` is the flow-initialisation probe; the
credentials go to the separate `/password/check`. There is **no bearer
token at all**: the session is 100% cookie-based (§1).

The script reproduces what the Angular SPA does:

1. Read `RELEVATE_LOGIN` and `RELEVATE_PASSWORD` from env (the
   wrapper sources them from `~/.secrets/relevate.env`). Missing →
   exit 64.
2. `GET /auth/ui/app/auth/flow/b2c/password?lang=en` primes the
   cookie jar. Airlock sets `AL_SESS-S` (68 chars, httpOnly,
   secure), `CSRFT759-S` (22 chars, not httpOnly) and
   `AL_LoginFromNewDevice` (185 chars, device-trust token).
3. `POST /auth/rest/public/authentication/applications/b2c/access`
   with an empty body and the `X-CSRFT759` header (the `CSRFT759-S`
   cookie value, double-submit pattern). Returns the expected
   HTTP **401** with the flow-state envelope.
4. `POST /auth/rest/public/authentication/password/check` with
   body `{"username": "...", "password": "..."}` and the
   `X-CSRFT759` + `X-Continue-Flow: true` headers. Returns HTTP
   200 with `data.attributes.{nextAuthStep, phoneNumber,
   resendPossible}`. Airlock has sent the mTAN to the registered
   number.
5. Print the masked phone number on stderr and read the OTP from
   stdin via `input()`. A rejected code re-prompts, up to
   `--max-otp-attempts` (default 3). Typing `resend` (or `r`)
   repeats step 4, which asks Airlock for a fresh mTAN. A resend
   or an empty line does not count as an attempt.
6. `POST /auth/rest/public/authentication/mtan/otp/check` with
   body `{"otp": "..."}` and the same `X-CSRFT759` +
   `X-Continue-Flow: true` headers. Returns HTTP 200. Server-side
   the existing `AL_SESS-S` session record is promoted from
   anonymous to authenticated. **The cookie value does not change**
   across the pre- and post-auth exchanges.
7. `GET /auth/rest/protected/self-service/ui/configuration/portal`
   confirms the session is live before it is persisted. 200 = good.
8. Persist the cookie jar to `~/.secrets/relevate-state.json`
   (chmod 0600), written atomically:
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
   Only the Airlock-domain cookies and the device-trust cookie
   matter. The SPA's Google Analytics cookies (`_ga`, `_gcl_au`,
   `_ga_*`) are set by JS and never appear in the `requests` cookie
   jar.

No bearer extraction step. No token decoding. Just cookies.

### 3.2 CLI surface

```
login.py [--state-path PATH] [--check] [--max-otp-attempts N]
         [-v/--verbose]
```

- `--state-path` — defaults to `/secrets/relevate-state.json`
  inside the container (host `~/.secrets/relevate-state.json`).
- `--check` — **no credentials submitted, no mTAN push.** Loads
  the existing state file, restores the cookie jar, hits the
  landmark probe, and prints `ALIVE` / `DEAD` / `MISSING` on stdout.
  Exit codes: 0 (ALIVE), 1 (MISSING), 2 (DEAD).
- `--max-otp-attempts` — how many OTP submissions before giving up
  (default 3). The prompt has no deadline: `input()` blocks on stdin
  until a code arrives.
- No `--password` flag. Credentials come only from env.

### 3.3 Session reuse

Airlock session cookies have a sliding lifetime set server-side. The
wire does not expose it (§11, question 1). `login.py --check` is the
probe for whether the stored session is still alive. Every other
`login` run sends a fresh mTAN.

### 3.4 Custom request headers

The toolkit sends the headers the SPA sends, so its requests look
like a browser's:

| Header | Value | Where |
| --- | --- | --- |
| `User-Agent` | recent Chrome on macOS | every request |
| `Accept` | `application/vnd.api+json, application/json, */*` | every request |
| `Accept-Language` | `en-US,en;q=0.9` | every request |
| `Origin` | `https://portal.pens-expert.ch` | every request |
| `Referer` | login-flow URL (`login`), dashboard URL (`download`) | every request |
| `Sec-Ch-Ua*`, `Sec-Fetch-*` | matching Chrome's client hints | every request |
| `X-Same-Domain` | `1` | every request |
| `Authorization` | `bearer undefined` | `download` GETs |
| `X-CSRFT759` | `CSRFT759-S` cookie value (22 chars) | every POST |
| `X-Continue-Flow` | `true` | `/password/check`, `/mtan/otp/check` |
| `Content-Type` | `application/json` | every POST that has a body |

### 3.5 What is NOT `login.py`'s job

- It calls no `/middlelayer/v2/` endpoint. Its only post-auth call
  is the `/configuration/portal` probe in the auth tree. Data
  extraction is `download.py`.
- It never prompts for credentials. They come from env, and a
  missing one is exit 64.

## 4. `download.py` (REST dump)

### 4.1 Flow

1. Load `/secrets/relevate-state.json` and restore the cookie
   jar into a `requests.Session`. Missing file → exit `64` with
   "run `./relevate login` first".
2. Set the session headers listed in §3.4. No `X-CSRFT759` on GETs:
   Airlock enforces double-submit only on POSTs.
3. Probe `/auth/rest/protected/self-service/ui/configuration/portal`.
   Non-200 → exit 1 and report that the session is dead. `download`
   never signs in.
4. Create the run dir `/data/<UTC-ts>/` and open the `Manifest`
   (the `run.json` writer). It is born `status: "in-progress"` and
   flushed at once, then flushed after every artefact, so a Ctrl-C
   run leaves an inspectable partial manifest. `finish()` stamps the
   terminal status at the end.
5. **Accounts phase** (when `--mode` is `all`, `accounts` or
   `portfolios`):
   - `GET /middlelayer/v2/portfolio/investment-overview` →
     `accounts/investment-overview.json`.
   - In `all` mode, also fetch the small ancillary endpoints into
     `accounts/*.json`: `/compliance/products`, `/contact/messages`,
     `/contact/notification`, `/contact/risk-protection` and
     `/sso/claims`. A 204 from `/contact/risk-protection` is saved
     as a `{_no_content: true, _status: 204}` marker. Each endpoint
     is independent; a failure is recorded and the run goes on.
   - If the overview fails, no portfolio or document work can run.
     The run is stamped `"incomplete"` and exits 1.
6. **Portfolios phase** (when `--mode` is `all` or `portfolios`):
   for each `portfolios[i]` in the overview, in order:
   - `GET /middlelayer/v2/Portfolio/{id}/deposits?year=YYYY` for
     every year from the `--lookback` start year to the current
     year → `portfolios/<slug>/deposits-YYYY.json`. Year is the
     smallest unit `/deposits` accepts (§4.2).
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
     stays inside the JSON files and never appears in a path.
7. **Documents phase** (when `--mode` is `all` or `documents`, and
   not `--no-documents`):
   - `GET /middlelayer/v2/documents` → `documents/index.json`. The
     full index is always written.
   - Entries whose `createDate` falls outside the window are
     skipped. An entry with no parseable date counts as in-window.
   - Each in-window `documents[].id` goes through the shared
     `collectorkit.docdedup` download-avoidance engine, keyed
     `(doc_id,)`. The mode follows the document kind, the same
     `fileName`-derived label `load.py` parses on, so a parsed kind
     is never mis-linked:
     - Executed-once immutable kinds that `load` never parses (fee
       statements, pension agreements and plans, investor profiles,
       account-opening documents) → **link**. An identical PDF from
       a prior complete run is hardlinked into this run dir and the
       fetch is skipped. A hardlink error falls through to a real
       fetch, so a document degrades to a fetch, never to a miss.
     - Parsed or tax-adjacent kinds (quarterly reports, credit
       notes, leaving statements) and any unrecognised kind →
       **fetch-verify**, the safe default. The PDF is always fetched
       with `GET /middlelayer/v2/document/{id}` and compared to the
       prior copy. A byte-identical one is hardlinked to reclaim
       disk; a re-issue keeps its fresh bytes.
   - `application/pdf` 200 → `documents/<id>.pdf`. Any other
     content type → `documents/<id>.unexpected.<ext>`. That yields
     no PDF and counts as a per-document error.
   - The skip index is rebuilt each run from the PDFs on disk in the
     COMPLETE prior runs. The current in-progress run is excluded, so
     it never seeds itself. A hardlink is a real in-run file, so each
     run dir stays self-contained and the loader needs no cross-run
     fallback.
   - `--documents-force` bypasses the index: every PDF is fetched
     and nothing is hardlinked.
8. `finish()` stamps the terminal `status`: `"complete"` for a
   finished walk, even when some endpoints errored. The exit code is
   0 with no recorded errors and 2 with at least one. The errors stay
   in `run.json`, and `load` ingests the dump; `dump_runs` records
   the error count.

**Run status.** The `status` lifecycle and how `load` and `prune` read
it are fleet-wide
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
relevate adds one terminal status, `"incomplete"`, for a run whose
master enumeration failed (step 5). `load` skips it and `prune`
reclaims it. The `ended_at` and `dry_run` fields are written alongside
`status`. `prune` falls back on them for a manifest with no `status`:
because `run.json` is flushed from the start, only `ended_at` stamped
on a non-`dry_run` run marks such a dump complete.

Manifest shape (as written):

```json
{
  "tool": "relevate.download",
  "schema_version": 1,
  "started_at": "...", "status": "complete", "ended_at": "...",
  "mode": "all", "dry_run": false,
  "state_minted_at": "...",
  "windows": {
    "since": "YYYY-MM-DD", "until": "YYYY-MM-DD",
    "documents_since": "YYYY-MM-DD", "documents_until": "YYYY-MM-DD"
  },
  "accounts": [
    {
      "id": NNNN, "slug": "<sha256-prefix>",
      "product_key": "FZPF", "product_name": "PensFree",
      "product_offer_id": NNNN,
      "currency": "CHF", "proposal_id": NNNN,
      "is_active": true,
      "first_investment_date": "0001-01-01T00:00:00",
      "portfolio_type_id": 0, "portfolio_status_id": 0,
      "deposits_year_window": [YYYY, YYYY]
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

`first_investment_date` carries Relevate's `0001-01-01` sentinel when
the date is unknown.

### 4.2 CLI surface

```
download.py [--state-path PATH] [--bronze-dir DIR]
            [--dry-run] [--debug]
            [--mode {all, accounts, portfolios, documents}]
            [--no-documents] [--documents-force]
            [--lookback PRESET|YYYY-MM-DD]
            [--limit-portfolios N]
            [--limit-documents N]
            [-v/--verbose]
```

Date window: `--lookback` follows the
[fleet contract](../README.md#collector-cli-conventions). `/deposits`
takes year granularity only, so `download` asks for every year from
the window's start year to the current year
(`year_from = since.year`, `year_to = until.year`). The fetch therefore
covers whole years, a superset of the window; no warning is printed.
`/documents` has no server-side date filter, so the same window
filters the per-PDF fetch by `createDate` on the client. The full
index is still written for traceability.

`--mode`, `--no-documents` and the `--limit-*` flags re-run one phase
cheaply while iterating on the loader or on one portfolio's response
shape.

`--dry-run` reuses the stored session; it never signs in and never
sends an mTAN. With no state file it exits 64, and with a dead session
it exits 1, as a real run does. With a live session it calls only the
two master listings (`investment-overview` and `documents`) and logs
how many portfolios and in-window documents a real run would fetch.
Its manifest goes to a throwaway temp dir that is removed on exit, so
nothing lands under `--bronze-dir`. `--debug` captures nothing on a
dry run.

`--debug` writes an HTTP trace to `<run>/screenshots/http-trace.jsonl`:
one line per Relevate request with its status, timing, size and
rate-limit headers. It holds no bodies and no credential. `load` never
reads it, and `prune` reclaims it.

### 4.3 Idempotency + dedup

- Each invocation gets its own timestamped run dir; a run dir is never
  rewritten.
- Document fetches avoid re-downloads across runs through
  `collectorkit.docdedup` (§4.1 step 7). Its freshness window (35
  days, keyed on `createDate`) keeps a just-issued immutable document
  out of the skip index, so a same-period correction is fetched, not
  linked.
- Silver dedups across runs on its own: `documents` is keyed by
  content hash (§7). Download-time hardlinking saves disk and fetches
  and leaves each run dir self-contained.

### 4.4 Errors + retry policy

There is no retry loop. A failed request is logged and recorded in
`manifest.errors`, and the walk moves to the next work item.

- HTTP 401 mid-run → the session expired after the initial probe.
  Recorded; the walk continues.
- HTTP 5xx or 429 → recorded; the walk continues. A retry-with-backoff
  layer would slot in here.
- A document GET returning HTML or `application/json` instead of
  `application/pdf` → saved as `documents/<id>.unexpected.<ext>` and
  recorded in `manifest.documents.unexpected_content_type`.

## 5. `load.py` (silver loader)

`load` applies the migrations on each invocation, then projects bronze
JSON into the silver schema (§7). It loads each pending dump, oldest
first, in one transaction (`accounts`, `cash_balances`, `positions`,
`instruments`, `performance_points`, `documents`). The `dump_runs` row
is the last insert before the commit, so a failure rolls the whole
dump back and the next run retries it. Which run dirs count as pending
follows the fleet `run.json` status rule
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).
`load` does not read the `deposits-YYYY.json`, `fees.json` or
`investment-allocation.json` files (§7.3).

Two PDF passes then run over every document silver indexes, each in
its own transaction:

- `load_historical_snapshots` parses the quarterly reports into
  `historical_position_snapshots` and `historical_cash_balances`.
- `load_credit_note_transactions` parses the credit notes into
  `transactions` with `source='credit_note_pdf'`. Each row takes the
  latest `dump_runs.snapshot_at`, since a PDF carries no dump time.

A PDF that fails to parse is logged and skipped. Both passes re-read
the whole archive on every load. When the `pdf_parsers` fingerprint
changes (`parser_generations`, migration 0003), `load` first drops the
rows both passes wrote, so a re-parse replaces them. If the re-parse
yields fewer rows than before, the new generation is not stamped and
the next load tries again.

`load --force` deletes the silver DB and rebuilds it from all bronze.

## 6. Bronze layout

```
$XDG_DATA_HOME/wealthdb/relevate/                     (= /data inside container)
├── 20260101T120000Z/                        one bronze dump per run
│   ├── run.json                             manifest (§4.1)
│   ├── accounts/
│   │   ├── investment-overview.json         master list
│   │   ├── compliance-products.json
│   │   ├── contact-messages.json
│   │   ├── contact-notification.json
│   │   ├── contact-risk-protection.json     204 marker when empty
│   │   └── sso-claims.json
│   ├── portfolios/
│   │   └── <sha256-16>/                     dir per portfolio, slug hides externalId
│   │       ├── deposits-YYYY.json           one per year of the window
│   │       ├── performance.json
│   │       ├── fees.json
│   │       ├── investment-allocation.json
│   │       └── modelportfolio.json
│   ├── documents/
│   │   ├── index.json
│   │   ├── <docId>.pdf
│   │   └── <docId>.unexpected.{html,json}   only if the API returned non-PDF
│   └── screenshots/http-trace.jsonl         only with download --debug
├── 20260815T120000Z/
│   └── ...
└── relevate.db                              silver SQLite (default path)
```

Path conventions:

- **Portfolio slugs are `sha256(externalId)[:16]`**, the same
  pattern as ubs-web's `<sha256-prefix>`. Raw account numbers stay
  out of any path string that could leak into shell history, `ps`
  output or error messages.
- **Document filenames key on Relevate's own document `id`**
  (a small integer). Stable across runs, so a fetch is idempotent.
- **`run.json`** is written incrementally (§4.1 step 4). Its mere
  presence does not mean the walk finished.

**Pruning.** `prune` follows the
[fleet rules](../README.md#debug-artefacts-run-status-and-pruning-bronze).
Its relevate specifics:

- From a complete dump it reclaims only `screenshots/`, the HTTP trace
  of `download --debug` (`debug_subdirs`). relevate drives no browser,
  so there are no DOM dumps or Playwright traces.
- Every other file of a complete dump is kept. The JSON payloads and
  PDFs are load inputs; the PDFs are read cross-dump by both PDF
  passes via `documents.bronze_path`.
- A non-complete dump is a crashed walk (`"in-progress"`), an
  `"incomplete"` abort, a `"dry-run"` shell, or a statusless
  manifest without a stamped `ended_at` (§4.1).
- Deleting a non-complete dump shows in silver after the next
  `load --force` rebuild.

## 7. Silver schema

SQLite + JSON1. Migrations live in `migrations/`. The loader reads
`MAX(silver_schema_version)` from `schema_meta` and applies any newer
files in order. Every change lands as a new numbered file.

Storage conventions:

- Unix-seconds-UTC integers for all timestamps.
- Stable filter columns promoted; the rest in `payload TEXT` JSON.
- Snapshot tables monotemporal on `snapshot_at` (the PK starts with
  it, so the implicit B-tree is the as-of index).
- Event tables (`transactions`) keyed by a stable external id;
  `INSERT OR REPLACE`, so re-loading converges.
- Documents content-deduped via PRIMARY KEY `content_sha256`.

### 7.1 Tables

| Table | PK | Purpose |
|---|---|---|
| `schema_meta` | `silver_schema_version` | Migration version registry. |
| `dump_runs` | `snapshot_at` | One row per ingested bronze run. Carries the `run.json` manifest verbatim in `payload`, plus `bronze_files_total` and `bronze_errors_total`. |
| `accounts` | `(snapshot_at, account_external_id)` | One row per (snapshot, Relevate portfolio). Promotes product_key, currency, portfolio_proposal_id, is_active, first_investment_date, contact_id; the rest in payload. |
| `cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | One row per (snapshot, account, currency, kind). `balance_kind` ∈ {cash, invested, current, securities, saving, investment, virtual, target_inv, target_sav}, derived from the matching `portfolios[i]` fields. |
| `positions` | `(snapshot_at, account_external_id, instrument_external_id)` | One row per (snapshot, account, modelportfolio position). **TARGET allocation**, not actual unit holdings. Promotes isin, asset_class, country_code, allocation, trading_price. |
| `instruments` | `instrument_external_id` | Slow-changing master data; an upsert advances `last_seen_at`. Keyed on `security.id`, shared across portfolios. |
| `performance_points` | `(snapshot_at, account_external_id, value_date)` | Daily time series from `/portfolio/{id}/performance`. One point per calendar day since the portfolio's inception. `value_date` is Unix seconds at the day's midnight UTC. |
| `transactions` | `transaction_external_id` | Contribution events parsed from the credit-note PDFs (`source='credit_note_pdf'`, id `credit_note:<doc id>`). The loader does not read `/deposits`, which returns no rows for vested-benefits portfolios. |
| `documents` | `content_sha256` | Content-deduped index of the PDFs on disk. `relevate_doc_id` is unique: new content under a known id is a re-issue and replaces the earlier row. `first_seen_at` is the earliest dump that captured the content; `last_seen_at` advances on later dumps. Promotes the numeric `document_type_code` and `category_code`. `doc_kind` comes from `fileName` needles (`DOC_KIND_PATTERNS`), `'other'` only for unrecognised names. |
| `historical_position_snapshots` | `(snapshot_at, account_external_id, isin)` | Per-quarter holdings parsed from the quarterly-report PDFs (migration 0002). |
| `historical_cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | Per-quarter cash and valuation figures from the same reports (migration 0002). |
| `parser_generations` | `scope` | Which generation of the PDF parsing logic produced the rows a pass holds (`collectorkit.srcfp` fingerprint, migration 0003). A moved generation drops those rows before the pass re-derives them, so a re-parse replaces rather than accumulates. |

#### 7.1.1 `accounts.product_key` is forensic, not behavioural

The two known values, `FZI` (product_name `Independent`) and `FZPF`
(product_name `PensFree`), look like two product lines with different
investment menus, fees or behaviour. They are not.

PensExpert runs these as **two legally separate foundations**. Both
offer the IDENTICAL investment menu, IDENTICAL fee schedule and
IDENTICAL strategy-configuration UI. The two-foundation structure is a
Swiss regulatory workaround. Vested-benefits law caps a customer's
vested-benefits assets at two custody accounts when transferring.
PensExpert runs `Independent` and `PensFree` side by side so a customer
can use both without moving to a third-party foundation. A customer's
assets can sit in either or both, with no difference in behaviour.

The column is kept verbatim because it is forensically useful: which
foundation an account lives in, legal-entity attribution, and
cross-checks against the credit-note PDFs that name the issuing
Stiftung. Both foundations classify identically, so `product_key` is
not a behavioural discriminator. A downstream consumer treats FZI and
FZPF the same. How gold interprets this column is owned by the
wealthdb relevate adapter — see [the canonical
model](../../DESIGN.md) and the adapter source
[`wealthdb/internal/silver/relevate/`](../../wealthdb/internal/silver/relevate/).

### 7.2 Row shape

One dump projects to:

- one `accounts` row per portfolio;
- one `cash_balances` row per (account, populated balance kind);
- one `positions` row per modelportfolio position;
- one `instruments` row per distinct security across the
  modelportfolios;
- one `performance_points` row per (portfolio, day of its history);
- `documents` rows, content-deduped across dumps.

Each `positions` row carries an `asset_class` label (from
`security.assetClass.name`) and an ISIN. Re-running `load` against the
same bronze is a no-op (the `dump_runs.snapshot_at` PK is the
idempotency anchor).

### 7.3 What's deliberately NOT a table

- **`fx_rates`**: portfolios are CHF-denominated, and `currentValue`
  comes back in `currency.currencyCode = "CHF"`.
- **`fees`**: `/portfolio/{id}/fees` returns a single `{value,
  isPercent, displayValue}` object per portfolio. The raw JSON stays
  in bronze under `portfolios/<slug>/fees.json`; silver does not
  project it.
- **`investment_allocation`**: the
  `/portfolio/{id}/investment/allocation` response
  (`{totalValuation, investmentValuation, savingValuation}`) repeats
  the `cash_balances` rows the loader derives from
  `investment-overview`. The raw response stays in bronze.
- **`deposits`**: `/deposits` returns no rows for vested-benefits
  portfolios (§1). The yearly files stay in bronze.

## 8. Identity strategy

The silver-side identity choices for the columns the loader
promotes. How gold interprets these columns (the
`tax_wrapper` / `management_style` / `account_kind` mapping) is
owned by the wealthdb relevate adapter — see [the canonical
model](../../DESIGN.md) and the adapter source
[`wealthdb/internal/silver/relevate/`](../../wealthdb/internal/silver/relevate/).

### 8.1 `account_external_id`

**Use `portfolios[].externalId`** (the `NNNN.NNNNNN.N` form,
e.g. `1234.567890.0`). It is the foundation-issued account number
and is stable across sessions. The `id` field is the foundation's
internal DB key. It serves as a join column but not as the canonical
external identifier.

### 8.2 `instrument_external_id`

`positions` and `instruments` key on `security.id`, Relevate's
numeric security key, and carry the ISIN in their own `isin` column
(wealthdb gold's `instruments.isin` is indexed for cross-bank joins).
The quarterly-report tables key on the ISIN, because the PDFs never
print `security.id`.

## 9. Container + wrapper architecture

### 9.1 Image

`wealthdb/base-python` + `requirements.txt` (requests, pypdf,
pytest). No browser stack: the toolkit replays REST calls and parses
PDFs.

### 9.2 Wrapper subcommand surface

```
./relevate <verb>

build       Build the Docker image.
login       login.py — mint a session, or probe it with --check.
download    download.py — fetch bronze from /middlelayer/v2/.
load        load.py — parse bronze into silver SQLite.
prune       prune.py — reclaim non-complete dumps and debug traces (runs on the host).
sh|bash     Interactive shell in the container.
help        Show usage.
```

The wrapper sources `~/.secrets/relevate.env` and the optional
`relevate.cfg`, manages the mounts, and allocates a TTY when stdin and
stdout are both TTYs. It refuses to evict a running container unless
`RELEVATE_FORCE_REPLACE=1` is set.

### 9.3 Mounts

The standard `/secrets` and `/data` bind-mounts follow the
[shared convention](../README.md#conventions-shared-across-collectors).
relevate adds one more:

| Container path | Host default | Purpose |
|---|---|---|
| `/debug` | `~/.cache/wealthdb/debug/relevate` | opt-in scratch logs / traces |

All mounts are read-write. No ports are published: the container is
purely an HTTP client.

## 10. What we do NOT do

The read-only surface is in [AGENTS.md](AGENTS.md) §1. The
authentication and privacy rules are in the root
[AGENTS.md](../../AGENTS.md) §2–§4. Specific to relevate:

- The SPA has `*/change` URLs (for example
  `/dashboard/depot/{id}/investment/allocation/change`). The toolkit
  never calls them, and no CLI flag triggers a write.
- Every fresh `login` prompts for the mTAN from the phone. There is
  no SMS-receiver integration and no stored second factor.
- Account numbers have the shape `NNNN.NNNNNN.N`, and AHV numbers
  `756.NNNN.NNNN.NN`. Neither belongs in a tracked file.

## 11. Open questions

1. **Session TTL.** Airlock typically defaults to 30 min idle
   / 8 h absolute, and its device-trust cookie to about 30 days.
   The real values can be measured by running `login.py --check`
   periodically after a fresh login and noting when it flips to DEAD.
2. **Session renewal.** No `/refresh` endpoint appears on the
   wire, but the SPA may issue keepalives that have not been
   captured. An idle-extending call that needs no fresh mTAN would
   let a session live longer.
3. **`documentType` + `category` enum mapping.** Labels could be
   derived by correlating the numeric codes with `fileName`
   patterns, or from a config endpoint that exposes the mapping.
4. **Pillar 3a presence.** The SPA has a `/dashboard/3a/depots`
   route. Unconfirmed: whether `investment-overview` lists 3a
   portfolios, or whether they need a separate endpoint.
5. **Actual unit holdings.** Modelportfolio gives the target
   allocation; investment-overview gives the total `currentValue`.
   No endpoint returning the actual units held (e.g. `/positions`,
   `/holdings`) is known. Until one is, live `positions` holds
   target allocations, and actual units come only from the
   quarterly reports.
6. **OTP retry behaviour.** Does Airlock invalidate the mTAN after
   one bad guess, or accept the next attempt? `login.py` allows 3
   attempts plus `resend`; whether a typo can be corrected before
   the session locks is unverified.
