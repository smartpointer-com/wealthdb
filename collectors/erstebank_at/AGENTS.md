# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The erstebank_at-specific surface below
applies on top of those shared rules.

This collector targets **George**, the retail e-banking portal of Erste
Bank und Sparkassen in Austria (`george.sparkasse.at`). The name is
namespaced to Austria because Erste runs a distinct George system in each
country. Scope is two account kinds: **checking accounts (Girokonto)** and
**credit cards**, as far as George itself serves them.

The surface is mapped in DESIGN.md §A–§G: an OAuth implicit-grant login
on `login.sparkasse.at` with a George app push, and a REST/JSON data API
on `api.sparkasse.at`. The allow list below is stated against the mapped
endpoints and errs wide on the forbid side. Never widen it without the
user opting in in writing. Mapping a new endpoint does not widen scope.

**The UI is entirely German.** Never key a selector on display text —
English *or* German — where an id / test-id / ARIA role exists. Where a
German label is load-bearing (no stable id), record it in DESIGN.md so
drift is diagnosable. Repo prose stays English.

## 0. Never run a real session unprompted

Root [AGENTS.md](../../AGENTS.md) §2 applies with extra force here.
`explore` mints a real George session and fires a real **push to the
George app** on a real phone. Repeated sign-ins can trip fraud heuristics
into a lock-out.

Allowed without asking:

- reading code, config and docs, and reading an existing capture under
  the debug dir;
- `make build-erstebank_at` / `make test-erstebank_at`, unit tests,
  `--help` on any verb, and a bare `login` (a no-op);
- `login --check`: it opens the sign-in page on the stored profile and
  never submits it, so no push fires. It reports DEAD between runs, by
  design;
- `prune` (host-side file walk; `--dry-run` first).

Not allowed unless explicitly asked: anything that touches the live site.
That covers a real `explore`, `download` or `vnc-login`, and "just
checking the selectors" against the live login page. No session survives
the browser, so every sign-in fires a push (DESIGN.md §F): `download`
signs in, and so does `download --dry-run`. Every live run is explicitly
requested with the user present. Never fire sign-ins in quick succession.

## 1. Read-only George access — never trigger writes

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. George is
especially dangerous: a new payment sits one click from every account
view, and a single submit plus a George app approval can move real money
irreversibly. The George app signs payments with the same push that
signs the login, so **any push other than the one the sign-in itself
fired must never be approved**. Navigation stays allow-list-tight.

Allowed — only these surfaces may be driven (all read-only except the
sign-in itself):

- **The sign-in and its push challenge** on `login.sparkasse.at`:
  `GET`/`POST /sts/oauth/authorize?…client_id=georgeclient&response_type=token`
  (the capability probe, the user-name form `form#credentials`, and the
  final redirect carrying the token), the push wait screen with its
  comparison code (`#verificationCode`) and cancel link (`#cancelLink`),
  and the poll `POST /sts/secapp/secondfactor?client_id=georgeclient`.
  The page's own sensor posts (DESIGN.md §A) ride along unprompted.
- **Checking accounts (Girokonto)**, read-only, on `api.sparkasse.at`:
  - roster: `GET /rest/netbanking/my/accounts` and
    `GET /rest/netbanking/my/accounts/<accountId>`;
  - history: `GET /proxy/g/api/my/transactions?id=<accountId>&page=…&pageSize=…`;
  - statements: `GET …/accounts/<accountId>/statements?page=…&size=…&sort=…&order=…`,
    `GET …/statements/current`, and the PDF
    `POST …/statements/download?format=…&statementIds=…&includeVoucher=…`
    in any of its formats (`PDF_A4`, `TXT_A4`, the other text layouts,
    `OFX`) — a read-only export trigger whose body carries only the
    bearer. Its first call for a format may answer `422` while the server
    renders the file; repeating it is the read the SPA itself makes.
- **Credit cards**, read-only, on `api.sparkasse.at`:
  - roster: `GET /rest/netbanking/my/cards` and
    `GET /rest/netbanking/my/cards/<cardId>` (credit-card rows only; a
    debit card is read only as the roster row it sits in);
  - history: the same transactions endpoint, keyed by the card account;
  - statements: `GET /proxy/g/api/my/accounts/<cardAccountId>-<cardId>/invoices?numberOfMonths=…`
    and the PDF `POST /rest/netbanking/my/cards/<cardId>/invoices/<billId>/pdf`
    (a read-only export trigger; its body carries only the bearer).
- Logout (optional; not required between runs, but harmless).

These are the only `POST`s the collector's own code may send: the
sign-in form, the push poll, and the two PDF downloads above.

The SPA fires calls outside this list **on its own** when its routes
load. That passive traffic is not scope widening, and the collector's own
code must never call those endpoints or navigate their surfaces. Seen so
far: `…/orders/payments`, `…/orders/payments/limits`,
`…/accounts/<accountId>/standingorders`, `…/contacts`, `…/plugins`,
`…/messages`, `…/securities`, `…/pensions`, `…/contracts/*`,
`…/instant-cash`, `…/watchdogs/*`, `…/cards/<cardId>/{trackings,authorized-cards,invoice-delivery}`,
`…/profile*`, `/rest/my/serviceRequests/*`, `/ebv-user/*`,
`cards-api.sparkasse.at/my/products/installment-plans`,
`sam.sparkasse.at` (electronic storage), `messaging.sparkasse.at`,
the `/proxy/ggw/*` dashboard, promotion and flag feeds, and
`POST /proxy/g/api/my/categorization/on-login`.

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money, in all its forms** — payments and
  transfers (domestic / SEPA / international / instant), standing orders
  (Daueraufträge), direct-debit mandates (Lastschrift / SEPA-Mandat),
  templates (Vorlagen), scheduled or batch payments, payment requests,
  QR / photo payments, transfers between own accounts, card repayment.
  Any control amounting to "Neue Zahlung", "Überweisung", "Auftrag",
  "Senden", "Unterschreiben", "Zeichnen", "Freigeben", "Bestätigen" —
  and any George app approval other than the sign-in's own.
- **Card lifecycle and card settings** — limits, blocks / unblocks,
  geo-blocking, PIN, activation, replacement, online-payment (3-D Secure)
  settings, digital-wallet enrolment, instalment or "pay later" options
  on a card charge. Reading a card's data is allowed; changing anything
  about the card is not.
- **Savings and securities** — any savings account (Sparen,
  Sparkonto, Kapitalsparbuch), Depot / Wertpapiere / Fonds / brokerage,
  pension or insurance products, Bausparen.
- **Loans and financing** — Kredite, overdraft (Überziehungsrahmen)
  changes, leasing, any credit offer.
- **Account lifecycle and offers** — opening or closing any account or
  product, product offers and upgrades, account switching services.
- **George plugins and the George store** — activating, buying or
  configuring any plugin.
- **Profile, settings and the second factor** — contact details, user
  name or alias, password, signature limits, notifications,
  statement-delivery settings, George app or George ID device pairing or
  method changes, and any privacy, data-sharing or third-party-access
  (open-banking) consent surface — granting it would expose data to an
  external party.
- **The Postfach (message center)** beyond reading statements — reading,
  composing or sending messages, and the advisor-contact surfaces. If
  statements are delivered there, only their listing and PDF fetch are
  allowed.
- Any "confirm" / "submit" / "send" / "save" control outside the sign-in
  flow itself.
- Anything that performs a `POST` / `PUT` / `PATCH` / `DELETE` other than
  the sign-in and its challenge, a read-only search or export trigger,
  and a statement download.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session and any device trust

The George session is the keys to the kingdom: it can move money through
the flows above, subject to a George app approval.

- `/secrets/erstebank_at-profile/` is the persistent Camoufox profile
  dir. It holds the session cookie and any browser or device-trust
  state. Keep it at 0700; treat the whole dir as a credential.
- `--fresh` wipes the profile deliberately, to force the unknown-device
  path. Never add a routine fresh-login pattern.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Per root [AGENTS.md](../../AGENTS.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA. Credentials arrive via env only, from
`~/.secrets/erstebank_at.env`: `ERSTEBANK_AT_USERNAME` (the George user
name). The form is expected to ask for nothing else. Should explore show
a password field, `ERSTEBANK_AT_PASSWORD` joins it — never a
`--password` flag, never persisted. The George app approval happens on
the phone; nothing code-like is ever accepted on argv. If the site
challenges the browser, the answer is a better stealth profile, never an
auth bypass.

## 4. Private data

Root [AGENTS.md](../../AGENTS.md) §4 applies in full. George PII covers:

- IBANs, account numbers, card numbers (even masked), the user name;
- balances, limits, transaction counterparties and amounts;
- statement PDFs and export files;
- names and addresses;
- **the account roster itself**: which accounts or cards the login
  holds, their number or type, or that a product is absent.

None of it enters tracked files (source, fixtures, comments, commit
messages). Use synthetic placeholders and round figures only. Synthetic
IBANs use the placeholder-letter pattern `AT<chk><BBBBB><RRRRRRRRRRR>`
with obviously fake digits. Describe scope as the account *kinds* handled
("checking accounts and credit cards"), never what this login was seen to
contain. German UI *labels* are not PII; recording load-bearing ones in
DESIGN.md is expected.

The explore debug dir (`~/.cache/wealthdb/debug/erstebank_at/`) carries
full response bodies and downloaded documents. The harness masks the user
name and session tokens
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)),
but everything else there is real account data. Treat the dir as
sensitive. Never commit anything derived from it without stripping
identifiers first.
