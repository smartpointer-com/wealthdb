-- Add 'synthetic' to the silver_kind whitelist so the synthetic adapter can
-- register a silver source. synthetic is the one generic kind: its silver
-- mirrors the canonical model one table per record type (accounts,
-- portfolios, instruments, positions, cash balances, fx rates,
-- transactions), and its adapter passes those rows through. No collector
-- feeds it; a generator writes it — the demo household under demo/, and
-- test fixtures. See internal/silver/synthetic/ and
-- docs/adapters/synthetic.md.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migration 0053.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0107;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex', 'synthetic'
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
  FROM silver_sources_pre_0107;

DROP TABLE silver_sources_pre_0107;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (107, CAST(epoch(now()) AS BIGINT));
