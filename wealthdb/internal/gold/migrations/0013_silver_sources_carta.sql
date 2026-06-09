-- Add 'carta' to the silver_kind whitelist so the carta adapter
-- (private-market holdings: cap-table equity + a fund LP interest)
-- can register a silver source.
--
-- carta reuses existing taxonomy values — account_kind 'custody',
-- tax_wrapper 'taxable_personal', management_style
-- 'discretionary'/'self_directed' — and the new asset_class values
-- ('private_equity', 'private_fund') are Go-validated only (the
-- positions/instruments.asset_class columns carry no SQL CHECK), so
-- only the silver_sources whitelist needs widening here.
--
-- DuckDB can't widen a CHECK constraint in place — same
-- rename-recreate workaround as migrations 0007–0011.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0013;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0013;

DROP TABLE silver_sources_pre_0013;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (13, CAST(epoch(now()) AS BIGINT));
