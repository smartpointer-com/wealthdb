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
  (manual map for accounts; date-splice for transactions) and a
  **logical-document dedup** on the web side because Schwab
  regenerates PDFs per download.
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
1234-5678`). The web-silver `documents` table already references
every PDF on disk; a small parser pass — currently TODO — can
extract `account_number_full` once per (`account_external_id`,
earliest snapshot) and write it into `accounts.payload`. Then:

```sql
-- Bridge:
SELECT api_acct.account_external_id  AS api_hash,
       web_acct.account_external_id  AS web_suffix
FROM api.accounts api_acct
JOIN web.accounts web_acct
  ON json_extract(api_acct.payload, '$.accountNumber') =
     json_extract(web_acct.payload, '$.account_number_full');
```

Until the parser pass lands, **maintain a manual map in the gold
config**: `{api_hashValue: web_suffix}`. The pivot key is the
web suffix (the customer recognises it; the api hashValue is
opaque). One-line lookup.

## 2. Transaction identifier mismatch

| | `schwab-web` | `schwab-api` |
| --- | --- | --- |
| Column | `transactions.activity_id` | `transactions.activity_id` |
| Value | Synthetic SHA-256 prefix of `<acct\|date\|amount\|description\|symbol\|index>` (sha256-independent since migration 0004) | Schwab-supplied `activityId` |
| Stable across re-loads? | Yes (deterministic; sha256-churn-safe since 0004) | Yes |
| Joinable? | **No** — different value spaces |

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
- `transactions` follows: synthetic `activity_id` includes
  `source_sha256`, so each physical PDF yields a distinct set
  of `activity_id`s. Same transaction, two activity_ids.

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

Add a **1099-XML parser** in the gold layer (or as a follow-up
in `schwab-web.load.py`); project the per-lot detail into
a gold-only `tax_lots` table. **Do not** modify the api silver —
the data simply isn't in the api.

## 5. No live position snapshots in web silver

The api silver emits positions per dump run; the web silver
captures them only annually via the 1099-B detail, and is not
yet parsed. Position history for pre-api dates will be sparse:
one snapshot per year, at year-end, if/when the 1099-B parser
lands.

### Gold-layer mitigation

For pre-api dates, mark position gaps explicitly. Do **not**
interpolate. If the user really needs mid-year positions for a
historical date, recompute from the statement-transaction feed
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
| Two parallel transaction sources (`statement_pdf` + `tx_history_json`) | Same logical event lands twice with different synthetic `activity_id`s. Gold should dedupe by (account, timestamp, amount, ±description) and prefer `tx_history_json` where both exist |
| Per-row "More"-modal data not captured by default | The opt-in `--with-more-detail` flag enables it (~1 click/transaction). When the sidecar is present, silver merges it into `payload._more` (Settle Date, CUSIP, Principal, Commission, Industry Fee) |
| 1099 XML / CSV not parsed | Tax-lot detail unavailable (see §4) |
| Account-number → suffix mapping not yet auto-extracted | Manual map maintenance for now (see §1) |
