# viac-dump — design

Design document for the `viac-dump` toolkit. The audience is the
engineer (current author, future contributor) implementing and
maintaining `explore.py`, `login.py`, `download.py`, and the
silver loader against the live `app.viac.ch` SPA. It is also the
contract between this silver and a future `wealthdb` VIAC adapter.

This document is the planning artefact, not the journal.
Decisions that get revised should be revised here, in place. The
silver-schema sketch in §8 is deliberately under-specified until
Phase 1 discovery reveals VIAC's actual JSON shape; the rest of
the design is committed.

The shared three-layer model (bronze on disk, silver SQLite +
JSON1, gold DuckDB cross-bank canonical) is documented in
[`schwab-api-dump/DESIGN.md`](https://github.com/ptu/schwab-api-dump/blob/main/DESIGN.md);
this document only covers what's VIAC-specific.

## 1. Context and non-goals

### 1.1 Why a web scraper

VIAC has no retail-accessible read API. The provider supplies a
web SPA at `app.viac.ch` and a companion mobile app; both run on
top of the same internal backend, but neither's API surface is
exposed for personal aggregation:

- **OpenWealth** — B2B-only. VIAC's parent (WIR Group / Terzo) has
  no published retail OpenWealth participation.
- **PSD2** — Switzerland is outside the EU PSD2 regime; the Swiss
  fintech-driven alternative (Open Banking Project Switzerland)
  has no Pillar-3a coverage today.
- **Aggregators** — Plaid / TrueLayer / Tink / Powers / Akoya all
  decline Pillar-3a providers as too niche.
- **Email feeds** — "document available" notifications with no
  payload.

The remaining channel is the SPA driven under Playwright. VIAC is
a relatively small Swiss financial-services brand and the SPA is
likely vanilla anti-CSRF + session cookies, not Akamai-grade
fingerprinting — so the starting point is the
[swissquote-dump](https://github.com/ptu/swissquote-dump) /
[ubs-web-dump](https://github.com/ptu/ubs-web-dump) shape (vanilla
Playwright Chromium), not the
[schwab-web-dump](https://github.com/ptu/schwab-web-dump) /
[fidelity-web-dump](https://github.com/ptu/fidelity-web-dump)
shape (Camoufox).

### 1.2 Account composition

VIAC users typically hold:

- One **Vorsorgekonto** — interest-bearing cash sub-account, the
  default landing for new contributions before they're invested.
- One or more **Vorsorge-Portfolios** — investment sleeves, each
  with a chosen strategy (e.g. Global 100 / Global 60 /
  Sustainable 40) and a per-strategy allocation across underlying
  ETFs / index funds.

The toolkit must enumerate and scrape every sub-account. The
silver schema needs to discriminate cash vs investment sleeves
because the gold-layer `account_kind` differs (`cash` vs
`brokerage`).

### 1.3 Non-goals

- **Real-time / near-real-time pull.** MFA gates every truly-fresh
  login.
- **Strategy changes, contributions, beneficiary edits,
  withdrawals.** See [CLAUDE.md §1](CLAUDE.md) — the contract is
  read-only.
- **MFA automation.** Human-in-the-loop on every truly-fresh
  login; see §5.
- **Cross-bank semantic alignment.** `wealthdb` gold's job.
- **The VIAC mobile app's signed-payload feed.** If it exists and
  is reachable, it lives in a separate `viac-mobile-dump` repo;
  this toolkit covers the web SPA only.

## 2. SPA / hash-route handling

VIAC's portal is a JavaScript single-page application served at
`https://app.viac.ch/`. The login URL is a hash-route:

```
https://app.viac.ch/#/ext(modal:core/session/login)
```

Two consequences for the scraper:

1. **Wait for content, not page-load events.** The initial HTTP
   response is a small SPA shell. Playwright's `wait_for_load_state`
   fires before the SPA bootstraps. Every navigation that depends
   on a particular view being rendered must wait for an explicit
   landmark (`page.wait_for_selector`, or a network response that
   the view's data hangs on).
2. **Direct navigation to deep hash routes is unreliable.** Setting
   `location.hash` after the SPA has bootstrapped is the convention
   for sibling SPAs (`swissquote-dump` does this for the `#documents`
   route); pre-bootstrap hash navigation tends to be stripped. The
   expected pattern is: navigate to `https://app.viac.ch/`, wait
   for the SPA to mount, then mutate `location.hash`.

The structured data is **almost certainly behind XHR / fetch
calls returning JSON**. Phase 1 discovery exists to capture those
bodies; the silver loader will likely consume them directly
rather than DOM-scraping HTML. The bronze tree is therefore
JSON-first, with HTML kept as a fallback for views that turn out
to be server-rendered or to have no clean endpoint.

## 3. Identity strategy

Two key identifiers must be stable across runs and bridgeable to
the future `wealthdb` VIAC adapter:

### 3.1 `account_external_id`

Provisional choice: **whatever opaque per-account identifier VIAC
uses in its own JSON payloads.** Most SPAs key sub-accounts on
either a UUID or a short numeric/string code embedded in API URLs
(`/api/v1/accounts/<id>/positions`). Phase 1 discovery will
confirm the shape. The bronze loader stores the verbatim VIAC
identifier; the silver loader promotes it to
`accounts.account_external_id`.

If VIAC also surfaces a customer-visible account number (e.g. a
12-digit Pillar-3a contract number on the annual statement PDF),
the loader carries that as a secondary `account_contract_number`
column in `accounts.payload` for human-readability and for the
wealthdb-side cross-bank bridge.

### 3.2 `instrument_external_id`

VIAC's investment sleeves hold a small basket of ETFs / index
funds (e.g. CSIF Switzerland Equity, iShares Core MSCI World).
**ISIN** is the natural cross-bank join key (`wealthdb`'s
`instruments.isin` is the canonical index) and ISIN is almost
certainly carried in VIAC's per-position JSON. The adapter keys
`instrument_external_id` on ISIN where present, falling back to
the fund's short code if ISIN is absent on a specific row.

### 3.3 `transaction_external_id`

Provisional choice: **synthetic SHA-256 prefix over the row's
promoted columns** (`account | occurred_at | kind | amount |
currency | counterparty-or-symbol`), mirroring the swissquote /
schwab-web pattern. VIAC may expose a stable per-event ID — if
so, the adapter uses it; otherwise the synthetic hash converges
under the gold-layer window-DELETE-then-INSERT model.

### 3.4 Wealthdb bridge

The wealthdb gold layer has `accounts.tax_wrapper = 'pillar_3a'`
for these accounts. The VIAC adapter:

- Sets every VIAC `accounts.tax_wrapper` to `pillar_3a`.
- Maps the Vorsorgekonto sub-account to `account_kind = 'cash'`.
- Maps each Vorsorge-Portfolio sub-account to
  `account_kind = 'brokerage'` (or whatever the post-Phase-1 shape
  best fits — `account_kind = 'custody'` is a possibility if VIAC
  is technically the custodian and the investment-strategy
  manager is the underlying fund issuer).
- Sets `management_style = 'self_directed'` for the cash sub-
  account; for the investment sleeves, the choice between
  `self_directed` (user picks the strategy from VIAC's menu) and
  `automated` (the strategy itself runs rule-driven rebalancing)
  is open until Phase 1 confirms the product model.

## 4. Anti-bot strategy

Three rungs, in escalation order. The current target is **rung
1**.

| Rung | Configuration | Use when |
| --- | --- | --- |
| 1 | Vanilla Playwright + stock headless Chromium, persistent profile dir | Default. The starting target. |
| 2 | + stealth plugins / undetected-chromedriver-style tweaks | If rung 1 fingerprints get flagged. |
| 3 | Camoufox-patched Firefox (à la schwab-web-dump / fidelity-web-dump) | Last resort if VIAC ships Akamai-grade detection. |

VIAC is a smaller Swiss brand and not generally believed to
deploy Akamai Bot Manager; rung 1 should suffice. The Phase 1
discovery container is built to support all three rungs by
mounting the profile dir read-write under `/secrets/viac-profile/`
— escalating to rung 3 means swapping the browser launch line
without touching the rest of the toolkit.

## 5. Bootstrap flow

Three phases, in order. Phase N+1 requires Phase N's artefacts.

### 5.1 Phase 1 — VNC-driven exploration (`vnc-explore`)

Container starts Xvfb + x11vnc + fluxbox on display `:99`,
forwards VNC on `127.0.0.1:5900` with a fresh single-use password
printed to stderr, and runs `explore.py` in a long-timeout
loop. The operator:

1. Connects with a VNC viewer.
2. Logs in to VIAC by hand.
3. Completes 2FA on their phone.
4. Clicks through every relevant view:
   - Account / portfolio list.
   - Per-account positions / allocation (the pie-chart strategy
     view).
   - Per-account contribution / transaction history.
   - Documents area (annual statements, *Bescheinigungen*, fund
     prospectuses if present).
   - Any "export" / "download" / "PDF" buttons on each view.

Before the operator connects, `explore.py` pre-fills the
`VIAC_LOGIN` / `VIAC_PASSWORD` env-var values into the SPA login
form. macOS Screen Sharing (the default VNC client on the
operator's host) does not synchronise the host clipboard with the
in-container browser, so without pre-fill the operator would have
to type the password character-by-character through VNC. The
operator still completes the click on "Log in" and the 2FA tap
on their phone via the VNC viewer (so we burn no MFA push the
operator didn't trigger themselves).

The username-field selector is heuristic: locate the visible
`input[type="password"]`, then mark the closest preceding visible
non-password input as the login field. Phase 1 confirms whether
that holds for VIAC's exact form (or whether a fixed selector
should replace the heuristic in Phase 2's `login.py`).

While the operator clicks, `explore.py` records:

- Every request URL + method + status, plus request and response
  headers (excluding `Cookie` / `Authorization`, which are
  separately captured under a redacted storage snapshot).
- Every response body for `application/json` — full, since
  VIAC's SPA almost certainly returns clean JSON here.
- Every response body for `text/html`.
- Every DOM snapshot at the URLs the operator lands on.
- Every download trigger + the resulting file.
- Screenshots at each landmark.
- Cookies + `localStorage` + `sessionStorage` at logical
  waypoints (SPAs commonly stash auth tokens in storage rather
  than cookies).

Output lands under `--discovery-dir`. No default; must be
user-provided per [CLAUDE.md §3](CLAUDE.md). Conventional path:
`/debug/discovery-<UTC-ts>/` on the container side,
`~/.cache/viac-dump-debug/discovery-<UTC-ts>/` on the host.

**Long-timeout human-in-the-loop.** Per the
[No immediate-response interactive flows] memory: the operator
may take an hour to complete the walk-through. `explore.py`'s
session-end heuristic must therefore be either operator-driven
(a "press Enter when done" stdin prompt, or a sentinel URL
visited by the operator) or a generous inactivity timer (≥1
hour). Do not impose a tight wall-clock cap.

### 5.2 Phase 2 — persistent session minting (`login`)

With Phase 1's discovery logs in hand, `login.py`:

- Maintains a persistent browser profile dir at
  `/secrets/viac-profile/` (configurable path, NOT in
  `~/.secrets/` for debug artefacts — see [CLAUDE.md §3](CLAUDE.md)
  for the secrets-vs-debug separation).
- On invocation:
  - If the profile already has a valid session, probe one cheap
    landmark URL (or one of the discovered JSON endpoints) and
    exit successfully with no MFA.
  - Otherwise: launch Chromium (headed or headless), navigate to
    the hash-route login URL, wait for the SPA login form to
    render, fill login + password from the env vars, surface the
    2FA prompt on stdin (`VIAC 2FA: enter the code`), wait for
    the post-MFA landing.
- Persists the session token / cookie set so the next
  `download.py` run can skip MFA if the session's still alive.
- `--check` mode: probe-only, no credential submit, reports
  "session alive / dead". Allowed without asking the user (see
  [CLAUDE.md §2](CLAUDE.md)).
- Running `login.py` without `--check` (mints a fresh session,
  fires an MFA push) is **not** allowed without the user's
  explicit ask.

### 5.3 Phase 3 — bronze scrape (`download`)

After login is reliable, `download.py` iterates over every
account and downloads:

- Positions / allocations (preferring whatever structured JSON
  the SPA endpoints surface; falling back to DOM scraping for
  any view that turns out to be server-rendered or to have no
  clean endpoint).
- Transactions / contributions for the longest available window.
- Documents (PDFs; *Bescheinigungen* highest priority).

Manifests each run as `run.json` (timestamp, accounts seen,
per-phase counts, paths to artefacts).

`--dry-run` walks the UI to confirm selectors / endpoints still
match landmarks, exits without exporting. Allowed without asking.
Live `download.py` runs are not allowed without the user's
explicit ask.

## 6. Login + MFA flow

Working assumptions (to be confirmed by Phase 1):

1. The login URL is a hash-route SPA modal:
   `https://app.viac.ch/#/ext(modal:core/session/login)`.
2. The login form takes a username (`VIAC_LOGIN`) and a password
   (`VIAC_PASSWORD`) loaded from `~/.secrets/viac.env`.
3. The 2FA mechanism is in-app TOTP / push via VIAC's own
   mobile app. (Plausible based on VIAC's product positioning;
   to be confirmed.)
4. The post-auth session sets either a session cookie, a bearer
   token in `localStorage`, or both. The profile-dir-based
   approach handles either case transparently.

### 6.1 The `--check` probe

The probe must be cheap (one short HTTP request, no UI walk)
AND must not refresh the session (some session backends extend
the cookie lifetime on every request — fine if VIAC works that
way; if not, the probe must use a verb that doesn't extend the
session). Phase 1 discovery picks the cheapest authenticated
JSON endpoint as the probe target.

### 6.2 Long timeouts on human-in-the-loop steps

Per the [No immediate-response interactive flows] memory:

- `vnc-explore` waits for the operator with a multi-hour timeout
  (default 4h); the operator may need to find their phone,
  authenticate to the VIAC mobile app, complete biometric, and
  walk every view.
- `login.py` (non-`--check`) prompts on stdin with a multi-hour
  timeout.
- `download.py` does not gate on human input.

The wrapper sets no hard wall-clock timeout on `docker run`; the
operator decides when to ctrl-C.

## 7. Bronze layout

Mirrors the sibling projects.

```
<bronze-dir>/                            e.g. ~/wealthdb/viac/
├── 20260526T120000Z/                    one bronze dump per run
│   ├── run.json                         manifest: accounts, per-phase counts
│   ├── positions/
│   │   └── <account>/                   one dir per sub-account
│   │       ├── positions.json           SPA response, verbatim
│   │       └── positions.html           DOM fallback
│   ├── transactions/
│   │   └── <account>/
│   │       ├── transactions_<since>__<until>.json
│   │       └── transactions_<since>__<until>.html
│   ├── documents/
│   │   ├── <doc_id>.pdf                 Bescheinigungen, statements, ...
│   │   └── ...
│   └── allocations/                     post-Phase-1: per-portfolio target weights
│       └── <account>/allocations.json
├── 20260527T120000Z/
│   └── ...
├── manual/                              user-uploaded bronze artefacts
└── viac.db                              silver SQLite (default name)
```

`run.json` carries the customer / login dimension (hashed so an
`ls` of the bronze tree doesn't expose the raw login), per-
account inventory, the transaction window bounds, and per-
document metadata. The canonical mapping `hash → raw login` lives
in the manifest of the most recent run only, never tracked.

Phase 1 discovery dumps stay in `/debug/discovery-<UTC-ts>/`,
outside the bronze tree.

## 8. Silver schema sketch

**Deliberately under-specified until Phase 1 discovery lands.**
VIAC's JSON shape will dictate the column inventory; pre-
designing it would either over-fit or under-fit the actual
response. The committed parts:

- One row per (snapshot, account, position_key) on `positions`;
  per-account JSON payload as `payload`.
- One row per (occurred_at, account, transaction_external_id) on
  `transactions`; per-event JSON payload as `payload`.
- One row per (account_external_id, snapshot_at) on `accounts`;
  per-account JSON payload as `payload`.
- One row per (content_sha256) on `documents`; PDFs themselves
  stay on disk.

Migrations land under `migrations/NNNN_<slug>.sql`. The loader
runs pending migrations on every invocation. Same discipline as
the siblings — never rewrite an applied migration, always add a
new file.

## 9. Gold-layer integration

VIAC silver feeds a future `wealthdb` VIAC adapter (separate
commit in the wealthdb repo; out of scope here). The adapter
contract:

- `accounts.tax_wrapper` = `'pillar_3a'` for every account.
- `account_kind` distinguishes the Vorsorgekonto (`cash`) from
  the Vorsorge-Portfolios (`brokerage` or `custody`, TBD).
- `instruments.isin` is the cross-bank join key.
- `transactions.kind` maps from VIAC's discriminator into
  wealthdb's canonical taxonomy (`buy` / `sell` / `dividend` /
  `coupon` / `fee` / `tax` / `deposit` / `withdrawal` /
  `interest` / `corporate_action` / `transfer_in` /
  `transfer_out` / `journal` / `other`).
- `LatestChangeNumber` = `MAX(dump_runs.snapshot_at)`, or `-1`
  if `dump_runs` is empty.

The taxonomy mapping table will be filled out in the wealthdb
adapter's own design doc once Phase 1 confirms VIAC's transaction
discriminator values.

## 10. What we do NOT do

- **Mutations** — no contributions, no strategy changes, no
  withdrawals, no beneficiary edits. See [CLAUDE.md §1](CLAUDE.md).
- **MFA automation** — human-in-the-loop on every truly-fresh
  login. See [CLAUDE.md §3](CLAUDE.md).
- **Cron / launchd / GitHub-Actions scheduling** — see
  [CLAUDE.md §2](CLAUDE.md). Unattended runs can't pass the MFA
  gate anyway.
- **Cross-bank semantic alignment** — gold's job, not silver's.
- **The VIAC mobile app's payload** — out of scope; if it
  becomes a viable channel, a separate `viac-mobile-dump` repo.

## 11. Open questions

Recorded here so Phase 1 discovery is targeted. Each gets either
"answered, here's what we found" or "still open" in the next
revision of this document.

1. **2FA mechanism.** In-app TOTP, push via VIAC's mobile app,
   SMS fallback, or something else? Affects the `login.py` stdin
   prompt wording.
2. **Session storage.** Cookie-only, `localStorage` bearer
   token, both? Determines the probe path for
   `login.py --check`.
3. **Session lifetime.** How long does an idle session last? How
   long does an active session last? Determines the runbook for
   "I logged in this morning, can I still run `download.py` this
   evening?".
4. **Per-account API shape.** Does VIAC return a clean
   per-account `/accounts/<id>/positions.json`-style endpoint, or
   are positions buried in a graph-shaped composite response?
   Determines whether the loader can stay JSON-first or has to
   fall back to DOM scraping.
5. **Document IDs.** What does the URL / filename of a downloaded
   PDF look like? Stable per document or regenerated per
   request? Determines the bronze dedup key.
6. **Contribution / *Bescheinigung* identifier.** Is the annual
   tax certificate keyed on `(year, customer_id)` or on a unique
   document ID? Determines how the silver loader dedups across
   years.
7. **Multi-account API.** Does the SPA require switching
   sub-accounts in the UI (and the URL) before each account's
   data is fetched, or is there a consolidated endpoint? Affects
   the `download.py` walk order.
8. **Currency.** All Pillar-3a balances are CHF, but the
   underlying ETFs may price in USD / EUR. Does VIAC surface the
   per-fund native currency + FX rate, or only the CHF-converted
   value? Determines the `cash_balances` / `positions` columns.
9. **Strategy identifier.** Is the per-portfolio strategy
   (`Global 100`, `Sustainable 40`, …) emitted as a stable
   machine identifier, a free-text label, or both? The wealthdb
   adapter needs the stable form for the `management_style`
   discriminator.
10. **Anti-bot escalation triggers.** Does rung 1 (vanilla
    Playwright Chromium) get all the way through login, or does
    something flag the headless / persistent-profile combination?
    First Phase 1 attempt is the empirical test.
