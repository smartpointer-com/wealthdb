-- Add 'amex' to the silver_kind whitelist so the amex adapter can register a
-- silver source. amex is the American Express card collector: credit and
-- charge cards only — it emits one `card` account per card (the 'card' kind of
-- migration 0037), the outstanding balance as negative cash at each statement
-- period plus the current balance, and the card transaction ledger with the
-- provider's own spend category wherever the issuer filed one. See
-- collectors/amex/ and internal/silver/amex/.
--
-- Unlike chase, whose adapter also projects deposit accounts, this one
-- projects cards only: no other account kind and no position, and the returns
-- engine drops every row of it (returnsInvisibleKind).
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0011, 0013–0018 and 0033–0037.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0053;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex'
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
  FROM silver_sources_pre_0053;

DROP TABLE silver_sources_pre_0053;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (53, CAST(epoch(now()) AS BIGINT));
