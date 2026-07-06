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
overview](../../ARCHITECTURE.md) and [collectors/README.md](../README.md).

Unlike `angellist` / `carta`, this design is **not** a pre-explore
skeleton: a discovery (`explore`) session against the live portal has
been run, so the API surface, auth flow, page routes, and data model
below are **observed**, not guessed. What remains deferred to the
implementation pass is `login.py` / `download.py` / `load.py` themselves
and the gold-side mapping (§6).

Sections:

1. [Why web, not a public API — the path investigation](#1-why-web-not-a-public-api--the-path-investigation)
2. [The investor GraphQL surface (observed)](#2-the-investor-graphql-surface-observed)
3. [Phase 1: discovery via `explore` (done)](#3-phase-1-discovery-via-explore-done)
4. [Phase 2: login + download](#4-phase-2-login--download)
5. [Phase 3: silver schema](#5-phase-3-silver-schema)
6. [Phase 4: gold mapping — open questions](#6-phase-4-gold-mapping--open-questions)
7. [Scope for the first implementation pass](#7-scope-for-the-first-implementation-pass)
8. [Read-only & PII](#8-read-only--pii)

## Why a separate collector

EquityZen is the system-of-record for the pre-IPO secondary book:
the SPV interests held across one or more single-company offerings.
These are illiquid, single-issuer-per-vehicle interests with no public
ticker and no market quote — the same shape as `viac` / `relevate` and
the `angellist` LP collector, not the public-market brokers. One
collector per source keeps the silver isolated and the gold
reconciliation explicit, exactly as for every other source.

## 1. Why web, not a public API — the path investigation

The task pointed at two starting URLs. The verdict, recorded so it isn't
re-litigated:

- **`status.equityzen.com/public-api` is a Statuspage.io feed, not a
  product.** It exposes only `/v3/summary.json` + `/v3/components.json`
  (uptime + incident data for EquityZen's *own* monitoring). It tells us
  an internal API exists and is monitored; it documents nothing about its
  surface or access rules.
- **No investor-facing *published* API exists.** `developers.equityzen.com`
  and `api.equityzen.com` both refuse connections; the help centre and web
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
  data we want, read-only, already shaped for the holder.

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

## 2. The investor GraphQL surface (observed)

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

**Confirmed against the captured responses** (structural inspection of the
explore session, no values retained):

- **Single- vs multi-company is a native field.** `assetClass` ∈
  `{ASSET_COMPANY, ASSET_MULTI_COMPANY_FUND}` distinguishes a
  single-company SPV from a multi-company fund directly — no inference
  from company counts needed. Maps to silver `offerings.kind` (§5) and
  gold `asset_class` (§6.3).
- **Distributions are in the structured API.**
  `primaryTransaction.distributedTransactions[]` was populated (several
  entries on some investments), with a `type` enum of `{ACH, DISTRIBUTION}`.
  So cash flows are derivable from GraphQL — a real distribution that
  arrived in a bank account with no obvious UI record *does* show up here.
  The K-1 / capital-account-statement PDFs corroborate but are not the
  only source (§7).
- **Document types** (`documentType` enum): `K1`,
  `CAPITAL_ACCOUNT_STATEMENT` (the quarterly statements), `SUMMARY_SHEET`,
  plus onboarding `SUITABILITY` / `W_8`. `sellOrders` was empty (no
  active listings).

**Not on the read path.** The session also fires marketplace / discovery
/ telemetry operations — `getInvOpps`, `getCarousels`, `searchGlobal`,
`getWatchlistIois`, `companiesWithIoiInfo`, `getReserveInvestmentDeal`,
`getBuyerOfferingsTrackingProperties`, and `createPVR` (a page-view-record
telemetry mutation the SPA fires automatically, **not** a financial
write). `download.py` issues only the read queries above plus the two
auth mutations; it must not call `createPVR` or any IOI / reserve /
order operation. See [CLAUDE.md](CLAUDE.md).

## 3. Phase 1: discovery via `explore` (done)

`explore.py` launches Camoufox in the container's Xvfb display, opens
`/accounts/login/`, pre-fills the credentials (origin- and login-path-
gated; never submits), and records three artefact channels under
`/debug/<UTC-ts>/`: **HAR** + a crash-safe `network.jsonl` (the primary
signal — this is where the GraphQL operations in §2 were read from), a
**Playwright trace** (DOM + screenshots), and a **clicks.jsonl** click /
lifecycle log (VNC clicks bypass Playwright's API). Credentials +
password fields are redacted from `network.jsonl`. The persistent
Camoufox profile carries the post-TOTP session between runs.

The first real session confirmed the login form selectors (email is
`input#email type=text` — no `name`, so id/placeholder are the only
hooks; password is `input#password`), the GraphQL surface (§2), the page
route map (§4), the cookie+CSRF auth, and the document set (statement +
K-1-equivalent PDFs plus a few zip bundles, spanning the document/tax
centre).

## 4. Phase 2: login + download

### login — SPA form + CLI-MFA, persistent profile (implemented)

Implemented and exercised end-to-end. Headed Camoufox under the
container's Xvfb (no VNC) — the proven stealth fingerprint, not a headless
guess — on a persistent context at `/secrets/equityzen-profile/`. Navigate
to `/accounts/login/`; short-circuit if a prior session is still valid;
else fill credentials and complete the two-step auth, reading the 6-digit
**TOTP** from stdin. `EQUITYZEN_USERNAME` (alias `EQUITYZEN_EMAIL`) /
`EQUITYZEN_PASSWORD` from the env file; `--totp CODE` supplies the code
non-interactively; `--debug` saves a screenshot + logs the auth GraphQL
ops on failure.

**Concrete flow + selectors** (from the explore capture, and the gotchas
that cost real login attempts — recorded so they are not rediscovered):

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
  manager is disabled (`signon.*` prefs) so a saved credential can't
  autofill on top and concatenate the password (an early failure mode —
  see the Camoufox password-manager prefs).
- Auth state is decided **purely by the URL leaving `/accounts/login`**
  (the whole login + TOTP flow stays on that path; a valid session
  redirects to `/welcome/`). The "#email-absent" heuristic is deliberately
  not used — the TOTP step also lacks `#email` and that false-positived a
  half-finished login as complete.

EquityZen 2FA is **TOTP-only** (no SMS/email, no "remember this device"
checkbox), but closing the browser does **not** log the account out — the
session cookie in the profile dir stays valid until an explicit logout or
server-side expiry, so the renewal short-circuit keeps nightly runs from
firing a fresh 2FA push (cf. `cointracking`). **Never log out at the end of
any verb.** `--check` probes the dashboard and exits 0/1 with no 2FA push.

### download — per-surface bronze capture (implemented)

Implemented (headed Camoufox under Xvfb, same engine as login; the session
from the profile carries auth). It navigates the SPA and captures the
GraphQL responses the page fires — the SPA supplies the Relay-id variables,
so no query text is reconstructed. Two mechanics, learned the hard way and
recorded so they are not re-litigated:

- **The investments list is per-stage, behind Ant-Design tabs.**
  `getBuyerInvestments` is keyed by a `stage` variable — `ONGOING`,
  `CLOSED`, `EXITED` — and each stage returns its whole set in one response
  (`pageInfo.hasNextPage=False`, `edges == totalCount`; no pagination). The
  `/portfolio/` page exposes them as tabs (`.ant-tabs-tab-btn`: "Ongoing",
  "Closed (N)", "Exited (N)"); download clicks each tab and captures that
  stage's response. **Replaying the query directly through the GraphQL
  endpoint — even byte-matching the SPA's body + `x-csrftoken` — returns a
  server-side `SYSTEM_ERROR` (500)**, so we let the SPA issue the request
  via the tab click instead.
- **Per-offering detail by URL.** `deal.id = base64("DealNode:<N>")`; the
  `<N>` is exactly the `/portfolio/<N>/` number. download decodes it,
  navigates the detail page, and captures the `getMyInvestmentDetails`
  response (matched on `variables.dealId`) — which carries the
  `primaryTransaction` + `distributedTransactions` + `transfers` cash-flow
  ledger plus `security`.
- **Documents (optional, `--documents`)** — fetches each offering's document
  PDF blobs (capital-account statements, K-1s, …) via
  `node.documents[].downloadUrl` through the authenticated request API into
  `documents/<deal-slug>/<doc-slug>.pdf`; load.py parses them (§5).

Observed page routes: `/welcome/` (landing), `/portfolio/` (list, tabbed),
`/portfolio/<N>/` (detail), `/equity/<uuid>/` (share lot — investigated then
dropped, §4), `/documents/` (doc + tax centre).

Layout (`collectorkit.bronze`; deal-slug = `sha256(deal.id)[:16]` so no
company name ever appears in a path): `$XDG_DATA_HOME/wealthdb/equityzen/<UTC-ts>/` with
`investments.json` (`{stage: getBuyerInvestments body}`),
`offerings/<deal-slug>/detail.json` (`getMyInvestmentDetails`),
`documents.json` (metadata, with `--documents`), and a `run.json` manifest
(slugs + counts only — no names/ids/amounts). `--dry-run` captures the list
across all stages, logs what it would fetch, and writes nothing
(CLAUDE.md-sanctioned read-only smoke test).

#### Why no `/equity/<uuid>/` capture (investigated, then removed)

The per-company equity pages (`/equity/<uuid>/`, enumerated via the
paginated `getEquityBlocks(buyerId)` off `/equity/`) were built and trialled
as a price-signal source, then **deliberately removed**. What they carry,
and why none of it is worth the extra per-company navigations + pagination
machinery per run:

- `getInvOpps(equityBlockUuid)` → the company's **open order-book asks**
  (live deals you could buy into). An ask need never transact, so it is
  **not reliable price data** — only *closed* deals are (the valuation
  rule). Of the live book, typically only a held name that happens to be
  actively offered has any ask; other held names return nothing.
- `getIssuerInformation(equityBlockUuid)` → company **metadata** only
  (`transferFee`, `canDisplayCapTable`, `cashlessExerciseAllowed`,
  `legalOpinionRequired`) — no valuation, no price.
- `getEquityDetails` → block detail (`securityType`, `isTransactable`,
  `alerts`, `userActions`) — no price.
- `getEquityBlockInformation` → `units{totalSum, exercisedSum, verified}`.
  In practice this came back **empty for almost every block**, so it is a
  *worse* share-count source than the holdings API.

Crucially, everything valuation needs is already in the **holdings** bronze
(`getMyInvestmentDetails`): remaining shares
(`primaryTransaction.sharesRemainingPostSplit`, populated for all
holdings), the entry price (`pricePostSplit`), the **closed secondary
prices** (`distributedTransactions[].pricePostSplit` — the tenders), and
distributions. The marketplace **live listings** were also rejected for the
same reason (order-book asks, not closed deals). If EquityZen ever exposes
a per-company *closed*-deal price feed, this is the surface to revisit — the
operation map (`getEquityBlocks` → `getInvOpps`/`getIssuerInformation`/
`getEquityDetails`/`getEquityBlockInformation`, all keyed by
`equityBlockUuid`) is recorded here for that.

## 5. Phase 3: silver schema

SQLite + JSON1, source-shaped, owned by `load.py` (via
`collectorkit.silver`). **Implemented** in `migrations/0001_initial.sql`
and loaded end-to-end against real bronze. `snapshot_at` is INTEGER
Unix-seconds-UTC; source *dates* (purchase / tender / deal start) stay as
their source ISO TEXT; stable filter columns promoted, rest in `payload`.
**Normalized into immutable vs time-varying** (a modelling call):
identity + entry terms live once in `offerings` (keyed by
`deal_external_id`, no `snapshot_at`); only what moves between snapshots
lives in `positions`; the ledgers key by stable source id.

| Table | Grain | Notes |
|---|---|---|
| `offerings` | deal_external_id (immutable, upserted) | Identity + entry terms, fixed at purchase: `kind` (`spv` ← `ASSET_COMPANY` / `private_fund` ← `ASSET_MULTI_COMPANY_FUND`), `asset_class`, `company_*`, `fund_*`, `parent_deal_name`, `ticker_symbol`, `flavor`, `date_start`, `deal_share_price`, **`basis`** (`investmentSize`), **`purchase_price`** (`pricePostSplit`), **`shares_original`** (`sharesPostSplit`), `currency`, `last_seen_at`, `payload`. Company/fund names are PII. |
| `positions` | (deal_external_id, event_seq) | **EVENT-SOURCED valuation history** — one row per capital *event* (changes only, not a per-date portfolio snapshot): `event_seq 0` = the original investment, then each disposition (tender) in date order, then a terminal `exit` event when `EXITED`. Per row: `as_of_date`, `event_type`, `status`, `is_open`, `shares_held`, `cost_basis_remaining`, `price_per_share`, `market_value` (= `shares_held × price_per_share`), `distributions_cumulative`, `total_value`. **Closed-deal prices only** (entry + tender prices), so the mark steps on real transactions; for **funds**, each parsed capital-account statement is injected as a `statement` revaluation event (NAV); un-tendered SPVs carry cost. **Reconstruct holdings as of any date D**: each position's latest event with `as_of_date ≤ D` (`MAX(event_seq)`), keep `is_open=1` (drops exited). Full query inline in `migrations/0001_initial.sql`. |
| `cash_flows` | cash_flow_external_id | One row per purchase / distribution (CLOSED transactions): `deal_external_id`, `flow_date`, `kind` (`purchase` / `distribution`), `method` (e.g. `ACH`), `amount`, `execution_fee` (informative), `currency`, `description`, `payload`. From `primaryTransaction` + `primaryTransaction.distributedTransactions`. |
| `tax_documents` | document_external_id | Per-offering document metadata + archive: `document_type`, `download_url`, and once `download --documents` fetches the blob, `local_path` + `content_hash` + `retrieved_at`. |
| `capital_account_statements` | document_external_id | Parsed quarterly partner's Statement of Capital Account: `period_end`, `beginning_balance`, `contributions`, `withdrawals`, `transfers`, `profit_loss`, `carried_interest`, **`ending_nav`** (Net Ending Capital Account Balance = the fund's fair-value NAV). Funds only in practice (SPVs issue no capital-account statements). |
| `k1_documents` | document_external_id | Parsed Schedule K-1 (Form 1065): `tax_year`, `is_final`, and Item L tax-basis capital account (`beginning_capital`, `current_year_income`, `withdrawals_distributions`, **`ending_capital`**). Part III box amounts are not extracted (form-grid; see statements.py). |
| `schema_meta`, `dump_runs` | — | collectorkit migration / snapshot bookkeeping. |

**Document parsing** (`statements.py`, via `pdftotext`): `download --documents`
fetches each offering's document PDFs (`node.documents[].downloadUrl`) into
`documents/<deal-slug>/<doc-slug>.pdf`; `load` parses them. **Capital-account
statements give the multi-company funds the fair-value NAV the holdings API
lacks** — each statement's `ending_nav` is injected into `positions` as a
`statement` revaluation event (kind=`private_fund` only; SPVs stay
tender-driven), so a fund's `positions` history runs investment → quarterly
NAV revaluations. This matters: a fund can run materially below cost, so a statement NAV well
under the original commitment is invisible in the API, which only reports
cost. K-1s contribute the tax-basis capital account; their
Part III boxes are deferred (form-grid parsing).

Identity: `deal_external_id` = `dealId` (base64 `DealNode:N`). EquityZen has
no ISIN/CUSIP (private SPV interests) — gold instrument joins key on
`(deal, company)`, a structural difference from every public-market
collector. Money stored as REAL to source precision, currency promoted.

### Why SQLite, not DuckDB

The repo default is SQLite + JSON1; the single DuckDB exception
(`cointracking`) was driven by a window-function holdings *replay* and a
`DECIMAL(38,18)` need — neither applies here (a small set of SPV
interests with a handful of cash flows each, shape transformation only).
**The Phase-0 task brief sketched "DuckDB tables"; this design overrides
that to stay on the documented default** — same call `angellist` and
`carta` made. Revisit if a DuckDB silver is in fact wanted.

## 6. Phase 4: gold adapter — implemented

The gold adapter lives at `wealthdb/internal/silver/equityzen/`
(adapter / status / snapshots / transactions / classmap `.go` +
`adapter_test.go`). It is the closest sibling of `angellist` (an
event-sourced LP book with distributions) and a cousin of `carta`. The
package doc comment in `adapter.go` is the canonical spec — like
`angellist`, there is no separate `docs/adapters/*.md` (the latest
convention is the package comment).

### Enums — no gold change needed

The `asset_class` enum already carries `spv` and `private_fund` (added for
carta/angellist) and `TxKind` carries `distribution` (added for angellist).
`ASSET_COMPANY → spv`, `ASSET_MULTI_COMPANY_FUND → private_fund`, and the
`buy` / `sell` / `distribution` transaction kinds all map onto existing
values. The only gold-schema change is migration `0015`, which widens the
`silver_sources.silver_kind` whitelist to admit `'equityzen'`.

### Account / instrument / position model

- **Two accounts** (the silver has no buyer-id column and the holder has one
  relationship, so both ids are constants):
  - `equityzen` — the **custody** account holding the positions (one per
    offering; no per-SPV accounts, no portfolio grouping). `account_kind =
    custody` (EquityZen administers the interests; the buyer places no trades
    — not a trading `brokerage`). `tax_wrapper = taxable_personal`,
    `management_style = self_directed` — the holder chooses *which* interests
    to hold; the GP management *inside* each vehicle is not modelled (the
    carta/angellist consensus). Overridable via gold `account_overrides`.
  - `equityzen-funding` — a **sentinel cash account** carrying the
    double-entry transaction pairs (see `transactions.go`). EquityZen does
    not expose the real external funding account (unlike angellist, whose
    source carries a true funding ledger), so each event is a *balanced* pair
    and this account's derived balance is always **exactly 0** — a pure
    pass-through clearing account. No positions, no `cash_balance` row.
- **One instrument per offering**, keyed by the raw `deal_external_id`
  (matching angellist's use of `position_external_id`), `asset_class = spv |
  private_fund` (from `offerings.kind`), `name` = the company / fund label.
  A single-company SPV also carries `symbol = offerings.ticker_symbol`
  (EquityZen's per-company EZ-internal ticker, e.g. shown as "(ABCD)" in the
  portal) so `wealthdb holdings positions` displays a symbol like public equities;
  multi-company funds have none. No ISIN/CUSIP — adapter-scoped.
- **Positions** map straight off the silver `positions` event rows:
  `market_value = market_value` (the collector's chosen mark — tender price
  for SPVs, statement NAV for funds, cost otherwise), `book_value =
  cost_basis_remaining`, `quantity = shares_held` for SPVs (NULL for funds —
  units are not a share count). `acquisition_date` = the deal's first event.

### Adapter shape (five files, mirroring angellist)

- `adapter.go` — `silver.Register`, `Open` (read-only SQLite), `Connection`.
- `status.go` — `Status` + `ChangeWindow`, driven by `dump_runs`
  (`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, idle reload = no-op).
  Snapshot extrema = MIN/MAX `positions.as_of_date`; transaction extrema =
  MIN/MAX `cash_flows.flow_date`; the change window spans both. Silver dates
  are source ISO TEXT → converted with `strftime('%s', …)` to the canonical
  unix seconds.
- `snapshots.go` — **forward-fill**: for each distinct position event date
  in the window, emit a COMPLETE portfolio snapshot = each deal's latest
  event `≤ t` (by `event_seq`) **where `is_open = 1`** (so an exited deal
  drops out at its exit date), one `PositionChange` + one `InstrumentChange`
  per held deal, plus the single account.
- `transactions.go` — `cash_flows` → **double-entry pairs** on the sentinel
  funding account, each event netting to 0 so the account's derived balance
  is always exactly 0. The investment leg's kind splits by `offerings.kind`
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
    Every leg links to the offering's instrument. EquityZen is funded
    upfront, so there are no capital calls beyond the initial purchase.
- `classmap.go` — `offerings.kind` → `asset_class`.

The parsed `capital_account_statements` / `k1_documents` stay silver-only:
the statement **NAV already reaches gold via the `positions` `statement`
events**, and K-1 tax figures have no canonical home (as carta's documents /
cap-calls stay silver-only).

**Exit representation.** Forward-fill drops an exited deal from snapshots at
its exit date (its latest event is the `is_open=0` exit), so exited
positions disappear from the as-of holdings — matching carta/angellist and
the public-equity convention (no tombstone row).

### Verification

`go test ./internal/silver/equityzen/` covers Kind / empty Status / Status +
ChangeWindow / forward-fill per event date (cost → tender → statement NAV,
exited drop-out, spv-quantity vs fund-NULL) / the double-entry transaction
pairs (deposit+buy, deposit+contribution, sell+withdrawal,
distribution+withdrawal, the $0-exit withdrawal omission) including the
**funding ledger nets to exactly 0** invariant. An end-to-end gold load of
the real silver reproduced the expected as-of history (0 holdings
pre-investment, the book growing then an exited SPV dropping out, fund marks
tracking statement NAVs, current total matching the silver-level figure) and
the funding account's transactions sum to 0.

## 7. Scope for the first implementation pass

**DECIDED (user): offerings + positions + cash flows.** The first pass
implements:

- **offerings + positions** — `getBuyerInvestments` + per-deal
  `getMyInvestmentDetails`: the holdings snapshot (basis / FMV / shares /
  status), one position per investment.
- **cash flows** — purchase + distribution ledger (fees informative) from
  `primaryTransaction` + `primaryTransaction.distributedTransactions`
  (confirmed populated, typed `ACH` / `DISTRIBUTION`) + `transfers`. The
  GraphQL is the primary source; the **K-1 + capital-account-statement
  PDFs corroborate** (and are the backstop for anything the structured
  feed misses — a distribution can land in the bank with
  only an email + a prose investor notice, so the PDFs are worth keeping
  even though the API does carry the event).

- **tax documents (implemented)** — `download --documents` fetches each
  offering's document PDF blobs; `load` parses capital-account statements
  (fund NAV → positions `statement` events) and K-1s (tax-basis capital) via
  `statements.py`. See §5.

Still deferred:

- **K-1 Part III box amounts** (income / gains / distributions) — the IRS
  form grid defeats a naive `pdftotext` parse (grabs box numbers); needs a
  grid-aware extractor. The blobs are archived in bronze, so this is a
  parse-only follow-up.
- **`/equity/<uuid>/` per-company pages** — investigated and intentionally
  dropped (asks + metadata only); see §4.

## 8. Read-only & PII

See [CLAUDE.md](CLAUDE.md). EquityZen is a **live marketplace**: the
portal exposes order-placement, Express-Deal sell, reserve/IOI, funding,
and e-sign surfaces — all forbidden; this collector only reads the
buyer's own holdings + documents. The pre-IPO **company names** in the
book, SPV/fund names, share counts, basis/FMV figures, and K-1 contents
are PII — synthetic placeholders only in any tracked file (e.g. company
"ACME-CO", `dealId 1234`, round example figures). Raw artefacts live only
under `$XDG_DATA_HOME/wealthdb/equityzen/`, `~/.secrets/`, and the debug dir, never in
the repo.
