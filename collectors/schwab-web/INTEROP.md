# schwab-web ↔ schwab-api interop notes

A focused cross-repo memo for the `schwab-api` maintainer
and the `wealthdb` gold-layer maintainer. Extracted from
[DESIGN.md](DESIGN.md) §§4–5 plus the bronze observations from
the May 2026 first-end-to-end run. Two silvers, one user, one
gold layer that needs to converge them.

## TL;DR

- The two silvers' schemas are **aligned by column name** (`snapshot_at`,
  `account_external_id`, `payload` JSON, content dedup against the
  most-recent matching payload, synthetic-or-real `activity_id` as
  PK on transactions).
- But the **values** in `account_external_id` and `activity_id`
  inhabit **disjoint identifier spaces**. The two silvers cannot
  be natively joined.
- The gold layer needs an **explicit bridge per identifier**
  (full-account-number join for accounts; date-splice for
  transactions) and a **logical-document dedup** on the web side
  because Schwab regenerates PDFs per download.
- The schwab-api collector itself does **not** need to change. One nice-to-have
  suggested below; everything else is gold-layer work.

---

## 1. Account identifier mismatch

| | `schwab-web` | `schwab-api` |
| --- | --- | --- |
| Column | `accounts.account_external_id` | `accounts.account_external_id` |
| Value | 3-to-5-digit account suffix (e.g. `"NNN"`) | Schwab opaque `hashValue` |
| Source | `…NNN` text in the account-selector dropdown | `/accounts/accountNumbers` response |
| Joinable? | **No** — there's no public Schwab mapping |

### Gold-layer bridge

Every Schwab statement PDF has the full account number printed
in its first-page header (typically `Account Number:
1234-5678`). The web loader parses that header
(`pdf_parsers.parse_account_number` — both the 2017-2019 inline
form and the 2020+ label-and-value-on-consecutive-lines form)
and writes the value into `accounts.payload` under
**`account_number_full`**, on every snapshot row of the account.
The value is stored **exactly as printed — dash kept**; the key
is set only when all of an account's statements agree on the
number (a disagreement logs a warning and leaves the key
absent). Accounts loaded before the parser existed are
backfilled from the bronze statement PDFs on the next `load`
run (header-only re-read; no `--reparse` needed), and a key
first set by an incremental load is re-verified the same way
against every statement in bronze — the key's end state
depends only on the statements in the bronze tree, never on
the order they were loaded in. Then:

```sql
-- Bridge. Normalise to digits-only on both sides: the api's
-- accountNumber is undashed, the web value keeps the printed
-- dash.
SELECT api_acct.account_external_id  AS api_hash,
       web_acct.account_external_id  AS web_suffix
FROM api.accounts api_acct
JOIN web.accounts web_acct
  ON replace(json_extract(api_acct.payload, '$.accountNumber'),
             '-', '') =
     replace(json_extract(web_acct.payload, '$.account_number_full'),
             '-', '');
```

For accounts without the key (no parseable statement header, or
conflicting headers), gold falls back to matching the web
suffix against the trailing digits of the api account number.
The pivot key is the web suffix (the customer recognises it;
the api hashValue is opaque).

## 2. Transaction identifier mismatch

| | `schwab-web` | `schwab-api` |
| --- | --- | --- |
| Column | `transactions.activity_id` | `transactions.activity_id` |
| Value | Synthetic SHA-256 prefix of `<acct\|date\|amount\|description\|symbol\|index>` (sha256-independent since migration 0004) | Schwab-supplied `activityId` |
| Stable across re-loads? | Yes (deterministic; sha256-churn-safe since 0004) | Yes |
| Joinable? | **No** — different value spaces |

All four web feeds — `statement_pdf`, `tx_history_json`, `form_1099b`,
`third_party_distribution` — share the **same** synthetic-id scheme
(content + ordinal index, sha256-independent). So they collide
deterministically only when their content genuinely matches; in
general the same economic event carries a *different* id per feed and
gold reconciles by content, not by id (see §4 + §8).

### Gold-layer bridge

**Date-splice, do not per-row-match across the boundary.**

```python
api_coverage_start = MIN(api.transactions.timestamp PER ACCOUNT)

gold.transactions ⊇
    web.transactions  WHERE timestamp <  api_coverage_start
  ∪ api.transactions  WHERE timestamp >= api_coverage_start
```

The Trader API wins for any date it covers (real `activity_id`,
structured fields, no parser approximation). The web feed is the
backfill for pre-api dates. Overlap is typically ≤ 24h of
double-counting per account; gold can de-duplicate within a
narrow window by `(account, timestamp, amount, ±description)`
if it cares, but per-row matching across a multi-day boundary is
not reliable — the web parser drops some Sale-row amounts and
substitutes synthetic descriptions.

## 3. Schwab regenerates PDFs per download (sha256 churn)

Empirically observed across multiple `schwab-web` scrape
runs: the **same logical statement** (same account, same period,
same Schwab-supplied filename) downloaded in two different
sessions yields **different sha256s**. We've seen up to 4
distinct sha256s for one statement. Schwab almost certainly
stamps a generation timestamp or download-token into the PDF
on each request.

Effects on the web silver:

- `documents` is keyed on `sha256`, so it preserves every
  physical fetch (no data loss). But the **logical** document
  count is roughly half the row count.
- `transactions` are unaffected: the synthetic `activity_id` is
  sha256-independent and the load gates on `logical_doc_key`, so a
  re-download does not duplicate rows (see below).

### Gold-layer mitigation

Dedupe `documents` on `(account_external_id, doc_date,
doc_kind, filename)`, pick the earliest-`snapshot_at`
representative:

```sql
WITH logical_docs AS (
    SELECT account_external_id, doc_date, doc_kind, filename,
           MIN(snapshot_at) AS first_seen,
           MIN(sha256) AS canonical_sha256  -- arbitrary tiebreak
    FROM web.documents
    GROUP BY account_external_id, doc_date, doc_kind, filename
)
SELECT d.*
FROM web.documents d
JOIN logical_docs ld
  ON d.sha256 = ld.canonical_sha256;
```

Transactions are **already deduped at the silver level** since
migration 0004: `activity_id` is sha256-independent and
`INSERT OR IGNORE` prevents duplicate rows even if the same logical
statement is re-downloaded with a new sha256. Gold does **not**
need a second dedup pass for sha256-churn duplicates; the silver is
clean. The only remaining overlap gold needs to handle is the
two-source case (§7: `statement_pdf` and `tx_history_json` covering
the same logical event).

## 4. Tax-form structure has no api equivalent

The web feed downloads 1099 Composite as PDF + XML + CSV.
The XML carries structured tax-lot data (cost basis, term,
wash-sale flag, schedule breakdown) that the Trader API does
not surface — the api only emits `TRADE` activity rows without
IRS-level categorisation.

### Gold-layer mitigation

**The 1099-B parser now lives in silver** (`source='form_1099b'`,
DESIGN.md §6a): per-lot proceeds, cost basis, acquisition date,
term, and wash-sale flag land in the row `payload`. Gold projects
these into its `tax_lots` table and treats them as
authoritative-for-sales (see §8). Remaining 1099 sections (DIV /
INT / OID) are still unparsed. **Do not** modify the api silver —
the data simply isn't in the api.

## 5. No live position snapshots in web silver

The api silver emits positions per dump run; the web silver has
no live per-dump positions. Web position history is
statement-cadence instead: `historical_position_snapshots` holds
the per-statement holdings parsed from the statement PDFs
(DESIGN.md §4.5). The 1099-B parser (§4) yields sale lots, not
positions.

### Gold-layer mitigation

For pre-api dates, mark position gaps explicitly. Do **not**
interpolate. Mid-year positions for a historical date can be
recomputed from the statement-transaction feed
(silver carries the activity; gold can replay it).

---

## 6. Asks for `schwab-api` (none required, one nice-to-have)

The api silver already does the right thing in every spot we
checked. The one nice-to-have:

- **Promote `account_number` as a column on `accounts`.** The
  api already gets `accountNumber` back from
  `/accounts/accountNumbers`, and the payload JSON already
  carries it (per `schwab-api/migrations/0001` comments).
  Promoting it would skip a `json_extract` step in the gold
  bridge above. Low-priority; the payload pull is cheap.

Everything else is web-side work or gold-side work. The api
silver is correct as-is.

---

## 7. Open issues in `schwab-web` itself (for transparency)

These are tracked in [DESIGN.md §7](DESIGN.md) and don't block
gold-layer work — but the gold-layer author should know what
the web feed is and isn't carrying:

| Gap | Impact on gold |
| --- | --- |
| pdf_parsers occasionally returns `amount=None` on Sale rows (≈1% — concentrated on money-market-fund proceeds and a handful of early-2025 fee rows) | A handful of missing transactions per year; gold can detect via a row-count sanity check |
| Overlapping transaction sources (`statement_pdf` + `tx_history_json` + `form_1099b`) | Same logical event can land more than once with different synthetic `activity_id`s. Gold dedupes statement↔tx-history by (account, timestamp, amount, ±description) preferring `tx_history_json`, and lets `form_1099b` supersede sales in its tax year (§8) |
| Per-row "More"-modal data may be absent | Captured by default (~1 click/transaction); `--no-more-detail` opts out. When the sidecar is present, silver merges it into `payload._more` (Settle Date, CUSIP, Principal, Commission, Industry Fee) |
| `form_1099b` lots have no ticker/CUSIP — `security_name` only | Gold must bridge name → instrument (its symbol/CUSIP map), §8 |
| `third_party_distribution` cash transfers may overlap a statement cash debit | Gold dedupes cash distributions against external flows; securities distributions are new data (§8) |
| `account_number_full` is absent for accounts with no parseable (or conflicting) statement headers | Gold falls back to suffix matching for those accounts (see §1) |

---

## 8. Gold-layer hand-off: consuming the two new silver sources

> **Paired task — schedule alongside the silver change.** Until the
> gold reader (`wealthdb/internal/silver/schwab/web_reader.go`) handles
> these sources, `form_1099b` and `third_party_distribution` rows
> either double-count against the existing feeds or sit unused. The
> silver side is complete; this is the gold side of the same feature.
>
> **Status — implemented.** The gold side landed in `web_reader.go`:
> `supersedeSalesWith1099B` makes the 1099-B authoritative for sales
> within a covered `(account, tax_year)` (dropping the statement /
> tx-history sells in that calendar year), and
> `supersedeStatementCashWithDistributions` makes a cash distribution
> authoritative over a matching statement / tx-history cash debit.
> Securities transfers route through `externalFlowKinds`, drawing their
> net-flow magnitude from `market_value` / `cash_amount`.

### 8.1 `form_1099b` — authoritative-for-sales within its tax year

The 1099-B is the complete, cost-basis-bearing record of sales. The
`statement_pdf` and `tx_history_json` feeds carry sales too (partially
— the statement parser misses most real sales), so the three overlap
and must not be summed.

- **Precedence.** Within a `(account, tax_year)` that has any
  `form_1099b` rows, `form_1099b` is authoritative for **sales**:
  drop `statement_pdf` / `tx_history_json` rows that map to `TxKindSell`
  whose `timestamp` falls in that calendar year, and use the
  `form_1099b` lots instead. Outside covered tax years, keep the
  existing feeds. (Apply this *after* the existing JSON-authoritative
  splice and the cross-feed external-flow dedup, as a third stage.)
- **Cost basis.** Each lot's `payload` carries `proceeds`,
  `cost_basis` (nullable — `null` means Schwab did not report it; see
  the `basis_not_shown` / `noncovered` flags + `cost_basis_raw`),
  `acquired_date` (ISO, `"Various"`, or null), `term`, and
  `wash_sale_disallowed`. Surface proceeds as the sale net amount
  (canonical `TxKindSell` → positive) and basis as the lot's
  acquisition outlay.
- **Instrument resolution.** `form_1099b` rows have **no ticker or
  CUSIP** — `instrument_key` is `NULL`, the only identifier is
  `payload.security_name`. Gold must bridge the name to an instrument
  via its existing symbol/name map; lots that don't resolve should be
  surfaced as name-keyed rather than dropped.
- **Tax year** is in `payload.tax_year` (form content, not filename).

### 8.2 `third_party_distribution` — transfer flows out

- **Securities transfers** (`payload.transfer_kind == "securities"`)
  are **new** data — no other feed records them; the statements show
  them only as an unexplained position drop. Map `kind='Transfer Out'`
  → `TxKindTransferOut`; the row carries `symbol`/`instrument_key`,
  `quantity`, and `market_value` (the external-flow magnitude). These
  reconciled cleanly against position deltas in testing (the position
  drops by the transferred quantity across the letter date).
- **Cash transfers** (`payload.transfer_kind == "cash"`, `method` ∈
  {`wire`, `schwab_third_party`}) **may overlap** a `statement_pdf` /
  `tx_history_json` cash debit for the same movement. Run them through
  the existing cross-feed external-flow dedup (3-day / 0.5%-amount
  tolerance); when matched, treat `third_party_distribution` as
  authoritative (it names the counterparty). Do **not** sum a cash
  distribution and a matching statement debit.
- `payload` also carries `counterparty`, `counterparty_bank`,
  `counterparty_account_suffix`, and `direction` for the transfer-flow
  surface.

### 8.3 Gold-side implementation

- `sourceFORM1099B = "form_1099b"` and
  `sourceThirdPartyDistribution = "third_party_distribution"`
  constants sit alongside `sourceStatementPDF` / `sourceTxHistoryJSON`.
- `webKind` maps `"Transfer Out"` / `"Transfer In"` →
  `TxKindTransferOut` / `TxKindTransferIn`; `"Sale"` → `TxKindSell`
  covers the 1099-B rows.
- `supersedeSalesWith1099B` runs in `transactionsBeforeAPIStart`
  beside `spliceNonExternalToJSON` + `dedupeCrossFeedExternalFlows`:
  within any `(account, tax_year)` that has `form_1099b` lots, other
  feeds' sells are dropped and the 1099-B lots (with basis) kept.
- `third_party_distribution` rows route through `externalFlowKinds`
  so they affect `net_flow`; securities transfers carry a position
  effect and cash transfers get the dedup-against-statement
  treatment.
