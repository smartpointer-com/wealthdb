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

## 1a. Donor-Advised Fund (Fidelity Charitable) surface

The DAF sits behind a distinct SPA on `charitablegift.fidelity.com`,
reached by an SSO hop that consumes the retail session cookie
(DESIGN.md §12). The `daf` phase drives its JSON REST API over
`page.request` rather than the DOM. The read-only contract applies
there in full, with DAF-specific stakes: on a DAF, "Recommend a
grant" and "Contribute" are the money-movement surfaces, and an
investment-pool "Exchange" reallocates assets.

Allowed — the SSO hop, the auth bootstrap, and read-only GETs under
`/fc-services/api/v1/` only:

- `CGFLogon.cgfdo` (the SSO hop) and `identity/self` / `self`
  (session-JWT + partyId bootstrap — the charitable-side analogue of
  login; the ONLY non-GET permitted here).
- The giving-account roster (`user/<partyId>/accounts`) and per-
  account master (`givingAccounts/<acctNbr>`).
- Investment pool positions (`poolBalances`) and exchanges
  (`poolExchange`).
- Grant, contribution, gift, and adjustment history
  (`transactionHistory/grants`, `.../contributions`, `gift`,
  `transactionHistory/adjustment`) and their `…/download` CSV twins.
- Document listing (`document`) and PDF fetch (`document/download`)
  for statements, grant/contribution confirmations, and tax forms.

Forbidden — never navigate into, click through, or call:

- Grant recommendation flows (`Recommend a grant`, `Grant`, charity
  search-to-grant funnels, recurring-grant setup) — this moves real
  money out of the fund, irreversibly.
- Contribution flows (`Contribute`, `Add funds`, asset transfers into
  the DAF) — money movement, even though it is "inbound".
- Investment pool exchanges / reallocation / model changes.
- Successor, advisor, or third-party access elections; profile,
  alerts, or settings mutations of any kind.
- Any "confirm" / "submit" / "review" button beyond read-only filter
  Apply and explicit export/download triggers.

When in doubt during `explore`, treat a DAF surface as forbidden
until it appears in the allow-list above; mapping a new endpoint does
not widen scope.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
