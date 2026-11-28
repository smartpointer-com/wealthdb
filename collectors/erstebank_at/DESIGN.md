# erstebank_at — design notes

Design notes for the Erste Bank und Sparkassen (George, Austria) retail
collector. Sections: scope (§1), the discovery harness (§2), the
discovery walk (§3), the source's behaviour (§3-Surface, with lettered
parts §A–§H), and the runtime (§4). The layout follows
[raiffeisen_at](../raiffeisen_at/DESIGN.md).

The source-behaviour sections hold **hypotheses** until an explore
capture confirms or refutes them. Each is marked *(unverified)* until
then. A confirmed entry is restated as fact; a refuted one is corrected.

## 1. Scope

- **The relationship.** A retail banking relationship at Erste Bank or
  an Austrian Sparkasse, served through **George** (entry
  `https://george.sparkasse.at/`). **Scope is two account kinds**:
  checking accounts (Girokonto) and credit cards, as far as George itself
  serves them. Savings, securities, loans, insurance and any other
  product the same login may expose are out of scope (see
  [AGENTS.md](AGENTS.md)). The name is namespaced to Austria because
  Erste runs a distinct George system in each country.
- **The model is raiffeisen_at for the checking half and amex for the
  card half.** raiffeisen_at is the same country and the same expected
  archetype: a Camoufox sign-in with a push approval, then REST. amex
  carries the card semantics: statement cycles, and balances signed as
  liabilities. This collector diverges from them only where a capture
  proves George differs.
- **The UI is German.** Selectors never key on display text where an id,
  test-id or ARIA role exists. Load-bearing German labels are recorded
  here. Repo prose stays English.
- **Bot defense is unmeasured.** It is treated as hostile until a
  capture shows otherwise, so the collector starts on
  `shared/images/base-camoufox.Dockerfile` with a persistent profile.

## 2. The discovery harness (`explore`)

`explore.py` runs the shared harness
([`collectorkit.explore`](../../shared/collectorkit/collectorkit/explore.py)),
which documents its artefacts and redaction. This collector adds the
entry URL, a host gate and the detector's field selectors.

- **Host gate.** `sparkasse.at` and its subdomains. The gate decides which
  frames get DOM snapshots and the in-page detector. The HAR and the
  network log record every host regardless. A load-bearing frame on
  another host (an Erste group domain, a card processor) widens the gate
  only once a capture shows it.
- **No pre-fill.** The shared pre-fill fills a username and a password
  together, in one frame. The George form is expected to ask for a user
  name only (§A), so the sign-in is typed by hand over VNC. The harness
  still reads `ERSTEBANK_AT_USERNAME`: it builds the redactor, so no
  capture holds the user name in clear.
- **The detector** reports a user-name field, a password field (which
  would refute §A), and any one-time-code or TAN field (a fallback
  factor).
- `--trace` stays off by default: a trace stores every input's value
  and cannot be redacted afterwards.

## 3. The discovery walk

An `explore` session walks four flows over VNC. Each is one continuous
segment.

1. **Sign-in + s Identity.** The user-name form, the push wait screen,
   any comparison code it shows, and its completion. A second short run
   on the same profile (without `--fresh`) shows what the profile
   remembers (§F).
2. **Checking accounts.** Each account's transaction list: its paging,
   its date filters, and how far back it reaches (§C). One transaction's
   detail. Each export format once (§D).
3. **Statements.** The statement archive: its listing, one PDF download,
   and its reach (§E).
4. **Credit cards.** Each card's overview, transactions and statements,
   and whether any of them leaves George for another site (§G).

## 3-Surface — the source's behaviour

Endpoints recorded here are masked (`<IBAN>` and `<id>` stand for elided
values). Account numbers, user names, balances and the roster live only
in the debug dir, never here.

### §A — Login form *(unverified)*

- The form asks for a user name only (Verfügernummer or a chosen alias).
  There is no password field. The second factor completes the sign-in.
- Open: the login host (`george.sparkasse.at` itself or a separate
  identity host), the form's stable hooks (ids, `name`, test-ids), and
  whether a "remember user name" control exists.
- Open: the web technology (framework, design-system components, shadow
  DOM or not).

### §B — The s Identity challenge *(unverified)*

- The factor is a push approval in the s Identity app. The browser may
  show a comparison code to match against the app.
- Open: the network shape of the challenge. Which request starts it,
  which response carries its lifetime, and what signals completion (a
  poll, a redirect, a websocket).
- Open: where the comparison code sits in the DOM, and its stable hook.
  The code is read from the DOM, never from a network event.
- Open: whether a fallback factor exists (SMS TAN, card reader), and how
  the screen offers it.
- Open: the provider's own error text for a declined or expired push.

### §C — The data API and transaction history *(unverified)*

- George is an SPA over JSON APIs. No public API is assumed.
- Open: the API hosts and paths, and how requests authenticate (Bearer
  token, cookies, CSRF header).
- Open: the account roster endpoint, and whether it enumerates every
  account or only what the overview tiles show.
- Open: the transaction rows. A stable row id, booking and value dates,
  a signed amount, counterparty fields, a running balance or not.
- Open: pagination (page number, offset, keyset) and the history floor:
  how far back the list reaches, and whether that is a server-side cap.
- Open: pending transactions, and how a pending row becomes booked.

### §D — Exports *(unverified)*

- George is expected to export transactions as CSV and/or JSON.
- Open: each format's columns, whether it carries the stable row id and
  a running balance, its date window, and whether it is built on the
  server or in the page from the same JSON the SPA reads. Each format is
  compared against the SPA's own JSON before a ledger source is chosen.

### §E — Statements *(unverified)*

- Statements (Kontoauszüge) are expected as a PDF archive.
- Open: the listing endpoint, the document's stable id, the download
  call, and whether statements are pre-generated or built on request.
- Open: the archive's reach (the statement floor), measured against the
  transaction-history floor (§C).

### §F — Session persistence and device trust *(unverified)*

- Unknown. The measurement: close the browser after a signed-in explore,
  then run a second short explore on the same profile, without
  `--fresh`.
  - Does the session survive the restart?
  - Does the push fire again?
- This one measurement decides the verb split (§4): a push at every
  sign-in folds `login` into `download`; durable trust gives a separate
  `login` verb.

### §G — Credit cards *(unverified)*

- Open: whether card transactions and card statements are served by
  George itself or by a separate card portal. A separate portal is out
  of scope for this build: it is recorded here, never followed.
- Open: the card account's fields (balance, limit, available credit,
  statement cycle and due date) and the sign of its amounts.

### §H — Still open

What the first explore must answer, in order of what it decides:

1. Bot defense: which vendor, if any, and **which endpoints carry the
   sensor headers**. That decides browser-for-login-only versus
   browser-everywhere.
2. The login form (§A) and the s Identity challenge with its completion
   signal (§B).
3. Session persistence and device trust across a browser restart (§F).
4. The roster, history depth and pagination (§C).
5. Each export format against the SPA's JSON (§D).
6. The statement listing and its reach versus the history floor (§E).
   This decides whether a statement backfill phase exists.
7. Where card data lives (§G).

## 4. Runtime

Built from the explore captures. The verb split (§F) and the runtime tier
(browser everywhere, browser-for-login + REST, or no browser) are
proposed from the evidence before `login` and `download` are written.
Until then only `explore` and `prune` run. `login`, `download` and
`load` are accepted by the wrapper and exit 2.
