# fidelity-web-dump — design

Design document for the `fidelity-web-dump` toolkit. The audience
is the engineer (current author, future contributor) implementing
and maintaining `login.py`, `download.py`, and the silver loader
against the live `www.fidelity.com` UI. It is also the contract
between this silver and the `wealthdb` Fidelity adapter (which
doesn't exist yet but will follow the [`schwab` adapter](https://github.com/ptu/wealthdb/blob/main/docs/adapters/schwab.md)
shape).

This document is the planning artefact, not the journal. Decisions
that get revised should be revised here, in place. See §11 for the
explicit punch list of unknowns that remain.

The shared three-layer model (bronze on disk, silver SQLite +
JSON1, gold DuckDB cross-bank canonical) is documented in
[`schwab-api-dump/DESIGN.md`](https://github.com/ptu/schwab-api-dump/blob/main/DESIGN.md);
this document only covers what's Fidelity-specific.

## 1. Context and non-goals

### 1.1 Why a web scraper

Fidelity's OFX Direct Connect endpoint (`ofx.fidelity.com`) is
**NXDOMAIN as of 2026-05-15**. The historical channel that
Quicken / GnuCash / hobbyists used to read Fidelity data
programmatically has been retired; verification was a `dig` against
the hostname (empty) vs `www.fidelity.com` / `oltx.fidelity.com`
(both still resolving via Akamai), confirming the OFX hostname
specifically was removed rather than a transient outage. Customer-
service FAQ pages for "Quicken / Direct Connect" have been
replaced by a chat-search that dead-ends on the topic.

The official consumer-facing path for 2026 is **Fidelity Access /
Akoya** (FDX-JSON over OAuth). It is B2B-only: data-recipient
registration is geared toward fintechs and family offices, not
individual users reading their own data. Aggregators built on
Akoya (Plaid, MX, Empower) are similarly B2B.

That leaves the client-web channel — `www.fidelity.com` driven
under Camoufox (a stealth-patched Firefox; rung 3 of §6) — as the
only realistic surface for a self-service personal-archive
toolkit today.

### 1.2 Account-category model

Fidelity's account selector groups accounts under section labels.
Silver classifies each label into a `portfolios.kind` (see §4.4). A
Donor-Advised Fund has a 7-digit account id (the others are 9-digit),
so walk() auto-excludes it by length.

### 1.3 Third-party managers and institutional feeds

A `trust_managed` account can be run by a third-party investment
manager, with Fidelity as custodian. Such a manager usually reads
Fidelity's institutional feed into its own system. A feed of that
kind belongs in its own collector, not in this one. The `manual/`
bronze subdirectory holds documents that arrive out-of-band.

### 1.4 Non-goals

- **Real-time / near-real-time pull.** MFA gates every truly-fresh
  login.
- **Trade execution, money movement, account configuration.** See
  [CLAUDE.md](CLAUDE.md) §1 — the contract is read-only.
- **The Fidelity Charitable DAF.** Excluded by id length.
- **Akoya / FDX / Plaid / SnapTrade.** B2B-only.
- **Prospectuses / fund supplements / disclosures.** Out of
  document-center scope per the user's spec.
- **Cross-bank semantic alignment.** `wealthdb` gold's job.

## 2. Bronze layout

```
<bronze-root>/
├── 20260524T120000Z/
│   ├── run.json                                manifest (trigger config,
│   │                                           account_dimensions, per-phase results)
│   ├── positions/
│   │   ├── positions_summary.csv               Overview view (all accounts in one CSV)
│   │   └── positions_dividend.csv              DividendView (ex-date, yield, est. annual income)
│   ├── activity/
│   │   └── activity_<YYYYMMDD>__<YYYYMMDD>.csv one CSV per date-window (Custom-range mode),
│   │                                           or activity_past_90_days.csv (preset mode);
│   │                                           consolidated across accounts, Account Number
│   │                                           column inside.
│   ├── documents/
│   │   ├── <fidelity-supplied-filename>.pdf    statement PDFs (where served) +
│   │   │                                       per-year, per-account tax-form PDFs
│   │   │                                       (Consolidated 1099, 1099-Q, etc.)
│   │   └── …
│   ├── balances/
│   │   └── balances.html                       full-page DOM (no CSV export available;
│   │                                           per-account totals in
│   │                                           data-testid$='-totalaccountvalue-label')
│   ├── performance/
│   │   └── performance.html                    full-page DOM (no structured export;
│   │                                           return % surface only via rendered text)
│   └── screenshots/
│       └── <ts>-<label>.{html,png}             per-landmark diagnostics for selector drift
├── 20260525T120000Z/
│   └── …
├── manual/                                     user-uploaded artefacts (documents that arrive out-of-band)
└── fidelity-web.db                             silver SQLite (default location)
```

Filename conventions:

- `<account-key>` is the first 16 hex chars of
  `sha256(account_external_id)`. `ls`-ing a bronze dir does not
  expose the 9-digit Fidelity account number; the mapping lives
  inside each artefact body plus `run.json`.
- Positions: TWO CSVs, one per view, **each containing all
  in-scope accounts**. Fidelity's positions Download exports the
  consolidated all-accounts view; per-account navigation does not
  filter the output. Account dimension lives in the CSV's
  `Account Number` column.
- Activity: one CSV per date-window, **each containing all
  in-scope accounts** (consolidated; account-selector state does
  NOT scope the export). Account dimension lives in the CSV's
  `Account Number` column, same shape as positions. With a
  `--since/--until` window, the request is bisected into ≤93-day
  chunks (Fidelity's per-export cap, configurable via
  `MAX_ACTIVITY_WINDOW_DAYS`); one CSV per chunk, named
  `activity_<YYYYMMDD>__<YYYYMMDD>.csv`. With no window, the
  rolling preset (default `Past 90 days`) produces a single
  `activity_past_90_days.csv`. Retention upper-bound is set by
  Fidelity's Custom-tab `min` attribute on the date input
  (currently ~4 years back from today; backfill calls below that
  are clamped, logged, and continue).
- Documents: Fidelity's own filenames preserved. A tax-form PDF
  can carry an account or agreement number in the filename
  (Fidelity-supplied; we don't redact, but bronze is gitignored).

`run.json` records the trigger config, account inventory, and
per-phase results (success + file paths, or per-failure error
strings).

## 3. Identifier strategy

### 3.1 `account_external_id`

Fidelity uses **9-digit account numbers** for brokerage / trust /
529 accounts and a 7-digit number for the DAF. Surfaces:

| Surface | Format | Notes |
| --- | --- | --- |
| Account-selector `data-testid` | `NNNNNNNNN` (no dash) | `[data-testid="ap143528-accounts-selector-account-link-NNNNNNNNN"]` |
| CSV exports (`Account Number` column) | `NNNNNNNNN` (no dash) | |
| Statement PDF header | `NNN-NNNNNN` (with dash) | Parser strips the dash on the way into silver |

**Canonical form: 9-digit, no-dash.** Promoted as
`account_external_id`; the dashed form lives only in PDF payloads.

The DAF's 7-digit id is the in-built exclusion criterion: walk()
auto-excludes any account whose id isn't exactly 9 characters.

### 3.2 `instrument_external_id`

CUSIP when present, ticker otherwise. Fidelity's CSVs surface
CUSIP for bonds (9-char alphanumeric in the `Symbol` column) and
ticker for equities / ETFs / mutual funds. Money-market core
positions use Fidelity-internal codes (`FDRXX**` and similar; the
asterisks are footnote markers, not part of the ticker). 529-plan
positions use plan-internal codes (e.g. `XXX######` for state
529-plan target-date sleeves).

Silver should discriminate via an `instrument_kind` column
(`cusip` / `ticker` / `plan_code`) so gold can route plan-internal
codes to a different lookup than CUSIPs.

### 3.3 `transaction_external_id`

Fidelity activity CSVs do NOT carry a stable per-row identifier
across exports. Plan: synthesise a deterministic SHA-256 prefix
over the row's promoted columns (`account_external_id |
timestamp | amount | description | symbol | source_sha256`),
mirroring `schwab-web-dump`'s pattern. Silver-internal only;
gold does not attempt cross-source per-row matching on it.

### 3.4 `owner` dimension

Two values, with sub-trust extensibility:

- `self` — the user's personal accounts (529 sleeves, the
  unidentified ninth account if it turns out to be retail).
- `trust` — accounts under any of the trust agreements.

The owner discriminator is derived at silver-load time from the
`portfolio` field captured at bronze time by
`enumerate_account_dimensions()` (see §3.5). Fidelity's
account-selector groups every account under a portfolio label
that is stable across logins:
- `Authorized` → `trust`
- `Education` → `self`
- `Fidelity Charitable® Giving` → out-of-scope (DAF, auto-
  excluded by 7-digit-id length check before walk reaches it)

### 3.5 `account_dimensions` capture

At the start of every walk, before any phase runs, we read the
account-selector's `<section aria-label="<portfolio>">` blocks
to capture three dimensions per account:

- `portfolio` — the section's `aria-label` (e.g. `Authorized`,
  `Education`); maps directly to the `owner` discriminator at
  silver-load time. Mirrors UBS PSN's `relationship_id`
  dimension: a single login, multiple parallel ownership
  contexts.
- `nickname` — the user-visible link text inside each
  `apex-kit-web-link`, with the account-id digit-sequence
  stripped (so e.g. a display name
  `<Nickname> NNNNNNNNN , balance: $...` becomes just `<Nickname>`).
  Accounts under a trust agreement can share one generic label;
  only the agreement number tells them apart, and it is not
  promoted.
- `account_id` — the 9-digit account number from the
  `data-testid` (`ap143528-accounts-selector-account-link-
  <id>`).

Persisted in `run.json` under `account_dimensions`, keyed by
`account_key(account_id)` so the run.json doesn't itself leak
raw 9-digit ids if viewed standalone. The mapping back to
canonical id lives inside the per-phase CSVs.

## 4. Silver schema (sketch)

Final schema lands in `migrations/0001_initial.sql` once the
silver loader is implemented. The shape below is the design
target.

### 4.1 Tables

| Table | Archetype | PK | Promoted columns |
| --- | --- | --- | --- |
| `schema_meta` | meta | `silver_schema_version` | `applied_at` |
| `dump_runs` | meta | `snapshot_at` | `loaded_at`, `bronze_dir` |
| `accounts` | snapshot, content-deduped | `(snapshot_at, account_external_id)` | `owner`, `account_kind`, `account_name_raw`; rest in `payload` |
| `positions` | snapshot | `(snapshot_at, account_external_id, instrument_key)` | `instrument_kind`, `symbol`, `cusip` (when present), `currency`, `quantity`, `market_value`, `cost_basis_total`, `average_cost_basis`; rest in `payload` |
| `transactions` | event | synthetic `transaction_external_id` PK + secondary index on `(account_external_id, timestamp)` | `account_external_id`, `timestamp`, `kind`, `amount`, `currency`, `instrument_key` |
| `documents` | event | `content_sha256` | `account_external_id` (when derivable), `doc_date`, `doc_kind` (`statement` / `tax_form_pdf`), `filename`, `bronze_path` |
| `tax_form_rows` | event | `(tax_year, account_external_id, form_subtype, row_index)` | structured per-lot detail from the 1099 Composite when Fidelity offers a structured-data variant — see §4.6 |

### 4.2 Snapshots vs events

`accounts`, `positions` follow the snapshot archetype: PK begins
with the temporal key, append-only across runs.

`transactions` follows the event archetype: window-DELETE-then-
INSERT per `(account, snapshot window)`. The activity scrape
emits one CSV per account at the page's default range; the
loader replaces exactly that range for that account.

`documents` follows the event archetype: INSERT-OR-IGNORE on
`content_sha256`. Each PDF is a discrete object; re-downloading
gives us a possibly-different sha256 (PDF regeneration question
deferred to §11.1) but the same logical doc.

### 4.3 Cash routing

Fidelity surfaces money-market core positions (`FDRXX`, `SPAXX`,
`FZFXX`, similar) as rows on the positions CSV with the
`HELD IN MONEY MARKET` description. Silver stores these as
`positions` rows (not split into a separate `cash_balances`
table); the wealthdb gold adapter re-routes them per its existing
[per-broker convention](https://github.com/ptu/wealthdb/blob/main/docs/adapters/schwab.md).

### 4.4 Owner dimension

`owner` lives on `accounts`. Other tables join via
`account_external_id`. Gold queries that want "all trust
positions" join positions → accounts on `account_external_id` and
filter on `accounts.owner`.

### 4.5 Historical reconstruction — statement PDFs

**Critical constraint:** Fidelity generates monthly / quarterly
statement PDFs only for some account groups, such as 529 plan
accounts; the document center shows nothing for the others. The DAF
has its own statement type but is out of scope.

Consequence: `historical_position_snapshots` and
`historical_cash_balances` (which would normally populate from
statement PDFs) are 529-only. For the trust, historical position
reconstruction requires:

- documents supplied out-of-band (PDF, via the
  `manual/` channel), OR
- The Activity & Orders transaction history, which lets gold
  *replay* positions forward from some baseline date.

This is the single biggest architectural caveat: the historical-
snapshot story is asymmetric across the owner dimension.

### 4.6 Tax-form structured data

Fidelity's Consolidated 1099 is available as **PDF only** from
the Tax forms sub-page. The download anchors carry aria-labels
ending in ` (pdf)` exclusively; an early DOM snapshot suggested
CSV / XML variants may exist for some 1099 types, but live
testing across the available years (2019-2025, 7 anchors total
for this account set) found no such variants on this customer's
forms. First-pass silver stores the PDFs in `documents`;
structured extraction into `tax_form_rows` is a follow-up
(parser TBD; pdfplumber + per-line layout heuristics, mirroring
the planned 529-statement reconstruction in §4.5).

### 4.7 Balances + Performance pages

Two additional Fidelity surfaces beyond positions / activity /
documents:

- **Balances** (`/portfolio/balances`) — no direct CSV / Excel
  / PDF export. The actions menu (`balwebex-actions-menu` in
  some builds, `more-action-menu` in others) offers 'Create
  Balance Letter', 'Print', and 'Glossary of terms'. 'Create
  Balance Letter' opens a multi-step wizard (select letter
  type: 'sample letter balance' or 'sample letter balance in
  excess' → select account(s) → Generate → PDF), with shapes
  that aren't a plain snapshot of the on-screen balance table.
  We persist the rendered `balances.html` instead; per-account
  totals are in `data-testid$='-totalaccountvalue-label'`
  elements (and accessible to the silver loader without
  driving the wizard). The Balance Letter wizard is left as
  a follow-up if a formal as-of PDF is ever required.

- **Performance** (`/portfolio/performance`) — no structured
  export of any kind. The page is a Highcharts SVG plus
  collapsible info tiles (return percentages by period,
  benchmark deltas). The only download-shaped action is
  "Print"; there is no kebab/menu CSV option. The full
  rendered DOM is captured as `performance/performance.html`
  so silver can extract metrics by parsing the rendered text;
  most return metrics are also derivable from positions +
  activity time-series, so the gap is acceptable.

## 5. Architecture: keep-alive trigger-file model

**Fidelity binds its session to the Firefox-process lifetime.**
Confirmed empirically: after a successful login that writes the
profile dir to disk, closing the Camoufox process and reopening
with the same profile dir lands on a signin redirect — Fidelity
treats the cookie as dead even though the file persists. This
matches the [schwab-web-dump session model](https://github.com/ptu/schwab-web-dump/blob/main/DESIGN.md#5-login--mfa-flow)
exactly.

Consequence: **login and scrape must share one continuous Camoufox
process.** The sibling-tool "login mints `storageState.json`,
download reuses it across runs" pattern (UBS, Swissquote) does
not apply.

### 5.1 Trigger-file dev model (current)

To let the operator iterate on scraping without paying for an
MFA round on every change:

```
Terminal A:  ./fidelity-web-dump login
             → login + (auto-skipped MFA per device-trust cookie)
             → HOLD Firefox open
             → poll /data/.download-trigger every 2s

Terminal B:  ./fidelity-web-dump download [flags]
             → wrapper writes $HOST_DATA/.download-trigger and
               exits immediately (no docker spawn)
             → running login container picks up the trigger and
               runs download.walk() in-place against the live
               Camoufox session
```

The wrapper mounts `$HERE:/app` so edits to `download.py` on the
host are immediately visible inside the container; `login.py`
calls `importlib.reload(download)` before each `walk()`, so a
host-side edit + a new trigger picks up the latest code with no
rebuild and no MFA hit.

### 5.2 One-shot model (planned for production)

Once the scraping logic stabilises, the trigger-file indirection
goes away and `./fidelity-web-dump download` becomes a one-shot:
login → walk → exit in one continuous Camoufox process.

### 5.3 Session timeout

Empirically observed: **Fidelity's idle timeout is ~15 minutes**.
A keep-alive container that's been idle for 15+ minutes will see
the next navigation redirect to a session-timeout modal. The
walk() functions detect this best-effort (URL check after the
documents nav) and abort the affected phase cleanly rather than
falling over. For long iteration sessions, restart the container
(MFA-less per the device-trust cookie).

## 6. Anti-bot configuration (Camoufox + behavioural mimicry)

The working config: **Camoufox + `os="macos"` + `humanize=True` +
`geoip=True`**, headed against Xvfb. This combination gets the
login flow past Akamai Bot Manager (Fidelity's bot-detection
vendor); see [`project_fidelity_akamai_block`](project_fidelity_akamai_block.md)
memory for the empirical diagnostic chain.

The escalation history:

| Rung | Configuration | Result |
| --- | --- | --- |
| 1 | Vanilla Playwright Chromium | Akamai-blocked at credential submit (`dom-system-error-header` "Sorry, we can't complete this action right now"). |
| 3a | Camoufox + `os="macos"`, no behavioural mimicry | Same block. The static fingerprint stack alone was not enough. |
| 3b | + `humanize=True` (cursor trajectories) + `geoip=True` (locale/tz match egress IP) + **one initial VNC-driven login** to seed the Akamai trust cookie | Works. Subsequent auto-driven logins (CLI-MFA or auto-MFA-skip via device-trust cookie) succeed. |

The first-ever login on a fresh profile dir requires the VNC
handoff — the operator clicks "Log in" manually in their VNC
client, generating real Firefox-sourced mousedown/mouseup events
that satisfy Akamai's behavioural-score gate. Once Akamai's
trust cookie is in the profile dir, automated clicks on the
button work fine.

The `--vnc` subcommand drives this: pre-fills the credentials,
prints a READY banner, polls for the post-auth URL while the
operator drives the login + 2FA via VNC.

## 7. Login + MFA flow

Default flow (`./fidelity-web-dump login`), assuming a profile dir
that already has Akamai trust + Fidelity device-trust cookies:

1. Launch Camoufox (rung 3b).
2. Navigate to `https://digital.fidelity.com/prgw/digital/signin/`.
3. Detect either signin form (US locale) or International Usage
   Agreement interstitial (non-US locale, our case via geoip=True);
   click "I Accept" on the IUA if served.
4. Fill `#dom-pswd-input` with `FIDELITY_PASSWORD`. Username field
   varies by device-known state (text input vs `<select>`); the
   script tries both paths.
5. Click `#dom-login-button`.
6. Either:
   - Device-trust cookie suppresses MFA → straight to post-auth.
   - 2FA prompt (TOTP-style 6-digit code in
     `#dom-totp-security-code-input`) → prompt operator on stdin,
     fill, submit.
7. Wait for the post-auth URL prefix
   `https://digital.fidelity.com/ftgw/digital/portfolio/` to
   appear (using `live_url()` via `location.href` — see workaround
   note in §6).
8. Drop into the keep-alive trigger loop (§5.1).

`--no-trust-this-browser` skips ticking the "Trust this browser"
checkbox at the 2FA page (useful for repeatedly exercising the
MFA flow during development).

`--check` loads the profile dir and navigates to the post-auth
landing without re-logging — reports session ALIVE / DEAD. Note:
DEAD on a profile that just completed a successful login is
expected if the Camoufox process exited in between (§5).

## 8. UI surface map

URLs and selectors anchored to the live DOM as of 2026-05-24/25.

### 8.1 URLs

| Page | URL |
| --- | --- |
| Signin SPA | `https://digital.fidelity.com/prgw/digital/signin/` |
| Post-auth landing | `https://digital.fidelity.com/ftgw/digital/portfolio/summary` |
| Positions SPA | `https://digital.fidelity.com/ctgw/digital/positions/poswebex/client/` (we use the wrapper URL at `/ftgw/digital/portfolio/positions` which routes through) |
| Activity & Orders | `https://digital.fidelity.com/ftgw/digital/portfolio/activity` |
| Documents hub | `https://digital.fidelity.com/ftgw/digital/portfolio/documents` (redirects to Statements sub-page) |
| GraphQL endpoint | `https://digital.fidelity.com/ftgw/digital/portfolio/api/graphql` (not used; flagged as a future-iteration alternative to HTML scraping) |

### 8.2 Login

| Element | Selector |
| --- | --- |
| Username (returning-device dropdown) | `#dom-select-username` (native `<select>` with `value="default"` = "Enter different username") |
| Username (fresh-device text input) | fall-back candidates; `input[aria-labelledby=dom-username-label]` etc. |
| Password | `#dom-pswd-input` |
| Submit | `#dom-login-button` |
| 2FA code | `#dom-totp-security-code-input` |
| Trust-this-browser checkbox | `<input type=checkbox id="dom-trust-device-checkbox">` — must click the `<label for="dom-trust-device-checkbox">` (input is intercepted by the PVD label overlay) |
| IUA accept | `a.accept-link` (calls `javascript:acceptAgreement()`) |

### 8.3 Positions

| Element | Selector |
| --- | --- |
| Account selector container | `[data-testid="ap143528-accounts-selector-container"]` |
| Per-account link | `[data-testid="ap143528-accounts-selector-account-link-<ID>"]` |
| All-accounts toggle | `.acct-selector__all-accounts` |
| Preset-view dropdown (native `<select>`) | `[data-testid="preset-views-dropdown"] select` — values: `Overview`, `DividendView`, `FundPerfView`, `ClosedPositionsView`, `MyView`, `EditMyView` |
| Kebab menu (download trigger) | `[data-testid="kebab-menu"]` |
| Download menuitem | matched via `role=menuitem[name=/download/i]` after kebab open |

**Per-account navigation does NOT filter the positions CSV.**
The all-accounts view's Download exports a consolidated CSV with
rows for every visible account. Two CSVs per scrape (one per
preset view); account dimension lives in the `Account Number`
column.

### 8.4 Activity & Orders

| Element | Selector |
| --- | --- |
| Filter dialog trigger | `button:has-text('Filter')` (also `[aria-label='Filter']`) |
| Time-period filter pill | `[data-testid='ap143528-timeperiod-filter']` (opens `#timeperiod-select-container`) |
| Preset day-count radios (Recent tab) | `helios-radio[pvd-value='<N>']` (`N` ∈ {10, 30, 60, 90}; default 30, preference 90) |
| Apply (preset tab) | `button[aria-label='Apply Recent Time Period']` |
| Custom tab | `apex-kit-segment[pvd-value='Custom']` |
| Custom-tab date inputs | `#customized-timeperiod-from-date` / `#customized-timeperiod-to-date` (HTML5 `<input type="date">`, ISO YYYY-MM-DD; `min`/`max` attrs bound retention) |
| Apply (Custom tab) | `button[aria-label='Apply Customized Time Period']` |
| Download dropdown trigger | `button[aria-label='Download']` (opens `#downloadContent` popover; the SPA disables it while re-fetching after Apply — we poll on `disabled` clearing before clicking) |
| CSV item inside popover | `#downloadContent button:has-text('CSV')` |

**Activity export is consolidated.** Clicking the account-selector
does NOT scope the resulting CSV — the same CSV comes down
regardless of selector state. One CSV per window, all accounts
inside (Account Number column as the per-row discriminator). The
silver loader fans out per-account from there.

### 8.5 Documents

The documents hub redirects to the Statements sub-page by default;
both Statements and Tax forms are accessed via left-sidebar
navigation.

| Element | Selector |
| --- | --- |
| Sidebar link (Statements / Tax forms) | `a.sidebar-link:has-text('<label>')` |
| Statement row description cells | `td.gridData.link[aria-label$=" (pdf)"]` (e.g. `"Jan-March 2026 — Statement (pdf)"`) |
| Statement row download trigger | `button.downloadIconButton[aria-label='download statement']` — popover trigger; opens an in-row dropdown |
| Statement popover items | `li.modal-options` inside `.downloadDropdownContainer` with text "Download as PDF" / "Download as CSV" |
| Tax-year filter | `#options-select-TimeFilter` (native `<select>`; option values are year strings, currently 2019–2025) |
| Tax-form anchors | `a[aria-label$=" (pdf)"]` — one per form per account. We click by the anchor's unique generated `id` (`link-<digits>`); the form-name aria-label is shared across accounts and the href is `javascript:void(0)` for all in-app downloads, so neither alone is unique. |

**Statement download mechanism.** Clicking "Download as PDF" in
the popover does NOT fire a Playwright `download` event — Fidelity
opens the PDF in a new browser tab and lets the browser's PDF
viewer render it. We catch this by polling `context.pages` for a
new tab after the click (Camoufox's juggler patch doesn't reliably
deliver `popup` events to ad-hoc listeners — same family as the
`live_url` URL-cache bug), grabbing the popup's URL via
`live_url()`, and fetching the bytes through `context.request.get`
(which inherits the session cookies). CSV downloads use the
canonical `page.expect_download` path. The icon button is
scrolled into view before clicking and a JS-dispatched click is
used as fallback — Playwright's actionability check flakes on
rows below the fold even when the locator resolves cleanly.

**Tax-form mechanism.** Per year (iterated across every option
in the TimeFilter select), wait for the spinner to clear AND
either form anchors to appear OR an empty-state message, then
enumerate the `(pdf)`-suffixed anchors and click each by its
unique id, capturing the CSV via `page.expect_download`. Most
forms can sit below the fold;
scroll-into-view is applied before each click.

**Scope filter.** Only Statements + Tax-forms sub-pages are
visited; prospectus / supplementary categories are off-limits per
the user's spec. The external IRS-instructions links on each
form row don't carry the `(pdf)` aria-label suffix, so they
don't enter the enumeration.

### 8.6 Balances + Performance

Two surfaces with no structured export. `download.py` persists
the rendered HTML; silver scrapes from there.

| Page | URL | Surface notes |
| --- | --- | --- |
| Balances | `/ftgw/digital/portfolio/balances` | Per-account totals in `[data-testid$='-totalaccountvalue-label']`. Actions menu (`balwebex-actions-menu` in some builds, `more-action-menu` in others) offers a multi-step "Create Balance Letter" wizard (select letter type → select account → Generate → PDF), 'Print' (browser dialog), and 'Glossary of terms' — none are plain snapshots, so we skip the menu entirely. |
| Performance | `/ftgw/digital/portfolio/performance` | Highcharts SVG + collapsible info tiles. No kebab/menu Download item; the only data path is parsing the rendered DOM (return percentages by period, benchmark deltas). |

## 9. What we do NOT do

- OFX — dead (§1.1).
- Akoya / FDX / Plaid / SnapTrade — B2B-only.
- The Fidelity Charitable DAF — auto-excluded.
- Prospectuses / supplementary documents.
- Trade / transfer / config writes — see [CLAUDE.md](CLAUDE.md) §1.
- MFA automation — human-in-the-loop on every truly-fresh login.
- Cross-bank semantic alignment — gold's job.
- Trust statement reconstruction from PDFs — no statements exist.
- Third-party investment-manager data sources — out-of-band; separate repo when needed.

## 10. Implementation status

| Step | Status |
| --- | --- |
| Container scaffolding + design docs | done |
| `login.py` — IUA gate, MFA, trust-device, profile dir | done |
| `download.py` — positions Overview + DividendView (consolidated CSVs) | done |
| `download.py` — activity preset 'Past 90 days' (page-level pill → radio → Apply Recent → networkidle) | done |
| `download.py` — activity Custom-range backfill (Custom tab, ISO date inputs, retention-clamped, bisected into ≤93-day windows) | done |
| `download.py` — documents: statements (per-row popover; PDF via popup-tab + `context.request`, CSV via `page.expect_download`; scroll-into-view + JS-click fallback for rows below the fold) | done |
| `download.py` — documents: tax forms (multi-year via `#options-select-TimeFilter`, one click per form by unique anchor id) | done |
| `download.py` — balances + performance HTML capture (no structured export available on either surface) | done |
| `migrations/0001_initial.sql` + `load.py` | not started |
| Statement-PDF parser (529 historical reconstruction) | not started |
| One-shot architecture collapse (login + walk + exit) | not started |
| `wealthdb` Fidelity adapter | separate repo |

## 11. Open questions

### 11.1 PDF regeneration
Does Fidelity regenerate statement / 1099 PDFs per request
(different sha256 each time, same logical content) like Schwab
does? Resolve by downloading the same statement twice and diffing
sha256s. The silver loader currently plans to dedup on
`content_sha256` (assumes stable hashes); if regenerated, we
switch to `(account, doc_date, doc_kind, filename)` dedup à la
schwab-web-dump.

### 11.2 The unidentified ninth account
The tenth account in the selector (after auto-excluding the DAF)
doesn't appear in the consolidated positions CSV. Possibly
empty, possibly a non-brokerage account type. Resolved by
inspecting the account-detail page for it.

### 11.3 GraphQL endpoint
`https://digital.fidelity.com/ftgw/digital/portfolio/api/graphql`
is reachable from the post-auth session. Schema unknown; could
provide cleaner / more stable access than HTML scraping for
positions and activity. Out of scope for v1; flagged for a
future-iteration alternative.

### 11.4 Balance Letter wizard
The Balances actions menu's 'Create Balance Letter' opens a
multi-step wizard (select letter type → select account →
Generate). The letter types (`sample letter balance`,
`sample letter balance in excess`) are compliance / eligibility
artefacts, not a plain snapshot of the on-screen balance table,
so we skip the wizard. Deferred unless a formal as-of PDF is
later needed — the per-account totals are already accessible
from the persisted `balances.html`.

### 11.5 Third-party-manager institutional feed
Out-of-band; such a feed would live in its own collector
(see §1.3), so it blocks nothing here.
