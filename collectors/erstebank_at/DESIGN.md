# erstebank_at — design notes

Design notes for the Erste Bank und Sparkassen (George, Austria) retail
collector. Sections: scope (§1), the discovery harness (§2), the
discovery walk (§3), the source's behaviour (§3-Surface, with lettered
parts §A–§H), and the runtime (§4). The layout follows
[raiffeisen_at](../raiffeisen_at/DESIGN.md). Everything below is stated
from captures; §H lists what is still open.

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
- **2FA is a George app push** (§B). The browser shows a 4-character
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
- **The HAR** is written only when the run stops by one Ctrl-C or
  SIGTERM; further Ctrl-C presses are ignored while the browser closes and
  writes it. Closing the browser window leaves `network.jsonl` as the
  record, with headers and response bodies.
- **Captures run the image's collectorkit**, not the checkout's. A fix to
  the shared redactors reaches `explore` only after `make base-images`
  and a rebuild.
- **Screenshots** of the snapshots render blank on this site. The
  `frame0.html` beside each one is the record.
- `--trace` stays off by default: a trace stores every input's value
  and cannot be redacted afterwards.

## 3. The discovery walk

An `explore` session walks four flows over VNC. Each is one continuous
segment.

1. **Sign-in + push.** The user-name form, the push wait screen,
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

### §B — The push challenge (George app / George ID)

- Submitting the user name fires the push at once. It is approved in the
  **George app** on a phone, or in **George ID** on a computer; the info
  box names both. The wait screen shows the user name read-only
  (`input#user`), a **4-character comparison code in
  `b#verificationCode`**, a loader (`#loader`, `role="status"`), the info
  box (`#infoContainer` / `#infoMsgContainer`) and a cancel link
  (`a#cancelLink`). `a#cancelIcon` ("Benutzer wechseln") switches the
  user. The code is read from the DOM, never from a network event.
- The wait screen shows no deadline. A push approved four minutes after
  it fired still completed the sign-in, inside the fleet's five-minute
  MFA wait.
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

### §D — Exports: per statement, poorer than the JSON

- George has no free-range transaction export. The export is the
  statement download (§E) in another format. The dialog's format
  drop-down offers `PDF (A4)`, `Text (A4)`, `Text (A4/3)`, `Text (A6)` and
  `OFX`; the wire codes seen are `PDF_A4`, `TXT_A4` and `OFX`.
- **OFX** is OFX 1.02 SGML (`CHARSET:1252`). It carries one
  `BANKTRANLIST` over the statement's period, a closing `LEDGERBAL`, and
  per row only `TRNTYPE` (always `XFER`), `DTPOSTED`, `TRNAMT`, `FITID`,
  `NAME` and `MEMO`. There is no value date and no running balance.
- **Text** is the printed statement laid out as plain text.
- **The JSON is the ledger source** (§C): it carries everything the OFX
  does, plus value dates, a running balance, types and counterparties,
  over the whole history rather than one statement period. The exports
  are not used.

### §E — Statements (Kontoauszüge)

- **Listing:** `GET /rest/netbanking/my/accounts/<accountId>/statements?page=0&size=60&sort=statementDate&order=DESC`
  → `statements[]` with `id` (a long numeric string), `number`,
  `minBookingDate` / `maxBookingDate`, `statementDate`,
  `previousBalance`, `numberOfVouchers` and `flags` (`isAsynchronous`).
  The listing takes no date window and answers the whole archive in one
  page. Whether the server caps its reach is open.
- **Download:** `POST /rest/netbanking/my/accounts/<accountId>/statements/download?format=<fmt>&statementIds=<statementId>&includeVoucher=<bool>`
  with the bearer as a form field (`access_token=…`) → the file
  (`application/octet-stream`). The SPA turns it into a `blob:` URL. The
  dialog's toggles are `includeVoucher` and `bankSigned`. This is a
  read-only export trigger.
- **Statements render on demand** (the `isAsynchronous` flag). The first
  download of a statement in a format can answer
  `422 {"errors": [{"scope": "ASYNCHRON", "error": "FIELD_INVALID"}]}`
  while the server renders it. The same call a few seconds later returns
  the file. Once rendered, a format answers at once.
- **The open period:** `GET …/statements/current` returns the period not
  yet printed: `previousBalance`, `newBalance`, `totalCredits`,
  `totalDebits` and its `transactions[]`.
- `GET …/statements/<statementId>/at/fees?includeVoucher=…&format=…` lists
  the fees a statement charges.

### §F — Session persistence and device trust: none

- **Nothing survives a browser restart.** A run on the same profile
  without `--fresh` opens on an empty login form. Neither the session nor
  the user name is remembered, and the push fires again.
- The bearer lives in the page, handed over in the redirect fragment
  (§A), for one hour. One run fits inside one token.
- So `login` folds into `download` (§4): one browser lifetime and one
  push per run, as in raiffeisen_at.

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
- **Signs.** A card amount is signed like a checking one: outgoing
  (a purchase) negative, incoming (a refund or repayment) positive. The
  card's `balance`, in its detail and in each row's running balance, is
  negative while money is owed. That is already the fleet's liability
  sign, as in amex. A credit card's detail has `type: CREDIT`.
- Open: whether the server returns more than `numberOfMonths=36` reaches.

### §H — Still open

- The push's declined or expired answer (§B). The first run that meets
  one records it; the collector shows the page's own error text.
- The history floor (§C), the statement archive's cap (§E) and the
  invoice listing's cap (§G). The first download that pages to the end
  measures all three, and they decide whether a statement backfill is
  needed at all.

## 4. Runtime

- **Verbs.** No trust survives the browser (§F), so `login` folds into
  `download`: one Camoufox lifetime per run, signed in by one push.
- **Tier.** Bot defense rides the pages, not the data API (§A), so the
  browser signs in and the data is read as REST through `page.request`,
  which shares the browser's cookie jar, with the bearer from the
  redirect added to each call.

`login` and `download` are not built yet. Until then only `explore` and
`prune` run. `login`, `download` and `load` are accepted by the wrapper
and exit 2.
