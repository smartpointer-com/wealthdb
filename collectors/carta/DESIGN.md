# carta — design notes

A read-only collector for [carta.com](https://carta.com), the
private-investment / cap-table platform. This collector targets the
**portfolio-holder** surface: direct shareholders and option-holders in
private companies, and fund LPs. It never acts as a corporate issuer.
The collector replays the
holder's browser session (2FA login, React SPA) via Camoufox, captures
bronze, and parses it into a source-shaped SQLite silver. Part of the
**wealthdb** suite — see [the architecture overview](../../DESIGN.md)
and [collectors/README.md](../README.md).

The collector handles **non-public-market equity**: stock options (strike
and vesting), SAFEs / convertible notes, RSUs / RSAs, and private-company
share certificates. None has a ticker or a quotable price. The gold adapter maps
them to private asset classes (§6).

Sections:

1. [Why web, not API — the path investigation](#1-why-web-not-api--the-path-investigation)
2. [Carta's data model (API map, kept as a scraping treasure-map)](#2-cartas-data-model-api-map-kept-as-a-scraping-treasure-map)
3. [Discovery via `explore`](#3-discovery-via-explore)
4. [login + download](#4-login--download)
5. [Silver schema](#5-silver-schema)
6. [Gold mapping](#6-gold-mapping)
7. [Scope](#7-scope)
8. [Read-only & PII](#8-read-only--pii)

## Why a separate collector

Carta is the system-of-record for private-company equity: the
options, SAFEs/notes, RSUs, and shares held across one or more
issuers. These are illiquid, no public ticker, no market quote — the same
illiquid data shape as the `viac` / `relevate` pension collectors and the
`angellist` LP collector, not the public-market brokers. One collector per
source keeps the silver isolated and the gold reconciliation explicit,
exactly as for every other source.

## 1. Why web, not API — the path investigation

A clean, well-documented, investor-side REST API **exists** (per the public
docs at `docs.carta.com`, including its `llms.txt` OpenAPI index), but it is
**not obtainable by an individual shareholder**:

- **The API is real and well-scoped.** OAuth 2.0 (`client_credentials` for
  own-account data), base `api.carta.com`, token at
  `login.app.carta.com/o/access_token/` (1-hour token, no refresh). A
  Portfolio-Holder family (`/v1alpha1/portfolios/...`) exposes exactly the
  holder data this collector needs, read-only, with `read_portfolio_*`
  scopes. With credentials, this would be a host-venv API collector
  (schwab-api shape).
- **But access is gated, and the gate excludes individuals.** Carta's
  developer program is invite-only: it takes a developer-portal invite code
  or a cleared waitlist. As of **2025** the program tightened further —
  third-party partner access is invite-only **and requires a valid SOC 2
  Type 2 certification**, and the partner waitlist form was deactivated.
  Carta's own notice says *companies and investment firms* that use Carta
  can still get API access to consume **their own** data on demand — i.e.
  the carve-out is for corporate customers / firms, not an individual
  holding a few grants. Provisioning client_credentials also requires
  "creating a user in your Carta account," which presumes a
  company/firm account, not a personal holder login. No pricing is
  published for individuals.
- **Verdict.** A regular shareholder realistically cannot self-provision
  API credentials for their own holdings. So, like `angellist` (whose API
  is likewise the fund-admin surface, not the LP surface — see
  the angellist collector) and the bank portals, **this
  collector scrapes the holder web UI.** Runtime: Docker + Camoufox,
  matching `fidelity-web` / `schwab-web` / `cointracking` (a React SPA
  behind an aggregator-grade login fronted by Cloudflare Turnstile bot
  detection — see §3).

If API access is ever obtained (e.g. via a company/firm account,
or by emailing `developers@carta.com` as a Carta customer), this collector
can be re-pathed to host-venv without disturbing gold — silver is the only
contract gold reads, and §2 already documents the API shapes.

### Anchor endpoints

Two anchor endpoints of the public API, and what each one serves:

- **`investors/listInvestments`** →
  `GET /v1alpha1/investors/firms/{firmId}/funds/{fundId}/investments`. This
  is the **fund-LP** surface (firm → fund → investment), *not* the
  cap-table holder surface. Carta calls LPs "Investors"; a
  shareholder/option-holder is a **Portfolio Holder**, a different family.
  Relevant only for a fund-LP holder.
- **`portfolios/.../listPortfolios`** ("List Shareholder Portfolios") →
  `GET /v1alpha1/portfolios`. The **correct holder entry point**. Returns
  flat portfolios; the "broken out by issuing company" view is the separate
  sibling `List Portfolio Issuers` (`/portfolios/{pid}/issuers`). Hierarchy:
  Shareholder → Portfolio → Issuer(s) → securities.

## 2. Carta's data model (API map, kept as a scraping treasure-map)

The documented API is the best available map of *what data exists and how
it's shaped*. The SPA calls internal endpoints with different names and
shapes (§3), but the concepts match, so this map helps read an `explore`
capture. Holder-side (`/v1alpha1/portfolios/...`), all read-only:

| Surface | Endpoint (operation) | What it carries |
|---|---|---|
| Portfolios | `GET /v1alpha1/portfolios` | `portfolioId`, `legalName`, `createTime`. |
| Issuers in a portfolio | `GET /portfolios/{pid}/issuers` | The companies held: `id`, `legalName`, `doingBusinessAsName`, `website`. |
| Option grants | `GET .../issuers/{iid}/optionGrants` | **The rich one** — see below. |
| RSUs / RSAs | `.../restrictedStockUnits`, `.../restrictedStockAwards` | Restricted-stock holdings. |
| Certificates | `.../certificates` | Common / preferred share certificates. |
| Convertible notes | `.../convertibleNotes` | SAFEs / convertible notes. |
| Transactions | `.../securityTransactions` | 3 event types (below). |
| 409A FMV | `.../issuers/{iid}/fairMarketValue` | Fair market value for the issuer. |

**Option grant fields** (the novel, no-precedent shape):
- quantities: `quantity`, `outstandingQuantity`, `vestedQuantity`,
  `exercisedQuantity`, `canceledQuantity`, `forfeitedQuantity`,
  `expiredQuantity`, …
- price: `exercisePrice` (Money `{currencyCode, amount}`) — the **strike**.
- type: `stockOptionType` (ISO / NSO / OTHER), `isoNsoSplit`,
  `earlyExercisable`.
- dates: `issueDate`, `vestingStartDate`, `grantExpirationDate`,
  `lastExercisableDate`, `terminationDate`, …
- `vestingSchedule` + `vestingEvents[]` (`vestDate`, `quantity`,
  `isoQuantity`, `nsoQuantity`, `vested`, `performanceCondition`).
- `exercises[]` (`quantity`, `exerciseDate`, `fairMarketValueAsOfDate`,
  `status`, `exerciseType`, `certificateId`, …).

**Security-transaction types** — only three are enumerated:
`TRANSACTION_TYPE_OPTION_GRANT_EXERCISE`, `TRANSACTION_TYPE_SHARE_SALE`,
`TRANSACTION_TYPE_RSU_SETTLEMENT`. Transfers / distributions / repurchases
are *not* transactions; they show up only as quantity-field changes between
snapshots.

**Three different "prices" per security** — none a public last-price:
**strike** (`exercisePrice`), **409A FMV** (`fairMarketValue`), and
**liquidation preference / preferred price** (share-class detail). Which is
"the price" for valuing a position is settled in §5.1.

**Tax documents.** Even the API only surfaces *fund-LP* documents
(`fundInvestmentDocuments`); equity tax forms (3921 for ISO exercise,
1099-B) are not API-exposed at all — they live in the holder web UI's
documents/tax centre. So the **web scraper is the only path to the tax docs
regardless**, API access or not.

**Cadence.** Carta refreshes most operational data once a day (around noon
US Eastern time), so a second run on the same day adds nothing.

## 3. Discovery via `explore`

`explore.py` launches Camoufox in the container's Xvfb display, opens
carta.com, and records a session driven by hand over VNC:

- a HAR and `network.jsonl`, the main record of the SPA's internal JSON/XHR
  calls;
- a `clicks.jsonl` click log, since VNC clicks bypass Playwright's API;
- an optional Playwright trace (`--trace`, off by default; see
  [`collectorkit.explore`](../../shared/collectorkit/collectorkit/explore.py)).

Artefacts land under `/debug/<UTC-ts>/`, never under `/data`. The
persistent Camoufox profile carries the post-2FA session between runs.
Re-run it whenever Carta moves its UI or endpoints.

### Internal endpoints

**All endpoints are cookie-session REST returning `application/json`** (no
GraphQL); the scraper replays these GETs with the logged-in session via
Playwright's request API. Internal field names are snake_case and differ
from the public `/v1alpha1/` map of §2 — follow the *internal* shapes
below. (No values reproduced here — PII.)

- **Auth / infra.** Login at `login.app.carta.com/credentials/login/`
  (SPA), creds POSTed to `…/credentials/bff/login/`, 2FA verified at
  `…/credentials/2fa/bff/verify_challenge`. The form is
  **two-step** (same URL, client-side step change): an email screen
  (`#username`, `#email-next-btn`) then a password screen
  (`#email-display`, `#password`, `#password-continue-btn`) — neither
  button is `type=submit`, and the email step advances for any
  well-formed address (no existence check). **Cloudflare** Turnstile front
  (`/cdn-cgi/challenge-platform/`, `challenges.cloudflare.com`) — Camoufox
  cleared it. Session kept alive via `app.carta.com/common/keep_alive/`.
  Third-party noise to ignore: cloudfront, statuspage.io, stonly,
  forethought.ai, ujet.co, pendo, delighted.

- **Discovery / bootstrap.** The app lands on the portfolio,
  `app.carta.com/investors/individual/{id}/portfolio/`, or on a page
  under `/tasks/`. The individual id is read from the first
  URL that settles on the portfolio: the landing, then `/portfolio/`, then
  the account switcher's `/api/profiles/landing-page-redirect/o/{firm}/`.
  Then `/api/investors/portfolio/firm/{firm}/list_individual_portfolio_investments/{portfolio}/list/`
  returns the held entities, each: `corporation_id`, `legal_name`, `dba`,
  `entity_type`, **`is_fund_investment`** (the cap-table-vs-fund
  discriminator), `corporation_permission_data`. `account-switcher` /
  `navigation-config` seed the portfolio/firm ids.

- **Cap-table holder side** (per corporation):
  `/api/investors/holdings/portfolio/{pid}/corporation/{cid}/holdings-dashboard/`
  (`held_since`, `cash_cost`, `ownership`, …) then one list per security
  type — `…/options/`, `…/shares/`, `…/equity-grants/`, `…/rsu/`, `…/rsa/`,
  `…/warrants/`, `…/convertibles/`, `…/sar/`, `…/piu/` — each returning
  `{rows:[…], totals:{…}}`. **Row fields:** `id`, `pk_key`, `label`,
  `issue_date`, `issuable_type`, `status`, `currency`, `quantity`,
  `stock_type`, **`exercise_price`** (strike), `exercised`, `vested`,
  `exercisable`, `has_vesting`, `is_vesting`, `is_canceled`, `is_expired`,
  `is_terminated`, `is_fully_exercised`, … plus aggregate `totals`
  (`exercised`/`vested`/`quantity`/`cost_to_exercise`/`cost[{currency,value}]`).
  **Vesting:** `/api/corporations/{cid}/option_grant/{gid}/vesting-data/` —
  `vesting_start_date`, `vesting_end_date`, `vested_shares_quantity`,
  `net_total_shares_quantity`, `so_type`, `has_iso_nso_split`,
  `vesting_manager{acceleration, vesting_template{vesting_type,cliff…}}`,
  and `vesting_event_data[]` (`amount`, `cumulative`, `date`, `has_vested`,
  `vesting_type`, `progress_data{percent_exercised,total_exercised,vested}`).
  Also: `…/overview-captable/summary`, `/api/portfolio/v1/issuers/{id}/profile/`,
  exercise history (read-only GET) `/api/exercise/v2/corporations/{cid}/issuers/{iid}/exercise-requests/`,
  QSBS `/api/tax-advisory/v1/qsbs/…`, stock-based-lending pledged/collateral.

  **Liquidity events (acquisition).** When a company is acquired, the exit
  shows up in the holdings data, not as a transaction: every security flips to
  `status="Acquisition"` + `is_canceled=1` with `value` zeroed,
  `totals.quantity_canceled` carries the cancelled count, and
  holdings-dashboard's `show_offboarded_banner=true`. The gold adapter drops
  cancelled securities (no longer a current holding). The **payout / proceeds
  amount is NOT exposed to the holder**: the payments endpoint each row
  advertises via `transaction_receipts_update_url`
  (`/api/payments/corporation/{cid}/transaction-receipts/certificates/{id}/`)
  returns **403** — issuer/admin-gated. So acquisition consideration isn't
  API-obtainable; it would live in a 1099-B / payout statement, which the
  documents index does not carry. `download` makes no receipts fetch.

- **Fund-LP side** (`app.carta.com/api/investors/…` + `fund-admin.app.carta.com/…`):
  `/api/investors/portfolio/fund/{fid}/list/` (look-through portfolio
  companies); `fund-admin.app.carta.com/v2/partners/organization/{org}/fund/{fund}/metrics/`
  (the fund config/overview: `currency`, 409A flags, `fund-documents`
  {`bulk-download-url`, `lp-document-center-url`}, `wire-instructions`,
  `securities-account`); capital calls
  `/api/investors/portfolio/{pid}/list/individual_lp_active_cap_calls/`;
  `commitment/{id}/check_acceptance`.

- **Documents (the archive).** Structured index:
  `/api/investors/individual/{id}/get-all-received-documents/` →
  `{results:[{id, uuid, document_name, document_date, document_type, fund_id,
  fund_name, firm_id, firm_name, capital_account_name, stakeholder_name,
  document_url, …}], page, count, num_pages, …}` — one row per PDF, paginated.
  LP tax docs index at
  `fund-admin.app.carta.com/firm/{firm}/tax/lp-tax-documents/metadata/`
  (`{years[], default_tax_year}`). Each PDF: GET its `document_url` (or
  `app.carta.com/investors/funds/{fid}/lp_documents/{docid}/download/` → 302 →
  `documents.carta.com/…pdf`). **Bulk option:** POST
  `/api/investors/fund/{fid}/bulk-download-received-limited-partner-documents/queue/`
  → a `documents.carta.com/{id}/documents-for-<name>.zip`, foldered
  `<Fund>/<Category>/file.pdf` with categories *Capital calls / Annual and
  quarterly report / Capital account statements / Tax / Tax - Schedule K-1 /
  Distributions*. Document types include **K-1s and other tax forms, capital
  account statements, quarterly unaudited financials, and capital-call &
  distribution activity notices**. Prefer the structured index for idempotent
  per-doc capture; the bulk ZIP is a convenience fallback.

  **Download-avoidance (`collectorkit.docdedup`).** Two orthogonal layers cover
  the per-PDF fetch. The within-run guard fetches each `document_url` once even
  when it recurs across index pages. The cross-run layer classifies each row by
  `document_type` (case-folded substring, fetch-verify-first): a capital-account
  statement (parsed by `load`) or a tax document (K-1 / 1042-S / 1099) is
  restatement-prone under its stable doc id, so it is **always** re-fetched and
  byte-compared — an unchanged copy is hardlinked to reclaim disk, a re-issue
  keeps its fresh bytes. Capital-call and distribution notices are fetch-verified
  for the same reason — `load` parses them for the due date and the amount. Only
  an executed-once archival report nobody parses (quarterly & annual financials)
  is immutable, so an identical copy from a prior complete run is **hardlinked
  in** and the fetch skipped, provided that copy still passes
  `docdedup.is_pdf` — link mode is the one mode that never re-fetches, so a
  wrong copy would otherwise be carried forward for ever. Any hardlink error
  falls through to a real fetch; any other/unknown `document_type` is
  fetch-verified (the safe default). The classifier is fetch-verify-first and
  its statement keywords are a case-folded superset of every trigger `load`
  parses, so nothing `load` parses can fall to link-mode. A hardlink is a real in-run file, so run
  dirs stay self-contained and the loader is unchanged; `--documents-force`
  bypasses the cross-run index (re-fetch everything).

**Implication:** the "worst case, just scrape the PDFs" fallback is *not*
needed for the holdings — holdings, grants, vesting, exercises, cap calls,
and the latest fund metrics are all clean JSON. PDFs (K-1 / statements /
financials / notices) are captured as a document **archive** (metadata →
silver `documents` table; blobs → disk). `load` parses the ones that carry
figures the JSON lacks: capital-account statements and notices (§5.1, §5.2)
and K-1s (§5.3). Filenames embed the holder's legal name + fund name → slug
on capture; never let them reach the repo.

## 4. login + download

### login — SPA form + CLI-MFA, persistent profile

Mirrors the `cointracking` / `fidelity-web` pattern: launch a persistent
browser context on `/secrets/carta-profile/`, navigate to Carta,
short-circuit if a prior session is still valid, else walk the two-step
form (§3: email screen, then password screen; a single-screen variant is
also handled) with `CARTA_USERNAME` (or the legacy `CARTA_EMAIL`) and
`CARTA_PASSWORD`, submit, read the 2FA code from stdin, tick the "remember
this device" box (`#enable-bypass`), and `context.close()` to flush the
session back to the profile. `--check` loads the app and exits 0 or 1. It
posts no credentials and fires no 2FA push.

### download — per-surface bronze capture

With the session from login, the walk replays the SPA's internal JSON
endpoints (§3) through Playwright's authenticated request API — GET only.

Layout (`collectorkit.bronze`): `$XDG_DATA_HOME/wealthdb/carta/<UTC-ts>/`
with

- `bootstrap/{navigation-config,account-switcher,investments}.json` — the
  ids the rest of the walk is built from.
- `entities/<slug>/` per held entity: `meta.json`,
  `holdings-dashboard.json`, one file per security type (`shares`,
  `options`, `rsu`, `rsa`, `warrants`, `convertibles`, `sar`, `piu`,
  `equity-grants`), `overview-captable-summary.json`,
  `post-money-list.json`, `vesting/grant_<gid>.json`, the exercise-detail
  xlsx under `exercises/`, and on the fund side
  `fund-{tabs,companies,cap-calls}.json` +
  `fund-admin/{app-init,partner-metrics}.json`.
- `documents/index.json` + the PDF blobs.
- `run.json` — the manifest.

The entity slug is `corp_<id>` / `fund_<id>`: the numeric Carta id is a
surrogate key, not a name, and the bronze tree is gitignored, so the path
leaks nothing while staying greppable. Legal names stay inside `meta.json`.

Everything is pulled in full — holdings, activity and documents alike — so
the shared `--lookback` flag is accepted for fleet uniformity
but cannot narrow the walk: it is validated, warned about, and otherwise
ignored. `--no-documents` skips the documents pass (the run's dominant
cost). `--dry-run` lands in the app with the stored session, lists the held
entities, logs the work-list and writes nothing. It never signs in: a
session that bounced to the login host fails the run.

### run.json status and prune

`run.json` follows the
[fleet status lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze).
The carta specifics:

- A run dir with no `run.json` is non-complete. `load` skips it and
  `prune` reclaims it.
- A manifest without a `status` key counts as complete. Such a dump only
  got its `run.json` as the walk's final step.
- `--dry-run` creates no run dir, so it leaves nothing to classify.
- `prune` removes one thing from a complete dump: `screenshots/`. That is
  where `download --debug` puts the landing page's DOM and screenshot.
  The landing page is carta's only rendered surface; every later fetch is
  JSON replay whose failures land in `run.json`'s `errors`. `--debug` is a
  no-op under `--dry-run`.
- The `explore` artefacts live in the debug dir outside bronze. The
  wrapper passes that dir to `prune` as `--debug-dir`.
- `load` runs in the container only because it needs `poppler-utils`.
  `prune` runs host-side.

## 5. Silver schema

SQLite + JSON1, source-shaped, owned by `load.py` (via
`collectorkit.silver`), in `migrations/`. Timestamps INTEGER
Unix-seconds-UTC; stable filter columns promoted, rest in `payload`; snapshot
tables monotemporal on `snapshot_at` — which is the **event date**, not the
download time (§5.1). The internal API the scraper actually hit (§3) is
snake_case and shaped differently from the public `/v1alpha1/` map of §2, so
the tables follow the internal responses.

| Table | Grain | Notes |
|---|---|---|
| `entities` | (snapshot_at, entity_external_id) | One per held entity (corporation or fund). `is_fund_investment` discriminator; `legal_name`, `dba`, `entity_type`, `individual_id`, `firm_id`, plus cap-table summary `held_since` / `ownership` / `cash_cost`. |
| `securities` | (snapshot_at, entity_external_id, security_type, security_external_id) | One per cap-table security line. `security_type` ∈ share / option / rsu / rsa / warrant / convertible / sar / piu / equity_grant. Promotes `quantity`, `exercise_price` (strike), `cost`, `vested` / `exercised` / `exercisable`, `has_vesting`, dates, plus `market_value` (the per-snapshot valuation, §5.1) and, on a share born from an exercise, `exercise_type` / `exercise_date` / `exercise_fmv` (§5.3); rest in `payload`. |
| `vesting_schedules` | (snapshot_at, entity_external_id, grant_external_id) | Per option/RSU grant: `so_type`, `has_iso_nso_split`, `vesting_start/end_date`, `vested_shares_quantity`. |
| `vesting_events` | (snapshot_at, grant_external_id, seq) | The dated schedule: `vest_date`, `amount`, `cumulative`, `has_vested`. **First vesting concept in wealthdb** (silver-only — §6). |
| `fund_metrics` | (snapshot_at, entity_external_id) | The LP capital account: `commitment`, `called_capital`, `capital_contributed`, `distributions`, `net_asset_value` (decimal strings, kept verbatim as TEXT), `vintage_year`, `accepted_date`, and on a statement's row its inception-to-date fees, operating income, gains and carry (§5.3). |
| `cap_calls` | (snapshot_at, entity_external_id, call_external_id) | Active LP capital calls. |
| `documents` | content_sha256 | PDF archive index (K-1 / 1042-S / statements / notices / financials), content-deduped on SHA-256; the PDF blobs stay under the bronze tree. |
| `k1_capital_accounts` | content_sha256 | One per K-1 document (§5.3): the federal face page's tax year and, for a fiscal year, its period (migration 0005), the tax capital account (item L), net short- and long-term gain (boxes 8, 9a), and cash and property distributions (box 19 A, C). |
| `capital_events` | (snapshot_at, entity_external_id, event_kind) | The reconstructed timeline (§5.1): one row per snapshot-defining event — `acquired` / `disposition` / `exercise` / `price_change` / `statement`. |
| `cash_flows` | cash_flow_external_id | The dated money ledger (migration 0003, §5.2): one positive-magnitude row per cash event — `exercise` / `exit` (cap-table, carrying `shares` + `price_per_share`), `convertible_purchase` (a SAFE / note at its principal), and `capital_call` / `distribution` (fund). `kind` carries direction; the gold adapter projects each as a balanced double-entry pair on the custody account (§6). |
| `schema_meta`, `dump_runs` | — | collectorkit migration / snapshot bookkeeping. `dump_runs.snapshot_at` is the download time (idempotency only), distinct from the content tables' event-dated `snapshot_at`. |

Identity: `entity_external_id` = Carta's `corporation_id`;
`security_external_id` = the security row's `id`; `grant_external_id` = the
option-grant id. Carta has no ISIN/CUSIP/symbol (private securities), so the
gold instrument is adapter-scoped — no cross-source join key (see §6). Money
is stored to source precision: cap-table numbers as the API's JSON numbers
(REAL), the fund-LP capital-account decimal strings verbatim as TEXT.

### 5.1 Event-driven change deltas (migration 0002)

A single download is reconstructed into a **time series of per-position
changes**, not one download-dated snapshot — otherwise historical as-of
queries are wrong (a fund NAV is a quarter-end figure; a stake cancelled on
its company's acquisition date stops being held on that date — a download
today must report neither "as of today").

A position (a cap-table security line, or a fund interest) gets a row only on
a day its state **changes** — acquisition, exercise, exercise-price change,
disposition, acquisition / cancellation, or a new statement / NAV. Same-day
changes coalesce to the end-of-day state. `snapshot_at` is that **event date**
at UTC midnight; `dump_runs.snapshot_at` stays the download time (idempotency /
provenance only). Storing only deltas means an unrelated position's event day
never forces re-storing the rest of the portfolio.

**Read (holdings as of day D):** for each position take its latest row with
`snapshot_at <= D`, then discard the ones whose latest `position_status` is
`'exited'`. An exited position drops out exactly at its disposition date; no
full-portfolio snapshot is required.

The collector reconstructs, per dump:

- **Cap-table** — with a side-loaded valuation override present (below), each
  certificate gets a `held` delta at its issue date and a re-valuation delta at
  every FMV step (so the share count *and* per-share price move over time);
  otherwise a single `held` delta at `held_since`. Either way, an exited
  holding gets an `exited` delta at the acquisition date (`canceled_date`, from
  the option-grant vesting-data).
- **Fund** — a NAV `held` delta per capital-account statement: the quarterly
  ending-capital-balance parsed from each statement PDF, with the
  statement's inception-to-date capital contributions as
  `capital_contributed` (the structured partner-metrics supplies only the
  latest quarter, at its sharing date). One document index holds the
  documents of every fund a login is a partner in. A fund's documents are
  the index rows whose `fund_id` is its `entity_external_id`. A statement or
  notice whose `fund_id` names no fund entity of the run is logged and not
  read.

**Valuation — in `securities.market_value`:** held shares →
`quantity × FMV-as-of(snapshot)`; unexercised options → 0; exited → 0. The FMV
comes from a **side-loaded valuation override** when present (below); otherwise
the Carta-derived fallback — the fair-market-value at the last exercise (parsed
from the exercise-detail xlsx; shares exercised at a strike below FMV are
worth FMV, not the strike — the spread is the taxable gain),
falling back to the highest exercised strike when no exercise detail was
captured. Funds value off `fund_metrics.net_asset_value`.

Sources (parsed at load): the per-grant exercise-detail **xlsx** (date /
shares / strike / FMV-on-exercise, captured via the option modal's `edr`
attachments; stdlib `zipfile`), and the capital-account statement **PDFs**
(quarterly NAV via `pdftotext -layout`, `poppler-utils` in the image). The
notices (§5.2) are read the same way; the K-1s (§5.3) from `pdftotext -bbox`
word boxes.

**Valuation override (single source of truth).** When a company exits, Carta
purges its historical 409A timeline, so the Carta-derived fallback can only
value held shares flat at the FMV-at-last-exercise — exact from the last
exercise onward, but over-stating the count/value for earlier dates (the count
grew through intervening exercises, at then-lower FMVs). To value the position
*per date*, a CSV named `<entity_external_id>-valuations.csv` can be
side-loaded in the bronze root (e.g. `1234567-valuations.csv`) — rows of
`YYYY-MM-DD,fmv_per_share_usd`, each carried forward to the next (`#` / blank
lines ignored), built from 409A valuation reports and stock-price notification
letters, which do not parse reliably. When found it **overrides** the
Carta-derived value: each certificate is held from its issue date and
re-valued at every FMV step, so the share count (from the certificate issue
dates) and the per-share price both move correctly over time. The fund side,
by contrast, *is* already a true per-quarter NAV series, so it needs no
override.

### 5.2 Cash-flow ledger (migration 0003)

Carta exposes holdings but not the cash mechanics — an exercise is paid from an
external bank, a fund capital call is wired straight into the SPV/fund, and
exit / distribution proceeds leave to an external account. The Carta "account"
is therefore a **sentinel** for the opaque managed accounts (Carta custody +
the fund managers' books); no real cash balance is ever observed.

`cash_flows` records the dated cash EVENTS as positive magnitudes (`kind`
carries the nature + direction), reconstructed from the data Carta does
expose:

- **`exercise`** — one per share certificate: `amount` = quantity × strike (the
  cert cost), with `shares` + `price_per_share` carried. Fully derivable from
  the cap-table certs.
- **`convertible_purchase`** — one per SAFE / convertible note at its principal
  amount: cash out on issue, but no share lot until conversion.
- **`exit`** — at the acquisition / cancellation date: Carta purges the payout,
  so recorded proceeds are **$0** (`shares` = the held total) — unless a
  side-loaded `<entity_external_id>-transactions.csv` supplies the exit legs
  (for example a sale plus the withdrawals it splits into, as canonical kinds
  the gold emits 1:1), which then replace the $0 exit.
- **`capital_call`** / **`distribution`** — from the fund's own notices
  where it issues them, else from its capital-account statements (a fund's
  documents as in §5.1). A notice states the day the money was due and the
  amount to the cent, so it is the ledger for its kind. A statement reports
  only inception-to-date figures; differencing consecutive statements (by
  date) places a flow no more precisely than the period it fell in, so it is
  the fallback for a fund that shares no notices.
  The per-period statement columns mis-align under pdftotext when `—`
  placeholders are present, so the inception-to-date column — which reads
  cleanly as the line's last amount — is differenced instead.

  Either way the fund reports what preceded Carta's coverage only as a lump:
  the earliest notice's running total less its own amount, or the first
  statement's inception-to-date figure. The notice lump is emitted as one
  residue row dated at that notice — a bound, not an event, and its
  description says so. A side-loaded `<entity_external_id>-transactions.csv`
  can itemise the lump: rows of kind `capital_call`
  or `distribution` dated on or before it are emitted at their own dates, and
  the lump keeps only what they leave. A row dated after the lump is skipped,
  and rows that exceed it are kept with a warning, since they are dated and the
  lump is not.

The gold adapter (§6.1) pairs each auto-derived event into a balanced
double-entry on the custody account, so each event nets to exactly 0; a
company's side-loaded explicit legs are emitted 1:1 (the CSV supplies both
halves, so they too net to 0), and a fund's side-loaded calls and
distributions are paired like its own.

### 5.3 Cost-basis facts (migration 0004)

Silver states each cost-basis fact bronze carries as a column. None of it is
derived: a figure is stored as the source prints it, and NULL means the
source does not state it.

**Exercise at fair-market-value.** A share certificate born from an option
exercise names its grant (`exercise_from`, a label) and its option type
(`exercise_type`, ISO or NSO). The exercise-detail xlsx states the date, the
shares exercised and the fair-market-value per share at exercise. For an NSO
that value is the shares' tax basis; for an ISO it is the AMT basis. The
certificate is issued on or some days after the exercise, so the loader
pairs them by grant and quantity: within one grant and one quantity, each
certificate, oldest first, takes the oldest unpaired exercise dated on or
before its issue date. A paired certificate carries `exercise_date` and
`exercise_fmv` on every `securities` row. `cost` stays the cash paid
(quantity × strike).

**Fund acceptance.** `fund_metrics.accepted_date` is partner-metrics'
`partner.accepted_date`, the day the fund accepted the partner, as printed.
Only rows loaded from partner-metrics carry it.

**Statement lines.** A capital-account statement prints, per line, the
period, year-to-date and inception-to-date figures. Its `fund_metrics` row
carries the inception-to-date `management_fees`, `net_operating_income`,
`realized_gain`, `unrealized_gain` and `carried_interest`, signed as printed:
a parenthesised figure is negative, and the nil dash is 0. A line the
statement does not print is NULL. Together with the contributions and
distributions these lines add up to the ending balance. When partner-metrics
shares its figures at a statement's own date, the statement adds these
lines to that row and leaves the rest of it as partner-metrics states it.
A later run that shares at the same date rewrites only the partner-metrics
columns, so the statement lines stay when that run carries no statements
(`download --no-documents`).

**K-1 tax capital.** Each tax document is searched for the federal Schedule
K-1 (Form 1065) face page; a 1042-S has none and yields no row. `k1.py`
reads the page from `pdftotext -bbox` word boxes, because `-layout` folds the
form's three columns into each other on dense lines. A word belongs to the
column its left edge falls in, and to the box whose caption is nearest above
it. An item-L amount belongs to the caption line nearest it vertically.
`k1_capital_accounts` keeps one row per K-1 document, keyed on its sha256:

- item L: `beginning_capital`, `contributions`, `net_income`,
  `other_change`, `distributions`, `ending_capital`;
- `short_term_gain` (box 8) and `long_term_gain` (box 9a);
- `cash_distributions` (box 19, code A) and `property_distributions` (box
  19, code C).

The names follow angellist's `k1_capital_accounts` without its `_minor`
suffix, since money here is a decimal string as printed. `distributions` is
the figure inside the form's own parentheses, so it is positive; every
other figure carries the sign the form prints. The fund is the index row's
`fund_id`, which is the fund entity's id. A re-issued K-1 under the same
document id replaces the row. Two documents that print the same tax year
both stay; gold reconciles them.

**Tax year and period (migration 0005).** The heading reads "For calendar
year YYYY, or tax year beginning … ending …". A calendar-year form leaves
the two dates blank. `tax_year` is then the heading's year, and
`period_start` and `period_end` are NULL. A fiscal-year form fills in the
dates; they are stored as YYYY-MM-DD. A fiscal year is filed on the form
edition of the year it begins in, so `tax_year` is the year of
`period_start`. The index's own `tax_year` counts only when the form prints
no year. Where the two disagree, the form wins and the load logs it.

**Backfill.** `exercise_type` and `accepted_date` also sit in `payload`, so
migration 0004 fills them on existing rows. The other columns, including
migration 0005's period, come from bronze files only a load parses, so
existing rows gain them on a reload from bronze (`load --force`).

### Why SQLite, not DuckDB

The repo default is SQLite + JSON1; the single DuckDB exception
(`cointracking`) is driven by a window-function holdings *replay* and a
`DECIMAL(38,18)` need — neither applies here (a small set of securities,
shape transformation only). `angellist` made the same call.

## 6. Gold mapping

The gold adapter is `wealthdb/internal/silver/carta/`. Its document,
[`wealthdb/docs/adapters/carta.md`](../../wealthdb/docs/adapters/carta.md),
owns the gold side:

- the account taxonomy (one custody account for the whole portfolio);
- the (`asset_class`, `vehicle`) pair of each company and fund;
- how it forward-fills the event-dated deltas of §5.1 into a complete
  snapshot per event date, and the exit-day zero snapshot;
- book value, lots, and the K-1 in-kind reduction;
- vesting, documents and cap calls staying silver-only.

### 6.1 Transactions — double-entry pairs on the custody account

The adapter projects the `cash_flows` ledger (§5.2) as balanced
double-entry pairs on the custody account, following equityzen. Each event
splits into an external-bank leg and a holding leg that net to zero. A
side-loaded `<entity_external_id>-transactions.csv` names canonical kinds
and is emitted 1:1. Which `cash_flows.kind` becomes which signed pair is
in the adapter document, §7.

## 7. Scope

The collector captures every holder surface:

- equity / share certificates + the latest holdings snapshot;
- options / RSUs / RSAs — grants with strike + vesting;
- SAFEs / convertible notes (schema + `convertible_purchase` cash events);
- the fund-LP capital account + active capital calls (the fund-admin surface);
- the document archive — K-1 / 1042-S / capital-account statements /
  quarterly financials — with the statements, notices and K-1s parsed.

Carta's internal API exposes no transaction ledger (exercises live inside the
grant payloads), so the dated cash flows are **reconstructed** into the
`cash_flows` table (§5.2) — exercises from the certs, convertible purchases
from the note principals, the exit at cancellation, fund calls /
distributions from each fund's notices and statements — for projection to
gold transactions per §6.1.

## 8. Read-only & PII

[AGENTS.md](AGENTS.md) lists the allowed and forbidden surfaces and the
private data this source carries.
