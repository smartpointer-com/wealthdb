-- Add 'equityzen' to the silver_kind whitelist so the equityzen adapter
-- (EquityZen pre-IPO secondary marketplace: a buyer's SPV + multi-company-
-- fund membership interests) can register a silver source.
--
-- equityzen reuses existing taxonomy values — account_kind 'custody'
-- (EquityZen administers the interests; the buyer places no trades),
-- tax_wrapper 'taxable_personal', management_style 'self_directed' (the
-- holder chooses which interests to hold; GP management inside each vehicle
-- is not modelled, in line with carta/angellist) — and
-- asset_class 'spv' / 'private_fund' (added with carta/angellist;
-- positions/instruments.asset_class carry no SQL CHECK, Go-validated only).
-- Transactions use TxKind 'buy' / 'sell' / 'distribution' (the last added
-- with angellist; transactions.kind is Go-validated too). So only the
-- silver_sources whitelist needs widening here.
--
-- DuckDB can't widen a CHECK constraint in place — same rename-recreate
-- workaround as migrations 0007–0014.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0015;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist', 'equityzen'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0015;

DROP TABLE silver_sources_pre_0015;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (15, CAST(epoch(now()) AS BIGINT));
