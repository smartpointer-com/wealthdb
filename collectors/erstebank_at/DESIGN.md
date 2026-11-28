# erstebank_at — design notes

Design notes for the Erste Bank und Sparkassen (George, Austria) retail
collector. Sections: scope (§1), the discovery harness (§2), the
discovery walk (§3), the source's behaviour (§3-Surface, with lettered
parts §A–§H), and the runtime (§4). The layout follows
[raiffeisen_at](../raiffeisen_at/DESIGN.md).

A section marked *(unverified)* still holds a hypothesis that no capture
has confirmed. Everything else is stated from captures.

## 1. Scope

- **The relationship.** A retail banking relationship at Erste Bank or
  an Austrian Sparkasse, served through **George** (entry
  `https://george.sparkasse.at/`). **Scope is two account kinds**:
  checking accounts (Girokonto) and credit cards. George serves both
  itself (§G). Savings, securities, loans, insurance and any other
  product the same login may expose are out of scope (see
  [AGENTS.md](AGENTS.md)). The name is namespaced to Austria because
  Erste runs a distinct George system in each country.
- **The model is raiffeisen_at for the checking half and amex for the
  card half.** raiffeisen_at is the same country and the same archetype:
  a Camoufox sign-in with a push approval, then REST. amex carries the
  card semantics: statement cycles, and balances signed as liabilities.
- **2FA is an s Identity push** (§B). The browser shows a 4-character
  comparison code. Completion is a plain JSON poll.
- **The data is REST/JSON on `api.sparkasse.at`** (§C), authenticated by
  an OAuth implicit-grant bearer plus session cookies. The transaction
  rows carry stable ids, booking and value dates, and a running balance.
  No DOM scraping is needed.
- **The UI is German.** Selectors never key on display text where an id,
  test-id or ARIA role exists. Load-bearing German labels are recorded
  here. Repo prose stays English.
- **Bot defense is a client sensor on the page, not on the data API**
  (§A). Camoufox clears the sign-in unchallenged. The collector runs on
  `shared/images/base-camoufox.Dockerfile` with a persistent profile.

## 2. The discovery harness (`explore`)

`explore.py` runs the shared harness
([`collectorkit.explore`](../../shared/collectorkit/collectorkit/explore.py)),
which documents its artefacts and redaction. This collector adds the
entry URL, a host gate and the detector's field selectors.

- **Host gate.** `sparkasse.at` and its subdomains. The gate decides which
  frames get DOM snapshots and the in-page detector. Every host George
  uses is under it (§A). The HAR and the network log record every host
  regardless.
- **No pre-fill.** The shared pre-fill fills a username and a password
  together, in one frame, and the George form has no password field
  (§A). The user name is typed by hand over VNC. The harness still reads
  `ERSTEBANK_AT_USERNAME`: it builds the redactor, so no capture holds the
  user name in clear.
- **The token in the redirect.** The implicit grant hands the bearer over
  in a URL fragment (§A). The shared redactor masks secret parameters in
  a fragment and in URL-valued response headers (`Location` and kin), so
  the captures hold `#access_token=<redacted>`.
- **The HAR** is written only when the run stops by Ctrl-C or SIGTERM.
  Closing the browser window leaves `network.jsonl` as the record, with
  headers and response bodies.
- **Screenshots** of the snapshots render blank on this site. The
  `frame0.html` beside each one is the record.
- `--trace` stays off by default: a trace stores every input's value
  and cannot be redacted afterwards.

## 3. The discovery walk

An `explore` session walks four flows over VNC. Each is one continuous
segment.

1. **Sign-in + s Identity.** The user-name form, the push wait screen,
   the comparison code, and its completion. A second short run on the
   same profile (without `--fresh`) shows what the profile remembers
   (§F).
2. **Checking accounts.** Each account's transaction list: its paging,
   its date filters, and how far back it reaches (§C). One transaction's
   detail. Each export format once (§D).
3. **Statements.** The statement archive: its listing, one PDF download,
   and its reach (§E).
4. **Credit cards.** Each card's overview, transactions and statements
   (§G).

## 3-Surface — the source's behaviour

Endpoints are masked: `<accountId>`, `<cardId>`, `<cardAccountId>`,
`<statementId>` and `<billId>` stand for elided values. Account numbers,
user names, balances and the roster live only in the debug dir, never
here.

### §A — Login: OAuth implicit grant on `login.sparkasse.at`

Entry `https://george.sparkasse.at/` loads
`/config/at_sparkasse_george.json` and redirects to
`https://login.sparkasse.at/sts/oauth/authorize?response_type=token&client_id=georgeclient&state=…`.
The login app is server-rendered HTML with Bootstrap 5. There is no
shadow DOM.

1. `GET /sts/oauth/authorize` serves a capability probe. The page posts
   `javaScript=jsOK&publicKeyCredentialSupport=…&isUserVerifyingPlatformAuthenticatorAvailable=…&isConditionalMediationAvailable=…`
   back to the same URL, which answers with the login form.
2. **The form asks for a user name only.** There is no password field.
   `POST /sts/oauth/authorize?client_id=georgeclient&response_type=token`
   with `j_username=<user>&iwht=` (a document navigation; `iwht` is a
   hidden field that stays empty) answers with the push wait screen
   (§B).
3. After approval, `GET /sts/oauth/authorize?client_id=georgeclient&response_type=token`
   answers `302` to
   `https://george.sparkasse.at/index.html#access_token=…&token_type=…&scope=…&expires_in=3599`.
   The SPA reads the bearer from the fragment. **It lives one hour.**

Pinned selectors (all stable ids):

- Form: `form#credentials`.
- User name: `input#user` (`name="j_username"`, `maxlength="80"`).
- Submit: `input#submitButton` (`type="submit"`).
- Error: `#errorContainer` / `#error` (`role="alert"`).
- Hidden: `input#iwhtInput` (`name="iwht"`).
- Cookie banner: `#cookiealert`, dismissed by `button#cookiebutton`.

Load-bearing German labels (orientation only, never selector keys): the
user field's `aria-label` "Verfügernummer/Benutzername".

**Bot defense.** A first-party-proxied client sensor. The vendor is not
identified: no known vendor's cookie or script name appears.

- The login page loads a script from `tools.sparkasse.at/<random>/<random>.js`
  and posts opaque base64-like `text/plain` payloads to a random-looking
  path on `login.sparkasse.at/sts/<random>`, all through the sign-in.
- George loads a second script from `tools.sparkasse.at` and posts the
  same kind of payload to `tools.sparkasse.at/<random>?sid=…&tc9g=…`.
- **No data call carries a sensor header.** Requests to
  `api.sparkasse.at` carry only `authorization`, the cookies, and the
  standard browser headers. That licenses browser-for-login + REST.
- George also posts error reports to a UUID path on `george.sparkasse.at`
  (`sentry-trace` / `baggage` headers) and telemetry to
  `tools.sparkasse.at/proxy/lux-collector-internet/…`. Neither is bot
  defense.

### §B — The s Identity challenge

- Submitting the user name fires the push at once. The wait screen shows
  the user name read-only (`input#user`), a **4-character comparison
  code in `b#verificationCode`**, a loader (`#loader`, `role="status"`),
  an info box (`#infoContainer` / `#infoMsgContainer`) and a cancel link
  (`a#cancelLink`). `a#cancelIcon` ("Benutzer wechseln") switches the
  user. The code is read from the DOM, never from a network event.
- **Completion is a JSON poll.** The page calls
  `POST /sts/secapp/secondfactor?client_id=georgeclient` (no body,
  `X-Requested-With: XMLHttpRequest`) every `pollingIntervalMs` (3000).
  It answers `{"secondFactorStatus": "PENDING", "pollingIntervalMs": 3000}`
  until approval, then `{"secondFactorStatus": "DONE"}`. The page then
  navigates the authorize URL, which redirects with the token (§A).
- The poll lives on the login host. It touches no authenticated data
  surface, so a collector can read it without disturbing the challenge.
- Open: the poll's answer for a declined or expired push, and the error
  text the page shows then. Open: whether a fallback factor exists; the
  wait screen offers none.

### §C — The data API and transaction history

Data rides `api.sparkasse.at` (plus `cards-api.sparkasse.at` for card
add-ons). Every call carries `Authorization: Bearer <token>` **and** the
session cookies, preceded by a CORS preflight. Two API families serve it:
`/rest/netbanking/my/…` (the older netbanking API; vendor media types
`application/vnd.at.spardat.*`) and `/proxy/g/api/my/…` (JSON). Money is
`{value, precision, currency}`: an integer in minor units with its
scale.

- **Roster (checking):** `GET /rest/netbanking/my/accounts` → paged
  (`pageNumber`, `pageCount`, `pageSize`, `pageTotalSize`) `accounts[]`.
  Each row has `id` (opaque hex, the key in every other endpoint and in
  the SPA route `#/currentAccount/<accountId>`), `accountno.{iban,bic}`,
  `currency`, `description`, `product` / `type` (with `*I18N` labels),
  `balance`, `disposable`, `overdraftData.approvedLimit`, `owners[]`,
  `statementCreation.periodicity` and `flags[]`.
  `GET /rest/netbanking/my/accounts/<accountId>` returns one such row.
  `GET /proxy/g/api/my/accounts` is a second roster the dashboard reads.
  It is open whether it covers products the first omits.
- **History:** `GET /proxy/g/api/my/transactions?id=<accountId>&page=<n>&pageSize=<n>`
  (the SPA adds `sum`, `count`, `balance=false` and similar display
  flags). The answer is `{collection[], totalElements, pageSize,
  currentPage, totalPages, metadata}`. Page-number pagination, newest
  first; `totalElements` covers the whole history the server holds.
- **Each row is loader-grade:**
  - a stable `id`;
  - `amount` (signed) and `amountSender`;
  - **`bookingDate` and `valuationDate`** plus `transactionDateTime` and
    `insertTimestamp`, all epoch milliseconds;
  - **a running `balance` on every row**;
  - `txType` (e.g. `POSPAYMENT`, `ATM`, `STANDINGORDER`, `CREDITCARD`,
    `BANKFEE`) and `txDirection` (`INCOMING` / `OUTGOING`);
  - counterparty fields (`partnerName`, `partner.{iban,bic}`,
    `sender*` / `receiver*`), `reference`, SEPA fields (`mandateId`,
    `sepaE2eReference`, `creditorId`), `bookingType`;
  - card fields where a card paid (`cardId`, `maskedCardNumber`,
    `merchantName`, `mcc`, exchange-rate and fee fields), and
    `categories`.
- **History floor:** open. The SPA paged only the first pages. The first
  download that pages to the end measures it.
- `GET /proxy/g/api/my/transactions/<id>/similar` is the detail view's
  "similar transactions" read.

### §D — Exports *(unverified)*

- No export ran in the first capture, so its format, columns and
  mechanism are open. The transaction JSON (§C) already carries ids, both
  dates and a running balance, so an export is the ledger source only if
  it holds something the JSON lacks.

### §E — Statements (Kontoauszüge)

- **Listing:** `GET /rest/netbanking/my/accounts/<accountId>/statements?page=0&size=60&sort=statementDate&order=DESC`
  → `statements[]` with `id` (a long numeric string), `number`,
  `minBookingDate` / `maxBookingDate`, `statementDate`,
  `previousBalance`, `numberOfVouchers` and `flags` (`isAsynchronous`).
  The listing takes no date window and answers the whole archive in one
  page. Whether the server caps its reach is open.
- **Download:** `POST /rest/netbanking/my/accounts/<accountId>/statements/download?format=<fmt>&statementIds=<statementId>&includeVoucher=<bool>`
  with the bearer as a form field (`access_token=…`) → the PDF
  (`application/octet-stream`). The SPA turns it into a `blob:` URL. The
  dialog's toggles are `includeVoucher` and `bankSigned`. This is a
  read-only export trigger.
- **The open period:** `GET …/statements/current` returns the period not
  yet printed: `previousBalance`, `newBalance`, `totalCredits`,
  `totalDebits` and its `transactions[]`.
- `GET …/statements/<statementId>/at/fees?includeVoucher=…&format=…` lists
  the fees a statement charges.

### §F — Session persistence and device trust *(unverified)*

- Unknown. The measurement: close the browser after a signed-in explore,
  then run a second short explore on the same profile, without
  `--fresh`.
  - Does the session survive the restart?
  - Does the push fire again?
- This one measurement decides the verb split (§4): a push at every
  sign-in folds `login` into `download`; durable trust gives a separate
  `login` verb.
- The bearer lives one hour (§A), so one download run fits inside one
  token whatever the answer.

### §G — Credit cards: served by George itself

- **Roster:** `GET /rest/netbanking/my/cards` → `cards[]` covering debit
  and credit cards (`type` tells them apart). Each card has `id`,
  `number` (masked), `product`, `expiryDate`, `limit`, `state`, `brand`,
  `mainAccount` (the checking account it is tied to) and settings
  blocks. `GET /rest/netbanking/my/cards/<cardId>` adds `balance` and
  `directDebitAccount`.
- **The card account.** The SPA routes a credit card as
  `#/creditcard/<cardAccountId>-<cardId>`. The card account id keys its
  transactions and invoices.
- **Transactions** come from the same history endpoint as checking
  (§C), keyed by the card account, with `txType: CREDITCARD`.
- **Statements (Abrechnungen):**
  `GET /proxy/g/api/my/accounts/<cardAccountId>-<cardId>/invoices?numberOfMonths=36`
  → `[{id, accountId, billId, from, to, amount, previousAmount,
  chargeAmount, chargeDate, billingDate, …}]` (dates in epoch
  milliseconds). The PDF:
  `POST /rest/netbanking/my/cards/<cardId>/invoices/<billId>/pdf` with
  the bearer as a form field → `application/pdf`.
- Open: the sign of a card balance and of card transaction amounts, and
  whether the server returns more than `numberOfMonths=36` reaches.

### §H — Still open

What the next explore must answer, in order of what it decides:

1. Session persistence and device trust across a browser restart (§F).
2. The export formats against the SPA's JSON (§D).
3. The push's declined or expired answer (§B).

The history floor (§C) and the statement and invoice caps (§E, §G) are
measured by the first download that pages to the end.

## 4. Runtime

Built from the explore captures. The verb split (§F) and the runtime tier
are proposed from the evidence before `login` and `download` are written.
The captures so far point to a Camoufox sign-in followed by REST through
`page.request`, which shares the browser's cookie jar (§A, §C). Until then
only `explore` and `prune` run. `login`, `download` and `load` are
accepted by the wrapper and exit 2.
