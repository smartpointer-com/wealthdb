# amex — design notes

Design notes for the American Express card collector:

- scope (§1);
- the discovery harness (§2) and what it walks (§3);
- the source's behaviour as captured (§A–§H);
- the behaviour that shows only in real sign-ins (§I–§M);
- the build (§4) and its known limits (§5).

[README.md](README.md) covers usage, the bronze layout and the silver
tables. [AGENTS.md](AGENTS.md) holds the allow and forbid surface.

## 1. Scope

- **The relationship.** A card relationship at American Express
  (americanexpress.com). Scope is the **credit and charge card accounts,
  read-only**: card roster, per-card detail and balances, transaction
  history, exports, statement PDFs. Everything else the same login can
  reach is out of scope in code and docs. See [AGENTS.md](AGENTS.md) for
  the allow/forbid surface.
- **Two siblings supply the pieces.** [`chase`](../chase/) is the fleet's
  other card collector: Camoufox, terminal-driven 2FA, activity exports
  plus a statement backfill, and `download` as the one verb that signs in
  (§L). [`firstcitizens`](../firstcitizens/) supplies the data path: a
  browser login, then REST through `page.request` (§A).
- **A card is a liability, not a conduit.** Chase's conduit-returns
  rationale is about its *cash* accounts and is not an argument about
  cards. A card projects as `canonical.AccountKindCard`. Its balance is a
  liability in net worth, and it stays out of returns and allocation
  exactly as chase's cards do.
- **No public API.** Amex offers no self-service programmatic access for
  an individual card member. The site is a React SPA over two internal
  surfaces (§A), and those are what the collector drives.
- **Bot defense: Akamai Bot Manager (§A).** It is the same class of
  defense as at the US siblings. Camoufox clears the session end to end
  with no visible challenge, so the vanilla-browser rungs are skipped. The
  defense is **cookie-borne**, and **no data endpoint carries a sensor
  header**. That licenses a REST data path off the browser's own context.

## 2. The discovery harness (`explore`)

`explore.py` is copied and adapted from [`chase`](../chase/explore.py)'s
harness. Each explore harness drifts with its source, so they are copied
and adapted on purpose rather than extracted into a shared library.

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/amex/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map: every request + response. Flushed on the context close and rewritten through the redactor on the same unwind, error included. Playwright records it raw, so it is never crash-safe and secret-free at once. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR. Text bodies ≤ 4 MB captured inline, OFX/QFX content types included. A form-urlencoded request body is masked field by field, like the HAR's. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and OTP-field events. |
| `dom/<NNN>/` | **Every distinct screen's full DOM** (all americanexpress.com frames) + a screenshot, deduped by DOM structure. Selectors are pinned from it. |
| `downloads/` | Every file the session fetches (statement PDFs, exports), sequence-prefixed against reused filenames. |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace. Off by default, because the pinned Playwright tracer crashes the pinned Camoufox build (see base-camoufox). Written by Playwright in its own format and **not scrubbed**: its DOM snapshots store every input's value, so a trace holds the credential. |

The harness mechanics follow chase's ([chase DESIGN.md §2](../chase/DESIGN.md)):

- the login pre-fill runs only in americanexpress.com frames, and only
  when both fields sit in one frame;
- each field is filled once and verified by read-back;
- `signon.*` prefs are off, so a profile-saved credential never autofills
  on top of the fill;
- the OTP-field detector logs the field's static descriptor, never its
  value;
- the click log records element labels only, never a field value.

Every capture goes through the shared redaction described in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
Sign-in and 2FA are always submitted by hand over VNC. The harness never
clicks a button.

## 3. What discovery walks

An `explore` walk answers these questions, in one recording plus a short
second one:

- where the sign-in lives, and which factor it challenges with;
- **which endpoints carry bot-defense sensor headers**. That decides
  browser-everywhere versus browser-for-login plus REST;
- the roster's stable identifier;
- the transaction listing's shape and pagination, and whether a row
  carries a stable id and a merchant category;
- every export format, with its columns and its reach;
- the statement archive's reach;
- as a separate short run, **whether device trust survives a browser
  restart**.

§A–§H record the answers. Two of them differ from the obvious guess. There
is no OFX 1.x among the export formats (§E). Device trust alone does not
decide the verb split (§G, §L).

## Source behaviour (§A–§H)

The evidence comes from VNC-driven `explore` captures: login with an SMS
one-time passcode, device registration, the card overview, the activity
views, every export format, the statements area, the persistence probe
(§G) and the balance, credit-details and APR surfaces (§H).

Endpoints below are masked (`<account_key>`, `<accountToken>`, document
tokens). No account identifiers, balances, merchants or amounts are
recorded here. They live only in the debug dir.

The authenticated app is a **React SPA on `global.americanexpress.com`**
backed by two API surfaces: a **BFF of named "functions"** at
`functions.americanexpress.com` (POST, JSON in / JSON out, no REST
resource paths) and a **REST servicing API** at
`global.americanexpress.com/api/servicing/…` (documents and exports).
Everything else in the traffic is CDN or telemetry —
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
bearer token and no CSRF header anywhere in the traffic**. So the defense is
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

**A partially-filled code submits and fails.** A `continue-button` click
with any box empty returns **HTTP 400** from
`UpdateAuthenticationTokenWithChallenge.v3`, and the retry needs the boxes
re-entered. So the terminal drive fills all six, **verifies each by
read-back before clicking Continue**, and **clears all six before any
retry**. That is the segmented-input form of the fleet's
clear-OTP-before-retry rule.

**The sign-in is a capture hazard in two places.** The login body
percent-encodes the password. The serialized sign-in form carries the typed
password as a `value` attribute. `collectorkit.debugcap` covers both
(`secret_redactor`, `scrub_dom`) for every harness in the fleet. The
general point: **a credential does not reach the wire verbatim, and a DOM
snapshot is a capture surface of its own.**

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

**Credit limit and available credit are not read.** They sit behind the
card's account-details ("Vitals") surface, which `placementLinks.VITALS`
links to, and no capture has named their endpoint (§H). A limit is not a
balance and never becomes a gold row (the chase rule).

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
  same value (they are equal on every row).
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
row carries a `P`-prefixed, longer id and appears in **no** export. So a
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
asked for rather than the period's (§4.1):

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
- `status=posted` is the only value sent; **no export carries a pending
  row**.

**Statements** come from the same `ReadAccountActivity.web.v1` call with
`view: "STATEMENTS"`, which returns the **entire archive in one response**
as `billingStatements.{recentStatements, olderStatements, yearEndSummaries}`.
Each entry is a `statementEndDate` plus its own `downloadOptions`. The PDF
fetch is
`GET /api/servicing/v1/documents/statements/<opaque token>?account_key=…&client_id=OneAmex`.
The token is a long hex blob carried in that entry, never constructed.
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
backfill** worth building (§4.3). What `downloadOptions` adds over chase is
that the split is stated per period by the API itself rather than inferred.
The collector does not read it at runtime, though:

- the window for the activity and the exports, the two channels that
  stop, is bounded by a constant (`MAX_AVAILABLE_MONTHS`);
- the import seam is `MIN(posted_at)` over the activity rows a run
  actually landed (§4.3), which has to move as windows widen.

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
comes back without them. Before any sign-in, `ReadCustomerProducts.v2` and
`GET /api/servicing/v1/member` both return **401**, and the request headers
show the jar carrying `device-id` and the Akamai cookies but **none** of the
three session cookies. This is not an idle timeout a quicker restart could
beat — reopening the browser can never continue an Amex session.

**Device trust DOES survive, and it skips the passcode.** Re-submitting the
password on a trusted device returns `statusCode: 0`, `errorCode: ""`,
`reauth.trust: true`, `challenge: false`. An untrusted device returns
`statusCode: 1`, `errorCode: "LGON013"` and a `reauth.mfaId`. **No
challenge-flow request fires** (no
`ReadAuthenticationChallenges`, no `CreateOneTimePasscodeDelivery`, no
`UpdateAuthenticationTokenWithChallenge`). Read `reauth.trust`, **not**
`reauth.deviceRemembered`, which is not the trust signal and describes
something else.

**On the trust axis this matches firstcitizens.** Durable device trust
sits over a session that dies with the browser, as in
[firstcitizens](../firstcitizens/DESIGN.md) §3. firstcitizens therefore
splits `login` from `download`. Amex does not, because the source
rate-limits sign-ins (§K, §L). The mechanics below are why `download`
signs in without a passcode on a registered device.

Restoring the session cookies from a saved jar to skip the logon is
**rejected**. They are `Discard`-scoped by the provider, `aat` is
`HttpOnly`, and the server holds its own state. This is the "no cleverness
in auth flows" rule. A password-only logon is unattended anyway, so it
buys nothing.

**Trusted-device logon differs from the untrusted one in three ways** worth
coding against:

- The form arrives with a **masked user id** pre-filled, and submitting it
  unchanged works, because the `device-id` cookie resolves the identity.
  The sign-in therefore **clears the field and fills `AMEX_USERNAME`**.
  Explore's "field already has content, leave it alone" rule would submit
  the mask. That works only while trust holds, and would fail confusingly
  the day it lapses.
- The body carries `REMEMBERME` and a `signature` field the first login did
  not.
- Device identity is verified through
  `CreateDeviceIdentityVerificationChallenge.v1` rather than the untrusted
  path's `ReadDeviceIdentityRegistrationChallenge.v1`.

`login --check` reads the profile's own `device-id` cookie and reports
it: **exit 0 when the device is registered, 1 when it is not**. It makes no
navigation and no call to the source (§L).

### §H — Further surfaces

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
- **Credit limit / available credit have no named endpoint.** The
  balance-and-credit-details expander renders without a distinguishable
  data call. The figure lives behind a module
  (`myca-balance-credit-details`) whose endpoint no capture has named. It
  stays unresolved on purpose: a limit is not a balance and never becomes
  a gold row (the chase rule), so it does not justify another live
  session.

## Live behaviour (§I–§M)

Some source behaviour shows only in real sign-ins, never in a capture.
Each entry names such a property and the code's answer to it. Sign-ins
are this source's scarce resource (§K).

### §I — The CORS preflight looks like a refusal

The browser sends a **CORS preflight** to the logon URL. That `OPTIONS`
answers **200 with an empty body about a second before the real POST
response arrives**. A watcher that matches on the URL alone takes the
preflight for the outcome. It finds no `statusCode` in the empty body and
reports a refusal the provider never made.

So the logon watcher matches the POST only. A POST that carries no verdict
is reported as an outcome that could not be READ, not as a refusal. One of
those points at the credentials and the other does not.

**Fleet rule: a response watcher matches on the METHOD as well as the
URL.** Any endpoint the SPA calls cross-origin has a same-URL preflight
whose empty 200 arrives first. This is the response-side twin of the
fleet's "authentication is proven by a read, never by a URL" rule.

### §J — Rapid sign-ins are challenged, even on a trusted device

Rapid repeated password submissions draw a passcode challenge even on a
device whose trust is intact. That is a **rate response, not
lost trust**: `device-id` carries a ~396-day lifetime (§G). It is the
fleet's "logins are budgeted" lesson on this source.

So a challenge can fire on any run, and `download` answers one when it can.
`download.resolve_two_factor` decides how:

- **auto by default**: prompt in the terminal iff stdin is a TTY;
- `--cli-mfa` / `--no-cli-mfa` force either half.

Without a TTY a challenge raises `NeedsLogin` and fails loudly. An
unattended run never blocks on a prompt nobody will answer. The docker
wrapper gives the container a TTY only when stdin *and* stdout are both
TTYs. The container's own `isatty` is therefore a faithful answer to "is
there someone to prompt".

### §K — Bot defense fires, and it is not a passcode

A handful of sign-ins in a short window draw **a captcha**. The logon
response still announces a step-up, but the browser shows a bot-defense
challenge, not a passcode screen. A terminal cannot answer it. A human over
VNC can, and is then signed in with no passcode.

This settles three things.

**The step-up is not one thing.** `challenge: true` or an `mfaId` in the
logon response means "something more is required". §B's passcode is only
one such thing. A captcha is another, answered by a human or not at all.
The terminal drive checks for one and stops at once, naming `vnc-login`,
rather than timing out against UI that will never appear.

**The failure path is self-diagnosing.** When the passcode screen does not
appear within 30 seconds, the drive prints the page path, the title and
the identifiers of every visible control. It does so unconditionally, not
behind `--debug`, so a pasted log carries the evidence. It prints
identifiers only, never element text: a challenge screen's labels carry
the masked destination.

**The budget is real and small.** It arrives as a captcha, not a lockout.
Captcha reputation accrues per account and decays with idle time, so the
remedy is to stop, not to retry. A `login` → `download` pair would be two
sign-ins, which spends the budget without anyone noticing (§L).

The captcha selectors are **not pinned from a capture**: no DOM dump of a
captcha exists. The detector is a broad net over the shapes a captcha
widget takes. It is used only to abort with the right instruction, never
to drive anything. A false positive costs one run that says "use
vnc-login" when the terminal would have done.

### §L — The verb split: `login` folds into `download`

§G settles the mechanics. Device trust persists and the session does not,
so a separate `login` verb *could* mint the trust once and let `download`
run unattended against it. §K adds a second axis: **how many sign-ins the
source tolerates**. On that axis a split is wrong.

An orchestrator runs `login` → `download` → `load` for every source. A
split would make that two sign-ins per run, against a budget of a handful
in a short window. And the trust a `login` would mint is minted just as
well inside `download`, by the same code, on the same profile, when a
challenge is answered. So the shape is chase's and schwab-web's:
**`download` is the one verb that signs in**. The durable trust of §G is
what lets it sign in without a passcode on every run after the first.

| verb | what it does |
| --- | --- |
| `download` | the whole run: sign in, answer a passcode from the terminal if one fires (registering the device while there), fetch |
| `vnc-login` | the same walk, behind a sign-in completed **by hand** over VNC; the only way to answer a captcha, and the way to register a device with no terminal |
| `login` | a no-op, trapped **host-side** so an orchestrator's login → download → load neither trips nor pays a container start |
| `login --check` | reports whether this device is registered |

**`login --check` reads the profile, not the source.** The fleet's usual
`--check` makes the cheapest authenticated call the source allows, so a
credential that exists but is rejected is caught. Here that call is a full
sign-in. A probe that spends the scarce thing to ask whether the scarce
thing can be spent defeats itself.

So it reads the profile's own `device-id` cookie straight out of the
Firefox jar on disk, and reports that and its expiry. It opens no browser
at all: launching one resolves the egress IP over the network, which would
break the claim the probe rests on. It answers "is this device
registered", which decides whether `download` needs a human. It does
**not** answer "will the next sign-in succeed". The provider can revoke
trust server-side, and only a sign-in would see that. The help states the
limit too. It is the honest trade for not spending a sign-in on a
question.

### §M — Device registration and the moving seam

**A missed registration is loud.** After a passcode, the sign-in clicks
the "Add This Device" control. If that click finds nothing, the next run
pays another passcode, which is the expensive failure here (§K). So the
selector is a set covering the wordings the control ships under, matched
case-insensitively. A miss reports the identifiers of every visible
control. That tells a drifted control apart from a provider that did not
offer registration, and the two want different responses.

**The statement era is rebuilt on every load.** The seam is
`MIN(posted_at)` over activity rows, and it MOVES. A wider window reaches
further back, so periods below the seam become covered by the activity. An
incremental import never withdraws what it already wrote, so two loads at
different `--lookback` windows would carry the same charges twice. The
loader therefore applies the fleet's rebuild-derived-tables rule, as it
does for pending rows. It drops the statement-sourced rows and rebuilds
them from all of bronze on every load. The result depends only on what
bronze holds, never on the order loads happened in. A test widens the
window between two loads to pin this.

**Statement parses are deduped on the PERIOD.** Every run re-fetches the
same statements, so a tree with N runs holds N copies of each. Each copy
would cost a `pdftotext` subprocess. A re-rendered PDF is a new hash for
the same statement, so the dedupe key is the logical period, not the
bytes: the fleet's dedupe-on-logical-identity rule.

## 4. The build

The runtime is an `explore` harness (§2), a Dockerfile on base-camoufox, a
wrapper on `shared/wrappers/wrapper-lib.sh`, and a host-side `prune`.
[README.md](README.md) lists the bronze run layout and the silver tables.

### 4.1 `download`: the one verb that signs in (§L)

Camoufox does the logon. The data then comes over REST through
`page.request`, with no DOM scraping (§A).

- **The sign-in** submits the form. A trusted device lands signed in with
  no challenge. A passcode is answered from the terminal when there is one,
  on a tested browserless dialog core (`auth_dialog.py`). It fills all six
  boxes and verifies them by read-back before Continue (§B), then
  registers the device. With no terminal a challenge is a loud failure,
  never a blocked prompt. A captcha is never answerable here (§K). The
  sign-in clears the user id and fills it, rather than accepting the
  masked pre-fill (§G). `--fresh` moves the profile aside to force the
  untrusted path.
- **`vnc-login`** runs the same walk behind a sign-in completed by hand.
- **`login`** is a host-side no-op. **`login --check`** reports device
  registration from the profile's own cookie, with no sign-in (§L).

Authentication is proven by the **logon response**, which says outright
whether the sign-in succeeded, needs a passcode, or was refused (§B). An
authenticated read-only probe backs it: `ReadCustomerOverview.web.v2`
returning 200 rather than 401. The pre-login 401s in §G are what make that
reliable. Authentication is never proven by URL or SPA route. The probe is
gated off the sign-in routes, so it never fires mid-challenge. A refusal
is raised as `LogonFailed`, carrying the provider's own `errorCode` /
`errorMessage` verbatim.

**Bounded, on two windows.**

- `--lookback` rides `start_date`/`end_date` on the export and
  `DATE_RANGE` on the activity call (§E). A window reaching past the
  24-month horizon of those two channels is clamped with a warning, not
  silently truncated by the server.
- The statement pass takes the window **as requested**, unclamped. The
  archive reaches the provider's full retention, and clamping it too would
  put the deep backfill out of reach of every invocation.
- The year-end summaries have no period to compare against the window. All
  of them are fetched whenever the documents are: never under
  `--no-documents`, and never on a dry run.

**The manifest** (`run.json`) records:

- `since` with `window_clamped`, and `documents_since` (null under
  `--no-documents`);
- `pagination_mode`, the measured `transactionFilters.offset` unit (below);
- `coverage` for **every** account on the roster: rows fetched, rows the
  source said it had, and whether the two agree. `complete` is
  three-valued. It is null when the source reported no usable total, so an
  unmeasured fetch never reads as a claim of full coverage.

A run whose activity fetch came back short is still `complete` at the run
level, because what it holds is real and loadable. The coverage block is
what tells it apart from a full fetch. Without it the seam that run moves
would read as the account's true reach.

Three choices the captures did not settle:

- **Pagination units.** `transactionFilters.offset` is 1-based, but the
  SPA only ever sends `offset=1`. Whether it counts rows or pages is
  unmeasured. `download._paginate` walks with a row-based step and checks
  the result. A continuation returning only rows already seen means the
  step was wrong, so it retries that page page-based and keeps whichever
  works. Rows are deduped on their id throughout, which makes either
  reading safe. The unit found is logged at INFO and recorded as
  `pagination_mode`.
- **The masked user id.** Explore's pre-fill leaves a populated field
  alone. The sign-in passes `overwrite=True`, so the real `AMEX_USERNAME`
  is written over the mask a trusted device pre-fills (§G).
- **Export URLs are constructed, statement URLs are not.** Every export
  parameter is known, so the URL is built and carries the window the run
  asked for. A statement's URL contains an opaque document token that
  cannot be constructed, so the payload's own `downloadOptions` value is
  used verbatim.

### 4.2 `load`

`load` writes the SQLite silver
([`migrations/0001_initial.sql`](migrations/0001_initial.sql)) and is
idempotent. **The activity JSON is the ledger of record** (§D). It alone
carries the stable id, both dates, the category, pending rows and the
cycle balance summary. The exports are never read by the loader; they are
bronze provenance only. Keying on the 18-digit `identifier` means no
content-derived id and no export join, the two things that make chase's
loader hard.

- **The sign is flipped on the way in.** Amex's JSON and CSV state a
  purchase as a POSITIVE "amount charged". The fleet's silver card
  convention, chase's and Amex's own QFX export's, is the opposite. The
  loader negates, and `payload.provider_amount` keeps the original. The
  migration's header says why. Storing it verbatim would turn every
  purchase into a refund and every bill payment into spend.
- **Pending rows are replaced, not accumulated.** A pending row's id is
  provisional and changes when the charge posts. So the pending set is
  rebuilt per account from the newest run each load. That is the fleet's
  rebuild-derived-tables rule, and it lets the transition converge with no
  stale twin.
- **The category is resolved at load time** from the map the same payload
  ships. A category Amex adds later lands as its label rather than as an
  unmapped code, and the collector carries no vocabulary table.

### 4.3 The statement backfill

Structured channels stop at 24 months. Statement PDFs reach the provider's
~7-year retention. `statement_parser.py` reads a statement via
`pdftotext -layout`, and the loader imports what the activity cannot
reach. Four things the layout forces (all pinned in the parser's
docstring):

- **The document states only a CLOSING date.** There is no opening date
  anywhere, so a period's start is chained from the previous period's end,
  over the account's own set of periods.
- **The summary is a right-hand column interleaved line by line with legal
  prose.** It is read from a whitespace-collapsed blob, label by label.
  Each label is searched independently over the unmutated blob, so a label
  must be unambiguous on its own: the first `<label> <amount>` in the
  document wins.
- **Two reconciliation gates, both required.** The printed summary must add
  up (previous + payments/credits + new charges + fees + interest == new
  balance). Each section's rows must sum to the figure printed for it. The
  second gate is not redundant. Rows under a heading the parser does not
  know silently inherit the preceding section's kind, which leaves the
  whole-period total untouched. A statement failing either gate is skipped
  entirely, balances included.
- **`Card Ending N-XXXXX` is not a kind heading.** It sub-divides New
  Charges by card member, and its rows are still charges. They all belong
  to the one account, so chase's balance-chained *attribution between
  accounts* is not needed at all.

The seam gate is on the whole billing PERIOD, not each row's date. A
statement dates rows by transaction date while the cycle bills by post
date. A row-level gate would let a row transacted before the seam but
posted after it land from both channels. The one straddling period is
given up rather than double-counted, and marked
`transactions_covered = 0`. The seam is `MIN(posted_at)` over
**activity-sourced rows only**, so a statement row can never move it.

The accessible-PDF variant renders no summary this parser reads. The gates
refuse it rather than import it short.

The rebuild deletes the statement era before re-importing it, and that era
exists nowhere else. Three properties keep a bad render or a broken tool
from turning into lost history:

- **Every copy of a period is kept, newest first.** A period has one copy
  per run. The newest is parsed, and the walk stops there when it passes
  the gates, so the ordinary case costs one `pdftotext` per period. A
  refused render falls through to the older copies rather than erasing the
  period. A copy whose bytes match one already refused is skipped without
  parsing.
- **Parse first, write second, in one transaction.** Every document is
  read before anything is deleted. The delete plus re-import run inside an
  explicit `BEGIN IMMEDIATE`, so a failure leaves silver as it was. `load`
  refuses to start at all when `pdftotext` is absent.
- **A tooling fault is not a rebuild.** If every parse raises while silver
  already holds statement periods, the parser is broken; bronze has not
  lost its documents. The rebuild raises instead of proceeding. One
  unreadable document stays a per-document skip.

A parser change also triggers the rebuild. `parser_generations`
(migration 0002) stamps the parser generation that produced the statement
era, and a stale stamp opens the same gate a new bronze run does.

`statement_balances` is keyed `(account, period_end)`, which the activity
channel shares. When a statement closes on the same day a run's own
activity window ended, the statement wins that day. The rebuild first
re-derives the activity row it displaces from bronze. An incremental load
and a `--force` rebuild therefore agree, and a period whose statement
copies are all refused keeps a mark. An anchor's `snapshot_at` names the
run holding the copy that survived, not the newest run loaded.

### 4.4 Gold adapter

`wealthdb/internal/silver/amex/` follows the chase adapter's card half.
Each card is a `canonical.AccountKindCard`, and the owed balance is
negated into gold's canonical negative cash at exactly one point. Credit
limit and available credit never become balance rows. The adapter carries a
co-located `ReturnsPolicy` and fixture tests. The merchant-category
translation and the `card_spend` pairing are in
[`wealthdb/docs/adapters/amex.md`](../../wealthdb/docs/adapters/amex.md).

## 5. Known limits

Paths covered by unit tests only:

- the untrusted-device path (`--fresh`);
- the `--no-cli-mfa` failure branch.

Statement layouts vary by card product. A render the parser has not met
may need its section headings added.

Operation and examples are in [README.md](README.md). The rules for real
sessions are in [AGENTS.md](AGENTS.md) §0.
