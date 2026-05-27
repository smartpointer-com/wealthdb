-- Add 'relevate' to the silver_kind CHECK constraint so the
-- relevate adapter (silver source: relevate-dump) can be
-- registered alongside schwab / ubs / swissquote / fidelity.
--
-- DuckDB doesn't support modifying a CHECK constraint in place,
-- so we rename the existing table, recreate it with the wider
-- check, copy the rows back, and drop the renamed original.
-- Same pattern as migrations 0007 (fidelity) and 0008 (account
-- taxonomy).

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0009;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN ('schwab', 'ubs', 'swissquote', 'fidelity', 'relevate')),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0009;

DROP TABLE silver_sources_pre_0009;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (9, CAST(epoch(now()) AS BIGINT));
