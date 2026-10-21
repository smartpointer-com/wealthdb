-- Add 'curves' to the silver_kind whitelist so the curves adapter can
-- register a silver source. A curves silver holds the public government
-- yield curves of the euro (the ECB's) and the franc (the SNB's), one
-- rate series per tenor, which load into rate_observations as fred's
-- Treasury series do (collectors/curves, internal/silver/curves).
--
-- DuckDB can't widen a CHECK constraint in place — the rename-recreate
-- workaround of 0125, carrying every column.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0139;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex', 'synthetic', 'plaid', 'refdata',
        'curves'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL,
    fx_priority         INTEGER,
    observed_snapshots_through    BIGINT DEFAULT -1,
    observed_transactions_through BIGINT DEFAULT -1
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at, fx_priority,
       observed_snapshots_through, observed_transactions_through
  FROM silver_sources_pre_0139;

DROP TABLE silver_sources_pre_0139;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (139, CAST(epoch(now()) AS BIGINT));
