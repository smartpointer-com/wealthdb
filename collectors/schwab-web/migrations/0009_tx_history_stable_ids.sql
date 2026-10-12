-- ============================================================
-- schwab-web silver schema, migration 0009 — stable tx-history ids.
--
-- A tx-history export row's activity_id hashes its date, amount,
-- description and symbol plus an ordinal. The ordinal was the row's
-- position in the export file, so two exports of overlapping windows
-- gave one transaction two ids, and both rows stayed. The loader now
-- counts the ordinal only among the export's rows that share the hashed
-- fields, which is the same in every export that holds the row.
--
-- This migration:
--
-- 1. Collapses the copies already held. Within one export, rows with
--    the same account, timestamp and payload are numbered in insertion
--    order; across exports, the rows with the same account, timestamp,
--    payload and number are one transaction, and the smallest
--    activity_id is kept. Genuine twins inside one export get different
--    numbers and so stay apart.
--
-- 2. Clears the documents parser generation. The next load then reads
--    as stale, re-walks the archive with `--reparse` semantics, and
--    re-keys every tx-history row under the new ids.
-- ============================================================

DELETE FROM transactions
WHERE activity_id IN (
    SELECT activity_id FROM (
        SELECT activity_id,
               MIN(activity_id) OVER (
                   PARTITION BY account_external_id, timestamp, payload,
                                ordinal
               ) AS keep
        FROM (
            SELECT activity_id, account_external_id, timestamp, payload,
                   ROW_NUMBER() OVER (
                       PARTITION BY logical_doc_key, account_external_id,
                                    timestamp, payload
                       ORDER BY rowid
                   ) AS ordinal
            FROM transactions
            WHERE source = 'tx_history_json'
        )
    )
    WHERE activity_id <> keep
);

DELETE FROM parser_generations WHERE scope = 'documents';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (9, CAST(strftime('%s','now') AS INTEGER));
