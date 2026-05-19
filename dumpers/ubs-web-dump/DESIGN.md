# ubs-web-dump — design notes for the gold-layer merge

This document is the contract between `ubs-web-dump` and the
wealthdb gold layer that converges web-scraped UBS data with the
PSN-fed silver (`ubs-psn-dump`). It supplements the schema
comments in [migrations/0001_initial.sql](migrations/0001_initial.sql);
read that file for column-level definitions, this file for
inter-feed merge logic.

The companion silvers, by path:

| Silver | Source | Coverage | Default path |
| --- | --- | --- | --- |
| `ubs-web` | Web scrape via Playwright + PDF reconstruction | Live-fetch tables: ~28-month transactions + on-demand positions + 1.5k PDF document index. Historical tables: quarterly position snapshots back to 2022 + monthly cash balances back to late 2021, both reconstructed from the bronze PDF archive | `~/wealthdb/ubs-web/ubs-web.db` |
| `ubs-psn` | UBS PSN nightly SFTP feed | Forward-only daily snapshots + events, from agreement go-live date | `~/wealthdb/ubs-psn/ubs-psn.db` |

## 1. Lambda architecture overview

```
                        ┌────────────────────────┐
   web archive   ─────► │   ubs-web silver       │ ──┐
   (Playwright)         │   (this repo)          │   │
                        └────────────────────────┘   ▼
                                                   ┌─────────────────────┐
                                                   │   wealthdb gold     │
                                                   │  (separate repo)    │
                                                   └─────────────────────┘
                                                     ▲
                        ┌────────────────────────┐   │
   PSN nightly  ─────►  │   ubs-psn silver       │ ──┘
   (SFTP)               │   (ubs-psn-dump)       │
                        └────────────────────────┘
```

Both silvers expose the same logical entities (accounts, portfolios,
positions, transactions) plus a few feed-specific extras. The web
silver is the **historical source of truth** (it carries multi-year
data PSN can't); PSN takes over from its activation date forward.

Gold's job is the merge:

- For every entity that exists in both silvers, ensure the records
  reconcile (same account, same portfolio, same instrument).
- For transactions specifically, splice on `value_date` at the PSN
  activation date per banking relationship.
- For positions, prefer PSN snapshots (daily, machine-format) over
  web snapshots when both cover the same day.
- For documents (PDFs), use web only — PSN has no equivalent.

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

## 3. Per-entity merge contracts

### 3.1 Banking relationships

An e-banking login can hold 1+ banking relationships.
PSN sees each as a separate SFTP endpoint (`SFTPCH01`, `SFTPCH02`,
…). Web sees each as an opaque `bankingRelationId` URL token.

**There is no shared key**, only proxies:

- `account_number_prefix` — the 12-char shared prefix of all
  account numbers under the relationship (`BBBB AAAAAAAA`).
  PSN's `AcctId` values can be sliced to derive the same prefix
  (positions 1–4 + reformat the middle). Gold can match on this
  prefix if it's unique enough across the user's relationships.
- `description` — manually populated. The web silver leaves
  `banking_relationships.description` empty by default; the user
  (or gold's config) fills in a human label that pairs each web
  relationship with its PSN counterpart.

**Recommended gold config schema:**

```yaml
banking_relationship_map:
  - web_relationship_id: <ubs-opaque-token-1>           # bankingRelationId from SPA URLs
    psn_relationship_id: SFTPCH01
    label: "Primary"
  - web_relationship_id: <ubs-opaque-token-2>
    psn_relationship_id: SFTPCH02
    label: "Secondary"
```

When the user runs `download.py` against a different relationship
(by first switching it in the UBS UI), a new
`banking_relationships` row appears with its own opaque token.
Gold's config pairs it with the right PSN side.

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
   UBS homepage as separate tiles (e.g. a managed CHF mandate, a
   USD discretionary portfolio). The web loader pulls one
   `positions_<sha>.csv` per portfolio via the `portfolioUid`
   anchors enumerated from the homepage.
2. **A synthetic catch-all portfolio** that the consolidated
   default view (`positions.csv`, the `preselectFirstPortfolio
   =true` route) uses to file accounts that aren't attached to
   any real portfolio — strategy-cash sub-accounts for
   alternative-investment products, fee / charges accounts, etc.
   The web loader processes per-portfolio CSVs first, then the
   consolidated CSV, and only inserts accounts + positions for
   `(account, isin)` pairs not yet seen. Net effect: real
   portfolio assignments win; the catch-all only ever owns the
   genuinely-unattached accounts.

**Web loader contract:** when parsing each positions.csv,
extract the trailing token of the "Portfolio" column (split on
whitespace, take the last word) as `portfolio_external_id`.
Store the full original string in `portfolio_full_id` for
traceability. Read the "Valued in: <CCY>" footer line and write
it as `portfolios.base_currency`.

**Gold join:** equality on `portfolio_external_id`. The
catch-all is a real row in silver; gold can treat it as either a
distinct portfolio or as "unassigned cash" depending on the
roll-up.

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
and stores it as a parallel column. **Gold can join on either side;
both are present in both silvers.**

#### Accounts PSN sees but web doesn't

PSN can report internal UBS booking accounts that the customer UI
does not expose. For gold:

- Use PSN data for these accounts unconditionally; web has no
  rows to splice in.
- Mark them with a flag (e.g. `psn_only=true`) in gold so dashboards
  can hide them if the user doesn't care about internal flows.

### 3.4 Safekeeping accounts (securities depots)

PSN exposes them directly (`safekeeping_accounts` table); web
does not surface them as first-class rows. The web `positions.csv`
flattens everything to portfolio + product, hiding the depot
layer.

**Today's contract:** gold uses PSN for safekeeping-account
metadata when present. Web `positions` rows can be aggregated
into PSN safekeeping accounts via the portfolio link
(`positions.portfolio_external_id` → `psn.holdings` which carries
both `safekeeping_external_id` and `isin`). The mapping isn't
strict 1-to-1 — multiple safekeepings can exist under one
portfolio — but for snapshots it's good enough.

**Future work:** if the wealthdb gold layer needs per-safekeeping
attribution from web data, we'd need to scrape the
"Investment positions (custody account)" navigation tree, which
exposes the depot hierarchy. Not done in v1.

### 3.5 Positions (snapshots)

Both silvers emit complete snapshots. Web is on-demand (one per
`download.py` run); PSN is daily.

**Use PSN for identity; fold web payload on top.** PSN's
holdings carry the full safekeeping / portfolio hierarchy and the
authoritative pricing snapshot per (safekeeping, instrument). The
web silver's `positions` table flattens that hierarchy (one
portfolio code per row, no safekeeping link, market value in
portfolio base currency only) — it cannot express the same
identity faithfully. So the gold layer:

```
identity   = PSN.holdings(snapshot_date, safekeeping, isin)
payload    = LEFT JOIN web.positions
              ON  web.portfolio_external_id = PSN.holdings.portfolio_external_id
              AND web.instrument_isin       = PSN.holdings.isin
```

This picks up web's `cost_price`, `lending_value`,
`lending_value_ratio`, `description`, etc. without trying to
flatten PSN through the web's narrower identity model.

For dates before PSN's activation, gold falls back to web
`positions` standalone — accepting the flattened identity model
for the pre-PSN window. The pricing in that window is whatever
the customer's snapshot captured (no canonical bank mark).

**Cash positions** (the "Liquidity - Accounts" rows in
positions.csv) join to PSN's `cash_balances` the same way:
join on `(snapshot_date, account_external_id)` and fold the web
columns. `account_external_id` is the canonical IBAN per §2 — it
joins directly.

### 3.6 Transactions (the splice)

This is the cleanest merge. Per the user's design:

> Matching individual transactions will not be necessary, if there
> is a clean way to splice the transaction history based on
> transaction dates.

**Splice key: `value_date`** (promoted column on both silvers).

For each cash account A and each banking relationship R:

```sql
-- pseudo-SQL
SELECT * FROM web_silver.transactions
 WHERE account_external_id = A
   AND value_date < cutoff_date(R)
UNION ALL
SELECT * FROM psn_silver.events
 WHERE account_external_id = A
   AND kind = 'cash_movement'
   AND timestamp >= cutoff_date(R)
```

`cutoff_date(R)` is the date PSN's feed went live for relationship
R. Pull from `psn.dump_runs` (`MIN(snapshot_at)`) or the user's
own config.

**Why this works:**

- PSN's `events.timestamp` is the MT940 booking/value date. Web's
  `transactions.value_date` is the CSV "Value date" column.
  Spot-checked: identical for cash movements in the overlap window.
- Both promote the same field as the splice key; no per-row
  matching required.
- **Inter-account transfers carry both sides.** UBS reuses the
  same `Transaction no.` for the debit row in the source account
  and the credit row in the destination account; the web silver
  uses a compound PK on `(transaction_external_id, account_
  external_id)` so both rows survive. Gold's per-account splice
  picks up the correct side automatically — no special handling
  needed.
- If both feeds happen to carry the same transaction for an
  overlap day (unlikely with a strict `<` vs `>=` boundary), the
  PSN row wins and the web row is silently dropped. Acceptable.

**Do not attempt per-row identity matching.** The two silvers carry
totally different transaction-ID schemes (web uses UBS's "Transaction
no." e.g. `0104030TJ0060041`; PSN derives event IDs from SWIFT
message references e.g. `mt515:…`). Empirically there is **zero
overlap** between the two ID spaces, so any cross-silver join on
`transaction_external_id = event_external_id` returns nothing.
The hard `<` vs `>=` cut on `value_date` is the only safe merge.

**Note on Trade date vs Value date.** Web's CSV has four dates per
row: Trade date, Trade time, Booking date, Value date. PSN MT940
exposes booking + value date. Use **Value date** as the splice
key on both sides for consistency.

### 3.7 Documents (PDFs)

Web-only. PSN has no document concept. Gold either:

- Surfaces them as a flat table (one row per PDF, indexed by date
  and type) for browse / audit, OR
- Joins them to accounts / portfolios via best-effort label
  parsing (the web loader populates `documents.account_external_id`
  / `documents.portfolio_external_id` when the listing-row label
  contains a discoverable IBAN / portfolio code).

The PDF binaries live on disk under
`<bronze-root>/<dump-ts>/documents/` — the silver `documents`
table only indexes them.

### 3.8 Historical snapshots reconstructed from PDFs

The web silver also reconstructs **historical** position + cash
snapshots from the PDF document archive. Two dedicated tables:

| Table | Source PDF type | Granularity |
| --- | --- | --- |
| `historical_position_snapshots` | "Statement of assets" PDFs (semi-annual, sometimes quarterly) | One row per `(as_of_date, portfolio, account, ISIN)`. Cash positions have `instrument_isin = NULL` and a populated `account_external_id` (IBAN); securities have `instrument_isin` set and `account_external_id = ''` (UBS doesn't surface the safekeeping account in the printed text in a way we can extract). |
| `historical_cash_balances` | "Account Statement" PDFs (monthly) | One row per `(period_end, account_external_id)` with opening / closing balance + turnover totals. UBS only issues an Account Statement for a given month when the account had activity in that month, so coverage is uneven; year-end months tend to cover the full account inventory. |

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
   fetches are intra-day customer-side snapshots. Gold can pick
   per use case (e.g. PDF snapshots are the right source for
   historical performance attribution; live fetch is the right
   source for "what does the customer see right now").

**Gold join.** For overlap dates where PSN is also present, gold
should prefer PSN for both identity and pricing. For dates that
predate PSN's go-live, `historical_position_snapshots` is the
canonical source. The portfolio identifier joins directly:

```sql
SELECT *
FROM web_silver.historical_position_snapshots
WHERE as_of_date < cutoff_date(R)
UNION ALL
SELECT *
FROM psn_silver.holdings  -- shaped equivalently
WHERE snapshot_at >= cutoff_date(R);
```

`historical_cash_balances` joins to the live `accounts` table via
the IBAN (`account_external_id`) — same canonical form on both
sides.

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
  with non-trivial activity — empirically ~37% of the corpus).
  Treat NULL as "the source PDF did not print that line" rather
  than as 0.0.

## 4. Web loader implementation notes

- **Migration runner.** Applies pending migrations in numeric
  order, commit after each, record version in `schema_meta`.
  Mirror `ubs-psn-dump/load.py`'s pattern.
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
  parsing failure; user re-runs after fixing.

## 5. Open questions for the gold layer

- **Multi-relationship sweep.** The web SPA only exposes the
  currently-selected relationship; the toolkit currently
  produces one silver per relationship per session. Gold can
  load multiple silvers but the operator needs to remember to
  switch + re-run. Could be automated in download.py later.
- **Cost-basis carryforward.** Web carries `cost_price`; PSN
  does not. Once gold cuts over to PSN, cost basis goes stale.
  Either re-run web periodically for snapshot refresh, or
  derive cost basis from web's transaction history (buy/sell
  events).
- **FX rates.** PSN has 980+ FX rates; web has only the 8
  CHF/* pairs in `positions.csv` footer. Gold should source FX
  from PSN (or from a third-party feed) for any base-currency
  conversion.
- **Document → instrument linking.** Trade confirmations and
  corporate-action notices reference ISINs in the PDF body. The
  web loader doesn't parse PDF bodies; today `documents` is
  only indexed by type + date + account. If gold wants per-ISIN
  document attribution, add a PDF text-extraction pass.

## 6. Sanity checks gold should run

- **Account-count parity per relationship.** Web's
  `COUNT(DISTINCT account_external_id) WHERE kind = 'cash'`
  should equal PSN's count minus the 2 internal accounts. A
  larger diff suggests a missed account on the web side
  (relationship-switch needed) or a stale silver.
- **Position market-value sum per portfolio per snapshot.**
  Web's `SUM(market_value_base)` for the active portfolio
  should match PSN's portfolio-performance totals (PSN's
  `TDPOPF` records emit a portfolio-level NAV) within a small
  rounding tolerance.
- **Transaction continuity.** No date gaps between the latest
  web `value_date` and the earliest PSN `events.timestamp` per
  account. A gap = the user missed a `download.py` window;
  alert and let them re-run for the missing dates.
