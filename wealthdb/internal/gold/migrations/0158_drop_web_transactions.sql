-- ============================================================
-- gold schema, migration 0158 — web_transactions goes.
--
-- The view (0132's) read serving_transactions for the Wealth
-- Overview's income and fee charts. Those charts are retired; no card,
-- serving table or other view reads it. serving_transactions stays:
-- web_income_withheld (0146) and the dashboards' report_transactions
-- model read it.
--
-- IF EXISTS keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

DROP VIEW IF EXISTS web_transactions;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (158, CAST(epoch(now()) AS BIGINT));
