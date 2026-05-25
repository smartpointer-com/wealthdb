-- Account taxonomy split into three orthogonal dimensions:
--
--   account_kind      — technical container (brokerage, cash,
--                       safekeeping, custody, overlay,
--                       crypto_exchange, crypto_self_custody,
--                       other). Existing column; the value set
--                       widens via the recreate below.
--   tax_wrapper       — tax / regulatory registration. NEW.
--   management_style  — who places trades. NEW.
--
-- See internal/canonical/enums.go for the canonical value sets
-- and docs/DESIGN.md for the rationale. Both new columns are
-- nullable so adapters that don't know the wrapper / style can
-- leave them unset; gold's per-column upsert preserves prior
-- writers' values when the later writer has NULL (see
-- internal/gold/writer.go).
--
-- DuckDB doesn't support ALTER TABLE ADD COLUMN with a CHECK
-- constraint, nor altering a CHECK in place — so we rename the
-- existing table, recreate it with the widened account_kind
-- check plus the two new columns, copy rows back, and drop the
-- renamed original. Same pattern as migration 0007.

ALTER TABLE accounts RENAME TO accounts_pre_0008;

CREATE TABLE accounts (
    silver_source_id      TEXT    NOT NULL,
    account_external_id   TEXT    NOT NULL,
    account_kind          TEXT    NOT NULL CHECK (account_kind IN (
        'brokerage', 'cash', 'safekeeping', 'custody', 'overlay',
        'crypto_exchange', 'crypto_self_custody', 'other'
    )),
    display_name          TEXT,
    base_currency         TEXT,
    relationship_id       TEXT,
    nickname              TEXT,
    account_category      TEXT,
    portfolio_external_id TEXT,
    tax_wrapper           TEXT CHECK (tax_wrapper IS NULL OR tax_wrapper IN (
        -- Generic / cross-jurisdictional.
        'taxable_personal', 'taxable_joint', 'foundation',
        -- US retirement.
        'traditional_ira', 'roth_ira', 'sep_ira', 'simple_ira',
        '401k', '403b', '457b',
        -- US education / health.
        '529', 'coverdell_esa', 'hsa',
        -- US charitable.
        'daf',
        -- US custodial-for-minors.
        'custodial_utma', 'custodial_ugma',
        -- US trust.
        'trust_grantor', 'trust_non_grantor', 'trust_charitable',
        -- Switzerland.
        'pillar_2', 'vested_benefits', 'pillar_3a',
        -- Catch-all.
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

INSERT INTO accounts (
    silver_source_id, account_external_id, account_kind,
    display_name, base_currency, relationship_id,
    nickname, account_category, portfolio_external_id,
    first_seen_at, last_seen_at, payload
)
SELECT silver_source_id, account_external_id, account_kind,
       display_name, base_currency, relationship_id,
       nickname, account_category, portfolio_external_id,
       first_seen_at, last_seen_at, payload
  FROM accounts_pre_0008;

DROP TABLE accounts_pre_0008;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (8, CAST(epoch(now()) AS BIGINT));
