# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The fidelity-web-specific surface below
applies on top of those shared rules.

## 1. Read-only Fidelity web access — never trigger write actions

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
concrete surface for fidelity-web:

Allowed UI surfaces — `download.py` may only navigate to or click
within the surfaces listed below:

- The Fidelity login form and the MFA challenge page that follows
  it (Duo push, Symantec VIP code, Google Authenticator code, SMS,
  voice — exact factor depends on the user's configuration).
- Read-only listing pages for: account summary / Portfolio
  overview, positions / holdings, activity & orders / transaction
  history, realised gains / cost basis views, statements & tax
  forms / document center.
- Date-range / period filter inputs and "Apply" / "Search" /
  "Update" buttons on the above pages.
- Export / download buttons that produce CSV, XLS, or PDF copies
  of already-displayed data (the "Download" toolbars on
  Activity & Orders, Positions, and the document center).
- Document download endpoints reachable via the session cookie
  (typically REST endpoints fetched via Playwright's request API
  rather than per-row link clicks).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Trade entry / order forms (`Trade`, `Trade Stocks/ETFs`,
  `Trade Options`, `Trade Mutual Funds`, `Trade Bonds`, any
  quote-to-order surface).
- Transfer / movement forms (`Transfers`, `Bill Pay`, ACH, wire,
  internal account transfer, Bank Connection setup, deposit
  check, withdrawal request).
- Account opening / closing flows.
- Card management (Fidelity-branded debit / credit cards: block,
  unblock, request, change limits).
- Profile / settings pages that mutate account state (alert
  preferences, trading approvals, MFA factor management, contact
  details, beneficiary changes, third-party access grants).
- Anything related to the "Fidelity Access" third-party-app
  authorisation surface (this is the Akoya gateway; granting
  access there would expose data to an external party).
- Any "confirm" / "submit" / "place" button outside the login
  form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

A single Fidelity login may surface accounts of several
registrations, including accounts visible only through transitively
granted permissions. The read-only contract applies uniformly to
every account-type the login surfaces — do not use any elevated
permission the login may transitively grant for anything beyond
observation.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
