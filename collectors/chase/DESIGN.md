# chase — design notes

Design notes for the Chase retail collector, kept as a decision log.
Sections: scope and what is known (§1), the discovery harness (§2), the
capture brief the harness was pointed at (§3), the authenticated captures
that answered it ("Observed", §A–§F), what was built and the scope
decisions behind it (§4), and status and operation (§5). §1–§3 were written
before any capture; where a trace later settled a question, the "Observed"
sections and §4 say so and supersede them.

## 1. Scope & what is known so far

- **The relationship.** A retail banking relationship at JPMorgan Chase
  (chase.com). No brokerage / investment surface is in scope, even where
  the same login exposes one (see CLAUDE.md). Scope is the retail
  **deposit accounts** (checking, and savings if present) **and the
  credit-card accounts, read-only** — card roster, card detail, card
  transaction export, card statements. Card **management** (payments,
  autopay, limits, disputes, lock/unlock, anything that mutates state at
  the provider) is out of scope in code and docs, as is any other product
  the same login may expose. See the §4 scope note for the decision and
  its superseded history.
- **Conduit accounts.** The cash accounts are conduits — cash passes
  through them on its way to and from other sources. Their transactions
  matter for cross-source money-flow tracking; their own return rows are
  noise and are hidden by the registered ReturnsPolicy
  (`internal/silver/chase/policy.go`, `AccountsGrainHidden`); the coarse
  grains keep the balances and count the flows — see §4.
  The credit-card accounts are **not** conduits — they are
  revolving-credit liabilities, a different `account_kind` with different
  endpoints, and the conduit rationale above is not an argument about
  them. The policy is registered per source kind, though, so
  `AccountsGrainHidden` covers the card accounts too; if a card is ever to
  carry its own return rows, the policy has to learn the product.
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
  built in [`auth_dialog.py`](auth_dialog.py). Every automatable factor's
  method code is captured and mapped — push `"I"`, SMS `"S"`, voice `"V"`
  (§A).
- **Session lifetime: NOT persistent (answered §F).** Two clean re-logins
  both required the full 2FA challenge, so unattended refresh is
  impossible — `login` is always interactive. The profile does persist a
  device token (the challenge trigger shifted from `UNRECOGNIZED_DEVICE`
  to `EXTRA_SEC_AT_SIGN_IN`), but the account's posture forces step-up
  every time and no "remember this device" opt-out surfaced.

## 2. The discovery harness (`explore`)

`explore.py` is copied and adapted from `carta`'s — the richest of the
explore harnesses at the time. Their duplication across the fleet is a
tracked, deliberate decision: each harness drifts with its source, so they
are copied and adapted, not extracted into a shared library.

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/chase/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map — every request + response. Flushed on the context close and rewritten through the redactor on the same unwind, error included: Playwright records it raw, so it is never crash-safe and secret-free at once. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR; text bodies ≤ 200 KB captured inline, OFX/QFX content types included. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and OTP-field events. |
| `downloads/` | Every file the session fetches (statement PDFs, exports), sequence-prefixed against reused filenames. |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace — off by default because the pinned Playwright 1.49 tracer crashes the camoufox 152.0.4 build (matched-set drift; see base-camoufox). A trace cannot be redacted after the fact: its DOM snapshots store every input's value, the typed password included. |

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
  every header, POST body, and response body *the harness itself
  writes*, and password inputs are blanked out of the DOM snapshots
  (`collectorkit.debugcap`) — the debug dir is outside `~/.secrets/`, so
  a leak there is a real risk. The HAR comes from Playwright raw and is
  rewritten through the same redactor once the context close has flushed
  it; the opt-in trace is the one artefact nothing rewrites
  (collectors/README.md, "Captures carry credentials"). Response bodies
  still carry full account data; the whole debug dir is treated as
  sensitive.

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

**SMS OTP** (menu `["INAPP","OTP_SMS","OTP_VOICE","CALL_US"]`):
5. The user **picks a phone** from `phoneList` (when more than one is
   listed) — each entry a `contactReferenceIdentifier` + a masked
   `last4DgtsPhoneNumber` + `smsEnabledIndicator`.
6. `POST …/challenge-invocations/v1/…` with
   `communicationMethodTypeCode:"S"` + the chosen phone's
   `contactReferenceIdentifier` sends the code and **returns a 3-char
   `oneTimePasswordPrefixText`**, which the verification echoes back. It
   was read as an anti-phishing prefix the SMS would repeat; the message
   Chase actually sends does not carry it, so nothing displays it.
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

**Pinned UI controls** (from the explore DOM snapshots — the login/2FA sits
in a cross-origin iframe whose clicks never reach the top-document
listener, so these came from `dom/`, not the click log):

- **Sign-in** (a chase.com iframe): `#userId-input-field-input`,
  `#password-input-field-input`, a `#rememberMe` device-trust checkbox;
  submit via Enter in the password field.
- **Method picker** ("Confirm Your Identity"): `<mds-list id="optionsList">`
  with `<mds-list-item id="inAppSend|sms|voice">`, then a `<mds-button
  id="next-content">` **Next** — the code/push is sent only when Next is
  clicked. Phone picker (SMS, >1 number): `mds-list-item[label*=<last4>]`.
- **Code entry**: `<mds-text-input-secure id="otpInput">` — its real input is
  `#otpInput-input` in the open shadow root, reached by a real locator and
  typed into with key events; the zero-box host does not delegate
  keystrokes. Then Next again.

These MDS custom elements are `display:contents`: they render visibly but
have no bounding box of their own, and a dispatched click reaches no
handler. The first reading of the DOM snapshots took the shadow root for a
**closed** one and made activation focus + Enter/Space (the a11y path);
driving the live controls disproved that. The shadow root is **open**,
Playwright pierces it, and the reliable activation is a **real Playwright
click** on the element rendered inside — an `<a href>` (role=link) for a
list row, a role=button for a button. That is
[`mdsui.click_role`](mdsui.py), with [`mdsui.activate`](mdsui.py) as the
selector-addressed fallback; login.py and download.py drive every MDS
control through the pair.

**Encoded in [`auth_dialog.py`](auth_dialog.py)** — the browserless CLI core
login.py drives: parse `challenge-options`, choose a factor, **pick a phone
when several are on file**, read the 8-digit code, and build the invocation
/ verification payloads. The anti-phishing prefix is carried on the wire but
never shown: it does not appear in the message the code arrives in. Pure (no
network/browser), unit-tested, with a `--demo` that replays the dialog on
synthetic data: `./chase sh -c "python3 /app/auth_dialog.py --demo"`, or
host-side `python3 collectors/chase/auth_dialog.py --demo`. All method codes
are observed and mapped (push `"I"`, SMS `"S"`, voice `"V"`). The dialog
picks the factor/phone and reads the code; login.py drives the corresponding
MDS controls (the "Pinned UI controls" above).

### §B — Accounts

`POST /svc/rr/accounts/secure/v2/account/detail/dda/list` returns
deposit-account detail; `POST /svc/rl/accounts/secure/v1/dashboard/module/list`
and `POST /svc/rr/accounts/secure/v1/account/activity/download/options/list`
enumerate the roster; `POST /svc/rr/accounts/secure/v1/account/routing/list`
carries the account + routing numbers. The roster distinguishes deposit
accounts (`summaryType=DDA`, `detailType` in {`CHK`, `SAV`}) from
non-deposit products (`CARD`/`BAC`/`ATM`). Card rows came into scope with
the 2026-09-04 amendment (scope note below), so the parser keeps both DDA
and CARD rows — each stamped with a `product` discriminator (`dda` |
`card`) — and drops the rest (an `ATM` row is a debit card, not an account
of its own). Card detail is captured too, but only nested inside the
`dashboard/module/list` envelope: `overview/card/v2/list` (every card in
one payload) and `account/detail/card/list` are never delivered as
standalone requests.

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
- Per row, two save shapes are coded (`statement_click_paths`) and tried in
  order, because each is evidenced on only one surface: a direct "Saves
  document" anchor
  (`#icon-accountsTable-STATEMENTS-row{n}-cell3-requestThisDocumentLink-download`),
  seen in the card capture and preferred; and the legacy per-row
  `…-cell3-downloadDocumentDropdown` menu then `#item-0-{n}-downloadPDFOption`
  ("Save as PDF"), which the deposit path was built on. Whether the anchor
  replaced the dropdown everywhere is unverified, so neither may be assumed
  gone. Either fires a browser download captured via `expect_download`; saved
  as `statements/<ext>/<YYYY-MM-DD>.pdf`.
- The list is scoped by a **year styled-select** (`#header-filterstyledselect-0`,
  options `#container-{i}-filterstyledselect-0`). The collector pages through
  each year in the requested window — a **native click** on the option matching
  the year swaps the table (a `get_by_text` did not select it). No per-account
  selector on this surface: a statement belongs to every deposit account
  printed on it, so the PDFs file under one deterministic bucket — the lowest
  deposit external id — and `load` attributes each account's segments out of
  that one relationship-wide pool by balance chaining (§E). The bucket name
  is not a claim about whose statement it is.
- **Cards page their own route.** Card documents are per card, never
  combined, so each card is walked on the account-scoped
  `…/myDocs/index;accountId=<ext>;documentType=STATEMENTS;mode=accounts` and
  its PDFs file under that card's id. The documents menu
  (`documents/secure/v1/menu/list`) says which accounts carry STATEMENTS at
  all, so a card listing none is skipped rather than paged empty.

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
  `CSV`, `QFX`, `QIF`, `QBO` (all captured). The endpoint carries its own
  window as `dateLo` / `dateHi` — that is how the dropdown parameterised
  it in the discovery traces — and `statementPeriodId` is the alternative
  "whole statement period" selector. `download.py` sets neither: it drives
  the UI's activity select to "All transactions" instead, so `dateLo`
  never carries `--lookback`.
- **A `csrftoken` is mandatory** on the export POST — download.py must
  harvest it from the session/page (its source is in the trace); the
  count pre-flight does not need it.

**Which format is most comprehensive — none alone; capture CSV + QFX.**
All four formats carry the same rows for a given window and differ only
in the fields they state:

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
  carries the clean type/name/memo split and the `FITID`. download.py
  fetches both and load.py joins them on (post date, amount) to attach the
  balance to the QFX row — the "combining formats" the flow-3 brief
  anticipated. (The check number is deliberately NOT in that key, only a
  tie-break inside a bucket: the two exports disagree about it, and keying
  on it fails the join outright. The `FITID` is not the silver key either:
  because each format is fetched separately and can fail on its own, a key
  only one format carries makes a row's identity depend on which file a run
  landed. Silver keys on content instead — see "load — silver" below — and
  keeps the `FITID` as payload.)
- The two exports need not agree row-for-row: the CSV can carry rows the
  OFX omits (pending items, most likely). The loader keeps such a row:
  `merge_transactions` appends the unmatched CSV leftovers rather than
  dropping them.
- **Richer still, but more fragile:** the `/svc/` JSON listing (§D)
  carries a `transaction-identifier` and `ENRICHED_MERCHANT` segments the
  flat exports lack. A Phase 2 option is to capture that JSON alongside
  the exports; the exports are the stable, parseable baseline.

**Live-validated UI path (2026-08-12).** download.py drives the download
page (read-only), it does not replay the POST. The proven mechanics:

- **Roster** comes from the overview's `account/detail/dda/list` XHRs. A
  same-route SPA `goto` after login does not re-fire them, so a
  `page.reload()` nudge forces the fetch; capture keys on the endpoint URL,
  not a guessed body shape. The overview also exposes the cards: since the
  2026-09-04 amendment (§4 scope note) the reducer keeps the `CHK`/`SAV`
  (DDA) rows *and* the `CARD` rows, each stamped with its `product`, and
  drops everything else.
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
- **A row's description is what the statement prints about that row, and
  nothing else.** Checks Paid has columns of its own — `CHECK NO. |
  DESCRIPTION | DATE PAID | AMOUNT` — so the number is read into
  `check_number` (gold's column of the same name, DESIGN.md §10.8) and the
  description is whatever Chase knows about the payee, which for a check it
  holds only electronically is nothing at all. Two kinds of page furniture
  print *inside* the section markers and are refused explicitly rather than
  folded into the row above as continuation text: the footnote legend under a
  section total, and a document id stamped in the right margin.
- **Segments carry no account ids the exports know** (the printed numbers
  are a different form than the export `ACCTID`), so
  `load.load_statement_transactions` pools every deposit statement in the
  tree — one parse per content hash, one copy per period, whichever bucket
  directory it was filed under — and attributes them per account by **balance
  chaining**: the newest pre-seam statement anchors on the segment whose
  ending balance appears among the export's running balances in-period,
  and each older statement then chains on ending(month k) ==
  beginning(month k+1). An ambiguous or broken link stops the walk — older
  statements are skipped, never guessed at — and segments belonging to
  other products on the statement are never touched. A card's statements
  never reach this pass at all; they have their own (§4).
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
- **download.py** `walk()` reads the account roster from the
  `download/options/list` response, each record stamped with its `product`
  (§B), and per account — deposit and card alike — exports **CSV + QFX**
  through the download UI — the browser holds the cookie and sets the
  `x-jpmc-*` / CSRF headers, so the UI trigger is more robust than
  replaying the raw POST. The export always picks Chase's widest activity
  option ("All transactions", ≈ 24 months — a superset of any window, per
  the fleet lower-bound contract), so `--lookback` bounds only the
  statement-PDF pass, and a start earlier than the 24-month cap draws one
  warning per run. The collector's own `--help` (`entrypoint.sh`) says
  which pass the flag reaches. `--no-documents` skips statements;
  `--dry-run` walks without exporting.
  Bronze per run dir: `accounts.json`, `transactions/<id>.{csv,qfx}`,
  `statements/<id>/*.pdf`, `raw/*.json`, and a `run.json` status manifest.
  Which paths' selectors are validated live: §5.

### load — silver (as built, unit-tested)

`load.py` parses each bronze run into the silver schema
(`migrations/0001` … `0004`): accounts (content-deduped per snapshot), the
transaction ledger, and the statement inventory. The ledger is the
**CSV↔QFX join** (§E): the QFX row is the row of record and the CSV
running balance joins on **(post date, amount)**, with the check number as
a tie-break inside a bucket rather than part of the key — the two exports
disagree about it, and keying on it failed the join and emitted the CSV
row a second time as an unmatched leftover. A CSV-only pending row is
still kept. Idempotent (snapshot gate on `dump_runs`; the deposit,
statement and document ledgers `INSERT OR IGNORE` on their stable key,
while the card ledger is written by the field-authoritative
`_upsert_card_transaction`, which adds rows and repairs columns the export
that first landed a row could not carry, and removes nothing); `--force`
rebuilds. Fully covered by `tests/test_load.py` against synthetic exports
— no live site needed. QIF/QBO are deliberately
not fetched (QBO dupes QFX, QIF is poorest).

**Transaction ids are content-derived for both products**, never the
provider's `<FITID>`, which is carried in `payload.fitid` for
traceability. An export row's id is
`<product>:<ext>:<content key>:<occ>`. The reason both ledgers key this
way is that **each format is fetched separately and can fail on its own**:
an account can land CSV-only in one run and as a complete pair in the
next, and a FITID-keyed id — the FITID lives only in the QFX — gives those
rows two identities, so `INSERT OR IGNORE` lands both and the ledger
doubles without ever converging. The **content key holds only what both of
that product's exports state identically**: (account, post date, amount)
on a deposit row, and those plus the descriptor on a card row. The deposit
descriptor is deliberately absent — the QFX splits it into `<NAME>` +
`<MEMO>` while the CSV `Description` is one longer field, and the two do
not reduce to a common form, so hashing it would put the format
dependence straight back. The **occurrence index** is what keeps the
resulting narrow key safe: same-day same-amount rows are
ordinary on a deposit account, and without it they collapse into one id
and rows are silently lost.

**Cards share the tables, discriminated by `accounts.product`**
(`migrations/0002`). Their export is its own shape: a different CSV header
(transaction date, category, the 5-way `Type`, no running balance) and the
credit-card OFX message set, routed off the files themselves rather than
the roster. The join runs on (post date, amount) — the two files cover the
same rows and each carries what the other lacks. Card ids follow the same
scheme as the deposit ones above, with the descriptor in the key (the two
card exports differ in one known way — the CSV replaces a descriptor's
commas with spaces — which normalises away). On a card the FITID is
disqualified twice over: besides the format argument, a card's FITIDs are
**not unique** — a credit that offsets a charge reuses its FITID, so a
bare-FITID key would drop a leg of every reversal pair. Amounts and the
card's balance stay provider-verbatim (spend negative, balance the positive
amount owed); negating the liability is the gold adapter's job, which it
does at one point — see "Phase 4" below.

**Card statements are their own pass**, on their own parser. A card
statement is a different document, not a variant: no OpenText markers, one
account summary and one activity block, one card per file (so no segment
attribution), an `Opening/Closing Date  MM/DD/YY - MM/DD/YY` period line
the deposit regex cannot read, and rows that print their own sign — the
deposit section sign would negate the credits section twice. Which
accounts get which parser is decided by the same file-content signal that
routes the exports, so an unreadable roster cannot hand a card's PDFs to
the deposit parser. Each statement is read for two things on two gates:
its **period balances**, which land in `statement_balances` for **every
era** (they are what anchors the balance reconstruction, so a period the
export already covers is wanted too), gated on the printed summary adding
up; and its **transactions**, gated on the row identity and on the
account's export seam. The row identity is checked **section by section** —
Σ all rows == New − Previous, Σ each section's rows == the summary figure
printed for it, and the two addends no section maps to (cash advances,
balance transfers) are zero — because the whole-period sum alone cannot see
a section this parser does not know about: its rows silently inherit the
preceding section's kind, and a section netting to zero moves no total at
all.

That transaction gate is on the whole billing PERIOD, not on each row's
date: a statement prints the transaction date and Chase bills by the post
date, so a row-level gate would let a row transacted before the seam but
posted after it land from both sources at once. The cost is the one
part-period straddling the seam, given up rather than double-counted. That
hole cannot grow (the seam is a `MIN` over rows already loaded) but it is
real, so the pass **warns** with the period and the row count given up, and
stamps that period's `statement_balances` row `transactions_covered = 0`
(migration `0003`) — otherwise silver asserts a period's two balances while
carrying none of its rows, and anyone reconciling between two anchors is
left with an unexplainable residual. The same flag marks a period whose row
identity failed, and every period of a card with no export loaded at all: an
absent seam bounds nothing, so importing there would put a statement copy of
every row beside the copy the first export lands. Such a card records its
anchors and imports its transactions on the load after its export arrives.
A silver DB built before that gate can already hold statement rows for a card
that had no seam, and `load` is additive — nothing here withdraws them. Such a
DB must be rebuilt with `load --force` and gold reloaded from it before that
card's export lands, or the export's copy of those rows joins the statement's.
`posted_at` on a statement-era card row is the transaction date, a
deliberate approximation the payload records. The export seam is
**per account** — a card export caps at 24 months while a deposit backfill
can reach much further, and one global `MIN(posted_at)` would discard the
shallower account's whole pre-export history.

**The card running balance is reconstructed, between provider anchors.**
Neither card export carries a running-balance column, so `derive_card_balances`
rolls the posted ledger forward from one statement `New Balance` to the
next, and for the newest span on to the roster's live balance. A span must
land on its closing anchor or it keeps **no** derived balance at all — the
mismatch is logged rather than papered over, which makes the continuity
check double as the canary for a ledger missing a row or carrying one too
many. The newest span legitimately trails the live figure by whatever has
not posted yet, so where the roster reported `pending_charges` the gap is
checked as an identity (posted + pending == live) and where it did not the
implied amount is logged unconditionally — logging only a non-zero gap
would make the identity observable exactly when it fails.

Only the export era gets a per-row balance at all. Statement-era rows carry
**none**: a statement does roll from its own opening figure, but it dates
its rows by transaction date while the cycle bills by post date, so a row
transacted before its period opened carries that period's balance while
sitting, by date, inside the previous one — read as a series the cycles
contradict each other. `statement_balances` carries that era's balance
truth instead.

This is also the one **destructive** pass in the loader: it rewrites
`transactions.balance` authoritatively, clearing any row a landing span did
not reach. It therefore requires **two agreeing signals** before it will
touch an account — silver's roster *and* the export files' own shape — so
that neither a mis-stamped `product` nor a mis-read export can, on its own,
wipe a deposit account's provider-supplied CSV balances. Every
reconstructed balance in silver — this pass's and the deposit statement
pass's alike — is marked `payload.balance_basis`; the deposit CSV export's
running-balance column is the only balance that goes in unmarked, which is
what lets "no marker" mean "the provider stated this number for this row".

**A partial dump is ingested, not withheld.** run.json still scores each
product's coverage (`score_coverage`), but nothing gates on that flag — it
records which product fell short, and silver loads the run regardless:
silver ingest is purely additive (monotemporal
`accounts`, `INSERT OR IGNORE` deposit ledger and documents, a card
upsert that only ever fills columns the incoming export both is
authoritative for and actually carries, `--force` replaying all of
bronze), so a short run simply lands fewer rows and the next run
completes them. Withholding a snapshot bought nothing and broke two
things — coverage also requires the statement pass, so one flaky PDF froze
the balances gold reads from the roster; and a withheld card roster with
its rows admitted made the gold adapter, which classifies a transaction by
its account's `product`, file card spend into the deposit ledger. A record
with no `product` key reads as the deposit default, so pre-card bronze
loads exactly as before.

### Phase 4 — gold adapter (as built)

The Go adapter lives under `wealthdb/internal/silver/chase/` (kind
`chase`) and registers itself from `init()` like the others, so
`wealthdb load -a` merges chase silver into gold. It projects one ACCOUNT
per roster account — `account_kind=cash` for a deposit account, `card` for
a credit card — display name from the nickname falling back to the last-4
mask, `tax_wrapper=taxable_personal` and `management_style=self_directed`,
all overridable via config `account_overrides` where silver can't say.
Neither product carries an instrument, so it emits **cash balances, not
positions**, from three sources that cannot collide on a day: the ledger's
running balance (one CLOSING mark per day it moved — the whole series for a
deposit account, the export era for a card), a card statement's printed
closing figure at each `period_end` the reconstruction does not reach, and
the roster's CURRENT mark. The whole transaction ledger of both products
follows, keyed on silver's `fitid` column — a content-derived id, not the
provider's `FITID` (see "load — silver").

**A card is a liability, and the sign turns exactly once.** Silver keeps
every card figure the provider's way, so the owed balance arrives positive
and the adapter negates it into gold's canonical negative cash; a credit
limit and available credit are not balances of anything owned and never
become rows. Card transaction kinds map from the provider's own vocabulary
and from the statement sections — an adjustment carries the issuer's own
sign, netting as a refund when it is a credit and as a purchase when it is
a debit — and an unmapped type keeps its raw value in the payload rather
than being guessed at. The gold-side detail lives in
`wealthdb/docs/adapters/chase.md`.

**Returns: conduit accounts.** The Chase **cash** accounts are conduits
— their transactions feed cross-source money-flow tracking, but their own
return rows are noise. Resolved by the registered ReturnsPolicy
(`internal/silver/chase/policy.go`): the bank flow set with
`AccountsGrain = AccountsGrainHidden`, which emits no rows for them at
any grain while the coarse aggregates keep their balances and count
their external flows (transfer legs against tracked sources cancel). No
`returns_exclude` config is involved — that block (docs/DESIGN.md §5.5)
exists for another person's holdings in a shared login and would drop
the accounts and their flows from coarse grains entirely; per-deployment
adjustments go through `returns_policy_overrides` / `returns_hide`
(docs/DESIGN.md §5.6-§5.7) instead.

### Scope decision: credit cards — reads in, management out

**Current decision (2026-09-04, opted into in writing).** The collector
covers the retail **deposit accounts** *and* the **credit-card accounts,
read-only**: the card roster, per-card detail (current and statement
balance, credit limit and available credit, due date, minimum payment,
rewards balance), card transaction history / export, and card statement
PDFs. Card **management** stays forbidden verbatim — lock/unlock,
replacement, PIN, spending limits, travel notices, digital-wallet
enrolment, card payments and autopay, balance transfers and cash
advances, disputes, rewards redemption, credit-limit requests,
statement-delivery settings. The read/write line is the whole contract:
collectors are read-only, and a card is simply another thing to read.
Any other product the same login may expose (notably the investment
surface) remains out of scope entirely.

The driver is the **spending** surface: cards carry the consumption
ledger that deposit accounts only hint at, plus revolving-credit
liabilities for net worth and statement-balance payments for money-flow
reconciliation. The build cost is the one the superseded note priced —
separate card-activity / card-statement endpoints to capture, a
liability `account_kind` and revolving-credit shape in the adapter, and
**no conduit-returns rationale** (a card is not a conduit, so the
`AccountsGrainHidden` policy above must not be extended to it).

**Superseded 2026-09-04 — the original deposit-only decision, kept for
the record:**

> Decision: **deposit-only** — the collector covers the deposit accounts
> (checking, and savings if present); any credit-card or other products the
> same login may expose are out of scope in code and docs, exactly like the
> investment surface. This is the smallest build and the conduit-returns
> reasoning above applies as-is. CLAUDE.md keeps card
> surfaces out of scope for both reads and writes (card *management* stays
> forbidden regardless).
>
> **Recorded future expansion (not planned):** covering cards would add
> real value — liabilities for net worth, card spend + statement-balance
> payments for money-flow — but is a materially larger build: separate
> card-activity / card-statement endpoints in Phase 2, a liability
> `account_kind` and revolving-credit shape in Phases 3–4, and no
> conduit-returns rationale (a card is not a conduit). Adding it later is a
> deliberate scope expansion, not a default.

The amendment takes the "deliberate scope expansion" path that note
describes — half of it. Card *reads* moved into scope; card *management*
did not, and never will without a fresh written opt-in.

## 5. Status & operation

The pipeline is built bronze through gold. The deposit path is **validated
live end-to-end** (2026-08-12): push and SMS 2FA, roster discovery, CSV +
QFX export, and statement PDFs, plus the unit-tested silver load.

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

The **Phase 4 gold adapter** (Go, `kind: chase`,
`wealthdb/internal/silver/chase/`) is built and registered, so
`wealthdb load -a` merges chase silver into gold — both products, the
deposit accounts as cash and the cards as revolving-credit liabilities
(§4).

**Deposit ids are loader output, not schema.** Deposit transaction ids
follow the content-derived, occurrence-indexed scheme of §4 ("load —
silver") rather than the QFX `FITID`; no migration carries the change, so
`schema_meta` cannot tell a silver DB built before it apart. Such a DB — its
deposit `fitid` values are bare FITIDs rather than `dda:<ext>:<key>:<occ>` —
and any gold DB built on it need a `load --force` silver rebuild followed by
a gold reload: the old ids do not exist any more, and gold keys chase
transactions on silver's `fitid`. `load` does not leave that to be
remembered: it counts the export rows whose ids are neither `dda:` nor
`card:`-prefixed and exits with that instruction rather than inserting every
one of them again under its content-derived id. Statement-sourced ids and
row *content* (dates, amounts, descriptions, balances, kinds) are
unaffected.

The credit-card read surfaces brought into scope by the 2026-09-04
amendment (§4) run the whole way: the card routes are captured,
`download.py` walks the roster / detail / export / statement passes into
bronze, `load.py` ingests them into silver (`migrations/0002` + `0003`) —
roster, exports, statement inventory, parsed statement period balances and
the pre-export transaction tail — and the adapter projects them as
liabilities.

Remaining: a live card run. Everything on the card path is driven off the
exploration capture, so the first real `download` is what confirms the
export form's account picker, the statement view's save control and the
card documents route behave as coded; the passes are written to fail
loudly (a refused export, an uncovered product in `coverage`) rather than
file a file under the wrong account.
