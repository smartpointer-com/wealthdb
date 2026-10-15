-- ============================================================
-- gold schema, migration 0125 — reference data for the exposure
-- report: what each fund holds, what each security is, and the
-- `exposure` config block.
--
-- Two families of tables:
--
--   * Loaded, windowed by as_of like a snapshot table by snapshot_at:
--     fund_compositions, fund_lines, fund_attributes,
--     fund_counterparties, security_identities and security_facts.
--     A reference source (kind `refdata`, or the synthetic kind)
--     writes them; every composition ever loaded is kept, so a
--     holding on any day can be joined to the composition nearest in
--     time.
--   * Config, replaced whole at every load like holding_aliases:
--     exposure_proxies, exposure_reference_funds, exposure_size_funds,
--     exposure_tags, exposure_custody and exposure_settings
--     (gold.SyncExposure).
--
-- `refdata` joins the silver_kind whitelist. DuckDB can't widen a
-- CHECK constraint in place — the rename-recreate workaround of 0109,
-- carrying the observed columns 0122 added.
-- ============================================================

ALTER TABLE silver_sources RENAME TO silver_sources_pre_0125;

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN (
        'schwab', 'ubs', 'swissquote', 'fidelity',
        'relevate', 'viac', 'cointracking', 'carta', 'angellist',
        'equityzen', 'manual', 'fred', 'chase', 'firstcitizens',
        'raiffeisen_at', 'amex', 'synthetic', 'plaid', 'refdata'
    )),
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
  FROM silver_sources_pre_0125;

DROP TABLE silver_sources_pre_0125;

-- fund_compositions: one row per fund, day and basis. fund_ref is
-- isin:…, cusip:… or symbol:… (canonical.FundRef); as_of the UTC
-- midnight of the day the composition describes. basis is holdings,
-- collateral (a swap fund's basket, never looked through) or index.
-- weight_sum is in percent of net assets.
CREATE TABLE IF NOT EXISTS fund_compositions (
    silver_source_id   TEXT          NOT NULL,
    fund_ref           TEXT          NOT NULL,
    as_of              BIGINT        NOT NULL,
    basis              TEXT          NOT NULL CHECK (basis IN ('holdings', 'collateral', 'index')),
    provider           TEXT          NOT NULL,
    source_kind        TEXT,
    fund_name          TEXT,
    currency           TEXT,
    net_assets         DECIMAL(28, 4),
    line_count         BIGINT        NOT NULL,
    weight_sum         DECIMAL(18, 8),
    units_outstanding  DECIMAL(28, 8),
    fetched_at         BIGINT,
    payload            JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of, basis)
);

-- fund_lines: one row per composition line, labels as the provider
-- wrote them. security_key is the line's security (canonical.SecurityKey).
-- weight is in percent of net assets.
CREATE TABLE IF NOT EXISTS fund_lines (
    silver_source_id   TEXT          NOT NULL,
    fund_ref           TEXT          NOT NULL,
    as_of              BIGINT        NOT NULL,
    basis              TEXT          NOT NULL,
    line_no            BIGINT        NOT NULL,
    security_key       TEXT,
    isin               TEXT,
    cusip              TEXT,
    figi               TEXT,
    ticker             TEXT,
    exchange           TEXT,
    name               TEXT,
    asset_type         TEXT,
    weight             DECIMAL(18, 8),
    quantity           DECIMAL(28, 8),
    market_value       DECIMAL(28, 4),
    currency           TEXT,
    country            TEXT,
    risk_country       TEXT,
    sector             TEXT,
    industry           TEXT,
    maturity           DATE,
    coupon             DECIMAL(18, 8),
    long_currency      TEXT,
    long_amount        DECIMAL(28, 4),
    short_currency     TEXT,
    short_amount       DECIMAL(28, 4),
    payload            JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of, basis, line_no)
);

CREATE INDEX IF NOT EXISTS ix_fund_lines_security ON fund_lines(security_key);

-- fund_attributes: what a fund is, as of a day. domicile is ISO 3166
-- alpha-2; ter is percent a year; hedged_currency replaces the currency
-- of every line a hedged share class looks through to.
CREATE TABLE IF NOT EXISTS fund_attributes (
    silver_source_id   TEXT          NOT NULL,
    fund_ref           TEXT          NOT NULL,
    as_of              BIGINT        NOT NULL,
    domicile           TEXT,
    legal_form         TEXT,
    ucits              BOOLEAN,
    replication        TEXT,
    distribution       TEXT,
    ter                DECIMAL(18, 8),
    hedged_currency    TEXT,
    lending            BOOLEAN,
    index_name         TEXT,
    depositary         TEXT,
    payload            JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of)
);

-- fund_counterparties: a party a fund is exposed to, by role (swap for
-- a swap's counterparty); share in percent of net assets.
CREATE TABLE IF NOT EXISTS fund_counterparties (
    silver_source_id   TEXT          NOT NULL,
    fund_ref           TEXT          NOT NULL,
    as_of              BIGINT        NOT NULL,
    role               TEXT          NOT NULL,
    name               TEXT          NOT NULL,
    lei                TEXT,
    share              DECIMAL(18, 8),
    payload            JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of, role, name)
);

-- security_identities: the identifiers one lookup tied to a security,
-- as of the day of the lookup.
CREATE TABLE IF NOT EXISTS security_identities (
    silver_source_id   TEXT          NOT NULL,
    security_key       TEXT          NOT NULL,
    as_of              BIGINT        NOT NULL,
    provider           TEXT          NOT NULL,
    isin               TEXT,
    cusip              TEXT,
    figi               TEXT,
    lei                TEXT,
    ticker             TEXT,
    exchange           TEXT,
    name               TEXT,
    security_type      TEXT,
    listing_currency   TEXT,
    payload            JSON,
    PRIMARY KEY (silver_source_id, security_key, as_of, provider)
);

-- security_facts: long form, one row per security, field, day and
-- provider, the value as the provider states it in its own scheme.
CREATE TABLE IF NOT EXISTS security_facts (
    silver_source_id   TEXT          NOT NULL,
    security_key       TEXT          NOT NULL,
    field              TEXT          NOT NULL,
    as_of              BIGINT        NOT NULL,
    provider           TEXT          NOT NULL,
    value              TEXT          NOT NULL,
    scheme             TEXT,
    payload            JSON,
    PRIMARY KEY (silver_source_id, security_key, field, as_of, provider)
);

CREATE INDEX IF NOT EXISTS ix_security_facts_key ON security_facts(security_key);

-- The `exposure` config block, replaced whole at every load.
--
-- exposure_proxies: the fund whose composition stands in for another.
-- always replaces the fund's own compositions outright; otherwise the
-- proxy fills only the days the fund has none for. leverage scales the
-- proxy's lines, the rest going to cash.
CREATE TABLE IF NOT EXISTS exposure_proxies (
    fund_ref   TEXT     NOT NULL PRIMARY KEY,
    proxy_ref  TEXT     NOT NULL,
    always     BOOLEAN  NOT NULL,
    leverage   DOUBLE   NOT NULL
);

-- exposure_reference_funds: broad funds fetched for their lines alone,
-- which widen the security master.
CREATE TABLE IF NOT EXISTS exposure_reference_funds (
    fund_ref   TEXT  NOT NULL PRIMARY KEY
);

-- exposure_size_funds: the broad funds whose weights size the stocks no
-- market cap sizes, each with the share of its markets' value it
-- covers, in percent.
CREATE TABLE IF NOT EXISTS exposure_size_funds (
    fund_ref      TEXT    NOT NULL PRIMARY KEY,
    coverage_pct  DOUBLE  NOT NULL
);

-- exposure_tags: what a holding no composition and no security fact
-- describes is, keyed like instrument_overrides. A blank column is no
-- tag for that field.
CREATE TABLE IF NOT EXISTS exposure_tags (
    silver_source_id        TEXT  NOT NULL,
    instrument_external_id  TEXT  NOT NULL,
    sector                  TEXT,
    country                 TEXT,
    currency                TEXT,
    PRIMARY KEY (silver_source_id, instrument_external_id)
);

-- exposure_custody: who keeps each source's assets.
CREATE TABLE IF NOT EXISTS exposure_custody (
    silver_source_id  TEXT  NOT NULL PRIMARY KEY,
    custodian         TEXT  NOT NULL,
    country           TEXT
);

-- exposure_settings: one row. max_gap_days is how far from a day a
-- composition may lie and still describe it; aged_days how old one may
-- be before a row says so; partial_pct how much of a fund its weights
-- may leave unexplained before a row says so.
CREATE TABLE IF NOT EXISTS exposure_settings (
    max_gap_days   INTEGER  NOT NULL,
    aged_days      INTEGER  NOT NULL,
    partial_pct    DOUBLE   NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (125, CAST(epoch(now()) AS BIGINT));
