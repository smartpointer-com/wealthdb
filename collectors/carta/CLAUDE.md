# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The carta-specific surface below applies on
top of those shared rules.

This collector is **implemented and verified end-to-end** (2026-06-08):
`explore` / `login` / `download` / `load` all work. It **pivoted from an API
design to a web scraper**: Carta has a real `/v1alpha1/` REST API, but API
access is invite-gated and (as of 2025) also requires SOC 2 Type 2 for
third-party partners; the "customers can consume their own data" carve-out
is for *companies and investment firms*, not individual shareholders. An
individual holder cannot self-provision credentials, so we drive the holder
web UI (Camoufox) and replay its internal cookie-session REST/JSON API
read-only. See [DESIGN.md](DESIGN.md). The allow/forbid surface below is
binding.

## 1. Read-only Carta holder access — never trigger writes

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. Carta is
especially dangerous here: the holder UI exposes controls that **exercise
options, sell shares, and move money**, and a single click can create an
irreversible transaction and a taxable event.

Allowed — once mapped, only these surfaces may be navigated or clicked:

- The Carta login form and the 2FA challenge page that follows it (a
  6-digit code entered into a single `#two-factor-code-input` field).
- Read-only holder reporting surfaces: the portfolio / holdings overview,
  per-company (issuer) holdings, and security-detail pages — option grants
  (with strike, vesting schedule, ISO/NSO, exercised/outstanding/vested
  quantities), RSUs, RSAs, share certificates (common/preferred), and
  SAFEs / convertible notes; the per-company 409A fair-market-value display;
  the transactions / activity history (exercises, share sales, RSU
  settlements); and the documents / tax centre (read + download of legal
  docs, 3921s, 1099-Bs, board consents).
- Date-range / period filter inputs and "Apply" / "Search" buttons on the
  above pages.
- Export / download buttons that produce CSV / XLS / PDF copies of
  already-displayed data, and document-download endpoints reachable via the
  session cookie (fetched through Playwright's request API).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- **Exercise / liquidity flows** — exercising options (cash, cashless, or
  exercise-and-sell), accepting/declining a grant or stock-plan, filing an
  83(b) election, participating in a secondary sale / tender offer /
  company-sponsored liquidity program, or selling/transferring any
  security. Any "Exercise", "Accept", "Sell", "Transfer", "Participate",
  "Enroll" control.
- **Funding / tax mutations** — adding or editing a linked bank account,
  wire instructions, or exercise-financing method; setting or changing a
  tax-withholding election; submitting a W-9 / W-8 or other tax form.
- **Account / profile mutations** — email, password, 2FA factor,
  beneficiary, address, accreditation / KYC, notification settings.
- **The issuer / company-admin console.** A Carta login can carry more than
  the holder role — the same person may also administer a company's cap
  table (issuer admin), or be a fund admin/LP. Any "Company" / "Cap table" /
  "Issue securities" / "Manage equity" / "409A" / "Board" / "Fund admin"
  console is **out of scope** — this collector observes the portfolio
  holder's own *holdings* only. Treat admin/issuer surfaces as forbidden even if the
  login surfaces them; never use an elevated role to read or change
  anything.
- **Messaging / support** — contacting the company, document-request forms,
  e-signing anything.
- Any "confirm" / "submit" / "sign" / "save" / "send" button outside the
  login + 2FA forms themselves.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the login +
  2FA forms, the read-only filter Apply actions, and explicit export /
  document-download triggers.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session

The Carta session cookie is the keys to the kingdom (it can exercise
options and move money via the flows above). Its lifetime and any
device-trust behaviour are **TBD pending `explore`** — do not assume it
persists, and do not invalidate it without cause:

- Don't add a routine "fresh login per N runs" pattern — re-mint only when
  the session genuinely expires. Each fresh login fires a real 2FA
  challenge to the user's device.
- Don't log out programmatically at the end of `download`.
- `/secrets/carta-profile/` is the persistent browser profile dir (Camoufox
  or vanilla Firefox). It holds the session cookie and any device-trust
  value. Keep it at 0700; treat the whole dir as a credential.

## 3. login must not weaken authentication

Per root [CLAUDE.md](../../CLAUDE.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA to simplify the flow; never add a `--password`
flag (credentials arrive via env only); never persist a password to disk.
If a future Carta change breaks the browser flow, fix the selectors / waits
— do not reach for an auth shortcut. Carta is likely fronted by Akamai-style
bot detection (cf. [fidelity-web](../fidelity-web/)); the answer to a
challenge is a better stealth profile in `explore`, never an auth bypass.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and §4
(no private information in source). They apply in full here.

This collector's PII surface is unusually sensitive. Carta data names the
**private companies** in the book and exact share
counts, strike prices, 409A fair-market-values, vesting schedules, and
tax-document figures (3921 / 1099-B). The **company name alone is
identifying**. Never copy a real company / issuer / portfolio name or id,
share count, strike, FMV, or document into a tracked file (source,
fixtures, comments, commit messages). Synthetic placeholders only — e.g.
issuer "ACME-CO", `portfolioId "pf_EXAMPLE"`, round example figures. Raw
artefacts live only under `~/wealthdb/carta/` and `~/.secrets/`, never in
the repo.
