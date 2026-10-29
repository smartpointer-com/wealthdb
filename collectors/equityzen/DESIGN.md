# equityzen — design notes

A read-only collector for [equityzen.com](https://equityzen.com), the
pre-IPO secondary marketplace. This collector targets the **buyer / investor**
surface: purchased interests in single-company offerings. Each offering is
structured as a **special-purpose vehicle (SPV)** — an LLC, taxed as a
partnership, that holds the underlying pre-IPO shares; a member owns an
interest in the SPV, not the shares directly. Same data shape
as the [`angellist`](../angellist/) LP collector (SPV / fund interests,
illiquid, no public quote) and a sibling of [`carta`](../carta/). Part of
the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) and [collectors/README.md](../README.md).

The API surface, auth flow, page routes, and data model below are mapped
by the `explore` harness (§3), not guessed.

Sections:

1. [Why web, not a public API — the path investigation](#1-why-web-not-a-public-api--the-path-investigation)
2. [The investor GraphQL surface](#2-the-investor-graphql-surface)
3. [Discovery: the `explore` harness](#3-discovery-the-explore-harness)
4. [Login and download](#4-login-and-download)
5. [Silver schema](#5-silver-schema)
6. [Gold adapter](#6-gold-adapter)
7. [Read-only & PII](#7-read-only--pii)

## Why a separate collector

EquityZen is the system-of-record for the pre-IPO secondary book:
the SPV interests held across one or more single-company offerings.
These are illiquid, single-issuer-per-vehicle interests with no public
ticker and no market quote — the same shape as `viac` / `relevate` and
the `angellist` LP collector, not the public-market brokers. One
collector per source keeps the silver isolated and the gold
reconciliation explicit, exactly as for every other source.

## 1. Why web, not a public API — the path investigation

No published API serves an individual buyer:

- **`status.equityzen.com/public-api` is a Statuspage.io feed, not a
  product.** It exposes only `/v3/summary.json` + `/v3/components.json`
  (uptime + incident data for EquityZen's *own* monitoring). It shows that
  an internal API exists and is monitored; it documents nothing about its
  surface or access rules.
- **No investor-facing *published* API exists.** `developers.equityzen.com`
  and `api.equityzen.com` do not accept connections; the help centre and web
  search surface no developer portal, SDK, or OpenAPI. The
  `github.com/EquityZen` org is a backend stack — Django + GraphQL
  (`graphene` / `graphene-django` forks), BlockScore (KYC), Modern
  Treasury (payments) — with no client library. There is no OAuth app
  registration, no API-key issuance, and no paid developer tier an
  individual buyer could provision.
- **But the portal SPA is backed by a clean, investor-scoped GraphQL
  API** at `POST https://equityzen.com/api/graphql/`, authenticated by the
  ordinary **Django session cookie + an `x-csrftoken` header** — i.e. the
  same session a human login mints. Its operations are explicitly
  buyer/investor-scoped (`getBuyerInvestments`, `getMyInvestmentDetails`,
  `getBuyerDocuments`, `getK1EquivalentDocuments`). This is exactly the
  data the collector needs, read-only, already shaped for the holder.

**Verdict.** There is no credentialed public API for an individual, but
the SPA's internal GraphQL endpoint *is* the investor data surface. So —
like `angellist` (API is the GP/fund-admin surface, not the LP surface)
and the bank portals — **this collector replays the buyer's browser
session and drives the SPA's own GraphQL operations** through it, rather
than scraping rendered HTML. Runtime: Docker + Camoufox, matching
`fidelity-web` / `cointracking` / `angellist` (a React SPA behind an
aggregator-grade login, likely Cloudflare/Akamai-fronted). If EquityZen
ever opens a credentialed API to individuals, this collector can be
re-pathed to host-venv without disturbing gold — silver is the only
contract gold reads, and §2 already records the operation shapes.

## 2. The investor GraphQL surface

All operations are `POST /api/graphql/`, Relay-style (cursor pagination
via `pageInfo { hasNextPage, endCursor }`, `edges → node`). The data
model is `buyer → buyerDeals → { deal, fund, company, security,
equityBlock }`, where **deal** = the offering, **fund** = the SPV (the
LLC that holds the shares), **company** = the pre-IPO issuer,
**equityBlock** = an underlying share lot. The buyer is identified by
`buyerId`; an offering by `dealId`; a share lot by `equityBlockUuid`.

| Operation | Vars | Carries (read-only) |
|---|---|---|
| `getBuyerInvestments` | `buyerId, stage, isStageOngoing` | **The list.** `buyerDeals → node { deal, fund, company, primaryTransaction, sellOrders, actions }` — one node per offering held. Paginated. |
| `getMyInvestmentDetails` | `buyerId, dealId` | **The rich per-offering view.** `deal, parentDeal, fund, company, security, primaryTransaction, transfers, distributedTransactions, buyerStageInfo, fundBankAccount, sellOrders, timeline, investmentUpdates, documents, summaryChart`. |
| `getEquityDetails` | `equityBlockUuid` | The `/equity/<uuid>/` share-lot detail: `equityBlock → node { company, security, fund, alerts, userActions }`. |
| `getBuyerDocuments` | `buyerId, company, documentType, group, isActive, year` | The document centre (paginated `documents → node { deal, fund }`). |
| `getK1EquivalentDocuments` | `buyerId, company, documentType, isActive, year` | **K-1 (equivalent) tax docs** specifically — confirms the SPV/partnership pass-through. |
| `getBuyerInfoForDocuments`, `getTaxCompletedDocumentsYearsEzConstant` | `buyerId` / `shortCode` | Supporting: buyer info + the list of available tax years for filtering. |
| `submitLogIn`, `loginTotp` | `input` | **Auth mutations** (see §4). The only mutations the collector issues. |

**Field→fact mapping** (drives §5): basis ← `primaryTransaction`; current
FMV ← `deal` / `security` valuation; share count ← `security` /
`equityBlock`; status ← `buyerStageInfo`; distributions ←
`primaryTransaction.distributedTransactions`; movements ← `transfers`.

**Structure of the responses** (no values recorded):

- **Single- vs multi-company is a native field.** `assetClass` ∈
  `{ASSET_COMPANY, ASSET_MULTI_COMPANY_FUND}` distinguishes a
  single-company SPV from a multi-company fund directly — no inference
  from company counts needed. Maps to silver `offerings.kind` (§5) and
  gold `asset_class` (§6).
- **Distributions are in the structured API.**
  `primaryTransaction.distributedTransactions[]` carries distribution
  events, with a `type` enum of `{ACH, DISTRIBUTION}`.
  So cash flows are derivable from GraphQL — including distributions
  with no obvious UI record.
  The K-1 / capital-account-statement PDFs corroborate but are not the
  only source (§5).
- **Document types** (`documentType` enum): `K1`,
  `CAPITAL_ACCOUNT_STATEMENT` (the quarterly statements), `SUMMARY_SHEET`,
  plus onboarding `SUITABILITY` / `W_8`. `sellOrders` lists active
  listings (may be empty).

**Not on the read path.** The session also fires marketplace / discovery
/ telemetry operations — `getInvOpps`, `getCarousels`, `searchGlobal`,
`getWatchlistIois`, `companiesWithIoiInfo`, `getReserveInvestmentDeal`,
`getBuyerOfferingsTrackingProperties`, and `createPVR` (a page-view-record
telemetry mutation the SPA fires automatically, **not** a financial
write). `download.py` issues only the read queries above plus the two
auth mutations; it must not call `createPVR` or any IOI / reserve /
order operation. See [AGENTS.md](AGENTS.md).

## 3. Discovery: the `explore` harness

`explore.py` launches Camoufox in the container's Xvfb display, opens
`/accounts/login/`, pre-fills the credentials (origin- and login-path-
gated; never submits), and records three artefact channels under
`/debug/<UTC-ts>/`: **HAR** + a crash-safe `network.jsonl` (the primary
signal — this is where the GraphQL operations in §2 are read from), an
opt-in **Playwright trace** (`--trace`; DOM + screenshots), and a
**clicks.jsonl** click / lifecycle log (VNC clicks bypass Playwright's
API). Credentials + password fields are redacted from `network.jsonl`,
and the HAR is rewritten with its credentials, cookies and query-string
tokens out once the context close has written it. The trace is the one
artefact nothing redacts: it cannot be cleaned after the fact, because
its DOM snapshots store every input's value — which is why it is off by
default. The persistent Camoufox profile carries the post-TOTP session
between runs.

The explore capture is the source of the login selectors (§4), the
GraphQL surface (§2), the page route map (§4), the cookie and CSRF auth,
and the document set: statement and K-1-equivalent PDFs plus some zip
bundles from the document and tax centre.

## 4. Login and download

### login — SPA form + CLI-MFA, persistent profile

Runs headed Camoufox under the container's Xvfb (no VNC) — the proven
stealth fingerprint, not a headless guess — on a persistent context at
`/secrets/equityzen-profile/`. Navigate
to `/accounts/login/`; short-circuit if a prior session is still valid;
else fill credentials and complete the two-step auth, reading the 6-digit
**TOTP** from stdin. A typed code that is not six digits is asked for
again, up to three times. `EQUITYZEN_USERNAME` (alias `EQUITYZEN_EMAIL`) /
`EQUITYZEN_PASSWORD` from the env file; `--totp CODE` supplies the code
non-interactively; `--debug` saves a screenshot to `--screenshot-dir`
(default `/debug`) and logs the auth GraphQL ops on failure.

**Concrete flow and selectors** (from the explore capture; each item is a
trap a naive flow falls into):

- email `input#email` (type=text, **no `name`** — id/placeholder are the
  only hooks), password `input#password`. The login form submits on
  **Enter** in the password field → `submitLogIn`.
- TOTP `input#oneTimePassword` (single text input, `inputmode=numeric`,
  placeholder "Enter your code"). **The 2FA card submits via a
  `<button type="button">Submit</button>` onClick handler — NOT a form
  submit and NOT auto-submit-on-6-digits.** Enter and typing-alone both
  hang; the code must be typed and then the **Submit button clicked** →
  `loginTotp`. login.py types the digits as real keystrokes then clicks the
  Submit button (Enter / auto-submit kept only as fallbacks).
- Each field is filled clear → fill → **verify**, and Firefox's password
  manager is disabled (the `signon.*` prefs in the shared
  [`collectorkit.launch`](../../shared/collectorkit/collectorkit/launch.py)
  pref set) so a saved credential can't autofill on top and concatenate the
  password.
- Auth state is decided **purely by the URL leaving `/accounts/login`**
  (the whole login + TOTP flow stays on that path; a valid session
  redirects to `/welcome/`). The "#email-absent" heuristic is deliberately
  not used — the TOTP step also lacks `#email` and that false-positived a
  half-finished login as complete.

EquityZen 2FA is **TOTP-only** (no SMS/email, no "remember this device"
checkbox), but closing the browser does **not** log the account out — the
session cookie in the profile dir stays valid until an explicit logout or
server-side expiry, so the renewal short-circuit keeps later runs from
asking for a fresh TOTP code (cf. `cointracking`). **Never log out at the end of
any verb.** `--check` probes the dashboard and exits 0/1 without asking for
a code.

### download — per-surface bronze capture

Runs headed Camoufox under Xvfb (same engine as login; the session from the
profile carries auth), navigating the SPA and capturing the GraphQL
responses the page fires — the SPA supplies the Relay-id variables, so no
query text is reconstructed. Two mechanics matter:

- **The investments list is per-stage, behind Ant-Design tabs.**
  `getBuyerInvestments` is keyed by a `stage` variable — `ONGOING`,
  `CLOSED`, `EXITED` — and each stage returns its whole set in one response
  (`pageInfo.hasNextPage=False`, `edges == totalCount`; no pagination). The
  `/portfolio/` page exposes them as tabs (`.ant-tabs-tab-btn`: "Ongoing",
  "Closed (N)", "Exited (N)"); download clicks each tab and captures that
  stage's response. **Replaying the query directly through the GraphQL
  endpoint — even byte-matching the SPA's body + `x-csrftoken` — returns a
  server-side `SYSTEM_ERROR` (500)**, so the SPA issues the request
  via the tab click instead.
- **Per-offering detail by URL.** `deal.id = base64("DealNode:<N>")`; the
  `<N>` is exactly the `/portfolio/<N>/` number. download decodes it,
  navigates the detail page, and captures the `getMyInvestmentDetails`
  response (matched on `variables.dealId`) — which carries the
  `primaryTransaction` + `distributedTransactions` + `transfers` cash-flow
  ledger plus `security`.
- **Documents (default; `--no-documents` opts out)** — fetches each offering's document
  PDF blobs (capital-account statements, K-1s, …) via
  `node.documents[].downloadUrl` through the authenticated request API into
  `documents/<deal-slug>/<doc-slug>.pdf`; load.py parses them (§5). Fetches are
  download-avoidant via the shared `collectorkit.docdedup` engine, chosen per
  **document class** (keyed `(deal-slug, doc-slug)`) — mode is a property of the
  document *type*, not the collector:
  - **fetch-verify** the parsed / restatement-prone classes —
    `CAPITAL_ACCOUNT_STATEMENT`, `K1`, `QUARTERLY_REPORT`,
    `ANNUAL_FINANCIAL_STATEMENTS`. These are parsed for figures and/or can be
    **restated/corrected under a stable Relay `doc.id`** (Relay ids are entity
    ids, not content-addressed), so they are **always fetched and
    content-compared** against the prior copy: byte-identical → hardlinked to
    reclaim disk, changed → the fresh bytes kept (a restatement is never
    missed). Link-mode here would serve a stale figure into the NAV / positions
    replay — a correctness bug, which is why link is *not* used for these.
  - **link** the executed-once legal / offering classes — `SUB_AGT`,
    `COUNTERSIGN_SUB_AGT`, `SERIES_SCHEDULE`, `SUITABILITY`, `SUMMARY_SHEET`,
    `TERMSHEET`, `OFFERING_DOC`, `FUND_W_9`, `W_8`. These are immutable once
    signed/issued and are **not parsed by load.py**, so an identical prior copy
    is **hardlinked** in and the fetch skipped (any hardlink error falls through
    to a real fetch), realizing the fetch-avoidance win at zero silver-
    correctness risk. This is an explicit, curated allow-list defined in
    `_document_class`; anything not named in it is fetched, not linked.
  - **fetch-verify** (the default) any other / unrecognised `documentType`:
    always fetched, and a byte-identical prior is still deduped. Deduping an
    identical copy is always safe, so there is no reason to fetch a document
    without verifying it — link is the only opt-in, everything else defaults
    here.

  fetch-verify reclaims exactly the same disk as link on an unchanged document
  (both collapse to one hardlinked copy); it only pays the *fetch*. So the link
  allow-list is purely a **fetch-cost** optimisation for documents load never
  parses — it buys nothing for silver, and picking it wrong on a parsed doc
  would cost correctness, which is why only the never-parsed executed-legal set
  is opted in.

  A hardlinked blob is a real in-run file, so each run dir stays self-contained
  and load needs no cross-run fallback. A document node with **no `downloadUrl`**
  yields no blob by design (load records a null local_path) — an audited outcome
  distinct from a real fetch failure, not counted as an error.
  `--documents-force` bypasses the index (fetch every blob, no dedup).

Page routes: `/welcome/` (landing), `/portfolio/` (list, tabbed),
`/portfolio/<N>/` (detail), `/equity/<uuid>/` (share lot, not captured;
see below), `/documents/` (doc + tax centre).

Layout (`collectorkit.bronze`; deal-slug = `sha256(deal.id)[:16]` so no
company name ever appears in a path): `$XDG_DATA_HOME/wealthdb/equityzen/<UTC-ts>/` with
`investments.json` (`{stage: getBuyerInvestments body}`),
`offerings/<deal-slug>/detail.json` (`getMyInvestmentDetails`;
`node.documents[]` carries the document metadata), document blobs under
`documents/<deal-slug>/` (skipped by `--no-documents`), and a `run.json` manifest
(slugs + counts only — no names/ids/amounts). `--dry-run` reuses the stored
session, captures the list across all stages, logs what it would fetch,
and writes nothing. It never signs in.

#### run.json status lifecycle + prune

The fleet rules are in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
The equityzen specifics:

- `download` writes `{"status": "in-progress"}` as the run dir's first
  artefact, right before `investments.json`. The terminal manifest with
  `"status": "complete"` overwrites it at the very end.
- `--dry-run` returns before it creates the run dir, so it leaves nothing
  behind.
- A `--debug` run that fails the auth gate leaves a run dir holding only
  its capture, with no `run.json`.
- `load` skips a run dir with no `run.json`, or one whose `status` is not
  `complete`. Such a dump holds a partial capture. Its list nodes would
  replace positions that a complete run replayed from the full ledger.
- A `run.json` without a `status` loads, and `prune` keeps it. Such a
  dump only ever got its `run.json` at the end, so the file alone means
  the walk finished.
- `prune` deletes the run dirs `load` skips once they are quiet for
  `--min-age-hours`. From a complete dump it strips only `screenshots/`
  (`debug_subdirs`).

`screenshots/` holds the opt-in `--debug` captures. One is the portfolio
list page, which shows both the auth verdict and the Ant stage tabs the
sweep clicks. The others are offering pages whose
`getMyInvestmentDetails` never fired. Only that failure path captures an
offering, so the interesting page is not buried under one capture per
offering. `load` never reads them. The remaining diagnostics live outside
bronze: `login --screenshot-dir` and the `explore` verb's
`/debug/<UTC-ts>/`.

#### Why no `/equity/<uuid>/` capture

The per-company equity pages (`/equity/<uuid>/`, enumerated via the
paginated `getEquityBlocks(buyerId)` off `/equity/`) are **not captured**.
What they carry, and why none of it is worth the extra per-company
navigations and pagination per run:

- `getInvOpps(equityBlockUuid)` → the company's **open order-book asks**
  (deals currently open to buyers). An ask need never transact, so it is
  **not reliable price data** — only *closed* deals are (the valuation
  rule). An ask exists only while a company is actively offered.
- `getIssuerInformation(equityBlockUuid)` → company **metadata** only
  (`transferFee`, `canDisplayCapTable`, `cashlessExerciseAllowed`,
  `legalOpinionRequired`) — no valuation, no price.
- `getEquityDetails` → block detail (`securityType`, `isTransactable`,
  `alerts`, `userActions`) — no price.
- `getEquityBlockInformation` → `units{totalSum, exercisedSum, verified}`.
  This can come back **empty**, so it is a
  *worse* share-count source than the holdings API.

Crucially, everything valuation needs is already in the **holdings** bronze
(`getMyInvestmentDetails`): remaining shares
(`primaryTransaction.sharesRemainingPostSplit`, populated for all
holdings), the entry price (`pricePostSplit`), the **closed secondary
prices** (`distributedTransactions[].pricePostSplit` — the tenders), and
distributions. The marketplace **live listings** are not used either, for
the same reason (order-book asks, not closed deals). If EquityZen ever exposes
a per-company *closed*-deal price feed, this is the surface to revisit — the
operation map (`getEquityBlocks` → `getInvOpps`/`getIssuerInformation`/
`getEquityDetails`/`getEquityBlockInformation`, all keyed by
`equityBlockUuid`) is recorded here for that.

## 5. Silver schema

SQLite + JSON1, source-shaped, owned by `load.py` (via
`collectorkit.silver`) and defined in `migrations/` (`0001_initial.sql`,
`0002_cash_flow_shares_price.sql`).
`snapshot_at` is INTEGER Unix-seconds-UTC; source *dates* (purchase /
tender / deal start) stay as their source ISO TEXT; stable filter columns
promoted, rest in `payload`.
**Normalized into immutable vs time-varying** (a modelling call):
identity + entry terms live once in `offerings` (keyed by
`deal_external_id`, no `snapshot_at`); only what moves between snapshots
lives in `positions`; the ledgers key by stable source id.

| Table | Grain | Notes |
|---|---|---|
| `offerings` | deal_external_id (immutable, upserted) | Identity + entry terms, fixed at purchase: `kind` (`spv` ← `ASSET_COMPANY` / `private_fund` ← `ASSET_MULTI_COMPANY_FUND`), `asset_class`, `company_*`, `fund_*`, `parent_deal_name`, `ticker_symbol`, `flavor`, `date_start`, `deal_share_price`, **`basis`** (`investmentSize`), **`purchase_price`** (`pricePostSplit`), **`shares_original`** (`sharesPostSplit`), `currency`, `last_seen_at`, `payload`. Company/fund names are PII. |
| `positions` | (deal_external_id, event_seq) | **EVENT-SOURCED valuation history** — one row per capital *event* (changes only, not a per-date portfolio snapshot): `event_seq 0` = the original investment, then each disposition (tender) in date order, then a terminal `exit` event when `EXITED`; a fund also gets `statement` events (below). Per row: `as_of_date`, `event_type`, `status`, `is_open`, `shares_held`, `cost_basis_remaining`, `price_per_share`, `market_value` (= `shares_held × price_per_share`), `distributions_cumulative`, `total_value`. **Closed-deal prices only** (entry + tender prices), so the mark steps on real transactions; for **funds**, each parsed capital-account statement is injected as a `statement` revaluation event (NAV); un-tendered SPVs carry cost. **Reconstruct holdings as of any date D**: each position's latest event with `as_of_date ≤ D` (`MAX(event_seq)`), keep `is_open=1` (drops exited). Full query inline in `migrations/0001_initial.sql`. |
| `cash_flows` | cash_flow_external_id | One row per purchase / distribution (CLOSED transactions): `deal_external_id`, `flow_date`, `kind` (`purchase` / `distribution`), `method` (e.g. `ACH`), `amount`, `shares` and `price_per_share` (the event's `sharesPostSplit` / `pricePostSplit`), `execution_fee` (gold books it as its own leg, and a purchase's fee as part of the book value), `currency`, `description`, `payload`. From `primaryTransaction` + `primaryTransaction.distributedTransactions`. |
| `tax_documents` | document_external_id | Per-offering document metadata + archive: `document_type`, `download_url`, and once `download` fetches the blob (by default; `--no-documents` skips it), `local_path` + `content_hash` + `retrieved_at`. |
| `capital_account_statements` | document_external_id | Parsed quarterly partner's Statement of Capital Account: `period_end`, `beginning_balance`, `contributions`, `withdrawals`, `transfers`, `profit_loss`, `carried_interest`, **`ending_nav`** (Net Ending Capital Account Balance = the fund's fair-value NAV). Funds only (SPVs issue no capital-account statements). |
| `k1_documents` | document_external_id | Parsed Schedule K-1 (Form 1065): `tax_year`, `is_final`, and Item L tax-basis capital account (`beginning_capital`, `current_year_income`, `withdrawals_distributions`, **`ending_capital`**). Part III box amounts are not extracted (form-grid; see statements.py). |
| `schema_meta`, `dump_runs` | — | collectorkit migration / snapshot bookkeeping. |

**Document parsing** (`statements.py`, via `pdftotext`): `download` (default)
fetches each offering's document PDFs (`node.documents[].downloadUrl`) into
`documents/<deal-slug>/<doc-slug>.pdf`; `load` parses them. **Capital-account
statements give the multi-company funds the fair-value NAV the holdings API
lacks** — each statement's `ending_nav` is injected into `positions` as a
`statement` revaluation event (kind=`private_fund` only; SPVs stay
tender-driven), so a fund's `positions` history runs investment → quarterly
NAV revaluations. The API reports only cost for a fund, so a fund's fall
below cost shows only in its statements. K-1s contribute the tax-basis
capital account. Only Item L of a K-1 is parsed; the Part III grid is not
(`pdftotext` reads the box numbers, not the amounts).

The GraphQL feed is the primary cash-flow source; these PDFs corroborate
it and backstop anything it misses — a distribution can reach a bank
account with only an email and a prose investor notice, so the documents
are worth fetching even though the API does carry the event.

Identity: `deal_external_id` = `dealId` (base64 `DealNode:N`). EquityZen has
no ISIN/CUSIP (private SPV interests) — gold instrument joins key on
`(deal, company)`, a structural difference from every public-market
collector. Money stored as REAL to source precision, currency promoted.

### Why SQLite, not DuckDB

The repo default is SQLite + JSON1; the single DuckDB exception
(`cointracking`) was driven by a window-function holdings *replay* and a
`DECIMAL(38,18)` need — neither applies here (a small set of SPV
interests with a handful of cash flows each, shape transformation only).
So this collector stays on the documented default — the same call
`angellist` and `carta` made.

## 6. Gold adapter

The gold adapter lives at `wealthdb/internal/silver/equityzen/` and is
registered with the gold engine by the blank import in
`cmd/wealthdb/main.go`. It is the closest sibling of `angellist` (an
event-sourced LP book with distributions) and a cousin of `carta`. The
package doc comment in `adapter.go` is the canonical spec; there is no
separate `docs/adapters/*.md`. Gold migration `0015` admits
`'equityzen'` as a `silver_kind`.

### Enums — no gold change needed

The source maps onto existing canonical values. `ASSET_COMPANY → spv` and
`ASSET_MULTI_COMPANY_FUND → private_fund` (`asset_class`), and the
`buy` / `sell` / `contribution` / `distribution` transaction kinds all
exist in gold.

### Account / instrument / position model

- **One account** (the silver has no buyer-id column and the id is constant
  within a relationship): `equityzen` — the **custody** account holding the
  positions (one per offering; no per-SPV accounts, no portfolio grouping)
  and the double-entry transaction pairs (see `transactions.go`).
  `account_kind = custody` (EquityZen administers the interests; the buyer
  places no trades — not a trading `brokerage`). `tax_wrapper =
  taxable_personal`, `management_style = self_directed` — the holder chooses
  *which* interests to hold; the GP management *inside* each vehicle is not
  modelled (the carta/angellist consensus). Overridable via gold
  `account_overrides`.
- **One instrument per offering**, keyed by the raw `deal_external_id`
  (matching angellist's use of `position_external_id`), `asset_class = spv |
  private_fund` (from `offerings.kind`), `name` = the company / fund label.
  A single-company SPV also carries `symbol = offerings.ticker_symbol`
  (EquityZen's per-company EZ-internal ticker, e.g. shown as "(ABCD)" in the
  portal) so `wealthdb holdings positions` displays a symbol like public equities;
  multi-company funds have none. No ISIN/CUSIP — adapter-scoped.
- **Positions** map straight off the silver `positions` event rows:
  `market_value = market_value` (the collector's chosen mark — tender price
  for SPVs, statement NAV for funds, cost otherwise). `book_value`
  (`basis.go`) for an SPV = `cost_basis_remaining` plus the purchase's
  `execution_fee` in the same proportion (`shares_held ÷ shares_original`,
  at most 1); for a fund = the capital paid in (`offerings.basis`) plus
  the whole fee. A purchase that states no fee leaves the fee out.
  `quantity = shares_held` for SPVs (NULL for funds — units are not a
  share count). `acquisition_date` = the deal's first event.

### Adapter shape

- `adapter.go` — `silver.Register`, `Open` (read-only SQLite), `Connection`.
- `status.go` — `Status` + `ChangeWindow`, driven by `dump_runs`
  (`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, idle reload = no-op;
  the latest dump is also the source's observed watermark).
  Snapshot extrema = MIN/MAX `positions.as_of_date`, stretched to the
  latest dump that dropped a deal; transaction extrema = MIN/MAX
  `cash_flows.flow_date`; the change window spans both. Silver dates
  are source ISO TEXT → converted with `strftime('%s', …)` to the canonical
  unix seconds.
- `snapshots.go` — **forward-fill**: for each distinct position event date
  in the window, emit a COMPLETE portfolio snapshot = each deal's latest
  event `≤ t` (by `event_seq`) **where `is_open = 1`** (so an exited deal
  drops out at its exit date), one `PositionChange` + one `InstrumentChange`
  per held deal, plus the single account. A deal the portal stopped
  listing drops out at the first complete dump that omitted it: a dump
  stamps every deal it lists (`offerings.last_seen_at`), and it counts as
  complete when its `run.json` says `status: complete` with the offerings
  in scope and the deals listed around it number its `investments`. A
  dump without a manifest, or one whose listing fell short, says nothing
  about the roster. That dump time is a snapshot time too, where the
  roster is emitted without the deal and every other deal carries its own
  event date as `valued_at` (`silver.Removals`, wealthdb/docs/DESIGN.md
  §6.4). An offering with no `last_seen_at` is never read as removed.
- `transactions.go` — `cash_flows` → **double-entry pairs** on the custody
  account (the account carrying the value spine, so the returns engine sees
  the flows; EquityZen does not expose the real external funding account —
  unlike angellist, whose source carries a true funding ledger — so each
  event is a *balanced* pair netting to 0, implying no cash position). The
  investment leg's kind splits by `offerings.kind`
  (the source bucket can't distinguish a membership-sale from a true
  distribution — an SPV sale and a fund distribution share the identical
  `distributedTransactions` shape, no `sellOrders`):
  - **purchase, `spv`** → `deposit` (+) + `buy` (−, with
    `quantity`/`price` from the lot). An SPV is a tax-transparent
    single-stock vehicle, so a purchase is a stock buy.
  - **purchase, `private_fund`** → `deposit` (+) + `contribution` (−, no
    lot). A fund interest is an LP capital contribution, not a share buy.
  - **distribution, `spv`** → `sell` (+, with lot) + `withdrawal` (−). The
    tax authority treats an SPV distribution as a realization of the
    underlying. (An SPV *could* retain/reinvest/lever, but that doesn't
    happen for individual-stock vehicles — the simplification holds.)
  - **distribution, `private_fund`** → `distribution` (+, no lot) +
    `withdrawal` (−). Funds routinely reinvest, so a payout is not a sale.
  - A **$0 distribution** (an exit with no proceeds, e.g. a defunct SPV) keeps the
    $0 `sell`/`distribution` leg and omits the meaningless $0 `withdrawal`.
  - An **execution fee** is its own `fee` leg, linked to the investment leg
    by `payload.fee_for` with a `fee_role`. On a purchase it is charged on
    top, so the `deposit` carries amount + fee; on a distribution it is
    deducted from the proceeds, so the `withdrawal` carries amount − fee.
    Either way the bank leg matches the cash that crossed, to the cent.
    Every leg links to the offering's instrument. EquityZen is funded
    upfront, so there are no capital calls beyond the initial purchase.
- `basis.go` — the book value and its basis stamp (derived; `average` for
  an SPV, `paid_in` for a fund).
- `classmap.go` — `offerings.kind` → `asset_class`.
- `policy.go` — registers the shared `returns.PrivateMarketLedgerPolicy`:
  the ledger's `deposit`/`withdrawal` legs are real dated cash crossings of
  the EquityZen boundary and count as external capital; the paired
  investment legs stay internal (counting both halves would cancel every
  event). `CapitalCallRisk` keeps the `nav_only_capital_call_risk` tag on
  flow-less windows.
- `lotpolicy.go` — registers `lots.StatedPolicy`: the cost basis is what
  the source states, so the lot engine builds no lots and realizes a sale
  against that basis (`wealthdb/docs/LOTS.md`).

The parsed `capital_account_statements` / `k1_documents` stay silver-only:
the statement **NAV already reaches gold via the `positions` `statement`
events**, and K-1 tax figures have no canonical home (as carta's documents /
cap-calls stay silver-only).

**Exit representation.** Forward-fill drops an exited deal from snapshots at
its exit date (its latest event is the `is_open=0` exit), so exited
positions disappear from the as-of holdings — matching carta/angellist and
the public-equity convention (no tombstone row). The one exception is the
LAST open deal: dropping it would leave an empty batch, which is invisible
to gold, so the exit date instead gets the exit-day zero snapshot
(`silver.ClosureMarkerBatch` in the gold adapter) — the previous snapshot's
positions replayed at zero value, registering the closure on the exit day
itself instead of carrying the pre-exit marks forward as phantom value.

### Verification

`go test ./internal/silver/equityzen/` covers Kind / empty Status / Status +
ChangeWindow / forward-fill per event date (cost → tender → statement NAV,
exited drop-out, spv-quantity vs fund-NULL) / the double-entry transaction
pairs (deposit+buy, deposit+contribution, sell+withdrawal,
distribution+withdrawal, the $0-exit withdrawal omission) including the
**paired ledger nets to exactly 0** invariant. `basis_test.go` covers the
book-value arithmetic.

## 7. Read-only & PII

See [AGENTS.md](AGENTS.md) for the allowed and forbidden surfaces and the
private-data rules. Company names, SPV and fund names, share counts, basis
and FMV figures, and K-1 contents never enter a tracked file; examples use
synthetic placeholders.
