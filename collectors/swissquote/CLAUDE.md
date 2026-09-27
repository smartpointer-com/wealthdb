# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The swissquote-specific surface below
applies on top of those shared rules.

## 1. Read-only Swissquote access — never trigger write actions

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
concrete surface for swissquote:

Allowed UI surfaces — these are the only pages `download.py` may
navigate to or click within:
- F5 BIG-IP login form at `/my.policy` and the Mobile Level 3 MFA
  page that follows it.
- Trading Platform SPA `#transactions` route — date-range filter
  inputs and the export dropdown only.
- Trading Platform SPA `#portfoliooverview` route — the three
  export buttons (Positions, List of Assets, Export account
  overview), and hovering the per-position symbol cells to expose
  the long-name tooltip. The Buy/Sell buttons inside position rows
  are present in the DOM but must never be clicked.
- eBanking SPA `#documents` route — date-range filter and Apply
  only; document PDFs are fetched via Playwright's request API
  (the `getPdfDocument` REST endpoint with the session cookie),
  not by clicking download links.
- eBanking SPA root (`/sqc-web-client-portal/`) used by
  `login.py --check` to test session liveness via URL transition. F5 may
  redirect that request on to the Trading Platform
  (`/eding_trading-platform/`); both count as authenticated, and neither
  is read as such when opened directly.

Permitted client-side DOM cleanup (not a write action): removing
Pendo in-app-guide overlay nodes (`#pendo-base` / `._pendo-backdrop`
/ `_pendo-*`) that intercept pointer events and block export clicks.
This only deletes overlay DOM and calls Pendo's own `stopGuides()`;
it submits nothing. See `download.py::dismiss_guide_overlays`.

Forbidden — do not navigate to, click, or scrape:
- Trade entry forms (`Trade`, `Buy/Sell`, `Quote`, order-book widgets).
- Payment / transfer forms (`Payments`, `Withdraw`, IBAN entry).
- Settings pages that mutate account state (notification prefs,
  trading limits, MFA factor management).
- Anything that performs a `POST` other than the login form itself.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
