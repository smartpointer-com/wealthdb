# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The ubs-web-specific surface below
applies on top of those shared rules.

## 1. Read-only UBS netbanking access — never trigger write actions

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
concrete surface for ubs-web:

Allowed UI surfaces — `download.py` may only navigate to or click
within (final list TBD once the live UI is mapped, but the
allow-list pattern below is binding):

- The UBS login form and the MFA approval page that follows it.
- Read-only listing pages for: account overview, transactions,
  custody/portfolio holdings, eDocuments archive.
- Date-range / period filter inputs and "Apply" / "Search" buttons
  on the above pages.
- Export / download buttons that produce CSV, XLS, or PDF copies
  of already-displayed data.
- Document download endpoints reachable via the session cookie
  (typically REST endpoints fetched via Playwright's request API
  rather than per-row link clicks).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Payment / transfer entry (`Payments`, `New Transfer`, `eBill`,
  IBAN entry, beneficiary management).
- Trade entry / order forms (`Trade`, `Buy/Sell`, `New Order`).
- Card management (block, unblock, request, change limits).
- Settings pages that mutate account state (notification prefs,
  trading limits, MFA factor management, contact details).
- Any "confirm" or "submit" button outside the login form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
