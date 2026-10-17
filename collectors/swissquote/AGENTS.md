# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The swissquote-specific surface below
applies on top of those shared rules.

## 1. Read-only Swissquote access — never trigger write actions

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
concrete surface for swissquote:

Allowed UI surfaces — these are the only pages `download.py` may
navigate to or click within:
- F5 BIG-IP login form at `/my.policy` and the Mobile Level 3 MFA
  page that follows it.
- Trading Platform SPA `#transactions` route — the date-range
  filter inputs, its Apply button, and the export dropdown only.
- Trading Platform SPA `#portfoliooverview` route — the three
  export buttons (Positions, List of Assets, Export account
  overview), clicking a collapsed widget's header to expand it,
  and hovering the per-position symbol cells to expose the
  long-name tooltip. The Buy/Sell buttons inside position rows
  are present in the DOM but must never be clicked.
- eBanking SPA `#accountOverview/main` route — read the account
  list (DOM scrape only).
- eBanking SPA `#documents` route — date-range filter and Apply
  only; document PDFs are fetched via Playwright's request API
  (the `getPdfDocument` REST endpoint with the session cookie),
  not by clicking download links.
- eBanking SPA root (`/sqc-web-client-portal/`) used by
  `login.py --check` and by `download.py`'s session check to test
  session liveness via URL transition. F5 may redirect that request
  on to the Trading Platform (`/eding_trading-platform/`); both count
  as authenticated, and neither is read as such when opened directly.
  `download.py` also opens it as the entry to `#accountOverview/main`
  and `#documents`.

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

## 2. Sessions and MFA

A fresh sign-in sends a **Mobile Level 3** push to the registered phone.
Credentials come from `SWISSQUOTE_USERNAME` and `SWISSQUOTE_PASSWORD` in
`~/.secrets/swissquote.env`. The session is a Playwright storage-state
file, `~/.secrets/swissquote-state.json`.

- `login --check` and `download --dry-run` reuse that state file and
  never sign in. With a dead session they fail and send no push. Both are
  in the set root [AGENTS.md](../../AGENTS.md) §2 allows without asking.
- A plain `login` sends a push, and a real `download` exports data. Run
  them only when explicitly asked.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
