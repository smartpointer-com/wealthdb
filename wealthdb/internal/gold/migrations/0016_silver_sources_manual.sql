-- Add 'manual' to the silver_kind whitelist so the manual adapter (the
-- hand-maintained private-holdings collector: real estate, direct private
-- equity, convertible notes, fund LP interests, single-deal SPVs, and other
-- illiquid positions) can register a silver source.
--
-- The manual adapter reuses existing taxonomy values — account_kind 'other'
-- (directly-held assets with no institutional container), tax_wrapper
-- 'taxable_personal', management_style 'self_directed' (the holder decides
-- what to hold; no advisor/robo). One account holds every position; the
-- collector records positions + valuations only (no transactions, no funding
-- sentinel — manual cash flows are already wires in the bank collectors).
-- Asset classes are the canonical names used as the bronze `kind` directly
-- (identity classmap): the new 'real_estate' / 'convertible_note' (added in
-- internal/canonical/enums.go) plus the existing 'private_equity' /
-- 'private_fund' / 'spv' / 'other'. positions / instruments.asset_class carry
-- no SQL CHECK (Go-validated only), so only the silver_sources whitelist
-- needs widening here.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0015.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0016;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0016;

DROP TABLE silver_sources_pre_0016;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (16, CAST(epoch(now()) AS BIGINT));
