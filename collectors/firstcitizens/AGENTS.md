# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The firstcitizens-specific surface below
applies on top of those shared rules.

Phase 1 (discovery) is done — the 2026-08-13 `explore` capture mapped
the surface (DESIGN.md §3): a **Q2 `mobilews` REST/JSON API** at
`digitalbanking.firstcitizens.com/FCBTCOnline/`. The allow/forbid
surface below is now stated against the observed endpoints and still
errs wide on the forbid side where a surface is unmapped. Tighten the
allow list to concrete routes as more traces land; never widen it
without the user opting in in writing. Mapping a new endpoint does not
widen scope.

The runtime is a **Camoufox `login` + `download`, REST data path**
(DESIGN.md §4). The intended host-venv hybrid is ruled out by **Akamai
Bot Manager**, which gates `preLogonUser`/`logonUser` with
browser-generated sensor headers a plain host client cannot forge — so
the logon runs in Camoufox every run. `login` (interactive) registers
the device; `download` (unattended, trusted device skips 2FA) then
fetches the deposit data over `page.request` REST — no DOM scraping.

## 0. Never run a real session unprompted

Root [AGENTS.md](../../AGENTS.md) §2 applies with extra force here:
`explore`, `login`, and `download` all mint a real firstcitizens.com
session (and `login` fires a real 2FA challenge at a real device), and
repeated automated logins can trip fraud heuristics into a lock-out (the
Chase sibling soft-blocked after ~6 rapid logins in one day). Allowed
without asking: reading code/config/docs, `make build-firstcitizens` /
`make test-firstcitizens`, `--help`, unit tests, and `login --check`
(reads the trusted-device state; sends no code, fires no MFA). Not
allowed unless explicitly asked: anything that touches the live site — a
real `explore` / `login` / `download`, or "just checking the selectors"
against the live login page.

## 0a. The login browser surface — auth only, never act on the account

The browser exists solely to complete the logon: `login` / `download`
may pre-fill and submit the sign-in form, and `login` may drive the
access-code challenge. **The deposit-account data is fetched over REST**
(`page.request` against the `mobilews` endpoints listed below), not by
navigating or clicking account pages. Do not navigate to, click, or
script any in-app account surface (money movement, cards, settings)
reachable from the signed-in session.

## 1. Read-only First Citizens retail access — never trigger writes

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
First Citizens retail UI is especially dangerous: money movement (Zelle,
transfers, bill pay) sits one or two clicks from the account overview,
and a single submit can move real money irreversibly.

Allowed — only these surfaces / endpoints may be driven (all read-only
except the login sequence itself):

- The login sequence and 2FA: `mobilews/preLogonUser`,
  `mobilews/logonUser`, `mobilews/accessCode`,
  `mobilews/accessCode/validate`, `mobilews/registerDevice` (the
  "remember this device" step), and the session `keepalive`.
- The **deposit accounts** (checking + savings): the roster
  (`mobilews/accounts`, `v2/pfm/accounts`), per-account detail
  (`mobilews/account/<id>`), and transaction history
  (`mobilews/accountHistory/<id>`).
- Statements: list (`mobilews/accountStatement/<id>`,
  `mobilews/accountStatement/form`) and PDF fetch
  (`mobilews/accountStatement/<id>/<docId>/pdf`).
- Transaction exports (read-only copies of already-authorized data):
  `mobilews/accountExport/<id>/<Format>` for
  `Format ∈ {Csv, Xls, Ofx, QFX_1_0_2, Qbo}`.
- Logout (optional; not required between runs, but harmless).

These are the only `POST`s permitted besides login: the export and
statement-PDF triggers, which return copies of already-displayed data
and mutate nothing.

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money** — Zelle, internal and external
  transfers, ACH, wires, bill pay, check deposit, payment requests,
  loan / card payments. Any "Pay", "Transfer", "Send", "Deposit",
  "Schedule" control.
- **Card surfaces — read *and* write.** Scope is **deposit-only**, so
  any credit- or debit-card products the login exposes are out of scope
  entirely. Card *management* — lock/unlock, replacement, PIN, limits,
  travel notices, digital-wallet enrolment — is forbidden on top of
  that. (Reading cards would be a deliberate future scope expansion,
  not something to opt into unprompted.)
- **Account lifecycle & offers** — opening or closing accounts, product
  offers / upgrades, overdraft elections, linked / external account
  setup.
- **Profile / settings mutations** — contact details, password, 2FA
  factors, alerts, paperless / statement-delivery settings, privacy or
  data-sharing grants (including any third-party-access / open-banking
  consent surface — that is the aggregator gateway, and granting it
  would expose data to an external party).
- **The secure message center** — composing or sending anything.
- **Any wealth-management, trust, or brokerage surface** the login may
  also expose (First Citizens Wealth, investment or advisory consoles,
  trust reporting). This collector observes the retail deposit
  relationship only.
- Any "confirm" / "submit" / "send" / "save" control outside the
  sign-in + 2FA forms themselves.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the
  sign-in + 2FA forms and explicit read-only export / document-download
  triggers.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session & the device-trust cookie

The First Citizens session cookie is the keys to the kingdom (it can
move money via the flows above).

- `/secrets/firstcitizens-profile/` is the persistent Camoufox profile
  dir. It holds the session cookie and any "remember this device"
  state. Keep it at 0700; treat the whole dir as a credential.
- Whether device-trust persists across runs — and for how long — is
  exactly what Phase 1 measures (DESIGN.md §3, flow 1). Don't assume
  it, and don't invalidate it without cause: `--fresh` wipes the
  profile deliberately (to capture the full 2FA flow); never add a
  routine fresh-login pattern.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Per root [AGENTS.md](../../AGENTS.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA; credentials arrive via env only
(`FIRSTCITIZENS_USERNAME` / `FIRSTCITIZENS_PASSWORD` from
`~/.secrets/firstcitizens.env`) — never a `--password` flag, never
persisted; any OTP is read from stdin (or typed by hand over VNC),
never accepted on argv. The bot-defense stack is unmapped; if the site
challenges the browser, the answer is a better stealth profile in
`explore`, never an auth bypass.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.

First Citizens PII: account and routing numbers, balances, transaction
payees and amounts, statement PDFs, names and addresses — **and the
account roster itself** (which accounts/products the login holds, their
number or type, or that a product is absent; see root
[AGENTS.md](../../AGENTS.md) §4). None of it enters tracked files
(source, fixtures, comments, commit messages) — synthetic placeholders
and round figures only. Describe scope as the account *kinds* handled
("deposit accounts: checking + savings; cards out of scope"), never
what this login was seen to contain. The explore debug dir
(`~/.cache/wealthdb/debug/firstcitizens/`) carries full response bodies
and downloaded documents; the harness redacts the username + password
from `network.jsonl`, but everything else in there is real account data
— treat the dir as sensitive and never commit anything derived from it
without stripping identifiers first.
