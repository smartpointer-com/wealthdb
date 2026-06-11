-- Minimal cointracking silver schema (DuckDB) for adapter tests:
-- the tables and columns the adapter SELECTs.

CREATE TABLE dump_runs (
    snapshot_at           BIGINT PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               VARCHAR NOT NULL,
    payload               JSON
);

CREATE TABLE portfolios (
    portfolio_external_id VARCHAR PRIMARY KEY,
    cointracking_user_id  INTEGER NOT NULL,
    display_name          VARCHAR NOT NULL,
    snapshot_at           BIGINT NOT NULL,
    payload               JSON
);

CREATE TABLE wallets (
    portfolio_external_id VARCHAR NOT NULL,
    wallet_external_id    VARCHAR NOT NULL,
    display_name          VARCHAR NOT NULL,
    snapshot_at           BIGINT NOT NULL,
    payload               JSON,
    PRIMARY KEY (portfolio_external_id, wallet_external_id)
);

CREATE TABLE transactions (
    transaction_external_id VARCHAR PRIMARY KEY,
    portfolio_external_id   VARCHAR NOT NULL,
    wallet_external_id      VARCHAR NOT NULL,
    snapshot_at             BIGINT NOT NULL,
    occurred_at             TIMESTAMP NOT NULL,
    type                    VARCHAR NOT NULL,
    buy_amount              DECIMAL(38, 18),
    buy_currency            VARCHAR,
    sell_amount             DECIMAL(38, 18),
    sell_currency           VARCHAR,
    fee_amount              DECIMAL(38, 18),
    fee_currency            VARCHAR,
    comment                 VARCHAR,
    lpn                     VARCHAR,
    payload                 JSON
);

CREATE TABLE positions_daily (
    as_of_date             DATE NOT NULL,
    portfolio_external_id  VARCHAR NOT NULL,
    wallet_external_id     VARCHAR NOT NULL,
    instrument_external_id VARCHAR NOT NULL,
    amount                 DECIMAL(38, 18) NOT NULL,
    snapshot_at            BIGINT NOT NULL,
    PRIMARY KEY (as_of_date, portfolio_external_id, wallet_external_id, instrument_external_id)
);

CREATE TABLE portfolio_prices (
    as_of_date             DATE NOT NULL,
    portfolio_external_id  VARCHAR NOT NULL,
    instrument_external_id VARCHAR NOT NULL,
    quote_currency         VARCHAR NOT NULL,
    price                  DECIMAL(38, 18) NOT NULL,
    snapshot_at            BIGINT NOT NULL,
    PRIMARY KEY (as_of_date, portfolio_external_id, instrument_external_id, quote_currency)
);

CREATE TABLE coin_prices (
    as_of_date             DATE NOT NULL,
    instrument_external_id VARCHAR NOT NULL,
    price_usd              DECIMAL(38, 18) NOT NULL,
    source                 VARCHAR NOT NULL,
    fetched_at             BIGINT NOT NULL,
    PRIMARY KEY (as_of_date, instrument_external_id, source)
);
