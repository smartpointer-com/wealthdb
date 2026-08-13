-- Add 'chase' to the silver_kind whitelist so the chase adapter can register
-- a silver source. chase is the JPMorgan Chase retail deposit collector: cash
-- (checking / savings) accounts — it emits a cash balance per statement plus
-- the current balance, and the deposit transaction ledger. See collectors/chase/
-- and internal/silver/chase/.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0018.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0033;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL,
    fx_priority         INTEGER            -- added in 0019; preserved here
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at, fx_priority
  FROM silver_sources_pre_0033;

DROP TABLE silver_sources_pre_0033;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (33, CAST(epoch(now()) AS BIGINT));
