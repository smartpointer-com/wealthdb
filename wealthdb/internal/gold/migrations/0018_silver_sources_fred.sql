-- Add 'fred' to the silver_kind whitelist so the fred adapter can register
-- a silver source. fred is the FRED / US Federal Reserve H.10 collector: a
-- reference-data source that emits FX rates only (no accounts, positions, or
-- transactions), filling the deep historic FX tail the account collectors
-- lack. See collectors/fred/ and internal/silver/fred/. Its rates are
-- deprioritised relative to account sources via the per-source
-- silver_sources[].fx_priority config field (see internal/gold/fxpriority.go).
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0016.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0018;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0018;

DROP TABLE silver_sources_pre_0018;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (18, CAST(epoch(now()) AS BIGINT));
