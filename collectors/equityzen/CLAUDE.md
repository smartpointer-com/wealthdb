# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The equityzen-specific surface below applies
on top of those shared rules.

This collector is **implemented** — `explore`, `login`, `download`, and
`load` all work end-to-end (against the live portal / real bronze), and the
gold adapter is registered with the gold engine; enabling the source in a
gold run is operator config (a `wealthdb.cfg` `silver_sources` entry). The
allow/forbid surface below is mapped against real traces, not guesses. EquityZen is a
read-only target reached by replaying the buyer's browser session and
reading the SPA's own investor-scoped GraphQL API responses
(`POST /api/graphql/`); see [DESIGN.md](DESIGN.md). Note `download` captures
the list by **clicking the SPA's stage tabs** and reading the responses —
it does not replay the query through the endpoint (that returns a
server-side SYSTEM_ERROR), and it never sends a write mutation. `download
--documents` additionally fetches document PDF blobs via authenticated
`GET` on `node.documents[].downloadUrl` (read-only); `load` parses the
capital-account statements + K-1s locally (`statements.py` → `pdftotext`).
Those PDFs carry names / addresses / SSN-EIN fragments and exact figures —
they live only under `$XDG_DATA_HOME/wealthdb/equityzen/` (bronze) and in the silver DB,
NEVER the repo, and the full K-1 text is deliberately not stored.

## 1. Read-only EquityZen buyer access — never trigger writes

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. EquityZen
is a **live secondary marketplace** — a single click or mutation can place
or reserve an order, list shares for sale, or move money, creating an
irreversible transaction and a taxable event. Treat it as especially
dangerous.

Allowed — only these surfaces may be navigated, clicked, or queried:

- The `/accounts/login/` form and the TOTP 2FA step that follows it
  (the `submitLogIn` + `loginTotp` GraphQL mutations are the only
  mutations this collector ever issues).
- Read-only buyer reporting GraphQL **queries**: `getBuyerInvestments`
  (offerings/positions list), `getMyInvestmentDetails` (per-offering
  basis / FMV / status / distributions), `getEquityDetails` (share-lot
  detail), `getBuyerDocuments` + `getK1EquivalentDocuments` +
  `getBuyerInfoForDocuments` + `getTaxCompletedDocumentsYearsEzConstant`
  (document / tax centre).
- The corresponding read-only pages: `/welcome/`, `/portfolio/`,
  `/portfolio/<dealId>/`, `/equity/<uuid>/`, `/documents/`.
- Document-download endpoints (K-1-equivalent / statement PDFs + zip
  bundles) reachable via the session cookie, fetched through Playwright's
  authenticated request API.
- Logout (optional; not required between runs — and see §2, don't).

Forbidden — do not navigate to, click, query, or mutate:

- **Order / liquidity flows** — placing or reserving a buy
  (`getReserveInvestmentDeal` and any reserve/commit mutation), listing or
  selling via Express Deal / a sell order, accepting an allocation, or
  e-signing a subscription / side-letter. Any "Invest" / "Place Order" /
  "Reserve" / "Express Deal" / "Sell" / "Accept" / "Fund" control.
- **IOI / watchlist / discovery writes** — expressing an indication of
  interest, adding to a watchlist, or any marketplace-discovery mutation
  (`getBuyerIoiData`, `getWatchlistIois`, `companiesWithIoiInfo`,
  `getInvOpps`, `searchGlobal`, `getCarousels` are discovery surfaces —
  do not drive them, and never their write counterparts).
- **Telemetry mutations** — the SPA auto-fires `createPVR` (a page-view
  record) and `LogClientMetric`. `download.py` must **not** call these;
  issue only the read queries + the two auth mutations in the Allowed
  list.
- **Funding / banking mutations** — adding or editing a linked bank
  account or wire instructions (`fundBankAccount` data is read-only
  context, never a write target), initiating a payment, distribution-
  election changes.
- **Account / profile mutations** — email, password, TOTP factor,
  accreditation / KYC, tax forms (W-9/W-8) submission, notification
  settings.
- Any "confirm" / "submit" / "sign" / "save" / "send" control outside the
  login + TOTP forms themselves.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the
  `submitLogIn` / `loginTotp` mutations and explicit document-download
  triggers. (Note: the GraphQL endpoint is `POST` for *reads* too — the
  read/write line is the **operation name**, not the HTTP verb. Only the
  operations in the Allowed list may be sent.)

When in doubt, treat a surface as forbidden until it appears in the
Allowed list above.

## 2. Protect the session

The buyer session cookie is the keys to the kingdom (it can place orders
and move money via the flows above). EquityZen 2FA is **TOTP-only** with
**no "remember this device" checkbox**, but closing the browser does not
log the account out — so the session cookie carried in the profile dir
stays valid across runs until an explicit logout or a server-side expiry.

- Don't add a routine "fresh login per N runs" pattern — re-mint only when
  the session genuinely expires. Each fresh login fires a real TOTP
  challenge to the user's authenticator.
- Don't log out programmatically at the end of any verb.
- `/secrets/equityzen-profile/` is the persistent browser profile dir
  (Camoufox or vanilla Firefox), shared by `explore` / `login` /
  `download`. It holds the session cookie. Keep it at 0700; treat the
  whole dir as a credential.

## 3. login must not weaken authentication

Per root [CLAUDE.md](../../CLAUDE.md) §3: never bypass, downgrade, or
"temporarily disable" TOTP to simplify the flow; never add a `--password`
flag (credentials arrive via env only — `EQUITYZEN_PASSWORD`, with the
login id in `EQUITYZEN_USERNAME` or its alias `EQUITYZEN_EMAIL`); never
persist a password to disk. If a future EquityZen change breaks the
browser flow, fix the selectors / waits — do not reach for an auth
shortcut. EquityZen is likely fronted by Cloudflare/Akamai-style bot
detection (cf. [fidelity-web](../fidelity-web/)); the answer to a
challenge is a better stealth profile in `explore`, never an auth bypass.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and §4
(no private information in source). They apply in full here.

This collector's PII surface is unusually sensitive. EquityZen data names
the **private pre-IPO companies** in the book, plus the SPV/fund names,
exact share counts, basis and fair-market-value figures, and K-1-equivalent
tax-document contents (legal name, SSN/EIN fragments, per-vehicle dollar
amounts). The **company name alone is identifying**. Never copy a real
company / SPV / fund name or id, share count, basis, FMV, or document into
a tracked file (source, fixtures, comments, commit messages). Synthetic
placeholders only — e.g. company "ACME-CO", `dealId 1234`,
`equityBlockUuid "00000000-0000-0000-0000-000000000000"`, round example
figures. Raw artefacts live only under `$XDG_DATA_HOME/wealthdb/equityzen/`,
`~/.secrets/`, and the explore debug dir (`~/.cache/wealthdb/debug/equityzen/`),
never in the repo. The explore harness redacts the username + password
from `network.jsonl`, but response bodies there carry full holdings data —
treat the debug dir as sensitive and never commit anything derived from it
without stripping identifiers first.
