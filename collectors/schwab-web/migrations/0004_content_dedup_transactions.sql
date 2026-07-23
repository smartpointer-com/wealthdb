-- ============================================================
-- schwab-web silver schema, migration 0004
-- — sha256-churn dedup for transactions.
--
-- ## Background
--
-- Schwab regenerates statement PDFs on every download (INTEROP.md §3),
-- giving the same logical statement multiple distinct sha256s in the
-- bronze archive. The pre-0004 `_synthesize_activity_id` function
-- included `source_sha256` in its hash input, so each re-download of
-- the same statement produced an entirely distinct set of activity_ids.
-- Since the per-sha256 insert gate only checked
-- `WHERE source_sha256 = ?`, each re-downloaded PDF caused ALL of its
-- transactions to be re-inserted under new activity_ids — duplicating
-- every transaction once per additional download of the same statement.
--
-- ## Fix (applied by load.py ≥ 0004)
--
-- 1. `activity_id` is now a hash of
--    (account, date, amount, description, symbol, index) — no sha256.
--    Two parses of the same logical PDF content produce the same id.
--
-- 2. A new `logical_doc_key` column holds a stable content-key for
--    the source document:
--      "<account_external_id>|<doc_date_int>|<filename>"
--    This lets the load gate skip re-parsing when transactions for this
--    logical document already exist, and lets --reparse delete all
--    sha256-churn variants at once.
--
-- ## This migration
--
-- 1. Adds the `logical_doc_key` column (nullable so existing rows
--    don't need an immediate backfill to load — the column is
--    populated on INSERT for all new rows; backfill below covers
--    existing rows).
--
-- 2. Backfills `logical_doc_key` for all existing rows by joining
--    `transactions.source_sha256` → `documents.sha256` and composing
--    the key from `documents.account_external_id || '|' ||
--    documents.doc_date || '|' || documents.filename`.
--    (tx_history_export rows land in `documents` too — the same
--    join works for both source types.)
--
-- 3. Adds an index on `logical_doc_key` (the load gate now filters
--    on this column).
--
-- 4. Removes sha256-churn duplicates: for each group sharing the
--    same content-key (account_external_id, timestamp, kind,
--    instrument_key, source, payload), keeps the row with the
--    lexicographically smallest activity_id and deletes the rest.
--    The survivor is deterministic — same result every re-run.
-- ============================================================

-- Step 1: add the new column (nullable — backfilled below).
ALTER TABLE transactions ADD COLUMN logical_doc_key TEXT;

-- Step 2: backfill logical_doc_key for rows whose source_sha256
-- appears in the documents table (covers both statement_pdf rows
-- and tx_history_json rows, since both source types insert into
-- documents).
UPDATE transactions
SET logical_doc_key = (
    SELECT d.account_external_id || '|' || d.doc_date || '|' || d.filename
    FROM documents d
    WHERE d.sha256 = transactions.source_sha256
    LIMIT 1
)
WHERE logical_doc_key IS NULL;

-- Step 3: index for the load gate (SELECT 1 FROM transactions
-- WHERE logical_doc_key = ?) and for --reparse deletes.
CREATE INDEX IF NOT EXISTS ix_transactions_logical_doc_key
    ON transactions(logical_doc_key);

-- Step 4: collapse sha256-churn duplicates.
--
-- A "duplicate group" is a set of rows with identical
-- (account_external_id, timestamp, kind, instrument_key, source, payload)
-- that arose from loading the same logical statement under different
-- sha256s. Within each group we keep the row whose activity_id is
-- lexicographically smallest (deterministic tiebreak; activity_ids
-- are hex strings of the same length so lex order is well-defined).
-- All other rows in the group are deleted.
--
-- This does NOT collapse genuinely-distinct same-day same-amount
-- transactions that happen to share description/symbol (e.g. two
-- real identical $50 fees on one day) — those have different `index`
-- baked into their activity_id (even under the old sha256-dependent
-- scheme they had different index values, so their payloads differ by
-- at most raw_lines but they are still distinct rows; the group key
-- includes `payload` verbatim so they land in different groups).
DELETE FROM transactions
WHERE activity_id NOT IN (
    SELECT MIN(activity_id)
    FROM transactions
    GROUP BY account_external_id,
             timestamp,
             kind,
             COALESCE(instrument_key, ''),
             source,
             payload
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s','now') AS INTEGER));
