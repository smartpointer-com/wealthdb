-- ============================================================
-- gold schema, migration 0148 — the gap an issuer keeps between a
-- distribution's record day and its pay day, learned from the dividend
-- calendar (docs/YIELD.md §4).
--
-- Rebuilt whole by the yield pass at every load (gold.RebuildYield),
-- from the dividend calendar of 0147 and its settlement cycles, so the
-- yield report's reader takes a gap by a security's key:
--
--   * dividend_gaps: one row per security whose issuer's distributions
--     teach a gap, and one per pool of funds. key is the security's key
--     (exposure_key), or pool:<pool> for a pool (yield.FundPool: US, EU,
--     or a market). issuer is cik:<CIK> for a company EDGAR knows, whose
--     share classes of one market learn together (a class quoted in
--     another market learns apart, under the same issuer), else the
--     security's own key, else the pool. fund says the issuer is a fund
--     or a pool of funds. market is the market whose business days the
--     gap counts ('' for TARGET2's). days is the median gap in business
--     days, near the shortest and far the longest; events the
--     distributions it rests on, for a pool the funds.
--
-- IF NOT EXISTS keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS dividend_gaps (
    key     VARCHAR  NOT NULL PRIMARY KEY,
    issuer  VARCHAR  NOT NULL,
    fund    BOOLEAN  NOT NULL,
    market  VARCHAR  NOT NULL,
    days    INTEGER  NOT NULL,
    near    INTEGER  NOT NULL,
    far     INTEGER  NOT NULL,
    events  INTEGER  NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (148, CAST(epoch(now()) AS BIGINT));
