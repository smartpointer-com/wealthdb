# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The schwab-web-specific surface below
applies on top of those shared rules.

## 1. Read-only Schwab web access — never trigger write actions

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
concrete surface for schwab-web:

Allowed UI surfaces — `download.py` may only navigate to or click
within (landmarks.py pins the concrete selectors; the allow-list
pattern below is binding):

- The Schwab login form and the MFA challenge page that follows
  it (SMS code, voice call, push, security questions — exact
  factor depends on the user's configuration).
- Read-only listing pages for: account summary, positions /
  holdings, transaction history, realised gains / cost basis,
  statements & tax forms archive.
- Date-range / period filter inputs and "Apply" / "Search" buttons
  on the above pages.
- Export / download buttons that produce CSV, XLS, or PDF copies
  of already-displayed data.
- Document download endpoints reachable via the session cookie
  (typically REST endpoints fetched via Playwright's request API
  rather than per-row link clicks).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Trade entry / order forms (`Trade`, `Buy/Sell`, `Options`,
  `Mutual Funds`, `Bonds`, any quote-to-order surface).
- Transfer / movement forms (`Transfers & Payments`, ACH, wire,
  internal-transfer, MoneyLink setup, bill pay, check request).
- Account opening / closing flows.
- Card management (debit cards, credit cards: block, unblock,
  request, change limits).
- Profile / settings pages that mutate account state (alert
  preferences, trading approvals, MFA factor management, contact
  details, beneficiary changes).
- Any "confirm" or "submit" button outside the login form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
