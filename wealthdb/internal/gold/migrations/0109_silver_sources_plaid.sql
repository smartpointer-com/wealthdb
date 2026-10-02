-- Add 'plaid' to the silver_kind whitelist so the plaid adapter can register
-- a silver source. One plaid silver is one Plaid Item: one login at one
-- institution, read through the Plaid aggregator (collectors/plaid). Its
-- adapter maps each account by Plaid's account type. See
-- internal/silver/plaid/ and docs/adapters/plaid.md.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migration 0053.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0109;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex', 'synthetic', 'plaid'
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
  FROM silver_sources_pre_0109;

DROP TABLE silver_sources_pre_0109;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (109, CAST(epoch(now()) AS BIGINT));
