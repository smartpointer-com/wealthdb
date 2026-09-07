# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The amex-specific surface below applies on
top of those shared rules.

The surface is mapped and the pipeline is validated live through gold
([DESIGN.md](DESIGN.md) §A–§M): a React SPA on
`global.americanexpress.com` over a **`functions.americanexpress.com` BFF**
plus a **`/api/servicing/…` REST API**, behind cookie-borne Akamai Bot
Manager. The allow list below is stated against the observed endpoints and
still errs wide on the forbid side where a surface is unmapped. Tighten it
further as more traces land; never widen it without the user opting in in
writing. Mapping a new endpoint does not widen scope.

The runtime is a **Camoufox sign-in + REST data path** (§A): no data
endpoint carries a bot-defense sensor header, so once the browser holds the
session jar the data is fetched over `page.request` rather than by scraping
the SPA.

**`download` is the one verb that signs in** — `login` folded into it (§L)
because the sign-in budget is too small to spend one on a separate verb.
`download` answers a passcode from the terminal when it has a TTY,
registering the device while there; without one a challenge is a loud
failure, never a blocked prompt. `vnc-login` is the same walk behind a
hand-driven sign-in — the only way to answer a captcha. A bare `login` is a
no-op, and `login --check` reports device registration from the profile's
own cookie with no sign-in at all.

## 0. Never run a real session unprompted

Root [CLAUDE.md](../../CLAUDE.md) §2 applies with extra force here:
`explore` is not a probe — it mints a real americanexpress.com session and
can fire a real one-time passcode at a real device. **Amex's sign-in budget
is small and has already been hit**: about seven sign-ins inside twenty
minutes provoked a captcha (DESIGN.md §K), which no terminal can answer and
whose reputation decays only with idle time. That budget is why `login`
folded into `download` (§L): every run now costs ONE sign-in, not two.

Allowed without asking: reading code/config/docs, `make build-amex` /
`make test-amex`, `--help` on any verb, reading an existing capture under
the debug dir, unit tests, and `login --check` — which reads the profile's
own device-trust cookie and touches no network at all (§L).

**`download --dry-run` is NOT in that set here.** Unlike the fleet's usual
dry-run it still SIGNS IN — it only skips the exports and documents, and
still writes a run dir with its manifest and roster — and a sign-in is the
scarce thing on this source. Not allowed unless explicitly asked: anything
that signs in, which is a real `explore`, `vnc-login`, or ANY `download`
including `--dry-run`.

## 1. Read-only American Express card access — never trigger writes

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The Amex
account UI is especially dangerous: paying a bill, redeeming Membership
Rewards, enrolling a charge in Plan It, and sending money through Send &
Split all sit one or two clicks from the card overview, and several of
them are irreversible on submit.

Allowed — only these surfaces / endpoints may be driven (all read-only
except the login sequence itself):

- **The login sequence and its one-time-passcode challenge:**
  `POST global.americanexpress.com/myca/logon/us/action/login`, and on the
  `functions.americanexpress.com` BFF
  `ReadLegacyAuthenticationStatus.v1`, `ReadAuthenticationChallenges.v3`,
  `CreateOneTimePasscodeDelivery.v3`,
  `UpdateAuthenticationTokenWithChallenge.v3`,
  `ReadDeviceIdentityRegistrationChallenge.v1`,
  `CreateDeviceIdentityVerificationChallenge.v1` (its trusted-device
  counterpart), and `CreateIdentityTrustedDevice.v1` (the "remember this
  device" step), plus
  the `ReadUserSession.v1` / `UpdateUserSession.v1` session calls the SPA
  makes. Driving the sign-in form (`#eliloUserID`, `#eliloPassword`,
  `#rememberMe`, `#loginSubmit`) and the challenge controls
  (`challenge-options-list`, `otp-input-0…5`, `continue-button`,
  `resend-button`, the device-registration button) is part of this.
- **The credit and charge card accounts, read-only:** the roster
  (`ReadCustomerOverview.web.v2`) and the transaction history
  (`ReadAccountActivity.web.v1` / `.v1` in all its `view` modes, plus
  `ReadAccountTransactionDetails.web.v1`). What these carry — balances,
  credit limit and available credit, pay-over-time balance, due date,
  minimum payment, a rewards points *balance* — is read as displayed.
  Reading a card reads a liability; nothing here may submit, schedule,
  enrol, or change anything.
- **Statements and documents:** the `view: "STATEMENTS"` listing, the
  statement PDF fetch
  `GET /api/servicing/v1/documents/statements/<token>`, the year-end
  summary `GET /api/servicing/v2/financials/documents?fileFormat=pdf…`,
  and `GET /api/servicing/v{1,3}/financials/statement_periods`. The
  read-only `GET /api/servicing/v3/financials/interest_rates` and
  `…/financials/eligibilities` are allowed on the same footing.
- **Transaction exports** — read-only copies of already-authorized data:
  `GET /api/servicing/v1/financials/documents` for
  `file_format ∈ {csv, excel, quickbooks, quicken}`, with its
  `start_date`/`end_date` or `statement_end_date` window. Triggering such
  an export is a read.
- Logout (optional; not required between runs, but harmless).

These are the only `POST`s permitted besides the login sequence: none —
every data call above is a `GET`, apart from the BFF's read-only
`Read*` functions, which are `POST`-shaped reads carrying no mutation.
A `functions` call whose name does not begin with `Read` is forbidden
unless it is named in the login list above.

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money** — card payments one-off or scheduled,
  autopay enrolment or changes, Send & Split, bank-account linking or
  verification, balance transfers, cash advances, and any "Pay",
  "Transfer", "Send", "Schedule", "Confirm" control.
- **Membership Rewards redemption** in every form — redeeming for travel,
  statement credits, gift cards, merchandise, or checkout with points;
  transferring points to a partner programme. The points *balance* is a
  read; spending it is not.
- **Offers and enrolment** — Amex Offers ("Add to Card"), promotions,
  benefit or credit enrolment, subscription and membership sign-ups.
- **Plan It / pay-over-time** — creating, previewing-then-accepting, or
  cancelling a plan; opting a charge into any instalment or
  pay-over-time product.
- **Travel and booking surfaces** — Amex Travel, Fine Hotels + Resorts,
  trip booking, ticketing, and any reservation flow.
- **Account lifecycle & card management** — applying for or closing an
  account, replacing / freezing / unfreezing / cancelling a card,
  activating a card, PIN changes, adding or removing supplementary or
  authorized-user cards, credit-limit requests, digital-wallet enrolment,
  travel notices.
- **Disputes** — disputing or "reporting a problem with" a charge, and
  anything that opens a case.
- **Profile / settings mutations** — contact details, password, 2FA
  factors, alerts, paperless / statement-delivery settings, privacy or
  data-sharing grants (including any third-party-access / open-banking
  consent surface — that is the aggregator gateway, and granting it would
  expose data to an external party).
- **The message center** — composing or sending anything.
- **Any other product the same login may expose** — deposit / savings
  accounts, personal or business loans, business and corporate card
  programmes, expense-management surfaces, and any investment or trust
  console. This collector observes the credit and charge cards only.
- Any "confirm" / "submit" / "send" / "save" control outside the sign-in
  + 2FA forms themselves.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the
  sign-in + 2FA sequence, the BFF's read-only `Read*` functions, and
  explicit read-only export / document-download triggers. In particular
  every `Create*` / `Update*` / `Delete*` function other than the
  passcode and device-registration calls named in the allow list is
  forbidden.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session & the device-trust cookie

The Amex session cookie is the keys to the kingdom (it can pay bills and
redeem rewards via the flows above).

- `/secrets/amex-profile/` is the persistent Camoufox profile dir. It
  holds the session cookie and any "remember this device" state. Keep it
  at 0700; treat the whole dir as a credential.
- **The profile's `device-id` cookie is the device trust**, and it is what
  lets `download` run unattended: it survives a browser restart (~396-day
  `Max-Age`) and skips the passcode, while the session cookies die with the
  browser every time (DESIGN.md §G). Losing the profile costs a real
  passcode at the user's device, so don't invalidate it without cause:
  `download --fresh` — and `vnc-login --fresh`, which is the same code
  path — sets it aside deliberately (to exercise the untrusted-device
  flow) while `explore --fresh` deletes it outright; never add a routine
  fresh-login pattern.
- **Never persist or restore the session cookies** to skip a logon. They
  are `Discard`-scoped by the provider and the server holds its own state;
  working around that is the "no cleverness in auth flows" rule, and a
  password-only logon is unattended anyway.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Per root [CLAUDE.md](../../CLAUDE.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA; credentials arrive via env only
(`AMEX_USERNAME` / `AMEX_PASSWORD` from `~/.secrets/amex.env`) — never a
`--password` flag, never persisted; the one-time passcode is read from
stdin (or typed by hand over VNC), never accepted on argv. The bot-defense
stack is **Akamai Bot Manager** (DESIGN.md §A), as at the US siblings
([`chase`](../chase/), [`fidelity-web`](../fidelity-web/),
[`schwab-web`](../schwab-web/)); the answer to a challenge is a better
stealth profile in `explore`, never an auth bypass.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and §4
(no private information in source). They apply in full here.

Amex PII: card numbers and their last-4 masks, balances, credit limits,
transaction merchants and amounts, merchant categories as filed against
real rows, rewards balances, statement PDFs, names and addresses — **and
the account roster itself** (which cards or products the login holds,
their number or type, or that a product is absent; see root
[CLAUDE.md](../../CLAUDE.md) §4). None of it enters tracked files
(source, fixtures, comments, commit messages) — synthetic placeholders and
round figures only. Describe scope as the account *kinds* handled ("credit
and charge cards, read-only"), never what this login was seen to contain.
The debug tree (`~/.cache/wealthdb/debug/amex/`) carries `explore`'s full
response bodies, DOM snapshots and downloaded statements, plus the sign-in
diagnostics `download` and `vnc-login` write under `--debug`. The harness
masks the credentials in every wire spelling and blanks password fields
out of DOM snapshots (`collectorkit.debugcap`), but everything else in
there is real account data — treat the dir as sensitive and never commit
anything derived from it without stripping identifiers first. Two leaks in
the 2026-09-06 capture (a percent-encoded password in the network log, a
`value` attribute in a DOM snapshot) are what those two defences exist
for; if a capture predating them is still on disk, treat it as holding a
cleartext credential.
