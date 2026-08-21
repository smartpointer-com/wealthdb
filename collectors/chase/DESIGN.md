# chase — design notes

Discovery-phase notes for the Chase retail collector. Everything here
predates real captures unless marked otherwise — the `explore` harness
exists precisely to replace these assumptions with traces. Sections:
scope and what is known (§1), the harness itself (§2), what Phase 1 must
capture (§3), the phase roadmap (§4), and the handoff checklist (§5).

## 1. Scope & what is known so far

- **The relationship.** A retail banking relationship at JPMorgan Chase
  (chase.com). No brokerage / investment surface is in scope, even where
  the same login exposes one (see CLAUDE.md). **Scope is deposit accounts
  only** (checking, and savings if present); credit-card and other products
  the same login may expose are out of scope in code and docs. See the §4
  scope note for the rationale and the cards-as-future-expansion path.
- **Conduit accounts.** The cash accounts are conduits — cash passes
  through them on its way to and from other sources. Their transactions
  matter for cross-source money-flow tracking; per-account returns are
  meaningless and are blanked by the registered ReturnsPolicy
  (`internal/silver/chase/policy.go`, `AccountsGrainMeaningless`); the
  coarse grains keep the accounts and count their flows — see §4.
  (Credit cards, if scoped in, are **not** conduits — they are
  revolving-credit liabilities, a different `account_kind` with
  different endpoints and no conduit-returns rationale.)
- **No self-service API.** Chase's programmatic access for retail data
  runs through aggregator gateways (Akoya / Plaid-class), which are
  partner-gated — not something an individual retail login can
  self-provision. The web UI is the surface, as at the other
  portal-only sources (`carta`, `equityzen`, `angellist`).
- **Bot defense: Camoufox from the start.** The two US siblings both
  measured Akamai-class bot defense blocking vanilla Chromium outright
  (`fidelity-web`, `schwab-web`), and Chase fronts the same class of
  defense. The usual escalation rungs (vanilla Chromium → vanilla
  Firefox) are skipped rather than re-measured: the harness builds on
  `shared/images/base-camoufox.Dockerfile` and the persistent-profile
  pattern from day one. The 2026-08-11 capture confirmed the stack —
  Camoufox cleared the login end-to-end, and the traffic carried
  ThreatMetrix device fingerprinting (`h64.online-metrix.net`) and
  Akamai mPulse RUM (`s2.go-mpulse.net`) alongside the app.
- **2FA: alternates between in-app push and SMS OTP — both captured.**
  Chase alternates with no known pattern; the 2026-08-11 captures caught
  both the **in-app-push** and the **SMS OTP** paths (§A). The factor menu
  is data-driven (`challenge-options`), so login.py branches on what the
  menu offers rather than assuming one factor; the interactive core is
  already built in [`auth_dialog.py`](auth_dialog.py). Still uncaptured:
  the `OTP_VOICE` variant's method code (the menu lists it; the SMS code
  is `"S"`).
- **Session lifetime: NOT persistent (answered §F).** Two clean re-logins
  both required the full 2FA challenge, so unattended refresh is
  impossible — `login` is always interactive. The profile does persist a
  device token (the challenge trigger shifted from `UNRECOGNIZED_DEVICE`
  to `EXTRA_SEC_AT_SIGN_IN`), but the account's posture forces step-up
  every time and no "remember this device" opt-out surfaced.

## 2. The discovery harness (`explore`)

`explore.py` is copied and adapted from `carta`'s — the richest of the
four existing explore harnesses (`angellist`, `carta`, `cointracking`,
`equityzen`). Their ~4× duplication is a tracked, deliberate decision:
each harness drifts with its source, so they are copied and adapted, not
extracted into a shared library.

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/chase/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map — every request + response. Flushed only on a clean context close. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR; text bodies ≤ 200 KB captured inline, OFX/QFX content types included. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and OTP-field events. |
| `downloads/` | Every file the session fetches (statement PDFs, exports), sequence-prefixed against reused filenames. |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace — off by default because the pinned Playwright 1.49 tracer crashes the camoufox 152.0.4 build (matched-set drift; see base-camoufox). |

Mechanics worth knowing before reading the code:

- **Login pre-fill.** `CHASE_USERNAME` / `CHASE_PASSWORD` are sourced
  from `/secrets/chase.env`; `launch.firefox_prefs()` disables
  Firefox's password manager (`signon.*`) so a profile-saved credential
  can never autofill on top of the programmatic fill and double the
  field. Each field is filled at most once per page and verified by
  read-back (one clear-and-retry), and only when username + password
  fields co-exist in the same chase.com frame — so the password can
  never land in a lone input like the 2FA code entry. The fill is
  frame-aware: the standalone sign-in host carries the form in the main
  frame, while the www.chase.com homepage embeds it in a
  secure\*.chase.com iframe. Sign-in and 2FA are always submitted by
  hand over VNC; the harness never clicks a button.
- **OTP-field detector.** When a one-time-code input mounts in a
  chase.com frame, its static descriptor (id / name / autocomplete /
  maxlength — never its value) is logged, so the 2FA surface is
  recorded even if no click lands on it.
- **Credential redaction.** The username and password are scrubbed from
  every logged header, POST body, and response body — the debug dir is
  outside `~/.secrets/`, so a leak there is a real risk. Response
  bodies still carry full account data; the whole debug dir is treated
  as sensitive.

## 3. What Phase 1 must capture — the three flows

Each flow is one continuous VNC-driven session segment; all three fit in
a single recording.

### Flow 1 — Login

- The sign-in form (field ids / names — the pre-fill selectors are
  convention-based guesses until validated here).
- The 2FA challenge: which factors Chase offers (SMS / email / voice /
  app), the step order, and the OTP field's descriptor.
- The "remember this device" control: its exact wording and mechanism
  (checkbox? implicit?), and which cookie(s) it plants.
- **Session persistence:** after a successful login, close the browser
  and run `explore` again *without* `--fresh`. Whether the second run
  lands authenticated (device-trust honoured, no 2FA) or back at the
  challenge is the single most important data point for Phase 2 — it
  determines whether unattended refresh is ever possible. Observe only;
  no automation is designed around the answer now.

### Flow 2 — Account statements

- Navigate to the statements / documents area.
- How statements are listed: per-account or unified, year/date
  filtering, pagination, and the listing's backing XHR (the HAR shows
  it).
- Fetch at least one monthly statement PDF per account type — the fetch
  mechanism (signed URL? session-cookie GET? POST-then-redirect?) is
  what `download.py` will replay.

### Flow 3 — Transaction history + exports

- Each account's full transaction history: the listing UI, its date
  range limits, pagination, and the backing XHR + JSON shape.
- The export surface: Chase has historically offered CSV / QFX / OFX
  downloads with date ranges — verify in traces, do not hardcode. For
  every format actually offered, run one export with an explicit date
  range so the request parameters land in the HAR and the payload in
  `downloads/`.
- Note the maximum lookback the UI allows — it bounds what
  `download.py --lookback` can honour and what Phase 3 can backfill.

## Observed — authenticated captures (2026-08-11)

Four VNC sessions: one in-app-push login + full data walk (§B–§E), two
SMS-2FA logins (§A), and short re-login probes for session persistence
(§F). Endpoints are masked (`###` = elided id); no account numbers,
balances, device names, phone fragments, OTP codes, or card identifiers
are recorded here — they live only in the debug dir. The authenticated
app is a **hash-routed SPA**: every route stays on
`https://secure.chase.com/web/auth/dashboard`, only the `#/dashboard/…`
fragment changes, and all data arrives as JSON from `/svc/` endpoints —
so `download.py` replays `/svc/` calls with the session cookie + a CSRF
token, it does **not** navigate URLs. Non-app hosts in the trace are
CDN/telemetry (`asset|static|analytics|reco.chase.com`,
`go-mpulse.net`, `online-metrix.net`) and out of scope.

### §A — Login + 2FA

Credential submit and step-up run against `secure.chase.com`, in order:

1. `POST /svc/wl/auth/public/v1/site/availability/list` — pre-check.
2. `POST /auth/fcc/adaptive` — device / adaptive-fraud fingerprint.
3. `POST /auth/fcc/randomize` → `POST /auth/fcc/login` — credential
   submit (a `randomize` immediately precedes each `login`, presumably a
   field-encryption nonce).
4. The response demands step-up:
   `GET …/fraud/authentication/challenge-options/v8/options?…&event-identifier=<E>&challenge-token-identifier=<opaque>`
   returns the **factor menu** — `challengeMethodsDisplay.showAll`, plus a
   `phoneList` (SMS/voice destinations), `inAppDeviceList` (push targets),
   `emailList`, `cardList`, `callUs`. `<E>` was `UNRECOGNIZED_DEVICE` on
   the first-ever login and `EXTRA_SEC_AT_SIGN_IN` on every later one
   (§F). The `challenge-token-identifier` threads the whole step-up.

The factor menu is data-driven. **`challenge-invocations` has one body
shape for every factor** —
`{challengeTokenIdentifier, communicationMethodTypeCode, contactReferenceIdentifier}`
— differing only in the method code and which list the contact id comes
from. **Two completion paths were captured:**

**In-app push** (menu `["INAPP","CALL_US"]`):
5. `POST …/challenge-invocations/v1/…` with
   `communicationMethodTypeCode:"I"` + the chosen device's
   `oneTimeDeviceIdentifier` (from `inAppDeviceList`) sends the push.
6. `GET …/challenge-statuses/v4/…` is **polled** (×6) until approved.
7. *Glitch caught:* the first push didn't arrive, so the trace shows a
   **second `challenge-invocations` ~54 s later** with the polls between —
   re-invoking simply re-sends the push and the flow tolerates it. So the
   push path is a poll-until-approved loop with a re-invoke escape, not a
   one-shot.

**SMS OTP** (menu `["INAPP","OTP_SMS","OTP_VOICE","CALL_US"]`, two numbers
on file):
5. The user **picks a phone** from `phoneList` — each entry a
   `contactReferenceIdentifier` + a masked `last4DgtsPhoneNumber` +
   `smsEnabledIndicator`.
6. `POST …/challenge-invocations/v1/…` with
   `communicationMethodTypeCode:"S"` + the chosen phone's
   `contactReferenceIdentifier` sends the code and **returns a 3-char
   `oneTimePasswordPrefixText`** — Chase's anti-phishing prefix; the SMS
   carries the same prefix.
7. `POST …/challenge-verifications/v1/…` with
   `{…, otp:{oneTimeUserPasswordText:<8 digits>, oneTimePasswordPrefixText:<prefix>}}`
   submits the typed code. (This is where the push path polls
   `challenge-statuses` instead — the key structural difference.)

Voice (`OTP_VOICE`) was captured too: method `"V"` + a phone contact, and
its invocation returns the same 3-char prefix — so voice is SMS with the
code read out over a call instead of texted, and the same completion
(`challenge-verifications`) applies. `CALL_US` is a human phone call and
is never automatable. All method codes are now mapped in `auth_dialog.py`.

Both paths then finish with a `randomize` + `login` pair and
`POST /svc/wl/auth/l4/v1/user/router/list` into the app.

**Which factors are offered depends on device trust** (measured
2026-08-11): the **first login on a fresh profile** (`--fresh`, an
unrecognised device — `event-identifier=UNRECOGNIZED_DEVICE`) offers
**in-app push only** (+ Call Us); **later logins** on the same profile
(`EXTRA_SEC_AT_SIGN_IN`) also offer **SMS and voice**. So the very first
run must approve a push on the phone; only afterwards is a CLI code entry
available. Either way 2FA fires every time (§F).

**Pinned UI controls** (from the explore DOM snapshots — the login/2FA is a
closed-shadow iframe, so these came from `dom/`, not the click log):

- **Sign-in** (a chase.com iframe): `#userId-input-field-input`,
  `#password-input-field-input`, a `#rememberMe` device-trust checkbox;
  submit via Enter in the password field.
- **Method picker** ("Confirm Your Identity"): `<mds-list id="optionsList">`
  with `<mds-list-item id="inAppSend|sms|voice">`, then a `<mds-button
  id="next-content">` **Next** — the code/push is sent only when Next is
  clicked. Phone picker (SMS, >1 number): `mds-list-item[label*=<last4>]`.
- **Code entry**: `<mds-text-input-secure id="otpInput">` (its real input is
  in the shadow root — focus + type), then Next again.

These MDS custom elements are `display:contents` with a **closed shadow
root** and `tab-focusable="true"`: they render visibly but have no bounding
box, and a dispatched click reaches no shadow handler — so activation is
**focus + Enter/Space** (the a11y path). This is the shared
[`mdsui.activate`](mdsui.py); login.py and download.py drive every MDS
control through it.

**Encoded in [`auth_dialog.py`](auth_dialog.py)** — the browserless CLI
core the Phase 2 login.py wraps: parse `challenge-options`, choose a
factor, **pick a phone when several are on file** (the dialog this
capture reproduces), show the prefix, read the 8-digit code, and build
the invocation / verification payloads. Pure (no network/browser),
unit-tested, with a `--demo` that replays the dialog on synthetic data:
`./chase sh -c "python3 /app/auth_dialog.py --demo"`, or host-side
`python3 collectors/chase/auth_dialog.py --demo`. All method codes are
observed and mapped (push `"I"`, SMS `"S"`, voice `"V"`). The dialog picks
the factor/phone and reads the code; login.py drives the corresponding MDS
controls (the "Pinned UI controls" above).

### §B — Accounts

`POST /svc/rr/accounts/secure/v2/account/detail/dda/list` returns
deposit-account detail; `POST /svc/rl/accounts/secure/v1/dashboard/module/list`
and `POST /svc/rr/accounts/secure/v1/account/activity/download/options/list`
enumerate the roster; `POST /svc/rr/accounts/secure/v1/account/routing/list`
carries the account + routing numbers. The roster distinguishes deposit
accounts (`summaryType=DDA`, `detailType` in {`CHK`, `SAV`}) from
non-deposit products (`CARD`/`BAC`/`ATM`); the parser keeps the former and
drops the latter (scope note below).

### §C — Statements & documents

Doc centre route `#/dashboard/documents/myDocs/index` with a
`documentType` in {`STATEMENTS`, `TAX_DOCUMENTS`, `NOTICES`,
`CHECK_DOWNLOAD`} and a date filter (Last 7/30/90 days, or a year).

- **Listing:** `POST …/documents/secure/idal/v2/dockey/list`,
  `…/idal/v2|v4/docref/list`, `…/tax/v2/docref/list`,
  `…/v1/document/listing/list`.
- **Statement PDF:** `GET …/documents/secure/idal/v5/pdfdoc/star/list`
  → `application/pdf`. Both "Save as PDF" and "Save as accessible PDF"
  hit the same endpoint.
- **Check images:** `POST …/accounts/secure/v1/account/activity/dda/checks/createpdf/list`
  then `POST …/documents/secure/v1/document/listing/download/list`
  → `application/pdf`.

**Live-validated UI path (2026-08-12).** download.py drives the statements
centre (read-only), it does not replay the GET. Unlike the MDS surfaces
(§A/§E), this is a **classic light-DOM** page — stable ids, no shadow, so
plain Playwright clicks work:

- Route `#/dashboard/documents/myDocs/index;documentType=STATEMENTS`. The
  STATEMENTS accordion (`#button-accountsAccordian-STATEMENTS`) defaults open
  over a plain table `#accountsTable-STATEMENTS` whose rows are contiguous
  (`row0`, `row1`, …); `cell0` is the date ("Dec 17, 2025"), `cell3` the
  download control. The first missing row ends the walk.
- Per row: click `#header-accountsTable-STATEMENTS-row{n}-cell3-downloadDocumentDropdown`
  to open the menu, then `#item-0-{n}-downloadPDFOption` ("Save as PDF") fires
  a browser download captured via `expect_download`; saved as
  `statements/<ext>/<YYYY-MM-DD>.pdf`.
- The list is scoped by a **year styled-select** (`#header-filterstyledselect-0`,
  options `#container-{i}-filterstyledselect-0`). The collector pages through
  each year in the export window — a **native click** on the option matching
  the year swaps the table (a `get_by_text` did not select it). No per-account
  selector on this surface, so PDFs file under the primary deposit account.

### §D — Transaction history

- **Listing:** `GET …/deposit-account/transactions/inquiry-maintenance/etu-dda-transactions/v3/transactions?digital-account-identifier=<id>&requested-record-count=N`
  — paginated by record count.
- **Summaries:** `…/digital-transaction-summary/v1/activity-summaries`.
- **Per-transaction detail:** `…/digital-deposit-account-transactions/v2/deposit-account-transaction-details?transaction-identifier=<id>&…`.
- **Check image (inline):** `…/digital-checks/v1/images?…&item-type-name=CHECK`.

### §E — Transaction export (answers flow 3)

Route `#/dashboard/transactions/downloads/<id>/DDA/CHK`. The export is
`POST /svc/rr/accounts/secure/v1/account/activity/download/dda/list` with
a form body:

```
transactionType=ALL&filterTranType=ALL&statementPeriodId=ALL
&downloadType=CSV&accountId=<id>&dateHi=<date>&dateLo=<date>
&csrftoken=<token>&submit=Submit
```

returning `application/csv`. A `…/download/count/dda/list` (same params,
no csrftoken) pre-flights the row count; `…/download/options/list`
enumerates downloadable accounts.

- **Format = the `downloadType` field.** The dropdown offers four:
  `CSV`, `QFX`, `QIF`, `QBO` (all captured). `--lookback` → `dateLo`
  (with `dateHi` = today); `statementPeriodId` is the alternative "whole
  statement period" selector.
- **A `csrftoken` is mandatory** on the export POST — download.py must
  harvest it from the session/page (its source is in the trace); the
  count pre-flight does not need it.

**Which format is most comprehensive — none alone; capture CSV + QFX.**
The same day's export in all four formats compared (schema only; ~637
transactions each):

| field | CSV | QFX / QBO (OFX 1.02) | QIF |
| --- | --- | --- | --- |
| per-row running **balance** | **yes** (`Balance`) | no (only statement-level `LEDGERBAL`/`AVAILBAL`) | no |
| stable txn id **`FITID`** | no | **yes** | no |
| type | `Type` + `Details` | `TRNTYPE` | — |
| description | `Description` (one field) | `NAME` + `MEMO` (split) | payee only (`P`) |
| check no. | `Check or Slip #` | `CHECKNUM` | `N` |
| account id / type | — | `ACCTID` / `ACCTTYPE` | — |

- **QBO is byte-identical to QFX** (both OFX 1.02, same tags) — the
  QuickBooks vs Quicken label only. Pull **one** of them, not both.
- **QIF is strictly poorest** (no balance, no id, no type/memo) — skip.
- So the richest pair is **CSV + QFX**: CSV alone carries the per-row
  balance (needed to reconcile and to derive a balance series), QFX alone
  carries the `FITID` stable id (needed for **idempotent silver dedup**
  across overlapping windows — CSV has no stable key). download.py should
  fetch both and load.py join them (per account + date + amount + check
  no.) to attach the `FITID` to the balance-bearing CSV row — the
  "combining formats" the flow-3 brief anticipated.
- Note: CSV carried 3 more rows than the OFX/QIF exports (640 vs 637) —
  likely pending items OFX omits, or a multi-line-description artefact;
  reconcile in Phase 3.
- **Richer still, but more fragile:** the `/svc/` JSON listing (§D)
  carries a `transaction-identifier` and `ENRICHED_MERCHANT` segments the
  flat exports lack. A Phase 2 option is to capture that JSON alongside
  the exports; the exports are the stable, parseable baseline.

**Live-validated UI path (2026-08-12).** download.py drives the download
page (read-only), it does not replay the POST. The proven mechanics:

- **Roster** comes from the overview's `account/detail/dda/list` XHRs. A
  same-route SPA `goto` after login does not re-fire them, so a
  `page.reload()` nudge forces the fetch; capture keys on the endpoint URL,
  not a guessed body shape. The overview also exposes the cards; only the
  `CHK`/`SAV` (DDA) rows are kept.
- The form controls are MDS `<mds-select>`s (open shadow) plus a light-DOM
  **`<button id="downloadButton">`** — the real trigger (an earlier note
  named `downloadOtherActivity`, which is a *different* page's control and
  is absent here). Selects and options are driven by **real Playwright
  clicks** (`mdsui.click` / `get_by_role("option")`), never a synthetic
  dispatch (same open-shadow lesson as the 2FA list, §A).
- Two independent-state gotchas, both handled: re-picking the **default**
  format (CSV) leaves the option listbox open over the button → press
  Escape before clicking; and a second export on the same page load can be
  stranded by a leftover dialog → **one fresh page load per format**, each
  waiting for the file-select to render.
- `#downloadButton` fires a browser download captured via
  `expect_download`; CSV + QFX are saved to `transactions/<id>.{csv,qfx}`,
  and the roster to `accounts.json` (the loader's bronze contract).

**The export is hard-capped at ~24 months; statements fill the tail.**
`dateLo` earlier than the cap is rejected (the UI answers "choose a date
after <cap>"), so the CSV/QFX ledger only reaches back two years. The
statement PDFs, by contrast, list back seven years, and each one carries
its own transaction detail — so they are the only source of the older
rows.

- **A combined statement carries one segment per product**, each opened by
  a `*start*global product*` header with its own summary (beginning /
  ending balance) and its own transaction sections — and the section names
  repeat across segments, so rows must be attributed per-segment, never
  pooled. `statement_parser` splits on the header and, per segment, reads
  the deposit / withdrawal / check sections (signed by section) and
  validates the parse against the segment's own balance pair.
- **Segments carry no account ids the exports know** (the printed numbers
  are a different form than the export `ACCTID`), so
  `load.load_statement_transactions` attributes them by **balance
  chaining**: the newest pre-seam statement anchors on the segment whose
  ending balance appears among the export's running balances in-period,
  and each older statement then chains on ending(month k) ==
  beginning(month k+1). An ambiguous or broken link stops the walk — older
  statements are skipped, never guessed at — and segments belonging to
  other products on the statement are never touched (deposit-only scope).
- Only the rows **before the export seam** — `MIN(posted_at)` over the
  export-sourced (`qfx`/`csv`) rows — are imported. Anchoring the seam to
  the export rows (never all rows) keeps the two sources disjoint and
  keeps the seam from drifting as statements are added; a segment that
  fails to reconcile (beginning + Σ ≠ ending) is skipped, never injected.
  The running balance for a statement row is reconstructed from the
  segment's beginning balance in posted-date order, so the end-of-day
  balance is exact regardless of intra-day order — and the reconstructed
  tail must land exactly on the export's opening balance, which the first
  full-archive load verified to the cent.

### §F — Session persistence (answered: not persistent)

Two clean re-logins after the first, each on the persistent Camoufox
profile (no `--fresh`), **both required the full 2FA challenge** — so
unattended refresh is not possible; every `download` run needs an
interactive login. One nuance: the first-ever login fired
`event-identifier=UNRECOGNIZED_DEVICE`, but the later ones fired
`EXTRA_SEC_AT_SIGN_IN` — the profile **does** persist a device token
(Chase now recognises the device), yet the account's posture forces
step-up at every sign-in regardless. No "remember this device" opt-out
surfaced in any capture. Design implication for Phase 2: treat `login` as
always-interactive; there is no `login --check`-then-skip path to lean on.

## 4. As built, and what's left

### login + download — one-shot, browser-driven (as built)

`login.py` + `download.py` follow the **schwab-web twin**, not an API
replay: because the session dies with Firefox and 2FA fires every time
(§F), login and scrape run in one browser lifetime, and `login` folds into
`download`.

- **login.py** launches headed Camoufox under Xvfb, pre-fills + submits the
  sign-in form (reusing explore's frame-aware prefill; submit is Enter in
  the password field, no button selector), then completes 2FA one of two
  ways, then hands the authenticated `page` to `download.walk()`:
  - **CLI-MFA (default, `download`, no VNC):** it parses the captured
    `challenge-options` response into [`auth_dialog.py`](auth_dialog.py)
    and drives the challenge **from the terminal** — picks the number,
    reads the code from stdin, types it into the page. The browser holds
    the cookie and lets Chase's JS finish the sign-in (after
    `challenge-verifications` Chase runs the final `randomize`+`login`+
    `router`, §A — replaying it out-of-band would strand that), so nothing
    is API-replayed. Auth is confirmed by the response watcher (below).
  - **VNC (`vnc-login`, `--no-cli-mfa`):** the fallback — exposes VNC and
    waits for Sign In + 2FA to be done by hand, for when the CLI drive
    can't find a challenge control (`--debug` dumps each frame's DOM on
    failure to re-pin a drifted selector).
  - **Auth detection is response-based, pumped correctly.** The pre-auth
    logon page and the dashboard share a URL (§A), so login waits for a
    signed-in `/svc/` call (`/user/router/list` or `/accounts/secure/…`).
    The waiter pumps the Playwright sync event loop (`wait_for_timeout`,
    not `time.sleep`) — a bare sleep never delivers `.on()` events, which
    once made login miss a completed sign-in.
  `--check` probes a persisted session read-only (reports DEAD between
  runs by design, §F).
- **download.py** `walk()` reads the deposit-account roster from the
  `download/options/list` response (§B, credit-card rows dropped), and per
  account exports **CSV + QFX** through the download UI — the browser holds
  the cookie and sets the `x-jpmc-*` / CSRF headers, so the UI trigger is
  more robust than replaying the raw POST. `--lookback` bounds the export
  window; `--no-documents` skips statements; `--dry-run` walks without
  exporting. Bronze per run dir: `accounts.json`, `transactions/<id>.{csv,qfx}`,
  `statements/<id>/*.pdf`, `raw/*.json`, and a `run.json` status manifest.
  The UI selectors / hash routes (`ROUTE_*`, `SEL_*`) and the statement-save
  loop (`_save_statements`, §C) are validated live.

### load — silver (as built, unit-tested)

`load.py` parses each bronze run into the silver schema (`migrations/0001`):
accounts (content-deduped per snapshot), the transaction ledger, and the
statement inventory. The ledger is the **CSV↔QFX join** (§E): QFX rows key
on `<FITID>`, the CSV running balance joins on (date, amount, check no.),
and a CSV-only pending row is kept under a synthetic id. Idempotent
(`INSERT OR IGNORE` on `fitid`, snapshot gate on `dump_runs`); `--force`
rebuilds. Fully covered by `tests/test_load.py` against synthetic exports
— no live site needed. QIF/QBO are deliberately not fetched (QBO dupes
QFX, QIF is poorest).

### Phase 4 (remaining) — gold adapter

A Go adapter under `wealthdb/internal/silver/chase/` (new kind
`chase`), registered from `init()` like the others. The cash accounts
map to `account_kind=cash`; `tax_wrapper` / `management_style` follow
config `account_overrides` where silver can't say. If credit cards are
scoped in (see below), they map to a liability `account_kind` (revolving
credit), not cash.

**Returns: conduit accounts.** The Chase **cash** accounts are conduits
— their transactions feed cross-source money-flow tracking, but a single
deposit account's own TWR/MWR is noise. Resolved by the registered
ReturnsPolicy (`internal/silver/chase/policy.go`): the bank flow set with
`AccountsGrainMeaningless = true`, which blanks the accounts-grain
TWR/MWR (values and rows stay) while the source/global grains keep the
accounts and count their external flows. No `returns_exclude` config is
involved — that block (docs/DESIGN.md §5.5) exists for another person's
holdings in a shared login and would drop the accounts and their flows
from coarse grains entirely; per-deployment policy adjustments go
through `returns_policy_overrides` (docs/DESIGN.md §5.6) instead.

### Scope decision: credit cards — deposit-only

Decision: **deposit-only** — the collector covers the deposit accounts
(checking, and savings if present); any credit-card or other products the
same login may expose are out of scope in code and docs, exactly like the
investment surface. This is the smallest build and the conduit-returns
reasoning above applies as-is. CLAUDE.md keeps card
surfaces out of scope for both reads and writes (card *management* stays
forbidden regardless).

**Recorded future expansion (not planned):** covering cards would add
real value — liabilities for net worth, card spend + statement-balance
payments for money-flow — but is a materially larger build: separate
card-activity / card-statement endpoints in Phase 2, a liability
`account_kind` and revolving-credit shape in Phases 3–4, and no
conduit-returns rationale (a card is not a conduit). Adding it later is a
deliberate scope expansion, not a default.

## 5. Status & operation

The pipeline through silver is built and **validated live end-to-end**
(2026-08-12): push and SMS 2FA, roster discovery, CSV + QFX export, and
statement PDFs, plus the unit-tested silver load.

- `make build-chase`; `~/.secrets/chase.env` with `CHASE_USERNAME` /
  `CHASE_PASSWORD` at chmod 0600 (single-quote values with `$` / `!` /
  backticks).
- `wealthdb-collect chase download` pre-fills + submits the form, drives
  2FA **from the terminal** (picks the number, reads the code on stdin — no
  VNC), then scrapes. `--debug` captures each chase frame's DOM + a
  screenshot to the `/debug` mount on any failure; `wealthdb-collect chase
  vnc-login` is the by-hand fallback if a challenge control can't be driven.
- `wealthdb-collect chase load` parses bronze → silver. `wealthdb-refresh
  chase` drives login → download → load.

Lessons that shaped the browser code: auth detection is **response-based,
not URL-based** (logon page and dashboard share a URL, §A); the waiter
**pumps the Playwright event loop** (a bare `time.sleep` never delivers
`.on()` events); the MDS controls render in an **open** shadow root, so a
real Playwright click (`click_role` / a boxed descendant) fires the handler
where a native click, a dispatch, keyboard, and coordinate clicks did not.

Remaining: the **Phase 4 gold adapter** (Go, `kind: chase`) — separate from
the collector, needed for `wealthdb load -a` to merge chase silver into gold.
