# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The chase-specific surface below applies on
top of those shared rules.

This collector's pipeline is **implemented through silver** — `explore`,
`login`, `download`, `load`, `prune` — and validated live end-to-end
(push and SMS 2FA, CSV/QFX export, statement PDFs). The allow/forbid
surface below still errs wide on the forbid side where a surface is
unmapped. Tighten the allow list to concrete routes / selectors as more
traces land; never widen it without the user opting in in writing. When
extending the browser code, keep to the allowed surfaces — mapping a new
selector does not widen scope.

## 0. Never run a real session unprompted

Root [CLAUDE.md](../../CLAUDE.md) §2 applies with extra force here:
`explore` is not a probe — it mints a real chase.com session and fires a
real 2FA challenge at a real device, and repeated automated logins can
trip Chase's fraud heuristics into a lock-out. Allowed without asking:
reading code/config/docs, `make build-chase` / `make test-chase`,
`./chase explore --help`, unit tests, and `login --check` (probes the
persisted profile read-only, no new MFA). Not allowed unless explicitly
asked: anything that mints a new session or fires a challenge — a full
`login`/`download`, or "just checking the selectors" against the live
login page.

## 1. Read-only Chase retail access — never trigger writes

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
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
  Scope is deposit-only; any credit-card products the same login exposes
  are out of scope — see the forbidden list and DESIGN.md §4.
- The statements & documents area: listing statements and other
  documents, opening / downloading statement PDFs.
- Export / download controls that produce copies of already-displayed
  transaction data (CSV / QFX / OFX expected — verify in traces).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money** — Pay & Transfer in all its forms:
  Zelle / QuickPay, internal and external transfers, ACH, wires, bill
  pay, check deposit, payment requests, loan / card payments. Any
  "Pay", "Transfer", "Send", "Deposit", "Schedule" control.
- **Credit-card surfaces — read *and* write.** Scope is **deposit-only**
  (DESIGN.md §4), so if the login exposes any credit-card products their
  activity, statements, and detail are all out of scope — stay off card
  surfaces entirely. Card *management* — lock/unlock, replacement,
  PIN, spending limits, travel notices, digital-wallet enrolment — is
  forbidden on top of that. (Reading cards would be a deliberate future
  scope expansion, not something to opt into unprompted.)
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
  the retail deposit relationship only.
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
- Whether Chase's device-trust persists across runs — and for how
  long — is exactly what Phase 1 measures (DESIGN.md §3, flow 1). Don't
  assume it, and don't invalidate it without cause: `--fresh` wipes the
  profile deliberately (to capture the full 2FA flow); never add a
  routine fresh-login pattern.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Per root [CLAUDE.md](../../CLAUDE.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA; credentials arrive via env only
(`CHASE_USERNAME` / `CHASE_PASSWORD` from `~/.secrets/chase.env`) —
never a `--password` flag, never persisted; the OTP is read from stdin
(or typed by hand in the `vnc-login` fallback), never accepted on argv. Chase's bot defense is Akamai-class (cf.
[fidelity-web](../fidelity-web/), [schwab-web](../schwab-web/)); the
answer to a challenge is a better stealth profile in `explore`, never an
auth bypass.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.

Chase PII: account and routing numbers, balances, transaction payees and
amounts, statement PDFs, names and addresses — **and the account roster
itself** (which accounts/products the login holds, their number or type,
or that a product is absent; see root [CLAUDE.md](../../CLAUDE.md) §4).
None of it enters tracked files (source, fixtures, comments, commit
messages) — synthetic placeholders and round figures only. Describe scope
as the account *kinds* handled ("deposit accounts: checking + savings;
cards out of scope"), never what this login was seen to contain. The explore debug dir
(`~/.cache/wealthdb/debug/chase/`) carries full response bodies and
downloaded statements; the harness redacts the username + password from
`network.jsonl`, but everything else in there is real account data —
treat the dir as sensitive and never commit anything derived from it
without stripping identifiers first.
