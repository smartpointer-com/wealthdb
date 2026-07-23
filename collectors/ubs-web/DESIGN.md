# ubs-web — design notes

This document describes what the `ubs-web` silver carries and how it
lines up with the PSN-fed silver from the sibling
[ubs-psn](../ubs-psn/) collector — the source-specific facts a
reader needs to understand the two UBS feeds. It supplements the
schema comments in
[migrations/0001_initial.sql](migrations/0001_initial.sql); read
that file for column-level definitions, this file for the
feed-shape detail.

How gold actually merges the two UBS silvers (join keys, splice
rules, sign conventions, which feed wins) is owned by the wealthdb
UBS adapter — see [the adapter doc](../../wealthdb/docs/adapters/ubs.md).
The notes below describe the silver shapes that make those merge
decisions possible, not the merge policy itself.

The companion silvers, by path:

| Silver | Source | Coverage | Default path |
| --- | --- | --- | --- |
| `ubs-web` | Web scrape via Playwright + PDF reconstruction | Live-fetch tables: recent transactions window + on-demand positions + a PDF document index. Historical tables: quarterly position snapshots and monthly cash balances reconstructed from the bronze PDF archive, back to the earliest statement available | `$XDG_DATA_HOME/wealthdb/ubs-web/ubs-web.db` |
| `ubs-psn` | UBS PSN nightly SFTP feed | Forward-only daily snapshots + events, from agreement go-live date | `$XDG_DATA_HOME/wealthdb/ubs-psn/ubs-psn.db` |

## 1. The two UBS feeds

Both silvers expose the same logical entities (accounts, portfolios,
positions, transactions) plus a few feed-specific extras. The web
silver is the **historical source of truth** (it carries multi-year
data PSN can't); PSN takes over from its activation date forward.
The two feeds splice on a per-relationship cutover date: web before
it, PSN after it. Documents (PDFs) are web-only — PSN has no
equivalent.

How gold reconciles the overlap (which feed wins per entity, the
exact join keys and splice boundary) is owned by the wealthdb UBS
adapter — see [the adapter doc](../../wealthdb/docs/adapters/ubs.md).
The sections below document the silver-side facts that make the
splice possible.

## 2. Identifier conventions

The schema header in [migrations/0001_initial.sql](migrations/0001_initial.sql)
defines every identifier. Recap of the cross-silver join keys:

| Entity | Web silver | PSN silver | Join condition |
| --- | --- | --- | --- |
| Banking relationship | `banking_relationship_id` (UBS opaque token), plus `account_number_prefix` (e.g. `BBBB AAAAAAAA`) and `description` | `relationship_id` (`SFTPCH01` / `SFTPCH02`, the SFTP server name) | **Manual config required** — there is no shared key. See §3.1. |
| Portfolio | `portfolio_external_id` (e.g. `RNNN`) + `base_currency` (e.g. `CHF`, from positions.csv "Valued in:" footer) | `portfolio_external_id` (= UBS `PrtflId`) + `PrtflKey.PrtflCcyIsoCd` in payload | Equality on `portfolio_external_id` |
| Cash account | `account_external_id` = IBAN no-spaces uppercase (e.g. `CHKKBBBBRRRRAAAAAAAAC`); plus parallel `account_acct_id_psn_form` (e.g. `RRRR000000AAAAAAAA0000C`) computed by the web loader | `account_external_id` = IBAN per `psn/migrations/0001` comment; the PSN payload also has UBS `AcctId` in the 21-char form | Equality on `account_external_id` (IBAN) OR equality on `account_acct_id_psn_form` ↔ PSN payload `AcctId`. Either works. |
| Safekeeping account | Not currently surfaced by the web feed (would need future scraping) | `account_external_id` = UBS safekeeping code (e.g. `BBBB-AAAAAAAA.S1`) | PSN only for now. |
| Instrument | `instrument_isin` (ISO 6166 ISIN-12) on `positions` | `isin` on `instruments` / `holdings` | Equality on ISIN |
| Transaction | `(transaction_external_id, account_external_id)` compound PK — UBS reuses the Transaction no. for both debit + credit sides of an inter-account transfer | `event_external_id` (derived from the SWIFT message reference), unique per event | **DO NOT JOIN** — the two ID schemes do not overlap in practice. See §3.6 for the date-splice-only merge strategy. |

## 3. Per-entity silver shapes

This section documents the identifiers and entity shapes each feed
carries. How gold pairs them across the two silvers (config schema,
join keys, which feed wins) is owned by the wealthdb UBS adapter —
see [the adapter doc](../../wealthdb/docs/adapters/ubs.md).

### 3.1 Banking relationships

An e-banking login can hold 1+ banking relationships.
PSN sees each as a separate SFTP endpoint (`SFTPCH01`, `SFTPCH02`,
…). Web sees each as an opaque `bankingRelationId` URL token.

**There is no shared key**, only proxies the silver surfaces:

- `account_number_prefix` — the 12-char shared prefix of all
  account numbers under the relationship (`BBBB AAAAAAAA`).
  PSN's `AcctId` values can be sliced to derive the same prefix
  (positions 1–4 + reformat the middle).
- `description` — the web silver leaves
  `banking_relationships.description` empty by default, a slot for
  a human label pairing each web relationship with its PSN
  counterpart.

When `download.py` runs against a different relationship
(after it is switched in the UBS UI), a new
`banking_relationships` row appears with its own opaque token.

### 3.2 Portfolios

PSN's `portfolio_external_id` is UBS's `PrtflId` (4-char code
like `RNNN`). The web `positions.csv` "Portfolio" column carries
`<account_number_prefix> <PrtflId>` — e.g. `BBBB AAAAAAAA RNNN`.

Portfolios and accounts are **separate entities** in silver:
`portfolios` is keyed by `(snapshot_at, portfolio_external_id)`,
`accounts` carries a nullable `portfolio_external_id` foreign
key. A portfolio never owns positions / balances directly — they
hang off cash and safekeeping accounts.

**Three kinds of portfolio rows can appear in web silver:**

1. **Real customer-facing portfolios** that appear on the
   UBS homepage as separate tiles. The web loader pulls one
   `positions_<sha>.csv` per portfolio via the `portfolioUid`
   anchors enumerated from the homepage.
2. **A synthetic catch-all portfolio** that the consolidated
   default view (`positions.csv`, the `preselectFirstPortfolio
   =true` route) uses to file accounts that aren't attached to
   any real portfolio. The web loader processes per-portfolio
   CSVs first, then the consolidated CSV, and only inserts
   accounts + positions for `(account, isin)` pairs not yet seen.
   Net effect: real portfolio assignments win; the catch-all only
   ever owns the genuinely-unattached accounts.

**Web loader contract:** when parsing each positions.csv,
extract the trailing token of the "Portfolio" column (split on
whitespace, take the last word) as `portfolio_external_id`.
Store the full original string in `portfolio_full_id` for
traceability. Read the "Valued in: <CCY>" footer line and write
it as `portfolios.base_currency`. The catch-all is a real row in
silver, distinct from the customer-facing portfolios.

### 3.3 Cash accounts

Web shows the IBAN with spaces (`CHKK BBBB RRRR AAAA AAAA C`,
e.g. UBS Switzerland uses `BBBB = 0023`).
The web loader normalises to `account_external_id`:

```python
def iban_canonical(iban: str) -> str:
    return iban.replace(" ", "").upper()
```

PSN's loader stores the IBAN in `cash_accounts.account_external_id`
per the schema comment. The same canonical form on both sides
means equality joins.

#### IBAN ↔ PSN AcctId conversion

The PSN payload also carries `AcctId` in a 21-char UBS-internal
form (`RRRR000000AAAAAAAA0000C`). The relation between an IBAN and
the AcctId, for UBS Switzerland personal accounts:

```
IBAN structure (with spaces):
  CH<chk:2>  <bank:4>  <branch:4>  <base:8>   <chk:1>
  CHKK       BBBB      RRRR        AAAAAAAA   C

AcctId structure (21 chars, no spaces):
  <branch:4> <0000>    <00><base:8>  <0000>   <chk:1>
  RRRR       0000      00 AAAAAAAA   0000     C
```

So given a canonical IBAN `CH<chk:2><bank:4><branch:4><base:8><chk:1>`
of length 21, the AcctId is:

```
acct_id_psn_form = branch + "0000" + "00" + base + "0000" + chk
                 = iban[8:12] + "000000" + iban[12:20] + "0000" + iban[20]
```

The web loader computes `account_acct_id_psn_form` from the IBAN
and stores it as a parallel column, so both the IBAN and the
21-char AcctId form are present in both silvers.

#### Accounts PSN sees but web doesn't

PSN can report internal UBS booking accounts that the customer UI
does not expose. The web feed has no rows for them at all —
they exist only on the PSN side.

### 3.4 Safekeeping accounts (securities depots)

PSN exposes them directly (`safekeeping_accounts` table); web
does not surface them as first-class rows. The web `positions.csv`
flattens everything to portfolio + product, hiding the depot
layer. The web `positions.portfolio_external_id` does link to
`psn.holdings` (which carries both `safekeeping_external_id` and
`isin`), though not strictly 1-to-1 — multiple safekeepings can
exist under one portfolio.

**Future work:** per-safekeeping attribution from web data would
need scraping the "Investment positions (custody account)"
navigation tree, which exposes the depot hierarchy. Not done in v1.

### 3.5 Positions (snapshots)

Both silvers emit complete snapshots. Web is on-demand (one per
`download.py` run); PSN is daily.

The two `positions` shapes differ in fidelity. PSN's `holdings`
carry the full safekeeping / portfolio hierarchy and the
authoritative pricing snapshot per (safekeeping, instrument). The
web silver's `positions` table flattens that hierarchy: one
portfolio code per row, no safekeeping link, market value in
portfolio base currency only. What the web rows add on top is
`cost_price`, `lending_value`, `lending_value_ratio`,
`description`, etc. — fields PSN does not carry. The web
`positions` rows key on `portfolio_external_id` + `instrument_isin`.

**Cash positions** (the "Liquidity - Accounts" rows in
positions.csv) carry `account_external_id` as the canonical IBAN
per §2, the same form PSN's `cash_balances` use.

### 3.6 Transactions (the splice)

This is where the two UBS feeds meet. Rather than matching
individual transactions, the histories splice on a date boundary:
web before PSN's go-live, PSN after it. The wealthdb UBS adapter
owns the actual cut — see
[the adapter doc](../../wealthdb/docs/adapters/ubs.md). The
silver-side facts that make a clean date-splice possible:

- **Both silvers promote `value_date`.** PSN's `events.timestamp`
  is the MT940 booking/value date; web's `transactions.value_date`
  is the CSV "Value date" column. Identical by construction for
  cash movements in the overlap window, so the splice needs no
  per-row matching.
- **The cutover date is derivable from PSN silver:** PSN's feed
  go-live for a relationship is `MIN(snapshot_at)` in
  `psn.dump_runs`.
- **Inter-account transfers carry both sides.** UBS reuses the
  same `Transaction no.` for the debit row in the source account
  and the credit row in the destination account; the web silver
  uses a compound PK on `(transaction_external_id,
  account_external_id)` so both rows survive a per-account splice.
- **The two transaction-ID schemes do not overlap.** Web uses
  UBS's "Transaction no."; PSN derives event IDs from SWIFT
  message references (`mt515:…`). There is zero overlap between
  the two ID spaces, so per-row identity matching is not
  possible — the date-splice is the only safe merge.

**Note on Trade date vs Value date.** Web's CSV has four dates per
row: Trade date, Trade time, Booking date, Value date. PSN MT940
exposes booking + value date. Both silvers promote **Value date**
as the splice key for consistency.

### 3.7 Documents (PDFs)

Web-only — PSN has no document concept. The silver `documents`
table is one row per PDF, indexed by date and type. The web loader
populates `documents.account_external_id` /
`documents.portfolio_external_id` via best-effort label parsing,
when the listing-row label contains a discoverable IBAN / portfolio
code.

The PDF binaries live on disk under
`<bronze-root>/<dump-ts>/documents/`, named by their content hash
(`<sha256>.pdf`) — the silver `documents` table only indexes them.
Content-addressed naming means an unchanged document keeps the same
filename across runs (UBS otherwise serves it under a fresh per-session
token each time), so the tree stops accreting a new name per
re-download and each document gains a stable cross-run identity
(`content_sha256`, recorded in `run.json`) for later dedup tooling. The
loader is filename-agnostic — it catalogs whatever `*.pdf` the manifest
lists — so older token-named dumps keep loading unchanged.

### 3.8 Historical snapshots reconstructed from PDFs

The web silver also reconstructs **historical** position + cash
snapshots from the PDF document archive. Three dedicated tables:

| Table | Source PDF type | Granularity |
| --- | --- | --- |
| `historical_position_snapshots` | "Statement of assets" PDFs (semi-annual, sometimes quarterly) | One row per `(as_of_date, portfolio, account, ISIN)`. Cash positions have `instrument_isin = NULL` and a populated `account_external_id` (IBAN); securities have `instrument_isin` set and `account_external_id = ''` (UBS doesn't surface the safekeeping account in the printed text in a way we can extract). |
| `historical_cash_balances` | "Account Statement" PDFs (monthly) | One row per `(period_end, account_external_id)` with opening / closing balance + turnover totals. UBS only issues an Account Statement for a given month when the account had activity in that month, so coverage is uneven; year-end months tend to cover the full account inventory. |
| `historical_mortgages` | "Maturity notice" PDFs (one per fixed-rate / SARON interest-roll period, typically quarterly) | One row per `(as_of_date, account_external_id)` with the outstanding principal (negated to liability sign), the product line → rate type, and the collateral description (migration 0005). |

(The live-fetch side of mortgages is the `mortgages` table —
migration 0004 — fed by positions.csv's "Pro memoria - Mortgages"
rows, which reuse the IBAN column for the fixed-rate term and carry
the UBS-internal mortgage number as `account_external_id`. Gold
projects both tables through the same mortgage account/instrument
path: one `AccountChange{Kind: mortgage}` + a negative-value
position.)

**Position-row shapes the Statement-of-assets walker handles.**
The securities walker anchors on each `Valor … - ISIN …` line and
reads the headline row just above it, in three flavours:

1. **Listed securities** — the `cost-price / market-price /
   market-gain%` triple. A one-letter price qualifier UBS sometimes
   prints after the market price (e.g. a structured product's
   `120.00 B 20.00%`) is tolerated.
2. **Private-markets / SPV holdings** (UBS-sponsored Private Markets
   funds and SPV interests) — these print a single FX rate, or the
   literal `n.a.`, where listed rows print the triple. The funded
   "Outstanding Shares" row carries the NAV; the `n.a.` Net/Unfunded
   Commitment rows are 0-valued and skipped (the same fund's
   commitment ISINs would otherwise add value-less rows).
3. **Overview-only asset classes** — UBS issues no Detailed-positions
   page for some portfolio types (e.g. precious-metals custody), so
   such a holding has no per-instrument row anywhere in the PDF. Its
   asset-class total is recovered from the relationship overview as a
   single synthetic position (`description = "Precious metals &
   commodities"`, a non-ISIN-shaped `instrument_isin` key of the form
   `PM-<portfolio>`). The overview prints the figure once per
   portfolio-currency PDF, so the walker emits it only from
   USD-valued PDFs (the reporting-currency baseline); the
   duplicate USD copies collapse on the silver PK. The gold adapter
   recognises the non-ISIN key and leaves the canonical ISIN null.

**Why separate from the live-fetch `positions` / `accounts`
tables.** Two reasons:

1. **Identity model differs.** The PDFs use UBS's `NN` portfolio
   numbering (e.g. `01` … `06`), which the loader expands to the
   PSN-aligned `BBBBAAAAAAAANN` form — directly joinable against
   PSN's `PrtflId`. The live-fetch `portfolios` table uses 4-char
   UBS-internal codes (e.g. `RNNN`, `NNNN`) which are a different
   surface. Putting them in one table would require either a
   mapping that doesn't exist in either source, or a `source`
   column that gold would still have to filter on every query.
2. **Cadence + authority differ.** PDFs are bank-of-record end-
   of-period snapshots, semi-annual at best for positions. Live
   fetches are intra-day customer-side snapshots. Keeping them in
   separate tables lets a consumer pick per use case (PDF snapshots
   for historical attribution; live fetch for "what does the
   customer see right now").

**Cross-feed join keys.** `historical_position_snapshots` carries
the portfolio identifier in PSN's `PrtflId`-aligned form (per the
`BBBBAAAAAAAANN` expansion above), so it lines up directly with
PSN `holdings`. `historical_cash_balances` carries
`account_external_id` as the IBAN — the same canonical form PSN
uses. How gold splices the historical and PSN snapshots (which feed
wins per date) is owned by the wealthdb UBS adapter — see
[the adapter doc](../../wealthdb/docs/adapters/ubs.md).

**Known parser limitations.** The parsers in
[pdf_parsers.py](pdf_parsers.py) are best-effort:

- The "sector" field on securities sometimes captures the
  preceding `Distribution:` line instead of the actual sector
  label. Treat `sector` as informational, not authoritative.
- The cash-position "description" sometimes pulls in adjacent
  numeric noise (e.g. a balance-date that flowed into the
  description column on the printed page). The IBAN, currency,
  and market value are reliable.
- UBS only issues Account Statements for months with activity,
  so missing months don't imply missing balances — the closing
  balance from the most recent prior month is still valid until
  the next statement.
- Within a row, individual numeric columns may be NULL even when
  the row is present. The four columns come from two different
  PDF sections: opening / closing balance live on the in-table
  header / footer rows (present in essentially every statement),
  while `total_credits` / `total_debits` come from the "Your
  account at a glance" summary block (only emitted for statements
  with non-trivial activity, so it is present on only a subset of
  statements).
  Treat NULL as "the source PDF did not print that line" rather
  than as 0.0.

## 4. Web loader implementation notes

- **Migration runner.** Applies pending migrations in numeric
  order, commit after each, record version in `schema_meta`.
  Mirror `ubs-psn/load.py`'s pattern.
- **Bronze scan.** Walk `<bronze-root>` for subdirectories
  matching `YYYYMMDDTHHMMSSZ` and skip those already in
  `dump_runs`.
- **Per dump:**
  1. Parse `run.json` to get the window bounds; write `dump_runs`.
  2. Parse `positions/*.csv` (per-portfolio CSVs first, then the
     consolidated default-view CSV) → upsert
     `banking_relationships`, `portfolios`, `accounts`,
     `positions`. Per-portfolio first means real portfolio
     assignments win and only truly-unassigned accounts land
     under the synthetic catch-all portfolio.
  3. Parse `transactions/cash_*.csv` (per-account, per-window) →
     upsert `transactions` keyed by `(transaction_external_id,
     account_external_id)`.
  4. Catalog `documents/*.pdf` files referenced in `run.json` →
     upsert `documents`, computing `content_sha256` per file.
  5. Walk the `documents` table, route every PDF whose label
     matches a known statement type to `pdf_parsers.py`, and
     upsert the parsed positions / cash balances into the
     `historical_*` tables.

- **PDF parsing isolation.** `pdfplumber` is bundled in the
  Docker image (`requirements.txt`). The parsers live in
  `pdf_parsers.py` and have no DB-side dependencies, so they
  can be unit-tested against a static PDF without spinning up
  silver.
- **Idempotency.** Re-running the loader on the same bronze dir
  is a no-op (PK collisions caught + content-dedup).
- **Atomicity.** One transaction per dump-run. Roll back on any
  parsing failure; re-run after fixing.

- **Run status + pruning.** `download` writes `run.json` twice: a
  `{"status": "in-progress"}` marker the moment it creates the run
  dir, then an atomic overwrite with the terminal manifest carrying
  `"status": "complete"` once the walk finishes (`--dry-run` writes
  nothing to bronze at all). This makes a crashed walk —
  which never reaches `write_run_json` — legible without leaving an
  empty run dir. `load` is unaffected: `scan_bronze` selects every
  timestamped subdir regardless of `run.json`, and `_read_run_json`
  tolerates a missing manifest, so no dump-selection guard is needed.
  `prune.py` (a thin wrapper over the shared `collectorkit.prune`
  engine) uses the status field to reclaim disk: it deletes whole run
  dirs that are non-complete — `in-progress` / `dry-run` / no
  `run.json` — and keeps every complete dump's load inputs untouched.
  `debug_subdirs` names `screenshots/`, the one thing a complete dump
  gives up: the landmark DOM + screenshot captures `download --debug`
  writes, which `load` never reads. ubs-web's other diagnostics —
  screenshots, Playwright traces and QR PNGs — go to the external
  `--screenshot-dir` / `--trace` / `--qr-png` outputs (the `/debug`
  mount), outside bronze. Because the only deletion path that can touch
  a load input is a whole non-complete dir, the classification that must
  be exact is the
  legacy (statusless) fallback: a pre-change `--dry-run` shell carries
  a full-looking manifest with `dry_run: true`, so completeness there
  is `manifest present AND not dry_run`, not the bare manifest-presence
  fidelity-web uses.

## 5. Feed-coverage gaps the adapter must reckon with

These are source-specific limits of what the silvers carry. How the
wealthdb UBS adapter resolves them is owned by
[the adapter doc](../../wealthdb/docs/adapters/ubs.md); they are
listed here because they are properties of the feeds, not of gold.

- **Multi-relationship sweep.** The web SPA only exposes the
  currently-selected relationship, so a session captures exactly
  one: covering a second one takes a relationship switch and
  another run. Could be automated in download.py later.
- **Cost basis is web-only.** Web carries `cost_price`; PSN does
  not. Past the PSN cutover the web cost basis stops refreshing
  unless web is re-run or cost basis is derived from web's
  transaction history (buy/sell events).
- **FX coverage differs sharply.** PSN carries 980+ FX rates; web
  carries only the 8 CHF/* pairs in the `positions.csv` footer.
- **Documents are not indexed by instrument.** Trade confirmations
  and corporate-action notices reference ISINs in the PDF body, but
  the web loader does not parse PDF bodies; `documents` is indexed
  by type + date + account only. Per-ISIN attribution would need a
  PDF text-extraction pass.
