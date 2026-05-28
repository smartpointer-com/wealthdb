# schwab-web-dump — design notes for the gold-layer merge

This document is the contract between `schwab-web-dump` and the
`wealthdb` gold layer that converges the web-scraped silver with
`schwab-api-dump`'s Trader-API silver. It supplements the
column-level commentary in
[migrations/0001_initial.sql](migrations/0001_initial.sql); read
that file for the schema, this file for the inter-feed merge
logic and for the issues gold has to bridge that aren't fixable
in either silver alone.

The companion silvers:

| Silver | Source | Coverage | Default path |
| --- | --- | --- | --- |
| `schwab-web` | Web scrape via Playwright + camoufox, PDF parsing | Multi-decade historical: statement PDFs back to 2016 (Schwab's UI cap), tax forms back to 2014, transaction-history HTML drops for the same window | `~/wealthdb/schwab-web/schwab-web.db` |
| `schwab-api` | Trader API via OAuth refresh-token | Forward-only daily snapshots + transactions, from API access activation (mid-2024 for this user) | `~/wealthdb/schwab-api/schwab-api.db` |

This split mirrors the `ubs-web-dump` ↔ `ubs-psn-dump` pattern: a
slow, lossy, multi-year web archive plus a fast, lossless, recent
machine feed.

## 1. Architecture overview

```
                        ┌────────────────────────┐
   client web   ─────►  │   schwab-web silver    │ ──┐
   (Playwright +        │   (this repo)          │   │
    camoufox)           └────────────────────────┘   ▼
                                                   ┌─────────────────────┐
                                                   │   wealthdb gold     │
                                                   │  (separate repo)    │
                                                   └─────────────────────┘
                                                     ▲
                        ┌────────────────────────┐   │
   Trader API   ─────►  │   schwab-api silver    │ ──┘
   (OAuth)              │   (schwab-api-dump)    │
                        └────────────────────────┘
```

Both silvers expose the same logical entities (accounts,
documents-or-positions, transactions) but on **different
identifier spaces** and with **different field fidelity**. The
gold layer is responsible for the alignment.

The web silver is the **historical source of truth** for any
date the API doesn't cover. From the API activation date forward,
the API silver wins (it has stable `activity_id`, structured
positions, and dollar-accurate balances; the web silver has none
of those without screen-scraping work that's lossy by nature).

## 2. Identifier conventions

This section is the source of the irreconcilable differences in
§4. The two silvers share NO joinable native keys; gold has to
bridge them manually.

### 2.1 `account_external_id`

| | web silver | api silver |
| --- | --- | --- |
| Value | Last-3-to-5-digit account suffix (e.g. `"NNN"`) | Schwab opaque `hashValue` (e.g. `"E5A0...A1F2"`) |
| Source | `…NNN` text rendered next to the account name in the dropdown | `/accounts/accountNumbers` response |
| Stable? | Yes (Schwab doesn't recycle suffixes within a profile) | Yes |
| Joinable across silvers? | **No** — see §4.1 |

### 2.2 `activity_id` (transactions table)

| | web silver | api silver |
| --- | --- | --- |
| Value | Synthetic hex SHA-256 prefix of `<acct\|date\|amount\|description\|symbol\|index\|source_sha256>` | Schwab-supplied `activityId` from `/accounts/{hash}/transactions` |
| Stable across re-loads? | Yes (deterministic) | Yes |
| Joinable across silvers? | **No** — see §4.2 |

### 2.3 `instrument_key`

Both silvers use the convention `CUSIP > ticker symbol`. In
practice:

- **api silver** consistently has CUSIP for fixed-income +
  options + most equities (Schwab populates `cusip` on the
  `instrument` sub-object); falls back to `symbol` for the few
  asset classes Schwab leaves uncusiped (notably money-market
  fund proxies).
- **web silver** has whatever pdf_parsers picks out of the
  statement PDF's activity rows — typically the ticker only.
  CUSIP is in the statement's holdings/positions block, which
  pdf_parsers currently does not extract.

Gold can resolve `web.instrument_key` → CUSIP via api's
`instruments` table by symbol match. Pre-API-coverage dates have
no such bridge; the gold layer should retain the ticker form for
those rows.

## 3. Per-table mapping

| api silver table | web silver counterpart | Notes |
| --- | --- | --- |
| `schema_meta` | `schema_meta` | Identical convention |
| `dump_runs` | `dump_runs` | Identical convention; `snapshot_at` is the bronze dir's UTC timestamp on both |
| `accounts` | `accounts` | Both promote `nickname`. api also has `account_type` / `preference_type`; web doesn't expose either. Different `account_external_id` value space — see §2.1 |
| `user_preference` | — | api-only |
| `account_balances` | — | api-only. Web exposes balances only via the year-end summary PDFs, which pdf_parsers does not currently extract |
| `positions` | — | api-only on a per-snapshot basis. Web has annual snapshots in the 1099 Composite detail; not yet parsed |
| `open_orders` | — | api-only |
| `transactions` | `transactions` | Same column shape. Different `activity_id` value space — see §2.2 |
| `instruments` | — | api-only (and only when `--with-instruments`) |
| — | `documents` | web-only. One row per downloaded PDF/XML/CSV, sha256-deduped |

## 4. Irreconcilable differences (and gold-layer mitigations)

### 4.1 Account identity is different in the two feeds

`web.accounts.account_external_id` is the 3-5-digit account
suffix; `api.accounts.account_external_id` is Schwab's opaque
`hashValue`. There is no Schwab-side public mapping.

**Bridge**: every statement PDF the web scrape downloads has the
FULL account number embedded in two places:

1. The Schwab-supplied download filename's middle field —
   `Brokerage-Statement_<YYYY-MM-DD>_<suffix>.PDF`. This carries
   only the suffix; not useful.
2. The first page of the PDF text, in a header line like
   `Account Number: 1234-5678` (8-digit format). pdf_parsers does
   not currently extract this; **TODO for the gold layer or for
   a follow-up parser pass**: read account_number once per
   account from a statement, write it into `accounts.payload`,
   and have gold join api↔web through that.

Alternative: a hand-maintained map (suffix → hashValue) in `wealthdb`. Lower-effort but requires manual maintenance.

### 4.2 Transaction identity is different in the two feeds

`web.transactions.activity_id` is a deterministic SHA-256 of the
row's promoted columns; `api.transactions.activity_id` is
Schwab's internal activity id. Same logical transaction, two
different ids.

**Bridge**: at the splice boundary (api activation date), gold
should:

1. Compute `api_coverage_start = MIN(api.transactions.timestamp)`
   per account.
2. Apply:
   ```
   gold.transactions ⊇
       web.transactions  WHERE timestamp <  api_coverage_start
     ∪ api.transactions  WHERE timestamp >= api_coverage_start
   ```
3. Do NOT attempt per-row matching across the boundary — the web
   parser can't reliably extract every field the api carries, and
   per-row joins on (account, timestamp, amount, description)
   would have spurious matches and misses.

The cost of this approach: any transactions that appear in BOTH
feeds (the overlapping few days at the boundary) are counted
twice. The overlap is typically ≤24 hours per account; gold can
deduplicate by best-effort key `(account, timestamp, amount,
±description)` within a narrow window if it cares.

### 4.3 Tax-form data has no api equivalent

The 1099 Composite XML (downloaded by the web scrape) carries
structured tax-lot detail: cost basis, term, wash-sale flag,
schedule breakdown. The api emits only TRADE transactions; it
does NOT expose the IRS-level tax categorisation.

**Gold-layer recommendation**: parse the web silver's
`documents` table for XML files with `doc_kind = 'tax_form'`,
extract the structured per-lot data with a 1099-aware parser
(NOT YET WRITTEN), and project it into a gold-only `tax_lots`
table. The api silver should NOT be modified — the data simply
isn't in the api.

### 4.4 Schwab regenerates PDFs per download — sha256 dedup is byte-level only

Empirically: the same *logical* statement (same account, same
period, same filename) downloaded from Schwab in two different
sessions yields **different sha256s**. An archive can show
the same (account, doc_date, filename) tuple appearing with up
to 4 distinct sha256s across scrape runs. Schwab almost
certainly stamps a generation timestamp or download-token into
the PDF on each request, making each physical fetch unique at
the byte level.

The silver `documents` table is keyed on `sha256` — so it
preserves every physical fetch (no data loss), but the
LOGICAL document count is roughly half the row count. In the
current snapshot:

```
total docs                                 929
unique (account_external_id, doc_date, filename)   467
```

**Gold-layer mitigation**: when consuming web silver
`documents`, deduplicate on `(account_external_id, doc_date,
doc_kind, filename)` rather than `sha256`. Pick any one
representative row per logical doc (e.g. `MIN(snapshot_at)` —
the first time we saw the logical doc). The `transactions`
table is unaffected: synthetic `activity_id` is derived from
`source_sha256`, so even when two PDFs of the same statement
have different sha256s, the transaction rows extracted from
each have **different `activity_id`s** and BOTH get inserted.
Gold should also dedupe transactions by
`(account_external_id, timestamp, payload-key-subset)` if it
cares — silver fidelity is preserved.

### 4.5 No per-position snapshot in web silver

api silver has live position snapshots per dump run. Web silver
captures positions only annually via the year-end summary tax
form, and not currently parsed. Gold's position history before
api activation will be sparse: one snapshot per year, at
year-end, derived from the 1099 if the parser is implemented.

**Gold-layer recommendation**: don't try to interpolate
mid-year positions for pre-api dates. Mark gaps explicitly. The
statement transactions feed gives ENOUGH activity context to
recompute positions retroactively if the user really needs them,
but that's a gold-layer derivation, not a silver one.

## 5. Recommendations for schwab-api-dump

These are nice-to-haves for the api-dump maintainer; the web
silver doesn't strictly need any of them.

- **Persist a stable mapping `hashValue → accountNumber` in
  `accounts.payload`**. The api already gets `accountNumber`
  back from `/accounts/accountNumbers`; per-account JSON in
  `accounts.payload` should carry it verbatim so gold can join
  to web by suffix without an extra Schwab call. (Looking at
  the migration 0001 comments, this *is* the case — the
  payload stores `{accountNumber, hashValue}`. Good. The only
  thing gold needs to know is that the LAST 3 digits of
  `accountNumber` equal the web silver's `account_external_id`.)
- **Optionally surface the full Schwab account-number string in
  the promoted columns**. The current schema has only the
  hashValue promoted; widening to `account_number` would skip a
  JSON-parse step in gold. Low priority — the payload pull is
  cheap.

## 6. Bronze artefact layout (for reference)

```
<bronze-root>/
└── <UTC-timestamp>/                e.g. 20260520T120000Z/
    ├── run.json                    manifest written by download.walk()
    ├── statements/
    │   └── <suffix>/
    │       ├── Brokerage-Statement_2026-04-30_<suffix>.PDF
    │       ├── 1099-Composite-and-Year-End-Summary_2026-02-21_<suffix>.PDF
    │       ├── 1099-Composite-and-Year-End-Summary_2026-02-21_<suffix>.XML
    │       └── 1099-Composite-and-Year-End-Summary_2026-02-21_<suffix>.CSV
    └── transactions/
        └── <suffix>/
            ├── <Nick>_XXX<suffix>_Transactions_<ts>.csv
            ├── <Nick>_XXX<suffix>_Transactions_<ts>.json   ← row source
            ├── <Nick>_XXX<suffix>_Transactions_<ts>.xml
            ├── page-001.html       debug snapshot of the landing view
            └── more-details.json   optional: per-row "More"-modal
                                    contents when `download` was
                                    invoked with --with-more-detail
```

The silver loader reads `run.json` for the document inventory and
the on-disk PDFs (for statement parsing) + JSON (for tx-history
row ingestion, source = `'tx_history_json'`). CSV / XML are
sha256-keyed into `documents` for traceability but their rows
aren't re-parsed — JSON carries the same set plus Schwab's
`AcctgRuleCd`. If `more-details.json` is present, the loader
merges each record into the matching transaction's `payload`
under `_more` (keyed by `_tx_history_row_key`).

## 7. Known gaps (not blockers for the gold merge, but worth noting)

- **Statement-PDF parser handles three layout eras** (2017-2019
  bare "Investment Detail" / "Transaction Detail"; 2020-2024
  "Investment Detail - X" / "Transaction Detail - X"; 2025+
  "Positions - X" / "Transaction Details"). Each tier is tried
  in turn; the loader still passes `statement_year` as a
  fallback for the few pre-2025 quarterly headers that pdfplumber
  / pypdfium2 didn't surface.
- **Some sale rows lose their amount** (concentrated on
  money-market-fund proceeds and a handful of early-2025 fee
  rows). Skipped during load with a warning rather than
  failing the whole statement; about 1% of rows in the
  archive we tested against. Worth a follow-up parser pass.
- **Two parallel transaction sources**. After enabling the
  tx-history JSON ingest, the same logical event lands in
  silver from both `source='statement_pdf'` (parser-derived,
  multi-year coverage via quarterly statements) AND
  `source='tx_history_json'` (Schwab-rendered, ~4-year coverage
  for "All" date range). They have different synthetic
  `activity_id`s (different `source_sha256` in the hash input),
  so both rows insert without UNIQUE conflict. Gold should
  treat them as redundant feeds and dedupe by `(account,
  timestamp, amount, ±description)`; prefer tx_history_json
  where both exist (it has cleaner field separation).
- **1099 Composite XML/CSV are stored but not parsed for
  tax-lot detail**. Gold-only consumer can pick them up by
  filename from the `documents` table.
- **`--with-more-detail` is implemented but not enabled by
  default** — it adds ~1 modal click per transaction, on the
  order of an hour per ~5k-tx account. Use it for a one-off
  enrichment pass; routine runs should leave it off.

## 8. `account_registration` column (migration 0003)

Schwab's Trader API doesn't surface the per-account tax
wrapper (see `schwab-api-dump/DESIGN.md` §4.10). The web feed
does — every statement PDF prints the registration label at
the top of page 1, adjacent to the account number. Migration
0003 promotes that label to a top-level column on `accounts`:

```sql
ALTER TABLE accounts ADD COLUMN account_registration TEXT;
```

The column carries the **raw** Schwab label, verbatim, so the
wealthdb gold adapter can do the mapping to its canonical
`tax_wrapper` enum in Go where that enum is defined.
`pdf_parsers.parse_account_registration` handles all three
layout eras (see §7); the loader UPDATEs the column once per
load run, using the first non-null label it sees per account.
A re-load of newer statements overwrites with the most recent
seen value.

### 8.1 Registration labels

These are registration labels Schwab is known to print — i.e.
the values the `account_registration` silver column can carry. Schwab's typography drifts a little
across layout revisions — the registered-mark glyph migrates
between "Schwab One® International Account" and "Schwab One International®
Account" for the same account. The wealthdb adapter should treat the two
forms as equivalent.

  Schwab label (verbatim)              → wealthdb tax_wrapper
  ─────────────────────────────────────────────────────────────
  Schwab One® Account                  → taxable_personal
  Schwab One® International Account    → taxable_personal
  Schwab One International® Account    → taxable_personal
  Brokerage Account                    → taxable_personal
  Schwab One® Custodial Account (UTMA) → custodial_utma
  Schwab One® Custodial Account (UGMA) → custodial_ugma
  Schwab One® Custodial Account        → custodial_utma OR
                                         custodial_ugma — bare
                                         label means silver
                                         could not disambiguate
                                         (see §8.2)
  Contributory IRA                     → traditional_ira
  Roth IRA                             → roth_ira
  Rollover IRA                         → traditional_ira
  Inherited IRA                        → traditional_ira
  SEP-IRA                              → sep_ira
  SIMPLE IRA                           → simple_ira
  Education Savings                    → coverdell_esa
  Coverdell Education Savings Acct     → coverdell_esa
  529 College Savings Plan             → 529
  Solo 401(k) / Individual 401(k)      → 401k
  Trust Account                        → trust_non_grantor

### 8.2 UTMA vs UGMA — resolved via holder-block markers

Schwab prints "Schwab One® Custodial Account of" as the
header line regardless of the underlying UTMA / UGMA
structure, so the header alone is not enough. Silver bridges
the gap by scanning the holder block immediately below the
header (still on page 1) for a "<state>UTMA" /
"<state>UGMA" marker (e.g. `TXUTMA` for a Texas UTMA) and
promotes the label to
`Schwab One® Custodial Account (UTMA)` or
`Schwab One® Custodial Account (UGMA)`. Silver does the
refinement because it understands the Schwab statement
format intimately; the wealthdb gold adapter then keys off a
single column without having to re-read bronze.

If neither marker is present (defensive — custodial statements carry one), the bare
`Schwab One® Custodial Account` is preserved and the gold
adapter can default to `custodial_utma` (UTMA is the modern,
near-universal standard).

This is the only place silver consults content below the
registration header. Every other wrapper distinction Schwab
makes IS in the header line ("Contributory IRA of", "Roth
IRA of", "Education Savings of", etc.), so the header alone
is sufficient for every wrapper except the UTMA / UGMA
split.

### 8.3 Tax-form filename fallback

For accounts whose statements don't yield a parseable
registration (e.g. a brand-new account that hasn't received
its first monthly statement, or one in an unparseable layout
era), the loader falls back to the documents table:

  `5498-ESA*.PDF`  →  "Education Savings"
  `5498*.PDF`      →  "Contributory IRA"  (generic-IRA default;
                                           Roth / Inherited /
                                           SEP / SIMPLE require
                                           5498-body parsing
                                           which we don't do —
                                           gold can refine via
                                           documents.sha256)

5498 forms are uniquely issued for IRA / ESA accounts, so
their existence is itself a definitive signal. The fallback
does NOT touch the column if a statement label was already
landed; it only sets a value when the column was NULL.

### 8.4 Sanity histogram

At the end of every load, the loader logs the per-account
registration as a fixture-free smoke test:

  account_registration after load:
    …NNN  Schwab One® Account
    …NNN  Brokerage Account
    ...
    (W: K account(s) have NULL account_registration ...)

The WARNING line is suppressed when every account got a
label.
