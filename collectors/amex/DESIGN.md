# amex — design notes

Design notes for the American Express card collector, kept as a decision
log: scope (§1), the discovery harness (§2), what discovery was pointed at
(§3), **what it measured** (§A–§H), **what the live runs then corrected**
(§I–§M), what was built (§4), and status (§5).

Read §A–§M for the source's actual behaviour. §1–§3 are the framing the
build started from; where a capture or a live run later settled a question,
the lettered sections say so and supersede them.

## 1. Scope

- **The relationship.** A card relationship at American Express
  (americanexpress.com). Scope is the **credit and charge card accounts,
  read-only** — card roster, per-card detail and balances, transaction
  history, exports, statement PDFs. Everything else the same login may
  expose is out of scope in code and docs: money movement in every form,
  Membership Rewards redemption, offers and enrolment, Plan It /
  pay-over-time, travel and booking, account lifecycle, profile and
  settings, and the message center. See [CLAUDE.md](CLAUDE.md) for the
  allow/forbid surface.
- **The playbook was chase, and the shape landed there too — by a
  different route.** [`chase`](../chase/) is the fleet's other card
  collector: Camoufox, terminal-driven 2FA, activity exports plus a
  statement backfill. [`firstcitizens`](../firstcitizens/) supplied the
  other candidate shape — a browser login plus a REST data path through
  `page.request`. Discovery pointed at firstcitizens' split (§G), the live
  runs moved it to chase's (§L), and the data path is firstcitizens'
  throughout (§A). The pieces are borrowed from both.
- **A card is a liability, not a conduit.** Chase's conduit-returns
  rationale is about its *cash* accounts and is not an argument about
  cards. A card projects as `canonical.AccountKindCard`, its balance is a
  liability in net worth, and it stays out of returns and allocation
  exactly as chase's cards do.
- **No public API — confirmed.** Amex offers no self-service programmatic
  access for an individual card member. The site is a React SPA over two
  internal surfaces (§A), and those are what the collector drives.
- **Bot defense: Akamai Bot Manager — confirmed (§A).** The guess that
  Amex fronts the same class of defense as the US siblings held. Camoufox
  cleared the session end to end with no visible challenge, so the
  vanilla rungs stay skipped. What the capture added is the shape: the
  defense is **cookie-borne**, and **no data endpoint carries a sensor
  header** — which licenses a REST data path off the browser's own
  context.

## 2. The discovery harness (`explore`)

`explore.py` is copied and adapted from [`chase`](../chase/explore.py)'s —
the card sibling's harness, itself the fleet's most complete. The
duplication across the explore harnesses is a tracked, deliberate
decision: each drifts with its source, so they are copied and adapted, not
extracted into a shared library.

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/amex/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map — every request + response. Flushed on the context close and rewritten through the redactor on the same unwind, error included: Playwright records it raw, so it is never crash-safe and secret-free at once. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR; text bodies ≤ 200 KB captured inline, OFX/QFX content types included, and a form-urlencoded request body masked field by field like the HAR's. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and OTP-field events. |
| `dom/<NNN>/` | **Every distinct screen's full DOM** (all americanexpress.com frames) + a screenshot, deduped by DOM structure — the record selectors are pinned from. |
| `downloads/` | Every file the session fetches (statement PDFs, exports), sequence-prefixed against reused filenames. |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace — off by default because the pinned Playwright 1.49 tracer crashes the camoufox 152.0.4 build (matched-set drift; see base-camoufox). Written by Playwright in its own format and **not scrubbed**: a trace cannot be redacted after the fact, its DOM snapshots store every input's value, so a trace holds the credential. |

Mechanics carried over from chase (see that harness's DESIGN.md §2 for the
full rationale): frame-aware login pre-fill gated to americanexpress.com
frames, both-fields-in-one-frame before either is touched, fill-once with
read-back verification, `signon.*` prefs off so a profile-saved credential
can never autofill on top of the programmatic fill, an OTP-field detector
that logs the field's static descriptor (never its value), and credential
redaction across every logged URL, header and body. The redaction is two
independent masks: the known credentials in every wire spelling, and a mask
by parameter and header NAME, which is the only thing that reaches a
bearer, CSRF token or session cookie the site minted at runtime. The click
log goes through the same redactor and records element labels only, never a
field value. The HAR is rewritten through the redactor after the context
close (`debugcap.redact_har`, shared with every sibling harness), because
Playwright records it raw. The opt-in trace
stays outside that reach, as the table above records. Sign-in and 2FA are
always submitted by hand over VNC; the harness never clicks a button.

## 3. What discovery was pointed at

The `explore` sessions were briefed to answer, in one recording plus a short
second one: where the sign-in lives and what factor it challenges with;
**which endpoints carry bot-defense sensor headers** (the observation that
decides browser-everywhere vs browser-for-login + REST); the roster's stable
identifier; the transaction listing's shape, pagination, and whether a row
carries a stable id and a merchant category; every export format with its
columns and its reach; the statement archive's reach; and — as a separate
short run — **whether device trust survives a browser restart**, which was
expected to decide the verb split.

All of it was answered, and the answers are §A–§H. Two briefed expectations
were wrong and are corrected there: there is no OFX 1.x among the export
formats (§E), and the trust measurement did not in the end decide the verb
split (§G, revised by §L).

<!-- The per-flow question lists this section used to carry were removed once
     every one of them was answered; §A–§H are the answers, and a to-do list
     kept past its own completion reads as an open question. -->

## Observed — what the captures measured (§A–§H)

Two VNC-driven `explore` sessions. The first: login with an SMS one-time
passcode, device registration, the card overview, the activity views, every
export format, and the statements area. The second, on the same profile
after a browser restart: the persistence probe (§G) plus the balance /
credit-details and APR surfaces (§H). Endpoints below are masked (`<account_key>`,
`<accountToken>`, document tokens); no account identifiers, balances,
merchants or amounts are recorded here — they live only in the debug dir.

The authenticated app is a **React SPA on `global.americanexpress.com`**
backed by two API surfaces: a **BFF of named "functions"** at
`functions.americanexpress.com` (POST, JSON in / JSON out, no REST
resource paths) and a **REST servicing API** at
`global.americanexpress.com/api/servicing/…` (documents and exports).
Everything else in the trace is CDN or telemetry —
`www.aexp-static.com`, `iwmapapi…/beacon`, `omns…` (Adobe),
`js-cdn.dynatrace.com`, `qualtrics`, `contentsquare` — and is out of scope.

### §A — Bot defense: Akamai, cookie-borne, no sensor headers on the data calls

**Camoufox cleared the whole session with no visible challenge.** The stack
is **Akamai Bot Manager**: a sensor script POSTs to an obfuscated path on
`www` and `global` (plus `/akam/13/<id>`) and plants `_abck`, `bm_sz`,
`bm_mi`, `bm_sv`, `ak_bmsc`, `akaalb_*`. **Dynatrace** RUM rides along.

The decisive observation for the runtime tier: **no data endpoint carries a
sensor header.** A `functions.americanexpress.com` POST sends only
`cookie`, `content-type`, `origin`, `ce-source` and a correlation id; a
servicing GET sends only `cookie`. Authentication is **entirely
cookie-borne** — `amexsessioncookie`, `aat`, `JSESSIONID` — with **no
bearer token and no CSRF header anywhere in the trace**. So the defense is
a property of the browser context, not of individual requests, and once
the browser holds a valid jar the data can be fetched over
`page.request` — the firstcitizens architecture, for a different reason
(there the sensor was a header on the logon; here it is a cookie the
Akamai script keeps alive).

### §B — Login and 2FA: SMS/one-time passcode, six-box entry, device registration

The sign-in form is **classic light DOM in the main frame** (no iframe),
reached from the homepage's `#gnav_login`:

- `#eliloUserID` (`name=username`, `data-testid="userid-input"`),
  `#eliloPassword`, a `#rememberMe` checkbox
  (`data-testid="remember-me-checkbox"`), submit `#loginSubmit`.

Submitting drives this sequence:

1. `POST global.americanexpress.com/myca/logon/us/action/login` — a
   **form-urlencoded** body carrying `UserID`, `Password`,
   `encryptedData` (from `ReadDeviceIdentityRegistrationChallenge.v1`),
   `inauth_profile_transaction_id` and `DestPage`. The response is JSON:
   `statusCode`, `errorCode`, `redirectUrl`, and a **`reauth` block** with
   `mfaId` and an `assessmentToken` when step-up is required.
2. `POST functions…/ReadLegacyAuthenticationStatus.v1`
3. `POST functions…/ReadAuthenticationChallenges.v3` `{userJourneyIdentifier:
   "aexp.global:create:session", assessmentToken}` — the **factor menu**.
4. `POST functions…/CreateOneTimePasscodeDelivery.v3` `{userJourneyIdentifier,
   otpDeliveryRequest: {deliveryMethod, encryptedValue}}` — sends the code.
   `deliveryMethod` was `SMS`; the contact is an **opaque `encryptedValue`**
   read from the menu, never a phone number the client composes.
5. `POST functions…/UpdateAuthenticationTokenWithChallenge.v3` — submits the
   typed code.
6. `POST functions…/CreateIdentityTrustedDevice.v1` `{"name": "<browser> on
   <os>"}` → **201** — the "Add This Device" step.
7. A second `POST /myca/logon/us/action/login` carrying `mfaId` (and the
   browser's wall-clock fields) completes the sign-in.

**No push factor was offered** — the challenge is a one-time passcode. A
passkey path exists (`ReadAuthenticatorPasskeyVerificationChallenge.v1`)
and was not used.

**Challenge UI — `data-testid`s throughout, and a six-box code entry.**

- Delivery picker: `[data-testid="challenge-options-list"]`, one
  `<button>` per target (`select-button-heading` / `-detail` / `-caption`
  inside), then the option's own call to action.
- Code entry: **six separate inputs**,
  `[data-testid="otp-input-0"]` … `otp-input-5`
  (`name="otp-input-N"`, `inputmode="numeric"`, `pattern="[0-9]*"`,
  `aria-label="One-time password digit N of 6"`), then
  `[data-testid="continue-button"]`; `[data-testid="resend-button"]`
  re-sends.
- Device registration: an "Add This Device" button on the following screen.

**A partially-filled code submits and fails.** The capture caught exactly
that: a `continue-button` click with the boxes not all populated returned
**HTTP 400** from `UpdateAuthenticationTokenWithChallenge.v3`, and the
retry needed the boxes re-entered. So the terminal drive must fill all six,
**verify each by read-back before clicking Continue**, and **clear all six
before any retry** — the segmented-input form of the fleet's
clear-OTP-before-retry rule.

**Two credential leaks in the harness, found here and fixed.** The login
body percent-encodes the password, which the literal-substring redactor
missed; and the serialized sign-in form carried the typed password as a
`value` attribute, which nothing redacted. Both are fixed in
`collectorkit.debugcap` (`secret_redactor`, `scrub_dom`) and adopted by
every sibling harness, guarded by tests that key `scrub_dom` on any module
serialising a DOM, a redactor on any `capture_page` call, and `redact_har`
on any harness that records a HAR. The
lesson for the fleet: **a credential does not reach the wire verbatim, and
a DOM snapshot is a capture surface of its own.**

### §C — The card roster

`POST functions…/ReadCustomerOverview.web.v2` (empty body; the session
cookie identifies the customer) returns, under
`.products.data.products`, a **map keyed by a 32-char hex `accountKey`**,
with `listOrder`, `personalAccounts` and `businessAccounts` index lists
beside it. Per product:

| field | meaning |
| --- | --- |
| `accountKey` (32 hex) | the stable key the **servicing REST API** takes |
| `accountToken` (15 ch) | the stable key the **functions BFF** takes |
| `displayAccountNumber` | the last-digits mask |
| `productDisplayName`, `productType`, `subTypes[]` | the product's own naming; `productType` was `AEXP_CARD_ACCOUNT` |
| `lineOfBusiness`, `userType`, `accountStatus` | consumer/business, holder/supplementary, active/other |
| `balance.data.{amount,currency,balanceName}` | the overview balance, as a **string** amount with a label key |
| `paymentDueDetails.data.{paymentDueDate,remainingDaysToPay,dueDaysTense,titleKey}` | the payment due block |
| `isPartial` | the payload's own "this record is incomplete" flag |

**Two identifiers, and both are needed** — the BFF speaks `accountToken`,
the servicing API speaks `accountKey`. Silver keys on `accountKey`.

**Credit limit and available credit were NOT captured** — the walk never
opened the card's account-details / "Vitals" surface, which
`placementLinks.VITALS` links to. Not blocking: a limit is not a balance
and never becomes a gold row (the chase rule), but it is a cheap addition
to a later micro-capture.

### §D — Transaction history: one endpoint, five views

`POST functions…/ReadAccountActivity.web.v1` with
`{accountToken, axplocale, transactionFilters: {limit, offset}, view, …}`.
The **`view` discriminator** selects the window, and this is the whole
history surface:

| `view` | extra key | what it returns |
| --- | --- | --- |
| `VIEW_BY_DAYS` | `days` (30/60/90) | a rolling window |
| `VIEW_BY_YEAR` | `year` | a calendar year |
| `DATE_RANGE` | `dateRange {startDate, endDate}` | an arbitrary window |
| `BILLED` | `statementEndDate` | one billing cycle |
| `STATEMENTS` | — | the statement archive (§E), no transactions |

Pagination is `transactionFilters {limit, offset}` — **offset is 1-based** —
against `.activityData.totalTransactionCount`.

**A transaction row** (`.activityData.data[].transactions[]`):

- `identifier` — an 18-digit **stable id**, and `referenceNumber` is the
  same value (they were equal on every row observed).
- `chargeDate`, `postDate`, `displayDate`, `statementEndDate` — four
  ISO dates, so the transaction/post distinction is explicit.
- `transactionAmount {amount, currency}` — amount as a **string**.
- `type` ∈ {`DEBIT`, `CREDIT`}; `status` ∈ {`posted`, `pending`}.
- `displayDescription` — the merchant line.
- `categoryCode` — a short code resolved by the payload's own
  `.activityData.categories` code → label map (§F).
- `supplementaryDigits` — which card member's card, joining
  `.activityData.cardMembers`.

**A pending row's id is provisional.** Posted ids are 18 digits; a pending
row carried a `P`-prefixed, longer id and appeared in **no** export. So a
pending row's `identifier` must not be treated as durable — it changes when
the charge posts.

**The payload also carries the cycle's balance summary**
(`.activityData.balancesDetails.summary.standard`): `previousBalance`,
`newCharges`, `paymentsAndCredits`, `fees`, `interestCharges`,
`statementBalance`, plus `paymentDueDate`. That is the same reconciliation
identity a statement prints — available as JSON for the whole export era.

### §E — Exports and statements: the reach split, stated by the API itself

**Exports** are plain servicing GETs. `downloadOptions` carries a ready-made
URL per period, which is how the capture identified the endpoint; the
collector builds its own instead, so that the URL carries the window a run
asked for rather than the period's (§4.3):

```
GET global.americanexpress.com/api/servicing/v1/financials/documents
      ?account_key=<account_key>&client_id=AmexAPI
      &file_format=csv|excel|quickbooks|quicken
      &limit=ALL&status=posted
      &additional_fields=true&itemized_transactions=true
      # window: either start_date=<ISO>&end_date=<ISO>
      #         or statement_end_date=<ISO> for one cycle
```

- **`start_date` / `end_date` are arbitrary ISO dates**, so `--lookback`
  rides the export server-side: this is a **bounded** collector.
- **Four formats, no OFX 1.x**: `csv`, `excel` (xlsx), `quickbooks` (.qbo),
  `quicken` (.qfx). QFX/QBO are OFX **2.02 XML** and are *not* byte-identical
  to each other, but carry the same fields.
- `additional_fields=true` is what widens the CSV to 13 columns
  (`Date, Description, Card Member, Account #, Amount, Extended Details,
  Appears On Your Statement As, Address, City/State, Zip Code, Country,
  Reference, Category`) — that is the "extended details CSV", a parameter
  rather than a separate format.
- `status=posted` was the only value sent; **no export carried a pending
  row**.

**Statements** come from the same `ReadAccountActivity.web.v1` call with
`view: "STATEMENTS"`, which returns the **entire archive in one response**
as `billingStatements.{recentStatements, olderStatements, yearEndSummaries}`.
Each entry is a `statementEndDate` plus its own `downloadOptions`. The PDF
fetch is
`GET /api/servicing/v1/documents/statements/<opaque token>?account_key=…&client_id=OneAmex`,
the token being a long hex blob carried in that entry — never constructed.
`yearEndSummaries` add a per-year summary PDF via
`/api/servicing/v2/financials/documents?fileFormat=pdf&fileType=ysr&year=<Y>`.

**The reach split — and the API states it, so nothing has to guess:**

- `member.maxAvailableMonths` is **24**, and `member.startDateForSearch` is
  exactly 24 months back. The activity JSON *and* every structured export
  stop there.
- Statement PDFs are listed for the provider's full retention (**~7 years**).
- Correspondingly, **only the newest ~24 periods offer
  `CSV`/`EXCEL`/`QUICKBOOKS`/`QUICKEN` in their `downloadOptions`; every
  older period offers `STATEMENT_PDF` alone.**

So this is chase's situation exactly — a structured ledger capped at ~24
months over a much deeper PDF archive — and it is what makes the **statement
backfill** worth building (§4.5). What `downloadOptions` adds over chase is
that the split is stated per period by the API itself rather than inferred.
The collector does not read it at runtime, though: the fetch window is
bounded — for the activity and the exports, the two channels that stop —
by a constant (`MAX_AVAILABLE_MONTHS`), and the import seam is
`MIN(posted_at)` over the activity rows a run actually landed (§4.5) — the
figure that has to move as windows widen.

`GET /api/servicing/v1/financials/statement_periods` returns a flat
`[{index, statement_start_date, statement_end_date}]` — the same 24-month
horizon, so it is a convenience, not the archive.

**Statement PDFs parse.** `pdftotext -layout` yields the summary block
(`Previous Balance`, `Payments`, `Credits`, `New Charges`, `Total Fees`,
`Interest Charged`, `New Balance`, `Closing Date`, `Minimum Payment Due`)
and a transaction table of `MM/DD/YY[*]  merchant  …  $amount` rows with a
continuation line beneath, grouped into **per-card-member sections**
(`Card Ending ####`). Those sections all belong to **one** account, so
chase's balance-chained *attribution between accounts* is not needed — but
the per-section grouping still has to be parsed, because section subtotals
are what the reconciliation gate checks.

### §F — Merchant category: confirmed, and self-describing

A transaction carries a `categoryCode`, and the **same payload carries the
code → label map** (`.activityData.categories`, mirrored in
`.activityData.allFilters.categories`). Labels are Amex's own title-case
spend categories.

This is better than a hardcoded table: the collector resolves the label at
collection time and stores both code and label, so a category Amex adds
later resolves on its own rather than becoming an unmapped code. One
capture cannot enumerate the full vocabulary — the map only lists the
categories present in the rows it returns — so the gold-side
`providermap.go` table is built from the labels silver has accumulated,
and an unmapped label is counted drift and falls through to the model tier
(never guessed into a neighbour).

### §G — Session persistence and device trust

The two axes answer **differently**, and the cookie attributes say why —
this is not inferred from behaviour, it is read off `Set-Cookie`:

| cookie | attributes | outcome |
| --- | --- | --- |
| `amexsessioncookie` | `Discard`, no expiry | dies with the browser |
| `aat` | `HttpOnly`, `Secure`, no expiry | dies with the browser |
| `JSESSIONID` | `Secure`, no expiry | dies with the browser |
| `device-id` | `HttpOnly`, `Secure`, `Max-Age` ~ 396 days | **survives** |

**The session does NOT survive a browser restart — structurally.** All
three session cookies are browser-session-scoped (`amexsessioncookie` even
carries the explicit `Discard`), so Firefox drops them on exit and the jar
comes back without them. The second run proved it end to end: before any
sign-in, `ReadCustomerProducts.v2` and `GET /api/servicing/v1/member` both
returned **401**, and the request headers show the jar carrying `device-id`
and the Akamai cookies but **none** of the three session cookies. This is
not an idle timeout a quicker restart could beat — reopening the browser
can never continue an Amex session.

**Device trust DOES survive, and it skips the passcode.** Re-submitting the
password on the same profile returned `statusCode: 0`, `errorCode: ""`,
`reauth.trust: true`, `challenge: false` — against `statusCode: 1` /
`errorCode: "LGON013"` / a `reauth.mfaId` on the first, untrusted login.
**Not one challenge-flow request fired** (no `ReadAuthenticationChallenges`,
no `CreateOneTimePasscodeDelivery`, no
`UpdateAuthenticationTokenWithChallenge`). Read `reauth.trust`, **not**
`reauth.deviceRemembered`, which was `false` on both logins and describes
something else.

**On the trust axis this is the firstcitizens split** — durable device trust
over a session that dies with the browser, exactly
[firstcitizens](../firstcitizens/DESIGN.md) §3 — and the collector was built
that way: one interactive `login` minting the trust, each `download` logging
in password-only against it.

**§L revises that**, on evidence this section could not see: the source
rate-limits sign-ins hard enough (§K) that a two-sign-in pair costs more
than the split is worth, so `login` folded into `download` after all. The
*mechanics* below stand unchanged — they are why `download` can sign in
without a passcode at all. The `login --check` contract at the end of this
section does not: §L replaced it.

Restoring the session cookies from a saved jar to skip the logon is
**rejected**: they are `Discard`-scoped by the provider, `aat` is
`HttpOnly`, and the server holds its own state — this is the "no cleverness
in auth flows" rule, and a password-only logon is unattended anyway, so it
buys nothing.

**Trusted-device logon differs from the untrusted one in three ways** worth
coding against:

- The form arrives with a **masked user id** pre-filled, and submitting it
  unchanged works — the `device-id` cookie resolves the identity. `login`
  must therefore **clear the field and fill `AMEX_USERNAME`** deliberately:
  explore's "field already has content, leave it alone" rule would submit
  the mask, which works only while trust holds and would fail confusingly
  the day it lapses.
- The body carries `REMEMBERME` and a `signature` field the first login did
  not.
- Device identity is verified through
  `CreateDeviceIdentityVerificationChallenge.v1` rather than the untrusted
  path's `ReadDeviceIdentityRegistrationChallenge.v1`.

> **Superseded by §L — the contract this section originally drew, kept for
> the record:** `login --check` therefore has a precise contract, and it is
> firstcitizens' one: submit the form and read the outcome — **exit 0 when
> the logon lands authenticated with no challenge (device still trusted),
> non-zero when a challenge appears**. It sends no passcode, so it fires no
> MFA. It cannot be a bare "is the session alive" probe, because between
> runs the honest answer to that is always dead.

What ships instead reads the profile's own `device-id` cookie and reports
that — **exit 0 when the device is registered, 1 when it is not** — with no
navigation and no call to the source (§L). It answers "will `download` run
unattended", not "will the next sign-in succeed": only a sign-in sees a
trust the provider revoked server-side, and spending one to ask whether a
sign-in can be spent is self-defeating on this source.

### §H — Odds and ends the second capture added

- **`balancesDetails` is view-dependent.** `VIEW_BY_DAYS` carries
  `summary.standard.totalBalance` **and `pendingBalances.charges`** — the
  pending total, which is what makes the card's
  posted + pending == live identity checkable (chase's canary). `BILLED`
  carries `statementBalance` instead.
- **APR is available**: `GET /api/servicing/v3/financials/interest_rates`
  returns per-plan `annual_percent_rate`, `plan_description`,
  `variable_rate_indicator`, `start_date`. Not a balance, so not a gold
  row; worth carrying in silver's payload.
- `v3` siblings exist for `financials/eligibilities` and
  `financials/statement_periods` with the same shapes as `v1`.
- **Credit limit / available credit still not captured.** The
  balance-and-credit-details expander was opened, but it rendered without a
  distinguishable data call — the figure lives behind a module
  (`myca-balance-credit-details`) whose endpoint no capture has named.
  Left unresolved deliberately: a limit is not a balance and never becomes
  a gold row (the chase rule), so it does not justify another live session.

## Live validation — what the real runs corrected (§I–§M)

Each entry is a defect no capture could have shown, found by a real run on
2026-09-06. They are kept in full because each one cost a sign-in, and
sign-ins are this source's scarce resource (§K).

### §I — Live run 1: the CORS preflight reads as a refusal

The first live `login` failed 2.5 seconds after submitting the form with
"American Express refused the sign-in (HTTP 200, statusCode None)". The
provider had refused nothing.

The browser sends a **CORS preflight** to the logon URL, and that `OPTIONS`
answers **200 with an empty body about a second before the real POST
response arrives**. The response watcher matched on the URL alone, so it
took the preflight for the outcome, found no `statusCode` in its empty body,
and classified it as neither authenticated nor challenged — which the
caller surfaces as a refusal. Both explore captures show the pattern
plainly (`OPTIONS` 200 no-body, then `POST` 200 `application/json`); it was
there to be read before the run and was missed because the endpoint map
listed methods separately from the response bodies.

Fixed by filtering the watcher to the POST. The failure message for a POST
carrying no verdict now says the outcome could not be READ rather than that
the sign-in was refused — a distinction worth keeping, because one of those
tells the operator to check their credentials and the other does not.

**Fleet lesson: a response watcher must match on the METHOD as well as the
URL.** Any endpoint the SPA calls cross-origin has a same-URL preflight
whose empty 200 arrives first, and an auth signal keyed on the URL alone
will read it. This is the response-side twin of the fleet's
"authentication is proven by a read, never by a URL" rule.

### §J — Live run 2: the third sign-in in 90 seconds is challenged

With the preflight fixed, `login` and `download --dry-run` both signed in on
the trusted device with no passcode. The real `download`, 79 seconds later,
was **challenged** — its third password submission inside 92 seconds.

Read against §G, this is almost certainly a **rate response, not lost
trust**: `device-id` carries a ~396-day lifetime, and the two sign-ins
immediately before it were untroubled. It is the fleet's
"logins are budgeted" lesson arriving on a source that had not shown it yet.

**It exposes a real shape problem, not just an operating note.** Every
`login` → `download` pair is TWO sign-ins by construction, and
`wealthdb-refresh` runs exactly that pair for every source. So the routine
path spends two of whatever budget Amex allows, back to back, and the second
one is the one that carries the data.

Options considered, and the one taken:

- **Space the two runs out.** Works, but leaves the budget spent and makes
  the orchestrated path unreliable rather than fixing it.
- **Let `download` answer a challenge when it has a TTY** (built), falling
  back to the loud `NeedsLogin` when it does not. This is what the source's
  own behaviour argues for and what the chase sibling already does (its
  login folds into download precisely because a challenge can fire on any
  run). It keeps the unattended contract honest — a cron run still fails
  loudly rather than blocking on a prompt nobody will answer — while making
  the interactive path, which is how `wealthdb-refresh` is used, survive a
  challenge instead of aborting the source.

`download.resolve_two_factor` decides: **auto by default** (prompt iff stdin
is a TTY), with `--cli-mfa` / `--no-cli-mfa` forcing either half, spelled as
they are on `login`. The docker wrapper gives the container a TTY only when
stdin *and* stdout are both TTYs, so the container's own `isatty` is a
faithful answer to "is there someone to prompt".

**The residual, stated plainly at the time:** this made a challenge
survivable, it did not make the pair cheaper — `wealthdb-refresh` still
signed in twice per source. That residual is what §L then removed, by
folding `login` into `download` so the pair is one sign-in. This section is
kept because the TTY handling it added is still the mechanism; only the
"two sign-ins" premise is gone.

### §K — Live run 3: bot defense fires, and it is not a passcode

Seven sign-ins inside twenty minutes provoked **a captcha**. The step-up the
logon response announced was no longer a one-time passcode: the browser was
showing a bot-defense challenge, which a terminal cannot answer. The CLI
drive waited its full 30 seconds for a passcode screen that was never going
to render, twice, before `vnc-login` put a human in front of it — who solved
the captcha and was signed straight in, with no passcode at all.

Three things this settles.

**The step-up is not one thing.** `challenge: true` / an `mfaId` in the logon
response means "something more is required", and §B's passcode is only one of
the somethings. A captcha is another, and it is answered by a human or not at
all. The CLI drive now checks for one and stops immediately with the verb
that CAN answer it, rather than timing out against UI that will never appear.

**The failure path was not self-diagnosing.** "The passcode challenge screen
did not appear" was true and useless — it described what was absent, not what
was present, and the next drift would have needed another live run to learn
anything. The timeout now prints the page path, title and the identifiers of
every visible control, unconditionally rather than behind `--debug`, so the
pasted log carries the evidence. Identifiers only, never element text: a
challenge screen's labels carry the masked destination.

**The budget is real and it is small.** The sibling lesson said ~6 rapid
logins; this took about seven in twenty minutes, and it did not arrive as a
lockout but as a captcha. Captcha reputation accrues per account and decays
with idle time, so the remedy is to stop, not to retry. `login` → `download`
being two sign-ins (§J) is what makes the budget easy to spend without
noticing.

The captcha selectors are **not pinned from a capture** — the run that saw
one was driven by hand with no DOM dump — so the detector is a broad net over
the shapes a captcha widget takes, used only to abort with the right
instruction and never to drive anything. A false positive costs one run that
says "use vnc-login" when the terminal would have done.

### §L — The verb split, revised: `login` folds into `download`

§G settled the split on the trust axis, and it was right about the
mechanics: device trust persists, the session does not, so a separate
`login` verb *could* mint the trust once and let `download` run unattended
against it. §K settled a second axis §G did not weigh — **how many sign-ins
the source will tolerate** — and on that axis the split is wrong.

A `login` → `download` pair is two sign-ins, `wealthdb-refresh` runs that
pair for every source, and roughly seven sign-ins in twenty minutes provokes
a captcha. So the routine path was spending the budget twice as fast as it
needed to, in exchange for nothing: the trust `login` minted is minted just
as well inside `download`, by the same code, on the same profile, at the
same moment a challenge is answered.

So the shape is now chase's and schwab-web's after all — **`download` is the
one verb that signs in** — reached by a different route. §G's finding is not
retracted: the trust really is durable, and that durability is exactly what
lets `download` sign in without a passcode on every run after the first.
What changed is that a durable trust does not by itself justify a verb.

| verb | what it does |
| --- | --- |
| `download` | the whole run: sign in, answer a passcode from the terminal if one fires (registering the device while there), fetch |
| `vnc-login` | the same walk, behind a sign-in completed **by hand** over VNC — the only way to answer a captcha, and the way to register a device with no terminal |
| `login` | a no-op, trapped **host-side** so an orchestrator's login → download → load neither trips nor pays a container start |
| `login --check` | reports whether this device is registered |

**`login --check` reads the profile, not the source.** The fleet's usual
`--check` makes the cheapest authenticated call the source allows, precisely
so a credential that exists but is rejected is caught. Here that call is a
full sign-in — and a probe that spends the scarce thing to ask whether the
scarce thing can be spent is self-defeating. So it reads the profile's own
`device-id` cookie straight out of the Firefox jar on disk and reports that,
opening no browser at all — launching one resolves the egress IP over the
network, which would break the very claim the probe rests on. It
answers "is this device registered", which is what decides whether
`download` needs a human; it does **not** answer "will the next sign-in
succeed", because the provider can revoke trust server-side and only a
sign-in would see that. The limit is stated in the help and here rather than
papered over — it is the honest trade for not spending a sign-in on a
question.

Confirmed live on 2026-09-06: the probe reported the device registered for
~13 months, matching the cookie lifetime §G measured off `Set-Cookie`.

### §M — Live run 4: the first real data, and two defects it exposed

The run that finally fetched: the roster, the activity ledger, both export
formats and the statement archive, then a clean `load` into silver and a
full projection into gold. It also exposed two defects that only real data
could produce.

**Device registration silently did not happen.** The passcode was entered
from the terminal and the session authenticated, but the "Add This Device"
click found nothing — so the next run paid another passcode, which on this
source is the expensive failure (§K). The selector was one exact string;
it is now a set covering the wordings the control ships under, matched
case-insensitively. More importantly the miss is no longer silent: it
reports the identifiers of every visible control, so the next occurrence
says whether the control drifted or the provider simply did not offer
registration. Those want different responses and the old message
distinguished neither.

**The statement backfill double-counted across windows.** Two loads at
different `--lookback` windows in one session left statement rows at or
after the activity seam — the same charges carried twice, once from each
channel. The seam is `MIN(posted_at)` over activity rows and it MOVES: a
wider window reaches further back, so periods that were below the seam
become covered by the activity, and an incremental import never withdraws
what it already wrote.

The fix is the fleet's own rebuild-derived-tables rule, which the loader
already applied to pending rows and should have applied here: the
statement-sourced rows are dropped and rebuilt from all of bronze on every
load, so the result depends only on what bronze holds and never on the order
loads happened in. Verified on the real tree — no row on the wrong side of
the seam, no duplicate id — and pinned by a test that widens the window
between two loads.

The rebuild also made the parse cost visible, and it is now deduped on the
PERIOD rather than the file: every run re-fetches the same statements, so a
tree with N runs held N copies of each, and each copy cost a `pdftotext`
subprocess. Deduped on the period rather than the bytes, because a
re-rendered PDF is a new hash for the same statement — the fleet's
dedupe-on-logical-identity rule.

## 4. As built

The pieces, in the order they were built. Each is done; where a live run
later changed one, the lettered section that changed it is cited.

1. **Scaffold.** `explore` harness, Dockerfile on base-camoufox, wrapper via
   `shared/wrappers/wrapper-lib.sh`, host-side `prune`, this document,
   CLAUDE.md.
2. **Explore (2026-09-06).** Two sessions; §A–§H.
3. **download — the one verb that signs in (§L).** Camoufox for the
   logon, then REST over `page.request`; no DOM scraping of data (§A). Built
   first as firstcitizens' split on §G's trust finding, then folded when §K
   showed what a sign-in costs.
   - **`download`** (Camoufox) submits the sign-in form; a trusted device
     lands signed in with no challenge. A passcode is answered from the
     terminal when there is one, on a tested browserless dialog core
     (`auth_dialog.py`) — filling all six boxes and verifying by read-back
     before Continue (§B) — and the device is registered while there. With
     no terminal a challenge is a loud failure, never a blocked prompt; a
     captcha is never answerable here (§K). It clears and fills the user id
     rather than accepting the masked pre-fill (§G). `--fresh` sets the
     profile aside to force the untrusted path.
   - **`vnc-login`** runs the same walk behind a sign-in completed by hand.
   - **`login`** is a host-side no-op; **`login --check`** reports device
     registration from the profile's own cookie, with no sign-in (§L).
   Authentication is proven by the **logon response** — which says outright
   whether the sign-in succeeded, needs a passcode, or was refused (§B) —
   backed by an authenticated read-only probe (`ReadCustomerOverview.web.v2`
   returning 200 rather than 401, the pre-login 401s in §G being what makes
   that reliable). Never by URL or SPA route, and the probe is gated off the
   sign-in routes so it can never fire mid-challenge. A refusal is raised as
   `LogonFailed` carrying the provider's own `errorCode` / `errorMessage`
   verbatim, never reinterpreted.

   Bronze per run dir on the fleet layout: `run.json`, `accounts.json`,
   `activity/<accountKey>.json` (the merged ledger plus the category map and
   the cycle balances), `transactions/<accountKey>.{csv,qfx}`,
   `statements/<accountKey>/<date>.pdf` (and `yes-<year>.pdf`), `raw/*.json`;
   diagnostics only behind `--debug`. **Bounded, on two windows.**
   `--lookback` rides `start_date`/`end_date` on the export and `DATE_RANGE`
   on the activity call (§E); a window reaching past the 24-month horizon
   those two channels stop at is clamped with a warning rather than silently
   truncated by the server. The statement pass takes the window **as
   requested, unclamped** — the archive reaches the provider's full
   retention, so clamping it too would put the deep backfill out of reach of
   every invocation. Both are in the manifest: `since` with
   `window_clamped`, and `documents_since` (null under `--no-documents`).
   The manifest also records `coverage` for **every** account on the roster
   — rows fetched, rows the source said it had, and whether the two agree.
   `complete` is three-valued: null when the source reported no usable
   total, so a fetch that was never measured cannot read as a positive claim
   of full coverage. A run whose activity fetch came back short is still
   `complete` at the run level, because what it holds is real and loadable;
   without this block nothing would distinguish it from one the source
   honoured in full, and the seam it moves would read as the account's true
   reach.
   The year-end summaries are the
   one exception — the archive lists them per year with no period to
   compare, so every one is fetched whenever the documents are — which is
   to say not under `--no-documents`, and never on a dry run.

   Three things the build had to decide that the captures did not settle:

   - **Pagination units.** `transactionFilters.offset` is 1-based, but the
     SPA only ever sent `offset=1`, so whether it counts rows or pages is
     unmeasured. `download._paginate` walks with a row-based step and checks
     the result: a continuation returning only rows already seen means the
     step was wrong, so it retries that page page-based and keeps whichever
     works. Rows are deduped on their id throughout, which makes either
     reading safe and the wasted request harmless.
   - **The masked user id.** explore's pre-fill leaves a populated field
     alone; `login` passes `overwrite=True` so the real `AMEX_USERNAME` is
     written over the mask a trusted device pre-fills (§G).
   - **Export URLs are constructed, statement URLs are not.** Every export
     parameter is known, so the URL is built and carries the window this run
     asked for; a statement's URL contains an opaque document token that
     cannot be constructed, so the payload's own `downloadOptions` value is
     used verbatim.
4. **load (built)** — bronze → SQLite silver (`migrations/0001_initial.sql`),
   idempotent, unit-tested on synthetic fixtures. **The activity JSON is the
   ledger of record** (§D): it alone carries the stable id, both dates, the
   category, pending rows and the cycle balance summary, so the exports are
   never read by the loader — they are bronze provenance only. Keying on the
   18-digit `identifier` means no content-derived id and no export join, the
   two things that made chase's loader hard.

   Three decisions worth stating:

   - **The sign is flipped on the way in.** Amex's JSON and CSV state a
     purchase as a POSITIVE "amount charged"; the fleet's silver card
     convention — chase's, and Amex's own QFX export's — is the opposite.
     The loader negates, `payload.provider_amount` keeps the original, and
     the migration's header says why. Storing it verbatim would turn every
     purchase into a refund and every bill payment into spend.
   - **Pending rows are replaced, not accumulated.** A pending row's id is
     provisional and changes when the charge posts, so the pending set is
     rebuilt per account from the newest run each load — the fleet's
     rebuild-derived-tables rule, and what makes the transition converge
     with no stale twin.
   - **The category is resolved at load time** from the map the same payload
     ships, so a category Amex adds later lands as its label rather than as
     an unmapped code, and the collector carries no vocabulary table.
5. **Statement backfill (built).** Structured channels stop at 24 months;
   statement PDFs reach the provider's ~7-year retention.
   `statement_parser.py` reads a statement via `pdftotext -layout` and the
   loader imports what the activity cannot reach. Four things the layout
   forced (all pinned in the parser's docstring):

   - **The document states only a CLOSING date** — there is no opening date
     anywhere — so a period's start is chained from the previous period's
     end, over the account's own set of periods.
   - **The summary is a right-hand column interleaved line-by-line with
     legal prose**, so it is read from a whitespace-collapsed blob, label by
     label. Each label is searched independently over the unmutated blob,
     so a label must be unambiguous on its own — the first
     `<label> <amount>` in the document wins.
   - **Two reconciliation gates, both required.** The printed summary must
     add up (previous + payments/credits + new charges + fees + interest ==
     new balance), and each section's rows must sum to the figure printed
     for it. The second is not redundant: rows under a heading the parser
     does not know silently inherit the preceding section's kind, which
     leaves the whole-period total untouched. A statement failing either is
     skipped entirely — not even its balances are recorded.
   - **`Card Ending N-XXXXX` is not a kind heading.** It sub-divides New
     Charges by card member, and its rows are still charges. They all belong
     to the one account, so chase's balance-chained *attribution between
     accounts* is not needed at all.

   The seam gate is on the whole billing PERIOD, not each row's date: a
   statement dates rows by transaction date while the cycle bills by post
   date, so a row-level gate would let a row transacted before the seam but
   posted after it land from both channels. The one straddling period is
   given up rather than double-counted, and marked
   `transactions_covered = 0`. The seam is `MIN(posted_at)` over
   **activity-sourced rows only**, so a statement row can never move it.

   Validated against every statement in the captures, all of which
   reconcile on both gates. The accessible-PDF variant renders no summary this parser
   reads and is refused by the gates rather than imported short.

   Three properties keep the rebuild from turning a bad render or a broken
   tool into lost history, since it deletes the statement era before
   re-importing it and that era exists nowhere else:

   - **Every copy of a period is kept, newest first.** Each run re-fetches
     the same statements, so a period has one copy per run. The newest is
     parsed and the walk stops there when it passes the gates — one
     `pdftotext` per period in the ordinary case — but a refused render
     falls through to the older copies rather than erasing the period. A
     copy whose bytes match one already refused is skipped without parsing.
   - **Parse first, write second, in one transaction.** Every document is
     read before anything is deleted, and the delete plus re-import run
     inside an explicit `BEGIN IMMEDIATE`, so a failure leaves silver as it
     was rather than emptied. `load` also refuses to start at all when
     `pdftotext` is absent.
   - **A tooling fault is not a rebuild.** Every parse raising while silver
     already holds statement periods is a broken parser, not bronze losing
     its documents, and the rebuild raises instead of proceeding. One
     unreadable document stays a per-document skip.

   `statement_balances` is keyed `(account, period_end)`, which the activity
   channel shares: when a statement closes on the same day a run's own
   activity window ended, the statement wins that day. The rebuild
   re-derives the activity row it displaces from bronze first, so an
   incremental load and a `--force` rebuild agree, and a period whose
   statement copies are all refused keeps a mark. An anchor's `snapshot_at`
   names the run holding the copy that survived, not the newest run loaded.
6. **Gold adapter** — `wealthdb/internal/silver/amex/`, on the chase
   adapter's card half: `canonical.AccountKindCard`, the owed balance
   negated into gold's canonical negative cash at exactly one point, credit
   limit and available credit never becoming balance rows, a co-located
   `ReturnsPolicy`, fixture tests. Full detail in
   [`wealthdb/docs/adapters/amex.md`](../../wealthdb/docs/adapters/amex.md).

   Two card-specific pieces beyond the chase template, both now verified:

   - **Merchant category → `provider_category` (§F).** The activity JSON
     files every row under a category and ships the code → label map with
     it, so silver stores the resolved label and it projects into gold's
     `provider_category`. `amexCardCategories` in
     `wealthdb/internal/spending/providermap.go` translates it — a
     *categorical* vocabulary, so an unmapped value is counted drift and
     falls through to the model tier rather than being guessed into a
     neighbour. Amex's own residual "Other" bucket needed a third outcome
     the tier did not have: `untranslatable`, meaning reviewed, so not
     drift, and deliberately left for the model — which is exactly the row
     the model exists for.
   - **The `card_spend` placeholder retires for this card.** An Amex bill
     paid from a tracked cash account is placed as the `card_spend` delta by
     the built-in card-payment rule — a placeholder meaning "real
     consumption on a card this deployment does not itemise"
     (`wealthdb/docs/SPENDING.md` §2). With the card collected, its own
     `card_payment` leg exists and the internal-transfer matcher pairs the
     two, marking both `internal_transfer`; the matcher outranks the rule,
     so this follows from correct projection rather than from a new rule.
     Pinned by `TestPassAmexBillPairsWithTheCollectedCard`, which exercises
     the **cross-source** path — the card is its own silver source, not a
     sibling product of the paying bank, which is the commoner arrangement
     and the one the placeholder was written for. The `American Express`
     entry in the rule's issuer table needs no narrowing: it only LABELS the
     delta line, and once the pair forms the matcher's verdict replaces it.

## 5. Status & operation

**Validated live, bronze through gold (2026-09-06).** `download` (including
the terminal passcode drive and device registration), `vnc-login`,
`login --check`, `load` with its statement backfill, and the gold adapter
have all run against the real source and the real data it produced:

- `download` fetched the roster, the activity ledger, both export formats
  and the statement archive;
- `load` ingested two runs at different windows into silver — every
  statement reconciling on both gates — with no duplicate ids and no row on
  the wrong side of the seam;
- a gold load of that silver, in an isolated data root, projected each card
  as a `card` account carrying a negative cash balance, kinded and signed
  the whole ledger correctly across both eras, and translated the provider
  categories in the spending report.

What that leaves untested against the live source is narrow and named: the
untrusted-device path (`--fresh`) and the `--no-cli-mfa` failure branch.
Statement layouts vary by card product, so a render the parser has not met
may need its section headings added.

`transactionFilters.offset` — 1-based, but counting rows or pages was never
settled by a capture — is disambiguated at runtime, reported at INFO and
recorded in the run manifest as `pagination_mode`, so the measurement is
made once and kept rather than paid for again.

- `make build-amex` builds the image; `make test-amex` runs the unit tests
  in the container.
- Credentials: `~/.secrets/amex.env` at chmod 0600 with `AMEX_USERNAME` /
  `AMEX_PASSWORD` (single-quote values containing `$`, `!`, or backticks).
- `wealthdb-collect amex download` is the whole run: it signs in (a
  registered device needs no passcode), answers one from the terminal if it
  fires — it needs a real TTY, and the docker wrapper forwards stdin only
  when stdin and stdout are both TTYs — registers the device while there,
  and fetches. `--lookback` bounds the window, `--format` picks export
  formats, `--no-documents` skips statements, `--fresh` forces the
  untrusted-device path. Note that `--dry-run` still SIGNS IN; what it skips
  is the exports and the documents, and it still writes a run dir with its
  manifest and roster.
- `wealthdb-collect amex vnc-login` runs the same walk with the sign-in done
  by hand over VNC — the only way past a captcha.
- `wealthdb-collect amex login` is a host-side no-op, so
  `wealthdb-refresh amex` drives login → download → load with no per-source
  configuration and pays exactly one sign-in. `login --check` reports
  whether this device is registered, reading the profile and touching no
  network (§L).
- Run discovery any time with `./amex explore` (or `wealthdb-collect amex
  explore`), connect a VNC viewer to the forwarded port, and walk the §3
  flows. `--fresh` deletes the profile dir to force the untrusted-device
  passcode — where `download --fresh` moves it aside — and `--no-prefill`
  types the credentials by hand.
- Live sessions only when the user explicitly requests one and is present
  (CLAUDE.md §0); human-in-the-loop waits use long timeouts (1h+); never
  fire logins in quick succession.
