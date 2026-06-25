-- Minimal viac silver schema for adapter tests: the tables and
-- columns the adapter SELECTs, with the post-migration-0003 shape
-- (positions.quantity, positions.market_value_chf, accounts.
-- management_style, and positions/cash_balances.source — 'live'
-- for scraped rows, 'report:<docid>' for PDF-reconstructed
-- historical snapshots). SQLite syntax.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    product_code        TEXT    NOT NULL,
    name                TEXT,
    state               TEXT,
    currency_code       TEXT    NOT NULL DEFAULT 'CHF',
    management_style    TEXT,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE cash_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    currency            TEXT    NOT NULL,
    balance_kind        TEXT    NOT NULL,
    amount              REAL    NOT NULL,
    source              TEXT    NOT NULL DEFAULT 'live',
    payload             TEXT,
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);

CREATE TABLE positions (
    snapshot_at            INTEGER NOT NULL,
    account_external_id    TEXT    NOT NULL,
    instrument_external_id TEXT    NOT NULL,
    asset_class            TEXT    NOT NULL,
    currency_code          TEXT,
    name                   TEXT,
    quantity               REAL,
    market_value_chf       REAL,
    acquisition_price      REAL,
    asset_price            REAL,
    source                 TEXT    NOT NULL DEFAULT 'live',
    payload                TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_external_id)
);

CREATE TABLE instruments (
    instrument_external_id TEXT    NOT NULL PRIMARY KEY,
    isin                   TEXT    NOT NULL,
    name                   TEXT,
    currency_code          TEXT,
    asset_class            TEXT,
    first_seen_at          INTEGER NOT NULL,
    last_seen_at           INTEGER NOT NULL,
    payload                TEXT    NOT NULL
);

CREATE TABLE transactions (
    transaction_external_id TEXT    NOT NULL PRIMARY KEY,
    snapshot_at             INTEGER NOT NULL,
    occurred_at             INTEGER NOT NULL,
    account_external_id     TEXT    NOT NULL,
    type                    TEXT    NOT NULL,
    kind                    TEXT    NOT NULL,
    amount_chf              REAL,
    currency                TEXT    NOT NULL DEFAULT 'CHF',
    payload                 TEXT    NOT NULL
);
