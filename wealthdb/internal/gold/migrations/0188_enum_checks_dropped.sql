-- ============================================================
-- gold schema — silver_sources and accounts without their enum CHECKs.
--
-- The writer validates every enum in Go before a row reaches gold
-- (canonical's Valid methods, gold/writer.go), and a load opens a source
-- only through a registered adapter (silver.Get). A CHECK that lists the
-- values again only costs a table rebuild for each new kind. So these
-- four columns carry none:
--
--   silver_sources.silver_kind    the registered adapter kinds
--   accounts.account_kind         canonical.AccountKind
--   accounts.tax_wrapper          canonical.TaxWrapper
--   accounts.management_style     canonical.ManagementStyle
--
-- DuckDB cannot drop a constraint in place, so each table is renamed and
-- recreated. Every column keeps its place, type, default, NOT NULL and
-- primary key, and every row is copied. Neither table has an index or a
-- foreign key.
-- ============================================================

ALTER TABLE silver_sources RENAME TO silver_sources_pre_enum_checks;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL,
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL,
    fx_priority         INTEGER,
    observed_snapshots_through    BIGINT DEFAULT -1,
    observed_transactions_through BIGINT DEFAULT -1
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at, fx_priority,
       observed_snapshots_through, observed_transactions_through
  FROM silver_sources_pre_enum_checks;

DROP TABLE silver_sources_pre_enum_checks;

ALTER TABLE accounts RENAME TO accounts_pre_enum_checks;

CREATE TABLE accounts (
    silver_source_id      TEXT    NOT NULL,
    account_external_id   TEXT    NOT NULL,
    account_kind          TEXT    NOT NULL,
    display_name          TEXT,
    base_currency         TEXT,
    relationship_id       TEXT,
    nickname              TEXT,
    account_category      TEXT,
    portfolio_external_id TEXT,
    tax_wrapper           TEXT,
    management_style      TEXT,
    first_seen_at         BIGINT  NOT NULL,
    last_seen_at          BIGINT  NOT NULL,
    payload               JSON,
    PRIMARY KEY (silver_source_id, account_external_id)
);

INSERT INTO accounts
SELECT silver_source_id, account_external_id, account_kind,
       display_name, base_currency, relationship_id,
       nickname, account_category, portfolio_external_id,
       tax_wrapper, management_style,
       first_seen_at, last_seen_at, payload
  FROM accounts_pre_enum_checks;

DROP TABLE accounts_pre_enum_checks;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (188, CAST(epoch(now()) AS BIGINT));
