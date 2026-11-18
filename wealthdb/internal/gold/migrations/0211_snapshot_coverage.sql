-- ============================================================
-- gold schema, migration 0211 — the accounts a snapshot reports
-- whole.
--
-- A source that reads an account in full at an instant states it
-- (canonical.CoverageChange). Its positions and cash balances of the
-- account at that instant are then all the account held. A key whose
-- account a later covering snapshot reports without it ends there at
-- the latest (DESIGN.md §10.7). A source that states nothing keeps the inferred
-- endings.
--
--   * one row per source, instant and account. The load's windowed
--     delete clears it by snapshot_at, as it does positions.
--
-- The rows fill as each source loads its snapshots again: an older
-- gold holds none until its sources reload.
-- ============================================================

CREATE TABLE IF NOT EXISTS snapshot_coverage (
    silver_source_id    VARCHAR NOT NULL,
    snapshot_at         BIGINT  NOT NULL,
    account_external_id VARCHAR NOT NULL,
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (211, CAST(epoch(now()) AS BIGINT));
