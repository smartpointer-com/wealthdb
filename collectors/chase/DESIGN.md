# chase — design notes

Design notes for the Chase retail collector. Sections:

- §1 scope and the source's shape;
- §2 the discovery harness;
- §3 the flows a discovery session walks;
- "Source behaviour" §A–§F, from the authenticated captures;
- §4 how login, download and load work, and the card scope decision;
- §5 operation.

The README holds the quick start, the bronze layout and the silver tables.

## 1. Scope & what is known

- **The relationship.** A retail banking relationship at JPMorgan Chase
  (chase.com). No brokerage / investment surface is in scope, even where
  the same login exposes one (see AGENTS.md). Scope is the retail
  **deposit accounts** (checking, and savings if present) **and the
  credit-card accounts, read-only** — card roster, card detail, card
  transaction export, card statements. Card **management** (payments,
  autopay, limits, disputes, lock/unlock, anything that mutates state at
  the provider) is out of scope in code and docs, as is any other product
  the same login may expose. See the §4 scope note for the decision.
- **Conduit accounts.** The deposit accounts are conduits: cash passes
  through them on its way to and from other sources. Their transactions
  matter for cross-source money-flow tracking. Their own return rows are
  noise, so the registered ReturnsPolicy hides them while the coarse grains
  keep their balances and flows. The credit cards are not conduits. They
  are revolving-credit liabilities, and gold keeps every `card` account out
  of returns altogether. The gold side is
  [docs/adapters/chase.md](../../wealthdb/docs/adapters/chase.md) §7.
- **No self-service API.** Chase's programmatic access for retail data
  runs through aggregator gateways (Akoya / Plaid-class), which are
  partner-gated — not something an individual retail login can
  self-provision. The web UI is the surface, as at the other
  portal-only sources (`carta`, `equityzen`, `angellist`).
- **Bot defense: Camoufox.** Akamai-class bot defense blocks vanilla
  Chromium outright at the two US siblings (`fidelity-web`, `schwab-web`),
  and Chase fronts the same class of defense. The usual escalation rungs
  (vanilla Chromium → vanilla Firefox) are skipped: the harness builds on
  `shared/images/base-camoufox.Dockerfile` and the persistent-profile
  pattern. Camoufox clears the login end-to-end. The traffic carries
  ThreatMetrix device fingerprinting (`h64.online-metrix.net`) and
  Akamai mPulse RUM (`s2.go-mpulse.net`) alongside the app.
- **2FA: in-app push, SMS or voice.** The factor menu is data-driven
  (`challenge-options`), so login.py branches on what the menu offers
  rather than assuming one factor. The interactive core is
  [`auth_dialog.py`](auth_dialog.py). Every automatable factor's method
  code is mapped: push `"I"`, SMS `"S"`, voice `"V"` (§A).
- **Session lifetime: not persistent (§F).** Every sign-in needs the full
  2FA challenge, so unattended refresh is impossible. The profile does
  persist a device token, but Chase forces step-up every time.

## 2. The discovery harness (`explore`)

`explore.py` builds on the shared harness in
[`collectorkit.explore`](../../shared/collectorkit/collectorkit/explore.py).
It keeps only what is particular to Chase: the entry URL, the chase.com
host gate, the credential env names, the in-page detector script and the
login-form fill, which login.py reuses.

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/chase/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map — every request + response. Flushed on the context close and rewritten through the redactor on the same unwind, error included: Playwright records it raw, so it is never crash-safe and secret-free at once. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR; text bodies ≤ 4 MB captured inline, OFX/QFX content types included. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and OTP-field events. |
| `downloads/` | Every file the session fetches (statement PDFs, exports), sequence-prefixed against reused filenames. |
| `dom/NNN/` | Every distinct screen's DOM, per chase.com frame, plus a screenshot. The login and 2FA controls live in a cross-origin iframe, so their selectors are pinned from here (§A). |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace. Off by default: the pinned Playwright/Camoufox pair crashes on tracing. A trace cannot be redacted after the fact: its DOM snapshots store every input's value, the typed password included. |

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
- **Credential redaction** follows the fleet convention
  ([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze),
  "Captures carry credentials"). Response bodies still carry full account
  data, so the whole debug dir is treated as sensitive.

## 3. The discovery flows

A discovery session walks three flows. Each is one continuous VNC-driven
segment, and all three fit in a single recording. They are the place to
start when a selector or route drifts.

### Flow 1 — Login

- The sign-in form: its field ids and names.
- The 2FA challenge: which factors Chase offers, the step order, and the
  OTP field's descriptor.
- The "remember this device" control and the cookies it plants.
- **Session persistence:** after a successful login, close the browser
  and run `explore` again *without* `--fresh`. The second run shows
  whether device trust skips the challenge (§F).

### Flow 2 — Account statements

- The statements / documents area and how it lists statements:
  per-account or unified, year or date filtering, pagination, and the
  backing XHR.
- One statement PDF per account type, so the fetch mechanism lands in the
  HAR.

### Flow 3 — Transaction history + exports

- Each account's transaction history: the listing UI, its date-range
  limits, pagination, and the backing XHR + JSON shape.
- The export surface: one export per format offered, with an explicit
  date range, so the request parameters land in the HAR and the payload in
  `downloads/`.
- The maximum lookback the export allows.

## Source behaviour — authenticated captures

Authenticated discovery captures cover login with push and with SMS 2FA
(§A), a full data walk (§B–§E) and re-login probes for session persistence
(§F). Endpoints are masked (`<id>` = elided id). No account
numbers, balances, device names, phone fragments, OTP codes or card
identifiers are recorded here.

The authenticated app is a **hash-routed SPA**. Every route stays on
`https://secure.chase.com/web/auth/dashboard`; only the `#/dashboard/…`
fragment changes. All data arrives as JSON from `/svc/` endpoints. The
collector does not replay those calls: it drives the UI and lets the
browser make them (§4). Non-app hosts in the trace are CDN/telemetry
(`asset|static|analytics|reco.chase.com`, `go-mpulse.net`,
`online-metrix.net`) and out of scope.

### §A — Login + 2FA

Credential submit and step-up run against `secure.chase.com`, in order:

1. `POST /svc/wl/auth/public/v1/site/availability/list` — pre-check.
2. `POST /auth/fcc/adaptive` — device / adaptive-fraud fingerprint.
3. `POST /auth/fcc/randomize` → `POST /auth/fcc/login` — credential
   submit (a `randomize` immediately precedes each `login`, presumably a
   field-encryption nonce).
4. The response demands step-up:
   `GET …/fraud/authentication/challenge-options/v8/options?…&event-identifier=<E>&challenge-token-identifier=<opaque>`
   returns the **factor menu**: `challengeMethodsDisplay.showAll`, plus a
   `phoneList` (SMS/voice destinations), `inAppDeviceList` (push targets),
   `emailList`, `cardList`, `callUs`. `<E>` is `UNRECOGNIZED_DEVICE` on a
   fresh profile and `EXTRA_SEC_AT_SIGN_IN` afterwards (§F). The
   `challenge-token-identifier` threads the whole step-up.

The factor menu is data-driven. **`challenge-invocations` has one body
shape for every factor** —
`{challengeTokenIdentifier, communicationMethodTypeCode, contactReferenceIdentifier}`
— differing only in the method code and which list the contact id comes
from. **There are two completion paths:**

**In-app push** (menu `["INAPP","CALL_US"]`):
5. `POST …/challenge-invocations/v1/…` with
   `communicationMethodTypeCode:"I"` + the chosen device's
   `oneTimeDeviceIdentifier` (from `inAppDeviceList`) sends the push.
6. `GET …/challenge-statuses/v4/…` is **polled** until approved.
7. A second `challenge-invocations` re-sends the push, and the flow
   tolerates it. So the push path is a poll-until-approved loop with a
   re-invoke escape, not a one-shot.

**SMS OTP** (menu `["INAPP","OTP_SMS","OTP_VOICE","CALL_US"]`):
5. A phone is **picked** from `phoneList` when it lists more than one.
   Each entry is a `contactReferenceIdentifier` + a masked
   `last4DgtsPhoneNumber` + `smsEnabledIndicator`.
6. `POST …/challenge-invocations/v1/…` with
   `communicationMethodTypeCode:"S"` + the chosen phone's
   `contactReferenceIdentifier` sends the code and **returns a 3-char
   `oneTimePasswordPrefixText`**, which the verification echoes back. The
   SMS itself does not carry the prefix, so nothing displays it.
7. `POST …/challenge-verifications/v1/…` with
   `{…, otp:{oneTimeUserPasswordText:<8 digits>, oneTimePasswordPrefixText:<prefix>}}`
   submits the typed code. (This is where the push path polls
   `challenge-statuses` instead — the key structural difference.)

Voice (`OTP_VOICE`) is method `"V"` + a phone contact, and its invocation
returns the same 3-char prefix. So voice is SMS with the code read out over
a call, and the same completion (`challenge-verifications`) applies.
`CALL_US` is a human phone call and is never automatable.

Both paths then finish with a `randomize` + `login` pair and
`POST /svc/wl/auth/l4/v1/user/router/list` into the app.

**Which factors are offered depends on device trust.** The **first login
on a fresh profile** (`--fresh`, an unrecognised device,
`event-identifier=UNRECOGNIZED_DEVICE`) offers **in-app push only** (+ Call
Us). **Later logins** on the same profile (`EXTRA_SEC_AT_SIGN_IN`) also
offer **SMS and voice**. So the first run on a profile must approve a push
on the phone; a typed code is available only afterwards. Either way 2FA
fires every time (§F).

**Pinned UI controls** (the login/2FA sits in a cross-origin iframe whose
clicks never reach the top-document listener, so they are read from the
DOM, not the click log):

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
have no bounding box of their own. Their shadow root is **open**, and Playwright pierces it. The
reliable activation is a **real Playwright click** on the element rendered
inside: an `<a href>` (role=link) for a list row, a role=button for a
button. A native click, a dispatch, the keyboard and coordinate clicks do
not fire the handler. The click is
[`mdsui.click_role`](mdsui.py), with [`mdsui.activate`](mdsui.py) as the
selector-addressed fallback; login.py and download.py drive every MDS
control through the pair.

**Encoded in [`auth_dialog.py`](auth_dialog.py)**, the browserless CLI
core login.py drives. It parses `challenge-options`, chooses a factor,
**picks a phone when several are listed** and reads the 8-digit code. It is
pure (no network, no browser) and unit-tested, with a `--demo` that replays
the dialog on synthetic data (README, "Quick start"). login.py drives the
matching MDS controls (the "Pinned UI controls" above), and the page's own
JS makes the challenge calls.

### §B — Accounts

`POST /svc/rr/accounts/secure/v2/account/detail/dda/list` returns
deposit-account detail; `POST /svc/rl/accounts/secure/v1/dashboard/module/list`
and `POST /svc/rr/accounts/secure/v1/account/activity/download/options/list`
enumerate the roster; `POST /svc/rr/accounts/secure/v1/account/routing/list`
carries the account + routing numbers. The roster distinguishes deposit
accounts (`summaryType=DDA`, `detailType` in {`CHK`, `SAV`}) from
non-deposit products (`CARD`/`BAC`/`ATM`). The parser keeps the DDA and
CARD rows, each stamped with a `product` discriminator (`dda` | `card`),
and drops the rest (an `ATM` row is a debit card, not an account of its
own). Card detail is captured too, but only nested inside the
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

**The UI path.** download.py drives the statements centre (read-only); it
does not replay the GET. Unlike the MDS surfaces
(§A/§E), this is a **classic light-DOM** page — stable ids, no shadow, so
plain Playwright clicks work:

- Route `#/dashboard/documents/myDocs/index;documentType=STATEMENTS`. The
  STATEMENTS accordion (`#button-accountsAccordian-STATEMENTS`) defaults open
  over a plain table `#accountsTable-STATEMENTS` whose rows are contiguous
  (`row0`, `row1`, …); `cell0` is the date (`Mon D, YYYY`), `cell3` the
  download control. The first missing row ends the walk.
- Per row, two save shapes are coded (`statement_click_paths`) and tried in
  order, because each is evidenced on only one surface. The first is a
  direct "Saves document" anchor
  (`#icon-accountsTable-STATEMENTS-row{n}-cell3-requestThisDocumentLink-download`),
  seen on the card surface and preferred. The second is the per-row
  `…-cell3-downloadDocumentDropdown` menu, then `#item-0-{n}-downloadPDFOption`
  ("Save as PDF"), seen on the deposit surface. Neither may be assumed gone.
  Either fires a browser download captured via `expect_download`, saved as
  `statements/<ext>/<YYYY-MM-DD>.pdf`.
- The list is scoped by a **year styled-select**. Its options are light-DOM
  `<a role=option>` anchors whose ids start `container-`. The collector pages
  through each year in the requested window. A **native click** on the
  option matching the year swaps the table (`get_by_text` does not select
  it).
- This surface has no per-account selector. A statement belongs to every
  deposit account printed on it, so the PDFs file under one deterministic
  bucket: the lowest deposit external id. `load` attributes each account's
  segments out of that one relationship-wide pool by balance chaining (§E).
  The bucket name is not a claim about whose statement it is.
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
  `CSV`, `QFX`, `QIF`, `QBO`. The endpoint carries its own
  window as `dateLo` / `dateHi` — that is how the dropdown parameterises
  it — and `statementPeriodId` is the alternative
  "whole statement period" selector. `download.py` sets neither: it drives
  the UI's activity select to "All transactions" instead, so `dateLo`
  never carries `--lookback`.
- **A `csrftoken` is mandatory** on the export POST; the count pre-flight
  does not need it. The collector triggers the export through the form, so
  the browser supplies the token. The card export is a GET whose query
  string carries the token, triggered the same way.

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
  balance to the QFX row. (The check number is deliberately NOT in that key, only a
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
  flat exports lack. The collector does not capture it; the exports are
  the stable, parseable baseline.

**The UI path.** download.py drives the download page (read-only); it does
not replay the POST. The mechanics:

- **Roster** comes from the `/svc/` responses the overview fires:
  `account/detail/dda/list` per deposit account and the card detail inside
  the `dashboard/module/list` envelope. A same-route SPA `goto` after login
  does not re-fire them, so a `page.reload()` nudge forces the fetch. When
  the all-accounts `download/options/list` has not been seen, a visit to
  the first account's export page fires it. Capture keys on the endpoint
  URL, not a guessed body shape. The reducer keeps the DDA and `CARD` rows,
  each stamped with its `product`, and drops everything else.
- The form controls are MDS `<mds-select>`s (open shadow) plus a light-DOM
  **`<button id="downloadButton">`**, the real trigger. Selects and
  options are driven by **real Playwright
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
  `check_number` (gold's column of the same name, docs/DESIGN.md §10.8) and the
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
- Only the rows **before the account's export seam** — `MIN(posted_at)`
  over its export-sourced (`qfx`/`csv`) rows — are imported. Anchoring the
  seam to
  the export rows (never all rows) keeps the two sources disjoint and
  keeps the seam from drifting as statements are added; a segment that
  fails to reconcile (beginning + Σ ≠ ending) is skipped, never injected.
  The running balance for a statement row is reconstructed from the
  segment's beginning balance in posted-date order, so the end-of-day
  balance is exact regardless of intra-day order — and the reconstructed
  tail must land exactly on the export's opening balance.
- A statement row's id hashes the **description the parser read**, and
  `_insert_transaction` is `INSERT OR IGNORE`, so editing the parser re-keys
  the rows and the next load would insert the new ones beside the old — a
  silent, permanent double-count of those outflows. `parser_generations`
  (migration 0005) records which generation of `statement_parser` produced
  the rows silver is holding; when it has moved, every `source='statement'`
  row **and every period anchor** is dropped before the passes re-derive
  them, so a re-parse REPLACES. The anchors go too because
  `statement_balances` is keyed on the period the parser READ, so a shifted
  period mints a new anchor and orphans the old one — and
  `_card_balance_anchors` takes every anchor an account has, so the orphan
  lands in the span walk `derive_card_balances` rolls between. Both
  statement passes share one generation and one purge: they both write
  `source='statement'` and nothing on a row separates a deposit statement's
  from a card's. The export ledgers are untouched — migration 0002 moved
  them onto structural ids that carry no parsed text.
- A moved generation **raises the post-pass gate by itself**. A parser-only
  change lands no bronze run, so `post_passes_pending` stays down and
  nothing else would ever run the passes again. Two guards bound what a
  re-derivation can do: the stamp is written only once all three passes
  return, so a pass that dies leaves the older generation stored; and a
  re-derivation that comes back with FEWER rows than silver held is not
  stamped at all, so the next load tries again rather than committing the
  shortfall. That settles after one retry, which has nothing better to
  compare against, and warns each time.
- The export reduces a cheque to `CHECK <n>`, while the statement prints one
  the payee converted to an electronic debit as `Check # <n> <payee> Payment
  Arc ID: …` — the only place its payee appears. `annotate_export_cheques`
  runs on every load and reads, for each deposit export row whose narrative
  is a bare cheque, the statements that can cover its date; the one
  statement row with its number and amount gives the export row its
  narrative when it names more than the cheque (`payload.description_basis
  = "statement_pdf"`). A paper cheque, printed without a payee, keeps
  `CHECK <n>`. The export row's id carries no text, so nothing re-keys, and
  `payload.cheque_statement_read` keeps each statement from being read
  again; a cheque no statement covers yet is tried on the next load.

### §F — Session persistence: not persistent

A re-login on the persistent Camoufox profile (no `--fresh`) **requires
the full 2FA challenge**, so unattended refresh is not possible. Every
`download` run needs an interactive login. The first login on a profile
fires `event-identifier=UNRECOGNIZED_DEVICE`, and later ones fire
`EXTRA_SEC_AT_SIGN_IN`. So the profile **does** persist a device token,
yet Chase forces step-up at every sign-in regardless. No "remember this
device" opt-out appears in the captures. So `login` is always interactive,
and there is no `login --check`-then-skip path.

## 4. How it works

### login + download — one-shot, browser-driven

`login.py` + `download.py` follow the **schwab-web twin**, not an API
replay. The session dies with Firefox and 2FA fires every time (§F), so
login and scrape run in one browser lifetime, and `login` folds into
`download`.

- **login.py** launches headed Camoufox under Xvfb and pre-fills the
  sign-in form with explore's frame-aware fill. It submits with Enter in
  the password field, with a button as fallback. It then completes 2FA one
  of two ways and hands the authenticated `page` to `download.walk()`:
  - **CLI-MFA (default, `download`, no VNC):** it parses the captured
    `challenge-options` response into [`auth_dialog.py`](auth_dialog.py)
    and drives the challenge **from the terminal**. It picks the factor
    and number, then reads the code from stdin and types it into the page.
    A push is auto-detected for 90 seconds, then waits for Enter. The
    browser holds the cookie and lets Chase's JS finish the sign-in. After
    `challenge-verifications` Chase runs the final `randomize` + `login` +
    `router` (§A), and replaying that out-of-band would strand it. So
    nothing is API-replayed.
  - **VNC (`vnc-login`, `--no-cli-mfa`):** the fallback. It exposes VNC
    and waits for Sign In + 2FA to be done by hand, for when the CLI drive
    cannot find a challenge control.
  - **Auth detection is response-based, pumped correctly.** The pre-auth
    logon page and the dashboard share a URL (§A). So login waits for a
    signed-in `/svc/` call (`/user/router/list` or `/accounts/secure/…`),
    or for the app shell in the live DOM. The waiter pumps the Playwright
    sync event loop (`wait_for_timeout`, not `time.sleep`): a bare sleep
    never delivers `.on()` events.
  - `--check` probes the persisted profile read-only. It reports DEAD
    between runs, by design (§F).
- **download.py** `walk()` builds the account roster from the overview's
  `/svc/` responses and the download-options list (§B, §E), each record
  stamped with its `product`. Per account, deposit and card alike, it
  exports **CSV + QFX** through the download UI. The browser holds the
  cookie and sets the `x-jpmc-*` / CSRF headers, so the UI trigger is more
  robust than replaying the raw request. The export always picks Chase's
  widest activity option ("All transactions", about 24 months). That is a
  superset of any window, so `--lookback` bounds only the statement-PDF
  pass. A start earlier than the 24-month cap draws one warning per run.
  `--no-documents` skips statements. `--dry-run` signs in and walks the
  roster but exports nothing; its run dir is stamped `dry-run`.
- **Bronze per run dir:** `run.json`, `accounts.json`,
  `transactions/<id>.{csv,qfx}`, `statements/<id>/<YYYY-MM-DD>.pdf` and
  `raw/*.json`. With `--debug`, every other `/svc/` JSON body lands in
  `raw/svc/` too, so an unmapped shape can be pinned from one run.
  `prune` reclaims those bodies from the dumps it keeps.
- **Per-product coverage.** `run.json` carries a `coverage` block that
  scores each product on its own (`score_coverage`): accounts discovered,
  accounts exported, statement passes completed, and a `complete` flag.
  `status` says only that the walk finished. Nothing gates on the coverage
  block; see "A partial dump is ingested" below.

### load — silver

`load.py` parses each bronze run into the silver schema
(`migrations/0001` … `0005`): accounts (content-deduped per snapshot), the
transaction ledger, and the statement inventory. The ledger is the
**CSV↔QFX join** (§E). The QFX row is the row of record, and the CSV
running balance joins on **(post date, amount)**. The check number is a
tie-break inside a bucket, not part of the key: the two exports disagree
about it, so keying on it would fail the join and emit the CSV row a
second time as an unmatched leftover. A CSV-only pending row is still
kept. Idempotent (snapshot gate on `dump_runs`; the deposit,
statement and document ledgers `INSERT OR IGNORE` on their stable key,
while the card ledger is written by the field-authoritative
`_upsert_card_transaction`, which adds rows and repairs columns the export
that first landed a row could not carry, and removes nothing); `--force`
rebuilds. `tests/test_load.py` covers it against synthetic exports, with
no live site needed. QIF/QBO are not fetched (QBO duplicates QFX, QIF is
poorest).

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
amount owed). Negating the liability is the gold adapter's job, at one
point ("Gold adapter" below).

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

**A partial dump is ingested, not withheld.** run.json scores each
product's coverage, but nothing gates on that flag. It records which
product fell short, and silver loads the run regardless. Silver ingest is
purely additive: monotemporal `accounts`, an `INSERT OR IGNORE` deposit
ledger and documents, and a card upsert that only fills columns the
incoming export is authoritative for and actually carries. `--force`
replays all of bronze. So a short run lands fewer rows, and the next run
completes them. Withholding a snapshot would break two things:

- coverage also requires the statement pass, so one flaky PDF would freeze
  the balances gold reads from the roster;
- a withheld card roster with its rows admitted would make the gold
  adapter file card spend into the deposit ledger, since it classifies a
  transaction by its account's `product`.

A record with no `product` key reads as a deposit account.

### Gold adapter

The Go adapter under `wealthdb/internal/silver/chase/` (kind `chase`)
projects one account per roster account: `account_kind=cash` for a deposit
account, `card` for a credit card. Neither product carries an instrument,
so it emits cash balances, not positions, plus the whole transaction
ledger keyed on silver's content-derived `fitid`. Silver keeps every card
figure the provider's way, and the adapter turns the sign exactly once: a
card's owed balance becomes gold's negative cash. The deposit accounts are
conduits whose own return rows the registered ReturnsPolicy hides; cards
never enter returns. The mapping, the balance sources and the returns
treatment are in
[docs/adapters/chase.md](../../wealthdb/docs/adapters/chase.md).

### Scope decision: credit cards — reads in, management out

**Opted into in writing.** The collector covers the retail **deposit
accounts** *and* the **credit-card accounts,
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
reconciliation. Card *reads* are in scope; card *management* is not, and
never will be without a fresh written opt-in. Cards cost separate
card-activity / card-statement endpoints to capture, a liability
`account_kind` and revolving-credit shape in the adapter. They have **no
conduit-returns rationale**: a card is not a conduit, and gold keeps card
accounts out of returns by their kind, not by the conduit policy.

## 5. Operation & status

- `make build-chase`, then `~/.secrets/chase.env` with `CHASE_USERNAME` /
  `CHASE_PASSWORD` (README, "Quick start").
- `wealthdb-collect chase download` pre-fills and submits the form, drives
  2FA **from the terminal**, then scrapes.
- `wealthdb-collect chase load` parses bronze → silver.

**The challenge selectors are trace-derived** (§A, "Pinned UI controls").
When Chase moves a control, the CLI drive fails with a message that names
`vnc-login`. `wealthdb-collect chase vnc-login` is the by-hand fallback.
`--debug` captures each chase.com frame's DOM and a screenshot at every
login step into `--screenshot-dir` (the `/debug` mount), so a drifted
selector can be re-pinned.

**Deposit ids are loader output, not schema.** Deposit transaction ids
follow the content-derived, occurrence-indexed scheme of §4 ("load —
silver") rather than the QFX `FITID`. No migration rewrites ids, so
`schema_meta` cannot tell a silver DB with bare-FITID deposit ids apart.
Such a DB, and any gold DB built on it, needs a `load --force` silver
rebuild followed by a gold reload: gold keys chase transactions on
silver's `fitid`. `load` enforces this (`_refuse_pre_0002_ids`). It counts
the export rows whose ids carry neither the `dda:` nor the `card:` prefix,
and exits with that instruction rather than inserting each of them again.
Statement-sourced ids and row content are unaffected.

**Operation status.** The README's "Status" names the paths the collector
drives. The sign-in and export selectors for deposit accounts are pinned
from captures. The selectors for the export form's account picker, the
statement view's save control and the card documents route are pinned from
captures only. The passes fail loudly rather than save a file under the
wrong account: the export refuses an unreadable account binding, and an
incomplete product shows in `coverage`.
