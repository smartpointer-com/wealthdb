-- ============================================================
-- gold schema, migration 0122 — when each source was last observed.
--
-- Gold dates every fact by when it is true (a snapshot's as-of, a
-- transaction's date). This adds when the source was last looked at.
-- A portal that marks its holdings quarterly and was read this morning
-- is observed through this morning; its latest snapshot is the
-- quarter's end. A feed that stopped running is observed through its
-- last run, however current its carried values look.
--
-- The loader stamps both columns on every load, including a load that
-- finds no new data (loader.Load): a download that found nothing new is
-- the observation that matters most. The values are the silver's
-- canonical.Status watermarks (docs/DESIGN.md §6.2); -1 is unknown.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

ALTER TABLE silver_sources ADD COLUMN IF NOT EXISTS observed_snapshots_through    BIGINT DEFAULT -1;
ALTER TABLE silver_sources ADD COLUMN IF NOT EXISTS observed_transactions_through BIGINT DEFAULT -1;

-- source_observed: per source, the instants through which its holdings
-- and its ledger were observed, NULL where the silver does not say.
CREATE OR REPLACE VIEW source_observed AS
    SELECT silver_source_id,
           CASE WHEN observed_snapshots_through > 0 THEN observed_snapshots_through END AS observed_snapshots_through,
           CASE WHEN observed_transactions_through > 0 THEN observed_transactions_through END AS observed_transactions_through
      FROM silver_sources;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (122, CAST(epoch(now()) AS BIGINT));
