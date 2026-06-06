-- Add 'cointracking' to the silver_kind whitelist, and add 'crypto'
-- to the account_kind whitelist so the cointracking adapter can
-- write account rows tagged with a single crypto bucket (rather
-- than the existing exchange / self-custody split, which we don't
-- attempt to derive from a CT wallet's display name).
--
-- Same DuckDB rename-table workaround as migrations 0007–0010:
-- DuckDB doesn't support widening a CHECK constraint in place.

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0011;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking'
    )),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

INSERT INTO silver_sources
SELECT silver_source_id, silver_kind, silver_path,
       high_watermark, first_loaded_at, last_loaded_at
  FROM silver_sources_pre_0011;

DROP TABLE silver_sources_pre_0011;

ALTER TABLE accounts RENAME TO accounts_pre_0011;

CREATE TABLE accounts (
    silver_source_id      TEXT    NOT NULL,
    account_external_id   TEXT    NOT NULL,
    account_kind          TEXT    NOT NULL CHECK (account_kind IN (
        'brokerage', 'cash', 'safekeeping', 'custody', 'overlay',
        'crypto_exchange', 'crypto_self_custody', 'crypto', 'other'
    )),
    display_name          TEXT,
    base_currency         TEXT,
    relationship_id       TEXT,
    nickname              TEXT,
    account_category      TEXT,
    portfolio_external_id TEXT,
    tax_wrapper           TEXT CHECK (tax_wrapper IS NULL OR tax_wrapper IN (
        'taxable_personal', 'taxable_joint', 'foundation',
        'traditional_ira', 'roth_ira', 'sep_ira', 'simple_ira',
        '401k', '403b', '457b',
        '529', 'coverdell_esa', 'hsa',
        'daf',
        'custodial_utma', 'custodial_ugma',
        'trust_grantor', 'trust_non_grantor', 'trust_charitable',
        'pillar_2', 'vested_benefits', 'pillar_3a',
        'other'
    )),
    management_style      TEXT CHECK (management_style IS NULL OR management_style IN (
        'self_directed', 'advisory', 'discretionary', 'automated'
    )),
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
  FROM accounts_pre_0011;

DROP TABLE accounts_pre_0011;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (11, CAST(epoch(now()) AS BIGINT));
