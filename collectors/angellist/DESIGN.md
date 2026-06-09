# angellist — design notes

How the AngelList LP collector works, end to end. Implemented and proven
against a real account (June 2026): `byo-login` → `download` → `load` →
queryable SQLite silver.

## Why a separate collector

AngelList is the system-of-record for the private-market book: the
SPVs and fund deals held as a **limited partner** — illiquid,
single-issuer-per-vehicle interests with no public ticker. Same data
shape as the `viac` / `relevate` pension collectors, not the public-market
brokers. One collector per source keeps the silver isolated and the gold
reconciliation explicit.

## Access: why BYO-cookie, not API and not a headless login

Two dead ends, recorded so they aren't re-litigated:

1. **No LP API.** `docs.angellist.com` is the fund-administration / GP
   surface (org-gated GraphQL for *managing* investors). There is no
   LP-facing read API for one's own commitments/positions/distributions.
   The legacy `api.angel.co` REST API was the startup/talent platform (now
   Wellfound) and never served LP data.

2. **The LP web login is bot-walled.** `venture.angellist.com/v/login`
   (a Next.js app) gates sign-in behind an *invisible* Cloudflare
   Turnstile + Google reCAPTCHA challenge. The `SignupLogin` mutation
   returns `unprocessable_entity` with "Our security check couldn't verify
   your session." It's score-based with no solvable puzzle, so a human in
   a VNC session can't help — the **automation fingerprint** is what's
   flagged. Hardened Camoufox (block_webrtc, manual typing, profile
   warmup, no init-script tampering) **still failed**, confirming it's the
   browser stack, not the IP.

**The solution — bring-your-own-cookie.** `byo-login` runs a *genuine,
un-instrumented stock Mozilla Firefox* (Mozilla apt repo, arm64) under
Xvfb + VNC inside the container. A real Firefox clears the invisible
challenge; the user logs in by hand. On a clean Firefox close,
`extract_cookies.py` reads the profile's plaintext `cookies.sqlite` and
writes the session to `~/.secrets/angellist-cookies.json` (0600) — no
manual export/copying. The key cookie is `_angellist_v2` (domain-wide
`.angellist.com`, ~27-day expiry → re-login roughly monthly). Firefox
stores cookie expiry in **milliseconds**; extract_cookies normalises to
the seconds Playwright expects.

Injecting those cookies (`context.add_cookies`) lands a headless browser
on the authenticated portal with **no re-challenge and no 401/403**. So
the only browser+human step is the infrequent `byo-login`.

## Why `download` is browser-based (not a plain HTTP client)

The LP data is served by GraphQL at `venture.angellist.com/venture/graphql`
(the `portal.angellist.com/api/graphql` investor portal is a discovery
shell; the funded book lives in the venture app). The requests carry full
query text — no persisted-query hashes — so they *look* browserless-
replayable. But every request also carries a custom **`x-al-gql`** header:
a 32-byte signature that is per-(operation+variables), required (a request
without it 404s), and **not a plain hash** of the request (no SHA-256
match) — i.e. an HMAC/obfuscated value computed by venture-web JS.

Per the repo convention (don't reverse-engineer JS-derived state — the
cointracking lesson), `download` therefore runs a **headless Camoufox with
the injected cookie, navigates the venture LP routes, and captures the
GraphQL the SPA fetches and signs itself**. This is robust and read-only.
URLs are built from `ViewerQuery` (`currentUser.slug` = userSlug,
`currentUser.investAccounts[i].slugName` = investAccountSlug) — slugs are
**never hardcoded**.

Routes + operations captured (per invest account):

| Route | Operations | Data |
| --- | --- | --- |
| `/v/<u>/i/<a>/portfolio/dashboard` | `PortfolioDashboardQuery` | totals, IRR/TVPI/DPI, NAV time series |
| (same) | `PositionsTableQuery` | funded positions per vehicle — **infinite-scroll paginated**, ~20/page; download scrolls until `totalCount` |
| (same) | `ActivityQuery` | activity feed (VenturePosts) — see note below |
| `/v/<u>/i/<a>/commitments` | `OpenInvestmentsQuery` | unfunded commitments |

`/taxes-and-documents` (K-1s) is located but out of v1 scope.

## Bronze

`download` writes, under `~/wealthdb/angellist/<UTC-ts>/`:

- `captures.jsonl` — one line per captured GraphQL exchange:
  `{op, variables, data}`. The full source payload, untouched.
- `viewer.json` — `currentUser` (identity convenience).
- `run.json` — manifest (ops + counts).

## Silver

SQLite + JSON1 (the repo default; DuckDB is the cointracking-only
exception). Money is stored exactly as AngelList returns it — minor units
(`*_minor`, the GraphQL `fractional`) + ISO `currency`. JSON `payload`
columns carry the full node. Schema in
[`migrations/0001_initial.sql`](migrations/0001_initial.sql):

- **`vehicles`** — one row per investable, keyed by the stable
  `investableGuid`; `name`, `avatar_url`, first/last seen, and `kind`
  (`spv` | `fund`) derived from the guid suffix (`-f` → fund, else → spv;
  the `-f` set matches `portfolio_summary.totalFundsCount`).
- **`positions`** — a **change-based valuation history**: the loader writes
  a new row for a position only when a value-affecting field changed vs.
  its latest prior snapshot (not a full re-dump each run), so the table
  accumulates each investment's history. Columns: `commitment_minor`,
  `contributed_minor` (capital called), `investment_minor`,
  `realized_minor` (cumulative distributions), `recycled_minor`,
  `unrealized_minor` / `total_value_minor` / `tvpi` (nullable when no
  current value is reported), `investment_date`, `status`. **Holdings
  as-of a date** = each position's latest snapshot ≤ date, dropping the
  EXITED ones. ~67 positions / 63 vehicles in the test account.
- **`portfolio_summary`** — per snapshot: totals (committed/contributed/
  invested/realized/unrealized/value) + `irr`/`tvpi`/`dpi` + counts. (The
  full summary, including insights, also rides in `payload`.)
- **`portfolio_timeseries`** — the ~monthly NAV history AngelList computes
  back to the first investment (value / invested / realized / unrealized
  per date; ~4.6y in the test account), promoted from
  `summary.timeSeries`. One row per `(invest_account_slug, as_of_date)`,
  upserted latest-snapshot-wins (past months get revised as valuations
  settle). Longer than our own snapshot history and not reconstructable
  from positions alone.
- **`commitments`** — per snapshot, the open (unfunded) commitments:
  amount, payment, remaining-to-fund, opportunity/syndicate, deadlines.
  Bank/wire details are deliberately **not** promoted to columns.

### The cash-flow nuance (important for gold)

The venture portal exposes **cumulative** contributed (capital called) and
realized (distributions) *per position*, plus a portfolio-level value time
series — **not a dated per-event capital-call / distribution ledger**. The
"activity" feed is unstructured `VenturePost` updates, not transactions.
So a clean dated cash-flow ledger is not available from this surface;
`positions` carries the cumulative call/distribution state, and the
`portfolio_timeseries` table carries portfolio value / invested / realized
/ unrealized over time.

### Historical valuations & K-1s

AngelList's API/UI exposes only **current** per-position values plus the
**portfolio-level** NAV series — no per-position valuation history. The
per-SPV historical **capital account** (contributions, distributions,
ending capital — tax basis) lives in the **Schedule K-1 tax documents**
(PDF + a structured **CSV**) and quarterly financial statements on the
Taxes & Documents page (`AccountDocumentsQuery`: `taxDocuments[]` with
`pdfUrl`/`csvUrl` + completeness fields `documentType` /
`k1Count`/`totalK1Count` / `updatedAt`; `financialDocuments[]`).

**Parsed (built).** `load.py` parses the K-1 CSVs in
`angellist-documents/` into **`k1_capital_accounts`** — one row per
`(tax_year, SPV)`: SPV legal name + EIN (the investment entity — the SPV,
not the company), portfolio company, capital account
(beginning / contributions / net income / distributions / ending) + all
Schedule-K-1 tax lines in `payload` — and **`tax_documents`** (provenance +
completeness), idempotent by content sha. K-1 rows link to a vehicle by
company name (exact for single-SPV companies). Note `Ending Capital` is
**tax basis**, not FMV; distributions appear under Line 19(a).

**Download.** The file endpoints (`/k1_packets/<id>/{download,csv}`,
`/financial_reports/<id>/download`) accept a cookie GET but need a
**fresher session than GraphQL does** — a stale cookie 404s to the login
wall. `byo-login` saves downloads to `angellist-documents/`.

**Remaining:** (1) an automated cookie-GET in `download.py` that re-fetches
incomplete tax years (`estimate_provided`, or `k1Count < totalK1Count`)
until `complete` (which runs through ~Aug of the following year); (2)
feeding dated (Dec-31) tax-basis valuation snapshots into the `positions`
history via `tax_basis_capital_minor` (FMV untouched), pairing the 3
multi-SPV companies by contribution amount + date.

## Gold mapping (implemented)

The gold adapter lives in `wealthdb/internal/silver/angellist/`
(registered in `cmd/wealthdb/main.go`; `silver_kind` whitelisted by gold
migration 0014). It projects this silver into canonical `accounts` /
`instruments` / `positions` (no `transactions`):

- **Account.** One canonical `account` per AngelList invest account
  (`invest_account_slug`): `account_kind=brokerage`,
  `tax_wrapper=taxable_personal`, `management_style=discretionary`
  (GP-managed). Config `account_overrides` win on overlap.
- **Instruments.** One per vehicle, keyed by `investableGuid`.
  `asset_class` from silver `vehicles.kind`: single-company SPVs/RUVs →
  `spv` (a canonical enum value added for this), multi-company venture/PE
  funds → `private_fund`; `isin`/`symbol` NULL (non-quotable).
- **Positions.** One per vehicle per snapshot. `book_value=contributed`
  (capital called = cost basis); `market_value=total_value`, **falling
  back to `contributed` (cost)** when AngelList reports no current value
  (~half — pending / non-standard reporting); `quantity=NULL` (LP
  interests have no unit qty); `acquisition_date=investmentDate`.
  Commitment / uncalled / realized ride in the position `payload` (the
  chosen payload-only option — no first-class gold column).
- **No transactions.** AngelList exposes only cumulative contributed/
  realized per position (no dated capital-call/distribution ledger), so
  `Transactions()` returns an empty stream. Revisit if a per-position
  statements/transactions query surfaces (`/taxes-and-documents`,
  deferred).

Consequence: `positions.market_value` summed in gold won't equal
`portfolio_summary.totalValue` (the authoritative total) — AngelList
reports no FMV for ~half the positions, so those carry cost as a proxy.

Verified end-to-end against the real account: **1 account, 63 instruments,
67 positions** (62 `spv` + 5 `private_fund`, USD), 0 transactions;
`go build` + `go test ./...` green.

## Re-discovery

`explore` (Camoufox + VNC + HAR/trace/click-log, with `--cookies` to load
the BYO session and `--dump-links` to map routes) is kept for when
AngelList changes the venture SPA's GraphQL or routes.

## Read-only & PII

See [CLAUDE.md](CLAUDE.md). Navigation + passive GraphQL capture only;
never a mutate control, never the lead/admin surface. SPV/fund names,
amounts, and the commitments wire details are PII — synthetic placeholders
only in any tracked file; real data stays under `~/wealthdb` / `~/.secrets`.
