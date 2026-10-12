# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The chase-specific surface below applies on
top of those shared rules.

The collector verbs `explore`, `login`, `download`, `vnc-login`, `load`
and `prune` carry bronze → silver. The gold adapter in
`wealthdb/internal/silver/chase/` projects silver into canonical records.
Which paths the collector drives is in [README.md](README.md) § Status.

The allow/forbid surface below still errs wide on the forbid side where a
surface is unmapped. Tighten the allow list to concrete routes / selectors
as more traces land; never widen it without the user opting in in writing.
When extending the browser code, keep to the allowed surfaces. Mapping a
new selector does not widen scope.

**Card scope (opted into in writing):** credit-card **read** surfaces are
in scope alongside the deposit accounts. Card **management / write**
surfaces stay forbidden. See DESIGN.md §4 for the scope decision.

## 0. Never run a real session unprompted

Root [AGENTS.md](../../AGENTS.md) §2 applies with extra force here.
`explore` is not a probe: it mints a real chase.com session and fires a
real 2FA challenge at a real device. Repeated automated logins can trip
Chase's fraud heuristics into a lock-out.

- Allowed without asking: reading code/config/docs, `make build-chase` /
  `make test-chase`, `./chase explore --help`, unit tests, and
  `login --check` (probes the persisted profile read-only, no new MFA).
- Not allowed unless explicitly asked: anything that mints a new session or
  fires a challenge. That covers `download`, `vnc-login`, `explore`, and
  "just checking the selectors" against the live login page.
- `download --dry-run` **signs in**. No session survives between runs, so
  the dry run logs in and answers 2FA before its walk. It is a real
  session, not a probe.

## 1. Read-only Chase retail access — never trigger writes

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
Chase retail UI is especially dangerous: money movement (Zelle, wires,
bill pay) sits one or two clicks from the account overview, and a single
submit can move real money irreversibly.

Allowed — only these surfaces may be navigated or clicked:

- The chase.com sign-in form and the 2FA challenge that follows it
  (factor pick + code entry — push and SMS), including any "remember this
  device" control.
- Read-only surfaces for the **deposit accounts** (checking, and savings
  if present): the accounts overview, per-account detail, and the
  transaction history with its date-range / filter / search controls.
- Read-only surfaces for the **credit-card accounts**: the card roster on
  the accounts overview, per-card detail (current and statement balance,
  credit limit and available credit, payment due date, minimum payment,
  rewards balance), the card transaction history with its date-range /
  filter / search controls, and the card's statement listing plus
  statement-PDF download. Reading a card reads a liability — nothing on
  these surfaces may submit, schedule, enrol, or change anything; see the
  card-management entry in the forbidden list.
- The statements & documents area: listing statements and other
  documents, opening / downloading statement PDFs.
- Export / download controls that produce copies of already-displayed
  transaction data (the export form offers CSV, QFX, QIF and QBO).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money** — Pay & Transfer in all its forms:
  Zelle / QuickPay, internal and external transfers, ACH, wires, bill
  pay, check deposit, payment requests, loan / card payments. Any
  "Pay", "Transfer", "Send", "Deposit", "Schedule" control.
- **Credit-card management — every card write surface.** Card
  *management* — lock/unlock, replacement, PIN, spending limits, travel
  notices, digital-wallet enrolment — is forbidden, and with it every
  other control that mutates card state at the provider: card payments
  (one-off or scheduled) and autopay enrolment or changes, balance
  transfers and cash advances, disputes and transaction challenges,
  rewards redemption, credit-limit requests, and card
  statement-delivery settings. Card *reads* are in scope (see the allow
  list and DESIGN.md §4); changing anything about a card is not, and
  widening from read to write would need a fresh written opt-in.
- **Account lifecycle & offers** — opening or closing accounts, product
  offers / upgrades, overdraft elections, linked / external account
  setup.
- **Profile / settings mutations** — contact details, password, 2FA
  factors, alerts, paperless / statement-delivery settings, privacy or
  data-sharing grants (including any third-party-access / open-banking
  consent surface — that is the aggregator gateway, and granting it
  would expose data to an external party).
- **The secure message center** — composing or sending anything.
- **Any J.P. Morgan investment surface** the login may also expose
  (self-directed investing, advisory accounts). This collector observes
  the retail deposit and credit-card relationship only.
- Any "confirm" / "submit" / "send" / "save" control outside the
  sign-in + 2FA forms themselves.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the
  sign-in + 2FA forms and explicit read-only export / document-download
  triggers.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session & the device-trust cookie

The Chase session cookie is the keys to the kingdom (it can move money
via the flows above).

- `/secrets/chase-profile/` is the persistent Camoufox profile dir. It
  holds the session cookie and any "remember this device" state. Keep
  it at 0700; treat the whole dir as a credential.
- Device-trust does not buy a 2FA skip — every sign-in is challenged
  (DESIGN.md §F) — but the profile does hold a device token, and losing
  it narrows the factors Chase offers to push only (DESIGN.md §A). So don't
  invalidate it without cause: `--fresh` wipes the profile deliberately
  (to capture the first-login flow); never add a routine fresh-login
  pattern.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Root [AGENTS.md](../../AGENTS.md) §3 applies in full. The chase
specifics:

- Credentials arrive via env only: `CHASE_USERNAME` / `CHASE_PASSWORD`
  from `~/.secrets/chase.env`.
- The OTP is read from stdin, or typed by hand in the `vnc-login`
  fallback. It is never accepted on argv.
- Chase's bot defense is Akamai-class (cf. [fidelity-web](../fidelity-web/),
  [schwab-web](../schwab-web/)). The answer to a challenge is a better
  stealth profile in `explore`, never an auth bypass.

## 4. Private data

Root [AGENTS.md](../../AGENTS.md) §4 applies in full.

Chase PII: account and routing numbers, balances, transaction payees and
amounts, statement PDFs, names and addresses — **and the account roster
itself** (which accounts/products the login holds, their number or type,
or that a product is absent; see root [AGENTS.md](../../AGENTS.md) §4).
None of it enters tracked files (source, fixtures, comments, commit
messages) — synthetic placeholders and round figures only. Describe scope
as the account *kinds* handled ("deposit accounts: checking + savings;
credit cards read-only"), never what this login was seen to contain —
including how many cards or deposit accounts it exposes, or that a kind is
absent. Widening scope to cards does not license roster prose as
rationale.

The explore debug dir (`~/.cache/wealthdb/debug/chase/`) carries full
response bodies and downloaded statements. The harness masks the
credentials in what it writes, but an opt-in Playwright trace stays
unredacted ([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)),
and everything else in there is real account data. Treat the dir as
sensitive and never commit anything derived from it without stripping
identifiers first.
