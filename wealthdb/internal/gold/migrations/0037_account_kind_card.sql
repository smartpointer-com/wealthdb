-- Add 'card' to the account_kind whitelist so card adapters can emit
-- revolving-credit liability accounts. A card carries no position:
-- the outstanding balance lives as negative cash (the margin-debit
-- precedent), the adapter negating the provider's owed-positive
-- figure.
--
-- The card transaction kinds (purchase / refund / card_payment /
-- reward) land in internal/canonical/enums.go in the same release;
-- transactions.kind carries no SQL CHECK in gold (validated in Go by
-- Writer.InsertTransactions), so only account_kind needs a migration.
--
-- Same DuckDB rename-table workaround as 0008 / 0011 / 0017 / 0036:
-- DuckDB doesn't support altering a CHECK constraint in place.

ALTER TABLE accounts RENAME TO accounts_pre_0037;

CREATE TABLE accounts (
    silver_source_id      TEXT    NOT NULL,
    account_external_id   TEXT    NOT NULL,
    account_kind          TEXT    NOT NULL CHECK (account_kind IN (
        'brokerage', 'cash', 'safekeeping', 'custody', 'overlay',
        'crypto_exchange', 'crypto_self_custody', 'crypto',
        'mortgage', 'card', 'donor_advised_fund', 'other'
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
        'charitable',
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
  FROM accounts_pre_0037;

DROP TABLE accounts_pre_0037;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (37, CAST(epoch(now()) AS BIGINT));
