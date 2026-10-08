# angellist — design notes

How the AngelList LP collector works, end to end. Implemented:
`login` → `download` → `load` →
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

**The solution — bring-your-own-cookie.** `login` runs a *genuine,
un-instrumented stock Mozilla Firefox* (Mozilla apt repo, arm64) under
Xvfb + VNC inside the container. A real Firefox clears the invisible
challenge; login is completed by hand. A stock binary takes no Playwright
prefs, so `fxprofile.py` seeds the profile's `user.js` first — the shared
[`collectorkit.launch`](../../shared/collectorkit/collectorkit/launch.py)
pref set (disk cache, history, favicons and telemetry persistence off, so
the profile stays session-state-sized) plus the AngelList overrides that
persist the session cookie on shutdown and route document downloads to the
mounted dir. Nothing there is observable to web content: the fingerprint
that clears the challenge is untouched, and Firefox's own blocklist data
(`security_state/`, `safebrowsing/`) is left to populate because it is part
of looking like a real browser. On a clean Firefox close,
`extract_cookies.py` reads the profile's plaintext `cookies.sqlite` and
writes the session to `~/.secrets/angellist-cookies.json` (0600) — no
manual export/copying. The key cookie is `_angellist_v2` (domain-wide
`.angellist.com`, ~27-day expiry → re-login roughly monthly). Firefox
stores cookie expiry in **milliseconds**; extract_cookies normalises to
the seconds Playwright expects.

Injecting those cookies (`context.add_cookies`) lands a headless browser
on the authenticated portal with **no re-challenge and no 401/403**. So
the only browser+human step is the infrequent `login`.

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
| `/v/<u>/i/<a>/taxes-and-documents` | `AccountDocumentsQuery` | K-1 + financial-statement document list (PDF/CSV URLs) |
| `/v/<u>/i/<a>/funding-accounts` | `InvestmentEntityQuery` | the dated funding-account cash ledger + balance |

## Bronze

`download` writes, under `$XDG_DATA_HOME/wealthdb/angellist/<UTC-ts>/`:

- `captures.jsonl` — one line per captured GraphQL exchange:
  `{op, variables, data}`. The full source payload, untouched. The
  primary `load` input.
- `viewer.json` — `currentUser` (identity convenience; not a `load`
  input). Holds PII, so it stays out of the repo.
- `run.json` — manifest (`status`, ops + counts), written LAST. A
  `status: "in-progress"` marker is dropped when the run dir is created
  and atomically overwritten with `status: "complete"` at the end (all
  GraphQL capture completes in-memory first, so the in-progress window is
  the captures.jsonl write loop only). That terminal `status` is the
  signal `prune` keys on; `had_errors` does not gate completeness (a dump
  with GraphQL errors still finished). Dumps that predate the `status`
  field carry a statusless-but-present manifest — the walk wrote it only
  at the end, so its presence still means COMPLETE. `run.json` is also a
  `load` input (stored into `dump_runs.payload`).

The tax documents do NOT live in the run dir: `download` (and the manual
`login` grab) save the K-1 CSV/PDF and financial statements to a
bronze-ROOT sibling `angellist-documents/`, and the silver DB is
`angellist.db` at the same level. Neither is a timestamped run dir.

### Pruning bronze

`prune` (`prune.py`, a thin wrapper over the shared
`collectorkit.prune` engine) reclaims disk by deleting whole
**non-complete** run dirs — a `download` that crashed before writing its
terminal `run.json` (`status: "in-progress"`, or no manifest). It keys on
the `run.json` `status` above; an in-flight guard (`--min-age-hours`,
keyed on the newest write in the dir) protects a long download still in
flight, and a corrupt/unreadable manifest is UNKNOWN and never deleted.
`debug_subdirs` names `screenshots/`, the one thing prune strips from a
*complete* dump: the DOM + screenshot `download --debug` takes of the
bootstrap landing and of each LP route as it settles. Those say what the SPA
rendered when an expected op never fired — `captures.jsonl` can only record
the ops that did — and `load` never reads them, so reclaiming them cannot
change silver. (The `explore` harness's HAR/trace/click-log are separate and
land under `/debug`, outside bronze.) Every other file in a complete dump
survives, and the engine only ever iterates timestamped run dirs, leaving the
`angellist-documents/` sibling and `angellist.db` untouched. It runs host-side
(a file walk needs no container), so it can reclaim disk while a `download` is
mid-flight.

Because `--debug` writes while the browser is still open, it fixes the run
dir's slug up front and creates the dir before the walk — a run that captures
nothing (a stale cookie) never reaches the artefact write, and that is exactly
the run whose screenshots are worth having. Such a dir carries no terminal
`run.json`, so prune reclaims it as non-complete and `load` skips it: the same
lifecycle as a crash. `--dry-run` and `--check` write no bronze, so `--debug`
warns and captures nothing there.

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
- **`offerings`** — IMMUTABLE per investment (migration 0005), one row per
  position / SPV stake (keyed by the AngelList position id; SPVs are never
  merged). Identity + entry terms factored out of the time series:
  `vehicle_external_id` (the company), `kind`, `company_name`, the SPV legal
  name + EIN (`fund_name`/`fund_tax_id`, from the linked K-1), and
  `investment_date`. Upserted each load.
- **`position_snapshots`** — the per-position valuation TIME SERIES: one row
  per CAPITAL EVENT, stamped at the EVENT date (`as_of_date`), with a
  collector-computed `market_value_minor` + its `valuation_basis`. Events:
  `investment` (original capital at the investment date — cost), `statement`
  (each annual Schedule K-1 capital-account statement at its tax year-end —
  the tax-basis NAV, with cumulative contributed / distributions),
  `valuation` (the current portal FMV at the portfolio **data date**, not
  the download time — emitted only where the portal states a value; a
  Realized position's stated total is what came out, so it marks 0 and
  closes there). `is_open` flips to 0 at a final K-1 (exit) or at a
  Realized valuation. **Holdings as-of a date** = each position's latest
  snapshot ≤ date, dropping the is_open=0 ones. This mirrors the equityzen collector — one row per
  position per capital event.
- **`portfolio_summary`** — per snapshot: totals (committed/contributed/
  invested/realized/unrealized/value) + `irr`/`tvpi`/`dpi` + counts. (The
  full summary, including insights, also rides in `payload`.)
- **`portfolio_timeseries`** — the ~monthly NAV history AngelList computes
  back to the first investment (value / invested / realized / unrealized
  per date, spanning back to the account's first investment), promoted from
  `summary.timeSeries`. One row per `(invest_account_slug, as_of_date)`,
  upserted latest-snapshot-wins (past months get revised as valuations
  settle). Longer than our own snapshot history and not reconstructable
  from positions alone.
- **`commitments`** — per snapshot, the open (unfunded) commitments:
  amount, payment, remaining-to-fund, opportunity/syndicate, deadlines.
  Bank/wire details are deliberately **not** promoted to columns.
- **`funding_accounts`** + **`funding_transactions`** (migration 0006) — the
  funding account's dated cash ledger from `InvestmentEntityQuery`: one row
  per cash movement with a SIGNED `amount_minor`, `occurred_at`, and raw
  `type` (deposit / withdrawal / investment / disbursement / refund /
  transfer), plus the current `balance_minor`. Keyed by the AngelList
  transaction id (idempotent); reconciles exactly to the balance. (Bank /
  wire account details from the source are deliberately **not** stored.)

### Cash flows

The **funding-accounts page** (`InvestmentEntityQuery.investmentEntity`) is
the dated cash ledger: every deposit / withdrawal (external bank ↔ funding
account), investment / refund (capital ↔ an SPV), and disbursement (a deal
pays out), each with a SIGNED `amount` and a real date, plus the current cash
`balance`. The signed amounts reconcile **exactly** to the balance, so
`download` captures it (the GraphQL carries the full ledger — the page's CSV
export is redundant) into `funding_transactions` + `funding_accounts`.

By contrast the venture positions GraphQL exposes only **cumulative**
contributed/realized per position (no dated events), the "activity" feed is
unstructured `VenturePost`s, and `portfolio_timeseries` is portfolio-level
monthly NAV. The Schedule K-1 **Line 19(a)** annual distribution restates the
dated disbursements, so it is **deliberately not emitted as a transaction** —
emitting it would double-count every distribution. `k1_capital_accounts`
feeds no transaction: it serves the position tax-basis statement valuations
and holds the basis and gain lines (below).

### Historical valuations & K-1s

AngelList's API/UI exposes only **current** per-position values plus the
**portfolio-level** NAV series — no per-position valuation history. The
per-SPV historical **capital account** (contributions, distributions,
ending capital — tax basis) lives in the **Schedule K-1 tax documents**
(PDF + a structured **CSV**) and quarterly financial statements on the
Taxes & Documents page (`AccountDocumentsQuery`: `taxDocuments[]` with
`pdfUrl`/`csvUrl` + completeness fields `documentType` /
`k1Count`/`totalK1Count` / `updatedAt`; `financialDocuments[]`).

**Fund financial reports → fair-value marks (built).** For proper funds
AngelList also issues quarterly financial reports (and, at some
year-ends, a dedicated capital-account statement). Both embed a per-LP
capital statement whose ending balance is the partner's capital account
at **fair value** — the roll-forward carries a "Net change in
unrealized gains" line — which the K-1 (tax basis only) never shows.
`statements.py` parses that page (`pdftotext -layout`) and `load.py`
emits a `statement` event with `valuation_basis='fmv'` at each report's
period end, matched to a `kind='fund'` offering by the longest
normalised name (company or fund-legal) in the filename; a year-end
fair-value statement replaces the same-dated tax-basis K-1 mark. SPVs
get no financial reports, so their K-1/tender-free marks are unchanged.

**Parsed (built).** `load.py` parses the K-1 CSVs in
`angellist-documents/` into **`k1_capital_accounts`** — one row per
`(tax_year, SPV)`: SPV legal name + EIN (the investment entity — the SPV,
not the company), portfolio company, capital account
(beginning / contributions / net income / distributions / ending) + all
Schedule-K-1 tax lines in `payload` — and **`tax_documents`** (provenance +
completeness), idempotent by content sha. K-1 rows link to a vehicle by
company name (exact for single-SPV companies). Note `Ending Capital` is
**tax basis**, not FMV; distributions appear under Line 19(a).

Three more K-1 lines have their own columns (migration 0008), because
cost basis and realized gains read them:

- `property_distributions_minor` — Line 19(c), property distributed in
  kind (such as shares at an in-kind exit). It reduces the partner's
  basis in the vehicle, as Line 19(a) does for cash.
- `short_term_gain_minor` — Line 8, net short-term capital gain (loss).
- `long_term_gain_minor` — Line 9(a), net long-term capital gain (loss).

Each holds the K-1 figure as printed, in minor units. A blank cell, or a
row without the cell, is NULL. The migration fills rows already on disk
from `payload`, so they need no re-parse.

**Download (built).** The file endpoints (`/k1_packets/<id>/{download,csv}`,
`/financial_reports/<id>/download`) accept a cookie GET but need a fresher
session than GraphQL — a stale cookie 404s to the login wall. `download.py`
auto-fetches them after the GraphQL capture, re-fetching incomplete tax
years (`estimate_provided`, or `k1Count < totalK1Count`) until `complete`
(which runs through ~Aug of the following year); `login` saves the same
way.

**Fed into the timeline (built).** Each K-1 becomes a `statement` event in
`position_snapshots` (tax-basis ending capital as the mark, dated Dec-31,
with cumulative contributed / distributions). Pairing is by **fund
identity**: the SPV legal name is stable, so a fund is matched to a
position through every company label its K-1 rows ever carried — a renamed
portfolio company keeps its old label on old tax years, and matching only
the current label would split one fund across two instruments. Real
offerings always beat funding-ledger-derived thin ones; multi-SPV
companies disambiguate by investment year + cumulative contribution
amount. Statement events are rebuilt from scratch on every load (they
derive wholly from `k1_capital_accounts`), so a pairing that shifts leaves
no stale marks behind. The mark basis (`fmv` / `tax_basis` / `cost`) is
recorded per event, never blended.

## Gold mapping (implemented)

The gold adapter lives in `wealthdb/internal/silver/angellist/`
(registered in `cmd/wealthdb/main.go`; `silver_kind` whitelisted by gold
migration 0014). The collector does the valuation + lifecycle work; the adapter is a thin
forward-fill:

- **Account.** One canonical `account` for the whole LP book
  (`invest_account_slug`): `account_kind=custody` (LP interests held in
  custody, not a brokerage), `tax_wrapper=taxable_personal`,
  `management_style=self_directed` (the holder picks which deals to back;
  the GP's management inside each vehicle isn't modeled) — same as carta /
  equityzen. Config `account_overrides` win on overlap.
- **Instruments + positions — one per SPV stake** (the SPV, not the company;
  keyed by the AngelList position id). `asset_class` from `offerings.kind`:
  single-company SPVs/RUVs → `spv` (a canonical enum value added for this),
  multi-company funds → `private_fund`; `isin`/`symbol` NULL. Instrument
  name = the underlying company.
- **Positions are forward-filled** from `position_snapshots`: for each event
  date the adapter emits each position's latest snapshot ≤ it, dropping the
  is_open=0 (exited) ones — a complete portfolio per date, which is what
  gold's as-of query reads. `market_value` = the collector's
  `market_value_minor` (current FMV → quarterly fund fair-value statement
  → annual tax-basis NAV → cost, never blended within a snapshot);
  `book_value` = the capital contributed as the portal states it, on the
  latest portal event ≤ the date (gross: distributions do not reduce it). A
  K-1's cumulative contributions are tax-basis capital and can differ from
  the portal's figure, so they ride in the position payload as
  `tax_basis_contributed` instead; `quantity=NULL`;
  `acquisition_date=investment_date`.
- **Transactions = the funding ledger** (`funding_transactions`). Each cash
  movement maps to a canonical kind by its source type:
  deposit→`deposit`, withdrawal/transfer→`withdrawal` (external bank ↔
  account), investment→`contribution` and refund→`contribution` (a positive
  reversal — returned un-deployed capital, kept out of `distribution` so DPI
  stays clean), disbursement→`distribution`. `contribution` is a canonical
  TxKind added for this; AngelList's amounts are authoritatively signed and
  reconcile to the balance, so they're used directly (the source sign wins).
  Each contribution / distribution / refund carries `instrument_external_id`
  = the SPV / fund it concerns, resolved in the collector (recorded as
  `funding_transactions.position_external_id`): the company named in the
  description matches a current `offerings` row when still held; a multi-SPV
  company (the description names only the company) is disambiguated by picking
  the position whose invest date is closest to the transaction date; a
  pre-rename description (an old company label preserved in old K-1 rows)
  resolves through the K-1 alias map to the renamed position; and an
  EXITED investment (no match at all) gets a thin instrument DERIVED FROM
  THE FUNDING LEDGER (emitted even though it holds nothing). `funding:`
  offerings that no transaction references any more are dropped with their
  snapshots at the end of each load. Only external-bank
  deposits / withdrawals stay account-level. Every ledger row links to a
  vehicle.
- **Cash.** The funding account's current uninvested cash is one
  `BalanceKind=current` CashBalanceChange, so account value = positions + cash.

Consequence: `positions.market_value` summed in gold won't equal
`portfolio_summary.totalValue` for the current date — positions without a
reported FMV carry their latest dated mark, the K-1's tax basis, or cost
before the first K-1. A re-mark to cost at the data date would override a
later, lower statement, so none is written.

An exit paid in shares — an SPV distributing the listed stock it received
into a brokerage account — is booked through gold's equity-transfer
ledger (`wealthdb/docs/DESIGN.md` §13.10): the SPV's `transfer_out` on the
day the shares land at the broker, at that day's value, paired with the
broker's `transfer_in`. The returns policy admits the ledger's two kinds
for that and nothing else produces them; the SPV's own mark follows its
K-1s.

The gold load reconstructs positions as of any past date from the
event-sourced silver, and funding transactions (deposit / withdrawal /
contribution / distribution) sum to the funding balance plus the current
cash balance.

## Re-discovery

`explore` (Camoufox + VNC + HAR/trace/click-log, with `--cookies` to load
the BYO session and `--dump-links` to map routes) is kept for when
AngelList changes the venture SPA's GraphQL or routes.

## Read-only & PII

See [AGENTS.md](AGENTS.md). Navigation + passive GraphQL capture only;
never a mutate control, never the lead/admin surface. SPV/fund names,
amounts, and the commitments wire details are PII — synthetic placeholders
only in any tracked file; real data stays under `$XDG_DATA_HOME/wealthdb` / `~/.secrets`.
