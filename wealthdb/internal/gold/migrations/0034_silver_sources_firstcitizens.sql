-- Add 'firstcitizens' to the silver_kind whitelist so the firstcitizens
-- adapter can register a silver source. firstcitizens is the First Citizens
-- Bank retail deposit collector: cash (checking / savings) accounts — it emits
-- a cash balance per day the balance moved plus the current balance, and the
-- deposit transaction ledger. See collectors/firstcitizens/ and
-- internal/silver/firstcitizens/.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0018 and 0033.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0034;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens'
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
  FROM silver_sources_pre_0034;

DROP TABLE silver_sources_pre_0034;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (34, CAST(epoch(now()) AS BIGINT));
