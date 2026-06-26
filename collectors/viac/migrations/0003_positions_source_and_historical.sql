-- ============================================================
-- viac-dump silver schema, migration 0003.
--
-- Adds a `source` provenance column to `positions` and
-- `cash_balances` so the snapshot tables can carry BOTH:
--
--   'live'           rows from the REST assetsOverview endpoint,
--                    snapshot_at = the bronze dump's scrape time.
--   'report:<docid>' rows reconstructed from an INVESTMENT_REPORTING
--                    PDF (the periodic "Reporting" statement),
--                    snapshot_at = the report's period-end (as-of)
--                    date — going back to the contract's first year,
--                    long before live scraping began.
--
-- Both kinds coexist in one table (swissquote-style), distinguished
-- by snapshot_at (report dates vs scrape dates never collide) and
-- tagged by `source` for provenance + idempotent re-parse. The gold
-- adapter reads every snapshot_at uniformly, so a historical report
-- snapshot answers `wealthdb holdings positions --as-of <past date>` with no
-- adapter-side special-casing.
--
-- Existing rows are all live scrapes → backfilled to 'live'.
-- ============================================================

BEGIN;

ALTER TABLE positions     ADD COLUMN source TEXT NOT NULL DEFAULT 'live';
ALTER TABLE cash_balances ADD COLUMN source TEXT NOT NULL DEFAULT 'live';

UPDATE positions     SET source = 'live' WHERE source IS NULL OR source = '';
UPDATE cash_balances SET source = 'live' WHERE source IS NULL OR source = '';

-- Provenance lookups ("which doc produced this historical row").
CREATE INDEX IF NOT EXISTS ix_positions_source     ON positions(source);
CREATE INDEX IF NOT EXISTS ix_cash_balances_source ON cash_balances(source);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
