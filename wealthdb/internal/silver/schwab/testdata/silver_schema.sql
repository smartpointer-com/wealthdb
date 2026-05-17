-- Minimal subset of schwab-dump's silver schema, sufficient for
-- adapter tests. Mirrors the column types of the upstream
-- migrations/0001_initial.sql but omits PRAGMAs and FK declarations
-- that don't affect what the adapter reads.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE account_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    balance_kind        TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, balance_kind)
);

CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    instrument_key      TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);

CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);
