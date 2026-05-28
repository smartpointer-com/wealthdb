-- Add 'viac' to the silver_kind CHECK constraint so the viac
-- adapter (silver source: viac) can be registered alongside
-- schwab / ubs / swissquote / fidelity / relevate.
--
-- Same DuckDB rename-table workaround as migrations 0007 (fidelity),
-- 0008 (account taxonomy), and 0009 (relevate).

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0010;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN ('schwab', 'ubs', 'swissquote', 'fidelity', 'relevate', 'viac')),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0010;

DROP TABLE silver_sources_pre_0010;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (10, CAST(epoch(now()) AS BIGINT));
