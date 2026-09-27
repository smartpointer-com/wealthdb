# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The cointracking-specific surface below
applies on top of those shared rules.

## 1. Read-only cointracking.info access — never trigger write actions

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
concrete surfaces the collector reads are enumerated in DESIGN.md
("Page surfaces and endpoints"); the allow-list pattern below is the
binding read-only policy regardless of which specific URLs a later
UI change relocates.

Allowed — only these surfaces may be navigated or clicked:

- The cointracking.info login form and the Duo / Google
  Authenticator 2FA challenge page that follows it.
- Read-only listing pages for: consolidated portfolio,
  per-exchange / per-wallet positions, transaction history,
  realised gains, tax-report views, balance history.
- Date-range / period filter inputs and "Apply" / "Search"
  buttons on the above pages.
- Export / download buttons that produce CSV, XLS, JSON, or PDF
  copies of already-displayed data.
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- **Manual transaction entry** (`Enter Coins`, `New transaction`,
  any form that adds, edits, or deletes a transaction record).
- **Import management** (`Add new exchange`, `New import`, any
  exchange-API / CSV-upload / wallet-watch configuration).
- **Address book / tag management** (renaming exchanges,
  re-categorising transactions, editing notes).
- **Account / profile mutations** (email, password, 2FA factor,
  API key, billing settings).
- **Tax-method changes** (FIFO / LIFO / specific-lot selection,
  jurisdiction toggles — these change historical tax outputs).
- **Delete / clear** anything (`Delete all transactions`, `Reset
  imports`, archive operations).
- Any "confirm" / "submit" / "save" / "delete" button outside the
  login form itself.
- Anything that performs a `POST` / `PUT` / `DELETE` other than
  the login form, the read-only filter Apply actions, and
  explicit export-generation triggers.

The cointracking portal also exposes a paid-tier upsell + community
features (forum, comments). Those are out of scope — don't
navigate there, don't trigger anything that touches them.

When in doubt during `explore`, treat a surface as forbidden until
it appears in the allow-list above.

## 2. Long-lived session — protect it

The cointracking session cookie is multi-year.
That means a single MFA challenge bootstraps the collector for the
foreseeable future. **Do not** invalidate it without cause:

- Don't add a routine "fresh login per N runs" pattern — the
  collector should only re-mint when the cookie genuinely expires.
- Don't log out programmatically at the end of `download` — the
  cost of an unnecessary fresh login is a real 2FA push to the
  user's phone.
- `/secrets/cointracking-profile/` is the Playwright Firefox
  persistent profile dir. **Single profile dir shared across
  `login`, `download`, and `explore`.** Holds session cookies
  including the multi-year `ctfa<user_id>` device-trust value. Keep
  it at 0700; treat the whole dir as a credential.

## 3. login is headless Playwright Firefox, CLI-MFA

`login.py` drives vanilla Playwright Firefox (`headless=True`,
no Camoufox stealth, no Xvfb, no VNC) through the SPA login
form, then prompts for the 6-digit 2FA code on stdin. The
browser runs cointracking's JS-side password encryption for
free; the persistent profile dir captures the session.

Pure HTTP was attempted and abandoned: POST 1's `password_login`
is a session-encrypted blob (not a hash), and reverse-engineering
the encryption was deemed too fragile vs. just running a real
browser. See DESIGN.md.

This means **login.py must not bypass or downgrade any auth
control** to make HTTP-only work — if a future cointracking
change breaks the headless-Firefox flow, the right response is to
fix the selectors / wait conditions, NOT to reach for HTTP
shortcuts that skip JS-derived state.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.

cointracking aggregates EVERY user crypto position and transaction
across multiple sources — so the PII surface in test fixtures /
example commits is larger than for any single-source collector.
Synthetic addresses + ticker examples only; never copy a real
wallet address, exchange account label, or transaction hash into
tracked files.
