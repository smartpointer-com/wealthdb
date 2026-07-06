# schwab-web — design notes for the gold-layer merge

Part of the **wealthdb** suite — see [the architecture overview](../../ARCHITECTURE.md) for the bronze → silver → gold model and [collectors/README.md](../README.md) for shared collector conventions.

This document is the contract between `schwab-web` and the
`wealthdb` gold layer that converges the web-scraped silver with
`schwab-api`'s Trader-API silver. It supplements the
column-level commentary in
[migrations/0001_initial.sql](migrations/0001_initial.sql); read
that file for the schema, this file for the inter-feed merge
logic and for the issues gold has to bridge that aren't fixable
in either silver alone.

The companion silvers:

| Silver | Source | Coverage | Default path |
| --- | --- | --- | --- |
| `schwab-web` | Web scrape via Playwright + camoufox, PDF parsing | Multi-decade historical: statement PDFs back to 2016 (Schwab's UI cap), tax forms back to 2014, transaction-history HTML drops for the same window | `$XDG_DATA_HOME/wealthdb/schwab-web/schwab-web.db` |
| `schwab-api` | Trader API via OAuth refresh-token | Forward-only daily snapshots + transactions, from API access activation (mid-2024) | `$XDG_DATA_HOME/wealthdb/schwab-api/schwab-api.db` |

This split mirrors the `ubs-web` ↔ `ubs-psn` pattern: a
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
                                                   │  (sibling: wealthdb)│
                                                   └─────────────────────┘
                                                     ▲
                        ┌────────────────────────┐   │
   Trader API   ─────►  │   schwab-api silver    │ ──┘
   (OAuth)              │   (sibling: schwab-api)│
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
| Value | Synthetic hex SHA-256 prefix of `<acct\|date\|amount\|description\|symbol\|index>` (sha256-independent since migration 0004) | Schwab-supplied `activityId` from `/accounts/{hash}/transactions` |
| Stable across re-loads? | Yes (deterministic; sha256-churn-safe since 0004) | Yes |
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
  CUSIP lives in the statement's holdings/positions block;
  pdf_parsers extracts that block (`parse_positions` →
  `historical_position_snapshots`), but those rows too usually
  surface only the ticker.

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
| `account_balances` | `historical_cash_balances` | api has live per-dump balances; web parses the monthly statement cash-flow summary (opening/closing) into `historical_cash_balances` |
| `positions` | `historical_position_snapshots` | api has live per-dump positions; web parses the monthly/quarterly statement holdings block into `historical_position_snapshots` |
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
LOGICAL document count is roughly half the row count.

**Silver mitigation (since migration 0004)**: the `transactions`
table is now sha256-churn-safe. `activity_id` no longer includes
`source_sha256`; the load gate uses `logical_doc_key`
(`account|doc_date|filename`) instead of `source_sha256`; and
`INSERT OR IGNORE` on the `activity_id` PK prevents row-level
duplicates. Re-downloading the same logical PDF with a new sha256
is a clean no-op for transactions. **Gold no longer needs a
transaction dedup pass for this case.**

**Gold-layer mitigation for `documents`**: when consuming web silver
`documents`, deduplicate on `(account_external_id, doc_date,
doc_kind, filename)` rather than `sha256`. Pick any one
representative row per logical doc (e.g. `MIN(snapshot_at)` —
the first time we saw the logical doc). The `transactions` table
is clean.

### 4.5 Web position snapshots are statement-cadence, not live

api silver has live position snapshots per dump run. Web silver
captures positions at statement cadence — `parse_positions` reads
each monthly/quarterly statement's holdings block into
`historical_position_snapshots` (and the cash-flow summary into
`historical_cash_balances`). So pre-api position history is one
snapshot per statement period, not the per-dump granularity the
api gives.

**Gold-layer recommendation**: don't try to interpolate
mid-year positions for pre-api dates. Mark gaps explicitly. The
statement transactions feed gives ENOUGH activity context to
recompute positions retroactively when needed,
but that's a gold-layer derivation, not a silver one.

## 5. Recommendations for schwab-api

These are nice-to-haves for the schwab-api maintainer; the web
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
    ├── run.json                    manifest written by download.walk();
    │                               carries a `status` field
    │                               (in-progress → complete / dry-run)
    ├── statements/
    │   └── <suffix>/
    │       ├── Brokerage-Statement_2026-04-30_<suffix>.PDF
    │       ├── 1099-Composite-and-Year-End-Summary_2026-02-21_<suffix>.PDF
    │       ├── 1099-Composite-and-Year-End-Summary_2026-02-21_<suffix>.XML
    │       └── 1099-Composite-and-Year-End-Summary_2026-02-21_<suffix>.CSV
    ├── transactions/
    │   └── <suffix>/
    │       ├── <Nick>_XXX<suffix>_Transactions_<ts>.csv
    │       ├── <Nick>_XXX<suffix>_Transactions_<ts>.json   ← row source
    │       ├── <Nick>_XXX<suffix>_Transactions_<ts>.xml
    │       └── more-details.json   optional: per-row "More"-modal
    │                               contents when `download` was
    │                               invoked with --with-more-detail
    └── screenshots/                debug-only (download --debug):
        └── tx-<suffix>-landing.html   landing-view HTML baseline. Never
                                    read by load; `prune` reclaims the
                                    whole screenshots/ dir from a
                                    complete dump. Absent without --debug.
```

The `status` field is the completeness signal `prune` and `load`
key on: `download.walk()` writes `"in-progress"` at run-dir
creation and atomically overwrites it with `"complete"` (or
`"dry-run"` for a `--dry-run`, whose tx-history exports still fire)
at the end. `load` skips a dump whose `status` is `"in-progress"`
or `"dry-run"` (a statusless legacy manifest stays loadable); `prune`
deletes whole non-complete dumps plus `<run>/screenshots/` from
complete ones, and never a `load` input (`statements/`,
`transactions/`, `run.json`).

The silver loader reads `run.json` for the document inventory and,
per document kind, parses these into `transactions`:

- **Statement PDFs** → `source='statement_pdf'` (also positions +
  cash, see §5).
- **Tx-history JSON** → `source='tx_history_json'`. The tx-history
  CSV / XML twins are sha256-keyed into `documents` for traceability
  but not re-parsed — the JSON carries the same set plus Schwab's
  `AcctgRuleCd`. If `more-details.json` is present, the loader merges
  each record into the matching transaction's `payload` under `_more`
  (keyed by `_tx_history_row_key`).
- **1099-Composite XML / CSV** → `source='form_1099b'` (the 1099-B
  sale lots; see §6a). XML preferred over the CSV twin; the PDF copy
  stays an opaque document.
- **3rd-Party-Distribution letters** (the `Letters` doc kind) →
  `source='third_party_distribution'` (securities + cash transfers
  out; see §6b).

### 6a. 1099-B sale lots (`source='form_1099b'`)

The 1099 Composite is the **authoritative annual record of sales**,
carrying cost basis and acquisition date per lot — data the
statement parser cannot see when a sale prints only as a bare
position delta rather than as an activity row. Parsed by
[`tax_form_parsers.py`](tax_form_parsers.py).

- **Format precedence.** Schwab ships the form as PDF + XML + CSV
  twins sharing one base filename. We prefer the **XML** (OFX-2.x,
  cleaner per-field structure, an explicit `DTVAR` "Various" flag, a
  `TAXYEAR` element), fall back to the **CSV**, and leave the PDF as
  an opaque document. The `logical_doc_key` keys on the **base
  filename without extension**, so the XML and CSV twins dedup to one
  set of rows (whichever is parsed first wins; `--reparse` clears all
  formats at once).
- **Tax year** is taken from the content (`TAXYEAR`, cross-checked
  against the sold-date year), never the unreliable `.N` filename
  suffix.
- **One row per lot**, `kind='Sale'`. The payload carries
  `proceeds`, `cost_basis`, `acquired_date` (ISO / `'Various'` /
  null), `term`, `wash_sale_disallowed`, `quantity`, `security_name`,
  and the `noncovered` / `basis_not_shown` flags.
- **Cost-basis honesty.** A noncovered lot whose basis Schwab does
  not know renders `COSTBASIS=0`; we null the promoted `cost_basis`
  for those (keeping the raw `0` + flags in payload) so a placeholder
  is never mistaken for a real zero. A genuine `$0` basis on a
  *covered* lot (e.g. a lot with a zero basis) is preserved.
- **No ticker / CUSIP.** Neither the XML nor the CSV carries a
  security identifier — only `security_name`. `instrument_key` is
  therefore `NULL`; resolving the name to an instrument is gold's job
  (see INTEROP.md).

### 6b. 3rd-Party-Distribution transfers (`source='third_party_distribution'`)

These `Letters`-kind PDFs are the **only record of securities (and
cash) moved out of an account to a third party** — gifts to people /
DAFs, transfers to other custodians or accounts. The statements show
such a move only as an unexplained position drop. Parsed by
`pdf_parsers.parse_distribution_pdf` (text via the same pypdfium2
extractor the statement parser uses — a bake-off showed it recovers
every required field on every form, ~5× faster than poppler/pdftotext
and with no extra system dependency).

- Three observed layout families, all outbound: a **wire** ("To the
  account of NAME at BANK"), a **cash transfer to a Schwab third
  party** ("Account name: NAME"), and a **securities transfer** (a
  `Symbol / Quantity / Market Value` table).
- **One row per security line** for a securities transfer (one for a
  cash transfer), `kind='Transfer Out'`. The payload carries
  `transfer_kind` (`securities` / `cash`), `method` (`wire` /
  `schwab_third_party`), `counterparty`, `counterparty_bank`,
  `counterparty_account_suffix`, and `symbol` / `quantity` /
  `market_value` (securities) or `cash_amount` (cash). Securities
  transfers set `instrument_key` to the ticker.
- An unrecognised layout emits **no rows** and logs a warning, rather
  than corrupting the load.

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
- **Multiple overlapping transaction sources**. The same logical
  event can land in silver from more than one feed:
  `statement_pdf` (parser-derived, multi-year via quarterly
  statements), `tx_history_json` (Schwab-rendered, ~4-year "All"
  range), and now `form_1099b` (authoritative sales within a tax
  year). They get different synthetic `activity_id`s (their content
  fields differ — the id no longer depends on `source_sha256` since
  migration 0004), so all rows insert without UNIQUE conflict. Gold
  reconciles them: dedupe `statement_pdf` ↔ `tx_history_json` by
  `(account, timestamp, amount, ±description)` preferring
  tx_history_json; and treat `form_1099b` as authoritative-for-sales
  within its covered tax year (see INTEROP.md §4 + §8).
- **1099 Composite sales are now parsed** into `form_1099b`
  (§6a). Remaining 1099 sections (DIV / INT / OID / MISC) are not
  parsed; tax years available only as a PDF (no XML/CSV twin) are
  skipped with a logged `form_1099b_pdf_only` count.
- **3rd-Party-Distribution transfers are now parsed** into
  `third_party_distribution` (§6b). These are mostly new data the
  other feeds lack; cash transfers may overlap a statement cash
  debit and are gold's to dedupe (INTEROP.md §8).
- **`--with-more-detail` is implemented but not enabled by
  default** — it adds ~1 modal click per transaction, on the
  order of an hour for a high-activity account. Use it for a one-off
  enrichment pass; routine runs should leave it off.

## 8. `account_registration` column (migration 0003)

Schwab's Trader API doesn't surface the per-account tax
wrapper (see `schwab-api/DESIGN.md` §4.10). The web feed
does — every statement PDF prints the registration label at
the top of page 1, adjacent to the account number. Migration
0003 promotes that label to a top-level column on `accounts`:

```sql
ALTER TABLE accounts ADD COLUMN account_registration TEXT;
```

The column carries the **raw** Schwab label, verbatim.
`pdf_parsers.parse_account_registration` handles all three
layout eras (see §7); the loader UPDATEs the column once per
load run, using the first non-null label it sees per account.
A re-load of newer statements overwrites with the most recent
seen value. How gold maps this label to its canonical
`tax_wrapper` enum is owned by the wealthdb Schwab adapter — see
[the adapter doc](../../wealthdb/docs/adapters/schwab.md).

### 8.1 Registration labels

These are registration labels Schwab is known to print — i.e.
the values the
`account_registration` silver column can carry. Schwab's
typography drifts a little across layout revisions: the
registered-mark glyph migrates between "Schwab One® International Account" and "Schwab One International®
Account" for the same account, so the column holds both
forms across snapshots. (How gold collapses such variants and maps
them to canonical wrappers is the adapter's job — see
[the adapter doc](../../wealthdb/docs/adapters/schwab.md).)

  Schwab One® Account
  Schwab One® International Account
  Schwab One International® Account
  Brokerage Account
  Schwab One® Custodial Account (UTMA)
  Schwab One® Custodial Account (UGMA)
  Schwab One® Custodial Account        (bare — silver could not
                                        disambiguate UTMA vs UGMA;
                                        see §8.2)
  Contributory IRA
  Roth IRA
  Rollover IRA
  Inherited IRA
  SEP-IRA
  SIMPLE IRA
  Education Savings
  Coverdell Education Savings Acct
  529 College Savings Plan
  Solo 401(k) / Individual 401(k)
  Trust Account

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
`Schwab One® Custodial Account` is preserved in the column; how
gold resolves the undisambiguated case is the adapter's call —
see [the adapter doc](../../wealthdb/docs/adapters/schwab.md).

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
