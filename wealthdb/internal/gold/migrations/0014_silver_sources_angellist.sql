-- Add 'angellist' to the silver_kind whitelist so the angellist adapter
-- (AngelList venture LP book: SPV + fund-deal interests) can register a
-- silver source.
--
-- angellist reuses existing taxonomy values — account_kind 'custody',
-- tax_wrapper 'taxable_personal', management_style 'self_directed' (same as
-- the carta / equityzen private-market accounts) — and asset_class 'spv' /
-- 'private_fund' (positions/instruments.asset_class carry no SQL CHECK,
-- Go-validated only), so only the silver_sources whitelist needs widening
-- here.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0013.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0014;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0014;

DROP TABLE silver_sources_pre_0014;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (14, CAST(epoch(now()) AS BIGINT));
