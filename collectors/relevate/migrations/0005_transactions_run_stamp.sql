-- ============================================================
-- relevate silver schema, migration 0005 — the dump that brought each
-- credit note.
--
-- A credit note's value date can lie before every snapshot gold has
-- loaded. Gold's change window reaches such a row through the dump
-- that brought it.
--
-- transactions
--   run_snapshot_at   the dump_runs.snapshot_at of the dump that first
--                     indexed the credit note's PDF content
--                     (documents.first_seen_at). The PDF pass runs
--                     after the dumps, so the stamp is the document's
--                     dump, not the load's: one load per dump and one
--                     rebuild give the same value. NULL on a row
--                     written before this migration: it was in silver
--                     before every dump a stamp names, and its window
--                     was opened then.
-- ============================================================
BEGIN;

ALTER TABLE transactions ADD COLUMN run_snapshot_at INTEGER;

CREATE INDEX transactions_run_snapshot_at ON transactions(run_snapshot_at);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
