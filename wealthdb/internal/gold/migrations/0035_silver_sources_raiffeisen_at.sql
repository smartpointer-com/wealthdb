-- Add 'raiffeisen_at' to the silver_kind whitelist so the raiffeisen_at
-- adapter can register a silver source. raiffeisen_at is the Austrian
-- Raiffeisen (Mein ELBA) retail deposit collector: cash (checking / savings)
-- accounts — it emits a cash balance per day the kontostaende series carries a
-- saldo plus the current balance, and the deposit transaction ledger. See
-- collectors/raiffeisen_at/ and internal/silver/raiffeisen_at/.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0018, 0033, and 0034.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0035;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at'
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
  FROM silver_sources_pre_0035;

DROP TABLE silver_sources_pre_0035;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (35, CAST(epoch(now()) AS BIGINT));
