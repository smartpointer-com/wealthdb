-- Minimal relevate silver schema for adapter tests: the tables and
-- columns the adapter SELECTs. SQLite syntax.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    currency_code       TEXT    NOT NULL DEFAULT 'CHF',
    name                TEXT,
    product_name        TEXT,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE positions (
    snapshot_at            INTEGER NOT NULL,
    account_external_id    TEXT    NOT NULL,
    instrument_external_id TEXT    NOT NULL,
    isin                   TEXT,
    instrument_name        TEXT,
    asset_class            TEXT,
    allocation             REAL,
    payload                TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_external_id)
);

CREATE TABLE instruments (
    instrument_external_id TEXT    NOT NULL PRIMARY KEY,
    isin                   TEXT,
    name                   TEXT,
    asset_class            TEXT,
    first_seen_at          INTEGER NOT NULL,
    last_seen_at           INTEGER NOT NULL,
    payload                TEXT    NOT NULL
);

CREATE TABLE cash_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    currency            TEXT    NOT NULL,
    balance_kind        TEXT    NOT NULL,
    amount              REAL    NOT NULL,
    payload             TEXT,
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);

CREATE TABLE transactions (
    transaction_external_id TEXT    NOT NULL PRIMARY KEY,
    snapshot_at             INTEGER NOT NULL,
    occurred_at             INTEGER NOT NULL,
    account_external_id     TEXT    NOT NULL,
    instrument_external_id  TEXT,
    kind                    TEXT    NOT NULL,
    currency                TEXT    NOT NULL,
    gross_amount            REAL,
    net_amount              REAL,
    quantity                REAL,
    price                   REAL,
    source                  TEXT    NOT NULL
        CHECK (source IN ('deposits_endpoint', 'credit_note_pdf', 'manual')),
    payload                 TEXT    NOT NULL
);

-- 0002_historical_snapshots.sql: the quarterly reports' holdings and cash.
CREATE TABLE historical_position_snapshots (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    isin                  TEXT    NOT NULL,
    security_name         TEXT    NOT NULL,
    asset_class           TEXT,
    currency              TEXT    NOT NULL,
    units                 REAL,
    allocation_pct        REAL,
    market_value          REAL    NOT NULL,
    document_id           INTEGER,
    source_sha256         TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, isin)
);

CREATE TABLE historical_cash_balances (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    currency              TEXT    NOT NULL,
    balance_kind          TEXT    NOT NULL,
    amount                REAL    NOT NULL,
    document_id           INTEGER,
    source_sha256         TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);
