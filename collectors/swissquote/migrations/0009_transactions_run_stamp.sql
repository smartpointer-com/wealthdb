-- ============================================================
-- swissquote silver schema, migration 0009 — the dump that wrote each
-- transaction.
--
-- A dump's transactions CSV replaces its own span of the ledger, and
-- that span can lie before every position snapshot. Gold's change
-- window reaches such rows through the dump that wrote them.
--
-- transactions
--   run_snapshot_at   the dump_runs.snapshot_at of the dump whose CSV
--                     wrote the row. NULL on a row written before this
--                     migration: it was in silver before every dump a
--                     stamp names, and its window was opened then. The
--                     next dump whose span covers the row stamps it.
-- ============================================================

ALTER TABLE transactions ADD COLUMN run_snapshot_at INTEGER;

CREATE INDEX transactions_run_snapshot_at ON transactions(run_snapshot_at);


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (9, CAST(strftime('%s', 'now') AS INTEGER));
