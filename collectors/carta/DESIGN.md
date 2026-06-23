# carta — design notes

**Implemented end-to-end.** `explore` mapped the holder UI; `login`,
`download` and `load` are built.
The silver schema is realized in `migrations/0001_initial.sql` (§5) and the
**gold adapter is built** (`wealthdb/internal/silver/carta/`, §6). The chosen
path (web scraper, not API — see the investigation below), the observed
endpoints (§3), the silver schema, and the gold mapping are all locked in.

A read-only collector for [carta.com](https://carta.com), the
private-investment / cap-table platform. This collector targets the **portfolio-holder**
surface — a direct shareholder / option-holder in private companies, not a
corporate issuer and not (by default) a fund LP. The collector replays the
holder's browser session (2FA login, React SPA) via Camoufox, captures
bronze, and parses it into a source-shaped SQLite silver. Part of the
**wealthdb** suite — see [the architecture overview](../../ARCHITECTURE.md)
and [collectors/README.md](../README.md).

This is the **first wealthdb source for non-public-market equity** — stock
options (strike + vesting), SAFEs / convertible notes, RSUs/RSAs, and
private-company share certificates. None had an established home in gold's
canonical model, which assumed public-market instruments with a quotable
price. That mismatch — resolved with two new private asset classes — is the
substance of the gold mapping (§6).

Sections:

1. [Why web, not API — the path investigation](#1-why-web-not-api--the-path-investigation)
2. [Carta's data model (API map, kept as a scraping treasure-map)](#2-cartas-data-model-api-map-kept-as-a-scraping-treasure-map)
3. [Discovery via `explore`](#3-discovery-via-explore)
4. [login + download](#4-login--download)
5. [Silver schema](#5-silver-schema)
6. [Gold mapping](#6-gold-mapping)
7. [Scope (as built)](#7-scope-as-built)
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

The task pointed at Carta's API reference. The investigation (from the
public docs at `docs.carta.com`, incl. its `llms.txt` OpenAPI index — no
live call was made) verified that a clean, well-documented, investor-side
REST API **exists**, and then found it is **not obtainable by an individual
shareholder**:

- **The API is real and well-scoped.** OAuth 2.0 (`client_credentials` for
  own-account data), base `api.carta.com`, token at
  `login.app.carta.com/o/access_token/` (1-hour token, no refresh). A
  Portfolio-Holder family (`/v1alpha1/portfolios/...`) exposes exactly the
  holder data we want, read-only, with `read_portfolio_*` scopes. If we
  could get credentials, this would be a clean host-venv API collector
  (schwab-api shape).
- **But access is gated, and the gate excludes individuals.** Carta's
  developer program is invite-only: you need a developer-portal invite code
  (or to clear a waitlist). As of **2025** the program tightened further —
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
  behind an aggregator-grade login very likely fronted by Akamai-style bot
  detection — cf. fidelity-web).

If API access is ever obtained (e.g. via a company/firm account,
or by emailing `developers@carta.com` as a Carta customer), this collector
can be re-pathed to host-venv without disturbing gold — silver is the only
contract gold reads, and §2 already documents the API shapes.

### Anchor endpoints, verified (for the record)

Two anchor endpoints were checked; both readings needed a tweak, recorded
so they aren't re-litigated:

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
it's shaped* — and the SPA the scraper drives almost certainly calls the
same or closely-related internal JSON endpoints, so `explore`'s HAR capture
should be read against this map. Holder-side (`/v1alpha1/portfolios/...`),
all read-only:

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
"the price" for valuing a gold position is a real decision (§4).

**Tax documents.** Even the API only surfaces *fund-LP* documents
(`fundInvestmentDocuments`); equity tax forms (3921 for ISO exercise,
1099-B) are not API-exposed at all — they live in the holder web UI's
documents/tax centre. So the **web scraper is actually the only path to the
tax docs regardless**, which reinforces the pivot.

**Cadence.** Carta refreshes most operational data daily (~noon ET); a
nightly run after that is the right cadence — same "nightly batch" shape as
ubs-psn.

## 3. Discovery via `explore`

`explore.py` (stub) launches Camoufox in the container's Xvfb display,
opens carta.com, and records the live session via three channels — HAR
(the primary signal for the SPA's internal JSON/XHR endpoints; read it
against the §2 map), Playwright trace (DOM + screenshots), and a
`clicks.jsonl` click log (VNC clicks bypass Playwright's API). Artefacts
land under `/debug/<UTC-ts>/`, never under `/data`. The persistent Camoufox
profile carries the post-2FA session between runs.

Questions `explore` must answer before login/download can be written:

- **Login host + selectors**, and the post-login SPA route map. Capture the
  real hosts; never hard-code a company name into source.
- **2FA factor** (TOTP authenticator / SMS / email OTP) and whether a
  "trust this device" option exists and how long it persists — this drives
  whether the collector slots into a nightly cron unattended (like
  `cointracking`'s multi-year cookie) or needs a periodic 2FA push.
- **Stealth need.** Does the authenticated surface require Camoufox, or does
  vanilla Playwright Firefox get through once a session exists? Start strict
  (Camoufox, given the likely Akamai front), relax only if telemetry says
  we can.
- **The surfaces + their internal endpoints** for: portfolio/holdings
  overview, per-issuer holdings, per-security detail (grants w/
  strike+vesting, RSUs, shares, SAFEs/notes), transactions/activity, 409A
  FMV, and the documents/tax centre. **Export availability** (does the UI
  offer CSV/PDF, or must we read the XHR JSON / render HTML?).

### Observed internal endpoints (2026-06-08 run)

The first `explore` pass  answered the above. Findings, recorded so login/download can be
written against them. **All endpoints are cookie-session REST returning
`application/json`** (no GraphQL); the scraper replays these GETs with the
logged-in session via Playwright's request API. Internal field names are
snake_case and differ from the public `/v1alpha1/` map of §2 — follow the
*observed* shapes below. (No values reproduced here — PII.)

- **Auth / infra.** Login at `login.app.carta.com/credentials/login/`
  (SPA), creds POSTed to `…/credentials/bff/login/`, 2FA verified at
  `…/credentials/2fa/bff/verify_challenge`. **Cloudflare** Turnstile front
  (`/cdn-cgi/challenge-platform/`, `challenges.cloudflare.com`) — Camoufox
  cleared it. Session kept alive via `app.carta.com/common/keep_alive/`.
  Third-party noise to ignore: cloudfront, statuspage.io, stonly,
  forethought.ai, ujet.co, pendo, delighted.

- **Discovery / bootstrap.** `app.carta.com/investors/individual/{id}/portfolio/`
  (landing) → `/api/investors/portfolio/firm/{firm}/list_individual_portfolio_investments/{portfolio}/list/`
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
  documents index does not carry. Don't re-add a receipts fetch.

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

**Implication:** the "worst case, just scrape the PDFs" fallback is *not*
needed for the numbers — holdings, grants, vesting, exercises, cap calls,
and fund metrics are all clean JSON. PDFs (K-1 / statements / financials)
are captured as a document **archive** (metadata → silver `documents`
table; blobs → disk), not parsed for figures. Filenames embed the holder's
legal name + fund name → slug on capture; never let them reach the repo.

## 4. login + download

### login — SPA form + CLI-MFA, persistent profile

Mirrors the `cointracking` / `fidelity-web` pattern: launch a persistent
browser context on `/secrets/carta-profile/`, navigate to Carta,
short-circuit if a prior session is still valid, else fill credentials
(`CARTA_EMAIL` / `CARTA_PASSWORD`), submit, read the 2FA code from stdin,
tick any "trust this device" box, and `context.close()` to flush the
session back to the profile. `--check` probes the dashboard and exits 0/1
with no 2FA push — safe for cron healthchecks.

### download — per-surface bronze capture

With the session from login, capture raw bronze per surface, preferring the
SPA's internal JSON endpoints (from explore's HAR, read against §2) and
falling back to rendered HTML / export blobs:

- **portfolios + issuers** — the holder's portfolio(s) and the companies in
  each.
- **per-security holdings** — for each issuer: option grants (with strike,
  vesting schedule, quantities), RSUs, RSAs, certificates, SAFEs/notes.
- **transactions** — exercises, share sales, RSU settlements.
- **fair market values** — the per-issuer 409A FMV.
- **documents / tax centre** — legal docs + 3921 / 1099-B PDFs, fetched via
  Playwright's authenticated request API.

Layout (`collectorkit.bronze`): `$XDG_DATA_HOME/wealthdb/carta/<UTC-ts>/` with
`portfolios.json`, `portfolios/<pid-slug>/issuers/<iid-slug>/{option_grants,
restricted_stock_units,restricted_stock_awards,certificates,
convertible_notes,security_transactions,fair_market_value}.json`,
`documents/<id>.pdf`, and a `run.json` manifest. Path slugs are
`sha256(id)[:16]` (relevate's pattern) so raw portfolio/issuer ids — which
embed private-company identifiers — never appear in a path; raw ids stay
inside the JSON. The holdings snapshot is always pulled in full; activity
and documents can be windowed via the shared `--since/--until` +
`--documents-since/until` contract. `--dry-run` walks navigation without
firing any export.

## 5. Silver schema

SQLite + JSON1, source-shaped, owned by `load.py` (via
`collectorkit.silver`), in `migrations/`. Timestamps INTEGER
Unix-seconds-UTC; stable filter columns promoted, rest in `payload`; snapshot
tables monotemporal on `snapshot_at` — which is the **event date**, not the
download time (§5.1). The internal API the scraper actually hit (§3) is
snake_case and shaped differently from the public `/v1alpha1/` map of §2, so
the tables follow the observed responses.

| Table | Grain | Notes |
|---|---|---|
| `entities` | (snapshot_at, entity_external_id) | One per held entity (corporation or fund). `is_fund_investment` discriminator; `legal_name`, `dba`, `entity_type`, `individual_id`, `firm_id`, plus cap-table summary `held_since` / `ownership` / `cash_cost`. |
| `securities` | (snapshot_at, entity_external_id, security_type, security_external_id) | One per cap-table security line. `security_type` ∈ share / option / rsu / rsa / warrant / convertible / sar / piu / equity_grant. Promotes `quantity`, `exercise_price` (strike), `cost`, `vested` / `exercised` / `exercisable`, `has_vesting`, dates, plus `market_value` (the per-snapshot valuation, §5.1); rest in `payload`. |
| `vesting_schedules` | (snapshot_at, entity_external_id, grant_external_id) | Per option/RSU grant: `so_type`, `has_iso_nso_split`, `vesting_start/end_date`, `vested_shares_quantity`. |
| `vesting_events` | (snapshot_at, grant_external_id, seq) | The dated schedule: `vest_date`, `amount`, `cumulative`, `has_vested`. **First vesting concept in wealthdb** (silver-only — §6). |
| `fund_metrics` | (snapshot_at, entity_external_id) | The LP capital account: `commitment`, `called_capital`, `capital_contributed`, `distributions`, `net_asset_value`, `vintage_year` (decimal strings, kept verbatim as TEXT). |
| `cap_calls` | (snapshot_at, entity_external_id, call_external_id) | Active LP capital calls. |
| `documents` | content_sha256 | PDF archive index (K-1 / 1042-S / statements / financials), content-deduped on SHA-256; the PDF blobs stay under the bronze tree. |
| `capital_events` | (snapshot_at, entity_external_id, event_kind) | The reconstructed timeline (§5.1): one row per snapshot-defining event — `acquired` / `disposition` / `exercise` / `price_change` / `statement`. |
| `cash_flows` | cash_flow_external_id | The dated money ledger (migration 0003, §5.2): one positive-magnitude row per cash event — `exercise` / `exit` (cap-table, carrying `shares` + `price_per_share`) and `capital_call` / `distribution` (fund). `kind` carries direction; the gold adapter projects each as a balanced double-entry pair on a sentinel funding account (§6). |
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
  ending-capital-balance parsed from each statement PDF (the structured
  partner-metrics supplies only the latest quarter, at its sharing date).

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
(quarterly NAV via `pdftotext -layout`, `poppler-utils` in the image).

**Valuation override (single source of truth).** When a company exits, Carta
purges its historical 409A timeline, so the Carta-derived fallback can only
value held shares flat at the FMV-at-last-exercise — exact from the last
exercise onward, but over-stating the count/value for earlier dates (the count
grew through intervening exercises, at then-lower FMVs). To value the position
*per date* a user may side-load a CSV named `<account_external_id>-valuations.csv`
in the bronze root (e.g. `1234567-valuations.csv`) — rows of
`YYYY-MM-DD,fmv_per_share_usd`, each
carried forward to the next (`#` / blank lines ignored), built from 409A
valuation reports and stock-price notification letters, which do not parse
reliably. When
found it **overrides** the Carta-derived value: each certificate is held from
its issue date and re-valued at every FMV step, so the share count (from the
certificate issue dates) and the per-share price both move correctly over time.
The fund side, by contrast, *is* already a true per-quarter NAV series, so it
needs no override.

### 5.2 Cash-flow ledger + the sentinel funding account (migration 0003)

Carta exposes holdings but not the cash mechanics — an exercise is paid from an
external bank, a fund capital call is wired straight into the SPV/fund, and
exit / distribution proceeds leave to an external account. The Carta "account"
is therefore a **sentinel** for the opaque managed accounts (Carta custody +
the fund managers' books); we never observe a real cash balance.

`cash_flows` records the dated cash EVENTS as positive magnitudes (`kind`
carries the nature + direction), reconstructed from data we *do* have:

- **`exercise`** — one per share certificate: `amount` = quantity × strike (the
  cert cost), with `shares` + `price_per_share` carried. Fully derivable from
  the cap-table certs.
- **`exit`** — at the acquisition / cancellation date: Carta purges the payout,
  so recorded proceeds are **$0** (`shares` = the held total) — unless a
  side-loaded `<account_external_id>-transactions.csv` supplies the exit (a sale plus the withdrawals it splits into, as canonical kinds the gold
  emits 1:1), which then replaces the $0 exit.
- **`capital_call`** / **`distribution`** — from the capital-account statements.
  Each statement reports inception-to-date figures; differencing consecutive
  statements (by date) yields the per-period flow, so the running total
  reconciles to the fund's contributed-capital basis (the first statement lumps
  anything before the earliest available one). The per-period statement columns
  mis-align under pdftotext when `—` placeholders are present, so the
  inception-to-date column — which reads cleanly as the line's last amount — is
  differenced instead.

The gold adapter (§6.1) pairs each auto-derived event into a balanced
double-entry on a sentinel funding account, so its derived balance is always
exactly 0; side-loaded explicit legs are emitted 1:1 (the CSV supplies both
halves, so they too net to 0).

### Why SQLite, not DuckDB

The repo default is SQLite + JSON1; the single DuckDB exception
(`cointracking`) was driven by a window-function holdings *replay* and a
`DECIMAL(38,18)` need — neither applies here (a small set of securities,
shape transformation only). **The Phase-0 task brief sketched "DuckDB
tables"; this design overrides that to stay on the documented default** —
revisit if a DuckDB silver is in fact wanted (same call as
`angellist` made).

## 6. Gold mapping

The gold adapter is **built** — `wealthdb/internal/silver/carta/`, documented
in [`wealthdb/docs/adapters/carta.md`](../../wealthdb/docs/adapters/carta.md).
It projects this silver into canonical `accounts` / `instruments` /
`positions`, plus a **planned** `transactions` projection from the cash-flow
ledger (§6.1 — not yet built; the gold layer is locked by concurrent work).
What the earlier "open questions" posed, as resolved:

- **Asset classes** — two new canonical values in
  `internal/canonical/enums.go`: `private_fund` (the fund LP interest) and
  `private_equity` (all cap-table securities, options included — a private
  ESO isn't lumped with exchange-traded `option`, nor a private share with
  public `equity`). `asset_class` carries no SQL CHECK (Go-validated), so the
  only gold migration was `0013`, widening the `silver_kind` whitelist.
- **Account grain + taxonomy** — ONE gold account for the whole portfolio
  (`individual_id`); each held company is one position under it, its share
  certs / option grants aggregated as lots (brokerage-style), one instrument
  per company. `account_kind = custody`, `tax_wrapper = taxable_personal`,
  `management_style = self_directed` (the fund-vs-equity split rides on each
  position's `asset_class`, since management_style is account-level).
- **Valuation** — fund position: `market_value` = NAV, `book_value` =
  contributed capital. Cap-table position: `quantity` + `book_value` = cost,
  `market_value` = the silver `market_value` (the valuation override's
  count-as-of × FMV-as-of, else the Carta-derived fallback — §5.1).
- **Vesting / documents / cap_calls** — kept silver-only; gold has no
  canonical home for a vesting timeline or a document archive.

Non-blocking follow-ups (in the adapter doc's "Open questions"): capturing
409A FMV to value cap-table equity, and per-lot vs aggregate positions.

**Gold consumption of the event-driven silver (§5.1) — done.** The adapter
widens its change window to the content tables' `snapshot_at` span (not just
`dump_runs`) so the event-dated rows fall in-window, reads
`securities.market_value` for the cap-table value, and at each event date emits
the **forward-filled** state — every position's latest delta
`snapshot_at <= that date`, dropping `position_status='exited'` — so a complete
per-source snapshot reaches gold's as-of query and an exited holding drops out
exactly at its disposition date.

### 6.1 Transactions — the sentinel funding account

Following equityzen, the gold adapter projects the `cash_flows` ledger (§5.2) as
balanced double-entry transaction PAIRS on a sentinel funding account
(`carta-funding`, analogous to `equityzen-funding`) — distinct from the custody
account that holds the positions. Carta exposes no real funding balance, so
every event is a self-cancelling pair and the sentinel's derived balance is
always exactly 0 (a pass-through clearing account). `amount` is the positive
magnitude; the adapter signs + splits it:

| cash_flow `kind` | gold pair (signed) |
|---|---|
| `exercise`     | `deposit` (+) + `buy` (−, with shares + price) |
| `capital_call` | `deposit` (+) + `contribution` (−) |
| `exit`         | `sell` (+, with shares) + `withdrawal` (−); a $0 exit emits the $0 `sell` and omits the meaningless $0 `withdrawal` |
| `distribution` | `distribution` (+) + `withdrawal` (−) |

A **side-loaded** `<account_external_id>-transactions.csv` (§5.2) instead names
canonical kinds directly — `sell` / `withdrawal` / `deposit` / `buy` /
`contribution` — which the adapter emits **1:1** (no auto-pairing): the CSV
supplies both halves of the exit (a sale plus the withdrawals it splits into), so they net to 0 without synthesis.

Every leg links to the company's instrument (mirroring equityzen); the buy /
sell legs additionally carry the share lot + price. The funding account is an
`accounts` row of kind `cash` with **no** `cash_balance` row — the 0 is implicit
in the paired ledger (no real external balance is observed). `Status` reports
the `cash_flows` date range as the transaction extrema; the load window already
covers them (they coincide with the securities / fund_metrics deltas).
`TxKindContribution` already exists in `internal/canonical/enums.go` (added for
angellist / equityzen), so no enum change was needed.

## 7. Scope (as built)

The scope is **everything**; all of it is captured:

- equity / share certificates + the latest holdings snapshot;
- options / RSUs / RSAs — grants with strike + vesting;
- SAFEs / convertible notes (schema present);
- the fund-LP capital account + active capital calls (the fund-admin surface);
- the document archive — K-1 / 1042-S / capital-account statements /
  quarterly financials.

Carta's internal API exposes no transaction ledger (exercises live inside the
grant payloads), so the dated cash flows are **reconstructed** into the
`cash_flows` table (§5.2) — exercises from the certs, the exit at cancellation,
fund calls / distributions from the statements — for projection to gold
transactions per §6.1.

## 8. Read-only & PII

See [CLAUDE.md](CLAUDE.md). The holder UI exposes mutation surfaces
(exercise options, sell/transfer shares, funding + tax-withholding setup,
e-sign, account settings) and possibly an issuer / company-admin or
fund-admin console — all forbidden; this toolkit only navigates, filters,
and exports its own holdings. Private-company names, share counts, strikes,
FMVs, vesting data, and tax documents are PII: synthetic placeholders only
in any tracked file.
