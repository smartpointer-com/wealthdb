-- Minimal fidelity silver schema for adapter tests: the tables and
-- columns the adapter SELECTs. SQLite syntax.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE portfolios (
    snapshot_at           INTEGER NOT NULL,
    portfolio_external_id TEXT    NOT NULL,
    kind                  TEXT,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, portfolio_external_id)
);

CREATE TABLE accounts (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    portfolio_external_id TEXT,
    nickname              TEXT,
    management_style      TEXT,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    instrument_key      TEXT    NOT NULL,
    description         TEXT,
    asset_class         TEXT,
    currency            TEXT    NOT NULL DEFAULT 'USD',
    is_core_position    INTEGER NOT NULL DEFAULT 0,
    quantity            REAL,
    current_value       REAL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);

CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    instrument_key      TEXT,
    currency            TEXT    NOT NULL DEFAULT 'USD',
    quantity            REAL,
    price               REAL,
    amount              REAL,
    payload             TEXT    NOT NULL
);

CREATE TABLE historical_position_snapshots (
    as_of_date          INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    description         TEXT    NOT NULL,
    instrument_key      TEXT,
    quantity            REAL,
    price               REAL,
    market_value        REAL,
    percent_of_total    REAL,
    currency            TEXT NOT NULL DEFAULT 'USD',
    source_sha256       TEXT,
    payload             TEXT,
    PRIMARY KEY (as_of_date, account_external_id, description)
);
