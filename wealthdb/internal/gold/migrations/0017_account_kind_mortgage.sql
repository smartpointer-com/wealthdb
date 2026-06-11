-- Add 'mortgage' to the account_kind whitelist so the ubs-web
-- adapter can emit liability accounts (UBS surfaces each mortgage
-- as its own per-property liability account in positions.csv,
-- under `Group of products = 'Pro memoria - Mortgages'`).
--
-- AssetClass mortgage is added in the same release in
-- internal/canonical/enums.go; positions/instruments.asset_class
-- carry no SQL CHECK in gold so no migration is needed for that
-- side. Only account_kind is constrained at the DB level.
--
-- Same DuckDB rename-table workaround as 0008 / 0011: DuckDB
-- doesn't support widening a CHECK constraint in place.

ALTER TABLE accounts RENAME TO accounts_pre_0017;

CREATE TABLE accounts (
    silver_source_id      TEXT    NOT NULL,
    account_external_id   TEXT    NOT NULL,
    account_kind          TEXT    NOT NULL CHECK (account_kind IN (
        'brokerage', 'cash', 'safekeeping', 'custody', 'overlay',
        'crypto_exchange', 'crypto_self_custody', 'crypto',
        'mortgage', 'other'
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
  FROM accounts_pre_0017;

DROP TABLE accounts_pre_0017;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (17, CAST(epoch(now()) AS BIGINT));
